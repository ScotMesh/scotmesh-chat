package lxmf

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

// SPEC §5.8.3 — client-side retrieval of messages a propagation node is
// holding for us. This is the other half of SendPropagated: without it
// offline delivery is one-way, because we can post for others but never
// collect our own.
//
// The exchange is three §11 REQUESTs on one identified link:
//
//  1. /get [nil, nil]          -> list of transient_ids held for us
//  2. /get [wants, haves, kb]  -> the bodies we asked for; anything in
//     `haves` is deleted from the node's store
//  3. /get [nil, haves]        -> acknowledge what round 2 delivered,
//     which is what actually purges it
//
// The node answers only with messages whose stored recipient matches the
// identity that ran §6.7.6 LINKIDENTIFY on the link, so identifying is
// mandatory rather than optional: an unidentified /get gets
// ErrPropagationNoIdentity back.

// MessageGetPath is the §11.3 path a propagation node registers for
// retrieval (upstream LXMPeer.MESSAGE_GET_PATH).
const MessageGetPath = "/get"

// Propagation-node error responses (LXMF/LXMPeer.py:24-31). A /get
// answer that is an integer rather than a list is one of these. The
// values are NOT contiguous — 0xf2 is unallocated — so match them
// exactly rather than range-checking.
const (
	propErrNoIdentity  = 0xf0
	propErrNoAccess    = 0xf1
	propErrInvalidKey  = 0xf3
	propErrInvalidData = 0xf4
	propErrInvalidStam = 0xf5
	propErrThrottled   = 0xf6
	propErrNotFound    = 0xfd
	propErrTimeout     = 0xfe
)

var (
	// ErrPropagationNoIdentity means the node did not see a §6.7.6
	// identification on the link, so it will not say what it holds.
	ErrPropagationNoIdentity = errors.New("propagation node: no identity presented (0xf0)")
	// ErrPropagationNoAccess means the node knows who we are and is
	// refusing us anyway.
	ErrPropagationNoAccess = errors.New("propagation node: access denied (0xf1)")
	// ErrPropagationThrottled means back off — the node is rate-limiting
	// this client. Retrying immediately makes it worse.
	ErrPropagationThrottled = errors.New("propagation node: throttled (0xf6)")
)

// RetrievedMessage is one message collected from a propagation node.
type RetrievedMessage struct {
	// Message is the parsed, signature-verified LXMF message.
	Message *Message
	// TransientID is the node's store key for it: SHA-256 of the body
	// exactly as the node returned it. Acknowledging this id is what
	// deletes the message from the node.
	TransientID []byte
	// SenderVerified reports whether the signature checked out against
	// a sender we could recall. False means the body parsed but we had
	// no key to verify it with — the message is NOT trustworthy.
	SenderVerified bool
}

// RetrieveOptions tunes one retrieval round.
type RetrieveOptions struct {
	// Have reports whether we already hold a transient_id, so it is
	// acknowledged (and purged from the node) instead of re-downloaded.
	// Nil means "we have nothing", which downloads everything on offer.
	Have func(transientID []byte) bool

	// MaxMessages caps how many to fetch in this round. 0 = no cap.
	MaxMessages int

	// TransferLimitKB caps the total transfer the node will assemble,
	// passed as element [2] of the /get. 0 omits it, which lets the
	// node send everything wanted.
	TransferLimitKB int

	// RetainOnNode suppresses the purge rounds, leaving messages on the
	// node after download. Upstream's equivalent is retain_synced_on_node.
	RetainOnNode bool

	// Timeout bounds each individual request. 0 = the rns default.
	Timeout time.Duration
}

// RetrieveResult summarises what one call collected.
type RetrieveResult struct {
	// Offered is every transient_id the node listed for us.
	Offered [][]byte
	// Messages are the ones downloaded and parsed this round.
	Messages []*RetrievedMessage
	// Acknowledged are the ids we told the node it may delete —
	// downloaded ones plus any the Have callback already claimed.
	Acknowledged [][]byte
	// Failed maps hex(transient_id) to the reason a downloaded body
	// could not be parsed or verified. Those are NOT acknowledged, so
	// the node keeps them for another attempt.
	Failed map[string]error
}

// RetrievePropagated runs one §5.8.3 retrieval round against the
// propagation node at nodeDestHash. It blocks for the whole exchange.
//
// Messages that parse and verify are returned; ones that do not are
// reported in Failed and deliberately left on the node, because a
// parse failure is more likely to be our bug than the node's and
// acknowledging would destroy the evidence.
func (d *Delivery) RetrievePropagated(nodeDestHash []byte, opts RetrieveOptions) (*RetrieveResult, error) {
	if len(nodeDestHash) != rns.IdentityHashLen {
		return nil, fmt.Errorf("propagation node dest_hash must be %d bytes", rns.IdentityHashLen)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = rns.DefaultRequestTimeout
	}

	link, err := d.transport.AcquireLink(nodeDestHash, d.propagationTimeout())
	if err != nil {
		return nil, fmt.Errorf("link to propagation node %x: %w", nodeDestHash[:4], err)
	}
	// The node keys every stored message to a recipient destination and
	// will not answer until it knows which one is asking (§5.8.3).
	if err := d.transport.IdentifyOnLink(link.ID, d.identity); err != nil {
		return nil, fmt.Errorf("identify on propagation link: %w", err)
	}

	result := &RetrieveResult{Failed: map[string]error{}}

	// Round 1: what do you have for me?
	listing, err := d.propagationGet(link.ID, []any{nil, nil}, timeout)
	if err != nil {
		return nil, err
	}
	offered, err := asTransientIDList(listing)
	if err != nil {
		return nil, fmt.Errorf("message listing: %w", err)
	}
	result.Offered = offered
	if len(offered) == 0 {
		return result, nil
	}

	// Split into what we want and what we can already acknowledge.
	var wants, haves [][]byte
	for _, tid := range offered {
		if opts.Have != nil && opts.Have(tid) {
			if !opts.RetainOnNode {
				haves = append(haves, tid)
			}
			continue
		}
		if opts.MaxMessages > 0 && len(wants) >= opts.MaxMessages {
			continue
		}
		wants = append(wants, tid)
	}
	if len(wants) == 0 && len(haves) == 0 {
		return result, nil
	}

	// Round 2: fetch the wanted ones, purging the already-held ones in
	// the same call.
	req := []any{toAnyList(wants), toAnyList(haves)}
	if opts.TransferLimitKB > 0 {
		req = append(req, opts.TransferLimitKB)
	}
	fetched, err := d.propagationGet(link.ID, req, timeout)
	if err != nil {
		return nil, err
	}
	result.Acknowledged = append(result.Acknowledged, haves...)

	bodies, err := asBodyList(fetched)
	if err != nil {
		return nil, fmt.Errorf("message download: %w", err)
	}

	var downloaded [][]byte
	for _, body := range bodies {
		tid := sha256.Sum256(body)
		msg, verified, perr := d.parseRetrieved(body)
		if perr != nil {
			result.Failed[fmt.Sprintf("%x", tid[:])] = perr
			continue
		}
		result.Messages = append(result.Messages, &RetrievedMessage{
			Message:        msg,
			TransientID:    tid[:],
			SenderVerified: verified,
		})
		downloaded = append(downloaded, tid[:])
	}

	// Round 3: acknowledge what actually arrived, which is what deletes
	// it from the node. Skipped under RetainOnNode.
	if len(downloaded) > 0 && !opts.RetainOnNode {
		if _, err := d.propagationGet(link.ID, []any{nil, toAnyList(downloaded)}, timeout); err != nil {
			// The messages are ours either way; a failed purge just
			// means the node offers them again next round, and Have
			// will filter them out.
			return result, fmt.Errorf("purge round: %w", err)
		}
		result.Acknowledged = append(result.Acknowledged, downloaded...)
	}
	return result, nil
}

// propagationGet issues one /get and maps a node error constant onto an
// error rather than letting it look like an empty result.
func (d *Delivery) propagationGet(linkID []byte, data []any, timeout time.Duration) (any, error) {
	receipt, err := d.transport.SendRequest(linkID, MessageGetPath, data)
	if err != nil {
		return nil, fmt.Errorf("send /get: %w", err)
	}
	resp, err := receipt.Response(timeout)
	if err != nil {
		return nil, fmt.Errorf("/get: %w", err)
	}
	if code, ok := asErrorCode(resp); ok {
		switch code {
		case propErrNoIdentity:
			return nil, ErrPropagationNoIdentity
		case propErrNoAccess:
			return nil, ErrPropagationNoAccess
		case propErrThrottled:
			return nil, ErrPropagationThrottled
		default:
			return nil, fmt.Errorf("propagation node error 0x%02x", code)
		}
	}
	return resp, nil
}

// parseRetrieved decrypts and verifies one retrieved body.
//
// The node returns the propagated form with the §5.7 propagation stamp
// STRIPPED — upstream slices it off before responding
// (LXMRouter.py:1549) — so what arrives is dest_hash(16) ‖ ciphertext,
// which decrypts to the ordinary opportunistic body.
func (d *Delivery) parseRetrieved(body []byte) (*Message, bool, error) {
	if len(body) <= rns.IdentityHashLen {
		return nil, false, fmt.Errorf("body is %d bytes, too short to carry dest_hash + ciphertext", len(body))
	}
	destHash := body[:rns.IdentityHashLen]
	plain, err := d.decryptInbound(body[rns.IdentityHashLen:])
	if err != nil {
		return nil, false, fmt.Errorf("decrypt: %w", err)
	}
	msg, err := ParseOpportunisticBody(plain, destHash)
	if err != nil {
		return nil, false, fmt.Errorf("parse: %w", err)
	}
	sender := d.transport.Recall(msg.SourceHash)
	if sender == nil {
		// Parsed but unverifiable: we have never seen this sender
		// announce, so we hold no key to check the signature with.
		return msg, false, nil
	}
	if err := msg.Verify(sender.Ed25519Public()); err != nil {
		return nil, false, fmt.Errorf("verify: %w", err)
	}
	return msg, true, nil
}

func (d *Delivery) propagationTimeout() time.Duration {
	if d.PropagationSendTimeout > 0 {
		return d.PropagationSendTimeout
	}
	return DefaultPropagationSendTimeout
}

// asErrorCode reports whether a /get answer is one of the §5.8.2 error
// constants rather than a payload.
func asErrorCode(resp any) (int, bool) {
	switch v := resp.(type) {
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// asTransientIDList decodes the round-1 answer: a list of 32-byte
// transient_ids, sorted by message size ascending by the node.
func asTransientIDList(resp any) ([][]byte, error) {
	if resp == nil {
		return nil, nil
	}
	items, ok := resp.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list, got %T", resp)
	}
	out := make([][]byte, 0, len(items))
	for i, it := range items {
		b, ok := it.([]byte)
		if !ok {
			return nil, fmt.Errorf("element %d is %T, want bytes", i, it)
		}
		out = append(out, append([]byte(nil), b...))
	}
	return out, nil
}

// asBodyList decodes the round-2 answer.
//
// Upstream returns a FLAT list of lxmf_data bodies (LXMRouter.py:1557
// `return response_messages`), not the [timestamp, [bodies]] envelope
// used for UPLOAD bundles. SPEC §5.8.3 currently describes the upload
// shape here; upstream is authoritative and this follows upstream.
func asBodyList(resp any) ([][]byte, error) {
	if resp == nil {
		return nil, nil
	}
	items, ok := resp.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list of bodies, got %T", resp)
	}
	out := make([][]byte, 0, len(items))
	for i, it := range items {
		b, ok := it.([]byte)
		if !ok {
			return nil, fmt.Errorf("body %d is %T, want bytes", i, it)
		}
		out = append(out, append([]byte(nil), b...))
	}
	return out, nil
}

func toAnyList(ids [][]byte) any {
	if len(ids) == 0 {
		return nil
	}
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
