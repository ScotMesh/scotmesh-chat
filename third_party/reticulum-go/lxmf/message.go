// Package lxmf is a minimal-viable LXMF implementation built on top of the
// pure-Go rns package. It implements opportunistic single-packet delivery
// (SPEC §5.1), direct (Link) delivery (SPEC §5.2), and sender-side
// propagation-node submission (SPEC §5.8).
//
// Outbound stamps (SPEC §5.7) are generated in both flavors: delivery
// stamps for recipients whose announce declares a stamp_cost, and
// propagation stamps for nodes that declare one. Inbound stamps are
// parsed and stripped for signature verification but never validated —
// the "PoW outbound, tolerate-but-don't-validate inbound" interop
// minimum of §5.7.4. Tickets (§5.7.3) and the propagation-node *server*
// role are intentionally out of scope.
package lxmf

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/thatSFguy/reticulum-go/rns"
)

// safeUnmarshal wraps msgpack.Unmarshal for inbound attacker-controlled
// payloads. All receive-path decodes route through this.
//
// It pre-validates structure because the library's allocation limit is
// BROKEN in the pinned version: vmihailenco/msgpack v5.4.1 tests
// `flags&disableAllocLimitFlag != 1` where the flag is 8, so the guard
// is always bypassed and both the typed and untyped slice paths
// allocate straight from the length header. A 5-byte array32 header
// claiming 2^32-1 elements requests ~103 GB — an unrecoverable Go
// runtime OOM, not a catchable panic.
//
// This is reachable pre-authentication: an attacker can Token-encrypt
// to our announced public key, and unpackPayload decodes the resulting
// array BEFORE the LXMF signature is verified. rns.ValidateMsgpackBounds
// rejects any length header the remaining bytes cannot satisfy, which
// is exactly the shape of that attack and costs nothing for real input.
// See internal/rns/msgpack_guard.go for the full analysis.
func safeUnmarshal(data []byte, v any) error {
	if err := rns.ValidateMsgpackBounds(data); err != nil {
		return err
	}
	return msgpack.Unmarshal(data, v)
}

// LXMF wire constants (SPEC §5).
const (
	signatureLen = 64

	// Opportunistic body = source_hash(16) || signature(64) || msgpack_payload
	// (the recipient's dest_hash is in the outer Reticulum packet header
	// and is omitted from the body itself).
	minOpportunisticBodyLen = rns.IdentityHashLen + signatureLen

	// MaxOpportunisticPayload is the upstream LXMF limit on the msgpack
	// payload size for a single-packet opportunistic LXMF message,
	// matching LXMessage.ENCRYPTED_PACKET_MAX_CONTENT in upstream Python
	// LXMF 0.9.6. Larger messages downgrade to link-based delivery in
	// upstream — which we don't implement yet — so we surface
	// ErrPayloadTooLarge instead.
	MaxOpportunisticPayload = 295
)

// ErrPayloadTooLarge is returned by SignAndPackOpportunistic / Delivery.Send
// when the msgpack payload would exceed MaxOpportunisticPayload. Callers can
// catch it with errors.Is to provide structured feedback (e.g. tell the
// original sender their message was too long for single-packet relay).
var ErrPayloadTooLarge = errors.New("LXMF opportunistic payload exceeds size limit")

// AppName + AspectDelivery yield the dotted full name "lxmf.delivery"
// (SPEC §1.2 / §4.4).
const (
	AppName           = "lxmf"
	AspectDelivery    = "delivery"
	AspectPropagation = "propagation"
)

// FullName returns "lxmf.delivery" — the well-known LXMF delivery aspect.
func FullName() string { return rns.FullName(AppName, AspectDelivery) }

// PropagationFullName returns "lxmf.propagation" — the well-known
// propagation-node aspect (SPEC §5.8.1, name_hash e03a09b77ac21b22258e).
func PropagationFullName() string { return rns.FullName(AppName, AspectPropagation) }

// Message is a parsed LXMF message. On the send side, fill in the
// addressing + content fields and call SignAndPackOpportunistic. On the
// receive side, ParseOpportunisticBody fills it; call Verify before
// trusting Title/Content.
type Message struct {
	// Addressing.
	DestHash   []byte // recipient's destination_hash (16 bytes, from outer packet header on receive)
	SourceHash []byte // sender's destination_hash (NOT identity hash — SPEC §5.4)

	// Crypto.
	Signature []byte // 64 bytes Ed25519

	// Payload — the four logical msgpack array elements (SPEC §5.3).
	Timestamp time.Time
	Title     []byte
	Content   []byte
	Fields    map[any]any // usually empty {}

	// Stamp (optional 5th msgpack element; SPEC §5.7). Nil when the
	// payload had no element [4] AND when it had one this decoder could
	// not read as bytes — use stampElement, not this, to decide whether
	// the payload is the 5-element form.
	Stamp []byte

	// stampElement records that the payload array carried a 5th element,
	// whatever it decoded to. Both the §5.5 message_id rule and the §5.6
	// variant-2 retry key off the ELEMENT's presence: a peer that emits a
	// null or non-bytes stamp still wrote a 5-element array, and hashing
	// or verifying it as a 4-element one diverges from every other client.
	stampElement bool

	// StampChecked reports whether the receive path actually evaluated
	// the stamp. False means no check was attempted — we require no
	// stamp, or validation was shed under load — NOT that the stamp was
	// absent. StampValid is meaningless unless this is true.
	StampChecked bool

	// StampValid reports whether the stamp cleared the required cost.
	StampValid bool

	// StampValue is the leading-zero count the stamp actually achieved
	// (§5.7.2 step 3). It can exceed the required cost — surplus work a
	// sender chose to spend, which an application may use to prioritise.
	StampValue int

	// rawPayload preserves the exact msgpack bytes as received, for use in
	// Verify per the SPEC §5.6 dual-variant tolerance rule.
	rawPayload []byte
}

// SignAndPackOpportunistic builds the opportunistic LXMF body bytes that
// go inside the Token-encrypted Reticulum DATA packet.
//
//	wire = source_hash(16) || signature(64) || msgpack_payload
//
// destHash is the recipient's destination_hash; senderID signs.
// senderDestHash is the SENDER's destination_hash (NOT the identity hash
// — SPEC §5.4). title may be nil. content may be nil but typically isn't.
// fields may be nil (encoded as an empty msgpack map). The returned
// msgID is the 32-byte LXMF message_id the recipient will compute when
// they parse this body — used by the forwarder to register per-recipient
// IDs for cross-client reaction / reply rewriting.
func SignAndPackOpportunistic(senderID *rns.Identity, senderDestHash, destHash []byte, title, content []byte, fields map[any]any) (wire, msgID []byte, err error) {
	return signAndPackOpportunisticAt(senderID, senderDestHash, destHash, title, content, fields, time.Now(), StampOptions{})
}

// SignAndPackOpportunisticStamped is SignAndPackOpportunistic with a
// §5.7 delivery stamp attached when opts.Cost > 0 — for recipients whose
// announce app_data declares a stamp_cost (§5.7.4). Blocks for the grind
// (expected 2^Cost hashes over a 768 KiB workblock) and fails with
// ErrStampCostTooHigh rather than grinding past opts.MaxCost.
//
// The stamp lands inside the single-packet budget, so a message that fits
// unstamped may return ErrPayloadTooLarge once stamped; the caller routes
// it to link delivery exactly as it would any other oversize message.
func SignAndPackOpportunisticStamped(senderID *rns.Identity, senderDestHash, destHash []byte, title, content []byte, fields map[any]any, opts StampOptions) (wire, msgID []byte, err error) {
	return signAndPackOpportunisticAt(senderID, senderDestHash, destHash, title, content, fields, time.Now(), opts)
}

// SignAndPackDirect builds the link-form (direct) LXMF body bytes for
// transmission inside a Reticulum Link DATA packet (SPEC §5.2):
//
//	destination_hash(16) || source_hash(16) || signature(64) || msgpack_payload
//
// Unlike SignAndPackOpportunistic, the body includes the destination
// hash (the outer packet is addressed to a link_id, not the recipient's
// destination). No size cap is enforced here — link DATA can carry
// arbitrary-size payloads, fragmented by the link layer.
func SignAndPackDirect(senderID *rns.Identity, senderDestHash, destHash []byte, title, content []byte, fields map[any]any) (wire, msgID []byte, err error) {
	return signAndPackDirectAt(senderID, senderDestHash, destHash, title, content, fields, time.Now(), StampOptions{})
}

// SignAndPackDirectStamped is SignAndPackDirect with a §5.7 delivery
// stamp attached when opts.Cost > 0. Same grind semantics as
// SignAndPackOpportunisticStamped, minus the size cap — link DATA has no
// single-packet budget for the stamp to eat into.
func SignAndPackDirectStamped(senderID *rns.Identity, senderDestHash, destHash []byte, title, content []byte, fields map[any]any, opts StampOptions) (wire, msgID []byte, err error) {
	return signAndPackDirectAt(senderID, senderDestHash, destHash, title, content, fields, time.Now(), opts)
}

func signAndPackDirectAt(senderID *rns.Identity, senderDestHash, destHash []byte, title, content []byte, fields map[any]any, ts time.Time, opts StampOptions) (wire, msgID []byte, err error) {
	payload, sig, id, err := packSignedAndStamped(senderID, senderDestHash, destHash, title, content, fields, ts, opts)
	if err != nil {
		return nil, nil, err
	}

	out := make([]byte, 0, 2*rns.IdentityHashLen+len(sig)+len(payload))
	out = append(out, destHash...)
	out = append(out, senderDestHash...)
	out = append(out, sig...)
	out = append(out, payload...)
	return out, id, nil
}

// signAndPackOpportunisticAt is the testable form: timestamp is injected
// rather than read from the wall clock, so deterministic test vectors
// (which pin the timestamp) can be reproduced exactly. The returned
// msgID is the recipient-view LXMF message_id (independent of signature
// — it's just H(dest||source||payload)).
func signAndPackOpportunisticAt(senderID *rns.Identity, senderDestHash, destHash []byte, title, content []byte, fields map[any]any, ts time.Time, opts StampOptions) (wire, msgID []byte, err error) {
	payload, sig, id, err := buildSignedPayload(senderID, senderDestHash, destHash, title, content, fields, ts)
	if err != nil {
		return nil, nil, err
	}

	// The stamp counts against the single-packet budget — upstream picks
	// PACKET vs RESOURCE representation from the STAMPED size — so a
	// message that only fits unstamped must still route to a link.
	//
	// Project that size BEFORE grinding. The stamp costs a fixed
	// stampPayloadOverhead bytes, so an oversize message is knowable from
	// the unstamped payload, and grinding first would burn a full
	// proof-of-work only to throw the result away: Delivery.SendWithID
	// answers ErrPayloadTooLarge by re-packing through sendOverLink, which
	// grinds again over its own message_id. That made every oversize
	// message to a stamp-demanding recipient pay twice.
	projected := len(payload)
	if opts.Cost > 0 {
		projected += stampPayloadOverhead
	}
	if projected > MaxOpportunisticPayload {
		return nil, nil, fmt.Errorf("%w: msgpack payload is %d bytes, limit is %d (link-based delivery for larger messages is not implemented)",
			ErrPayloadTooLarge, projected, MaxOpportunisticPayload)
	}

	payload, err = applyStamp(payload, id, opts)
	if err != nil {
		return nil, nil, err
	}
	out := make([]byte, 0, len(senderDestHash)+len(sig)+len(payload))
	out = append(out, senderDestHash...)
	out = append(out, sig...)
	out = append(out, payload...)
	return out, id, nil
}

// StampOptions controls outbound §5.7.2 delivery-stamp generation for one
// packed message. The zero value means "no stamp", which is what every
// pre-stamp call site gets.
type StampOptions struct {
	// Cost is the required leading-zero bit count, taken from element [1]
	// of the recipient's announce app_data (§4.3 / §5.7.4). Zero or
	// negative means the recipient asks for no stamp.
	Cost int

	// MaxCost refuses to grind past this many bits. Zero means
	// MaxDeliveryStampCost. See that constant for why a cap is mandatory
	// on a stranger-supplied cost.
	MaxCost int

	// Ticket, when set, replaces the grind entirely: the stamp becomes
	// SHA-256(ticket || message_id) truncated to TicketStampSize
	// (§5.7.3). Upstream checks for an outbound ticket BEFORE it looks
	// at stamp_cost (LXMessage.py:297-305), so a ticket applies even to
	// a recipient who announced no cost — which is what lets a ticket
	// holder keep proving the relationship for free.
	Ticket []byte
}

func (o StampOptions) maxCost() int {
	if o.MaxCost > 0 {
		return o.MaxCost
	}
	return MaxDeliveryStampCost
}

// packSignedAndStamped is the shared core of every outbound pack variant.
// It marshals and signs the 4-element payload, then — if opts asks for a
// stamp — grinds one over the message_id and splices it in as element
// [4] (§5.7.1).
//
// The returned sig and msgID always correspond to the FOUR-element
// payload, even when the returned payload has five: that is the whole
// point of the §5.6 strip-and-reverify rule, and it keeps message_id
// stable whether or not a stamp is attached (§5.5).
func packSignedAndStamped(senderID *rns.Identity, senderDestHash, destHash []byte, title, content []byte, fields map[any]any, ts time.Time, opts StampOptions) (payload, sig, msgID []byte, err error) {
	payload, sig, msgID, err = buildSignedPayload(senderID, senderDestHash, destHash, title, content, fields, ts)
	if err != nil {
		return nil, nil, nil, err
	}
	payload, err = applyStamp(payload, msgID, opts)
	if err != nil {
		return nil, nil, nil, err
	}
	return payload, sig, msgID, nil
}

// applyStamp grinds the §5.7.2 proof-of-work over msgID and splices it
// into payload as element [4]. A zero or negative opts.Cost returns the
// payload untouched.
//
// Kept separate from packSignedAndStamped so a caller that can reject a
// message on SIZE does so before paying for the proof-of-work — see
// signAndPackOpportunisticAt.
func applyStamp(payload, msgID []byte, opts StampOptions) ([]byte, error) {
	// Ticket first, matching upstream's ordering: a ticket short-
	// circuits the cost check rather than being an alternative to it.
	if len(opts.Ticket) == TicketLength {
		stamp, err := TicketStamp(opts.Ticket, msgID)
		if err != nil {
			return nil, err
		}
		return appendStamp(payload, stamp)
	}
	if opts.Cost <= 0 {
		return payload, nil
	}
	stamp, err := generateStamp(msgID, opts.Cost, workblockExpandRounds, opts.maxCost())
	if err != nil {
		return nil, err
	}
	return appendStamp(payload, stamp)
}

// appendStamp turns a signed 4-element payload into the 5-element
// stamped form of §5.7.1.
//
// It SPLICES rather than re-marshalling, for the same reason
// reencodeFirstFour does: elements [0..3] must come back byte-identical
// when the recipient strips the stamp and re-encodes for §5.6 variant-2
// verification. Re-marshalling a Go fields map is order-nondeterministic,
// so a re-marshalled 5-element payload would verify only by luck on any
// message with more than one field. Bumping the fixarray header 0x94 ->
// 0x95 and appending the stamp leaves the signed bytes untouched.
// stampPayloadOverhead is what a stamp adds to a packed payload: the
// fixarray header stays one byte wide (0x94 -> 0x95), plus a 2-byte bin8
// envelope and StampSize bytes of stamp.
const stampPayloadOverhead = 2 + StampSize

func appendStamp(payload, stamp []byte) ([]byte, error) {
	// A stamp is either a StampSize proof-of-work value or a
	// TicketStampSize ticket-derived one (§5.7.3). The ticket form is
	// HALF the length — upstream derives it with truncated_hash — so a
	// length check that only knows about StampSize refuses every
	// ticket-stamped message we try to send.
	if len(stamp) != StampSize && len(stamp) != TicketStampSize {
		return nil, fmt.Errorf("stamp must be %d or %d bytes, got %d", StampSize, TicketStampSize, len(stamp))
	}
	if len(payload) == 0 || payload[0] != 0x94 {
		return nil, errors.New("payload is not a 4-element msgpack fixarray")
	}
	// Canonical: this becomes payload element [4], and the whole
	// payload is what a §5.7.1 recipient re-packs (SPEC §5.6.1).
	encoded, err := canonicalMarshal(stamp)
	if err != nil {
		return nil, fmt.Errorf("marshal stamp: %w", err)
	}
	out := make([]byte, 0, len(payload)+len(encoded))
	out = append(out, 0x95) // fixarray, 5 elements
	out = append(out, payload[1:]...)
	out = append(out, encoded...)
	return out, nil
}

// buildSignedPayload is the shared core of every outbound pack variant:
// marshal the 4-element msgpack payload at the injected timestamp, then
// sign per SPEC §5.4. Size policy (the opportunistic 295-byte cap) is the
// caller's concern — propagated and direct forms have no single-packet
// limit.
func buildSignedPayload(senderID *rns.Identity, senderDestHash, destHash []byte, title, content []byte, fields map[any]any, ts time.Time) (payload, sig, msgID []byte, err error) {
	if senderID == nil {
		return nil, nil, nil, errors.New("nil sender identity")
	}
	if len(senderDestHash) != rns.IdentityHashLen || len(destHash) != rns.IdentityHashLen {
		return nil, nil, nil, errors.New("dest_hash and source_hash must each be 16 bytes")
	}
	if title == nil {
		title = []byte{}
	}
	if content == nil {
		content = []byte{}
	}
	if fields == nil {
		fields = map[any]any{}
	}

	tsSeconds := float64(ts.UnixMicro()) / 1_000_000.0
	// MUST be canonical. `fields` routinely arrives from a decoded
	// inbound message, whose integer keys and values are int8/uint8/
	// int64 rather than int — and a stamp-requiring recipient re-packs
	// these four elements with umsgpack before verifying our signature
	// (SPEC §5.6.1, §5.7.1). Non-canonical bytes here are dropped by
	// every stamped recipient. See lxmf/msgpack_canonical.go.
	payload, err = canonicalMarshal([]any{tsSeconds, title, content, fields})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal payload: %w", err)
	}
	signedData, id := buildSignedDataWithID(destHash, senderDestHash, payload)
	return payload, senderID.Sign(signedData), id, nil
}

// ParseDirectBody decodes the link-form LXMF body (SPEC §5.2):
//
//	destination_hash(16) || source_hash(16) || signature(64) || msgpack_payload
//
// Unlike opportunistic, the dest_hash is in the body (the outer Reticulum
// packet is addressed to a link_id, not the recipient's destination_hash).
// The returned Message can be Verify()'d the same way as opportunistic.
func ParseDirectBody(body []byte) (*Message, error) {
	const minDirectBodyLen = 2*rns.IdentityHashLen + signatureLen
	if len(body) < minDirectBodyLen {
		return nil, fmt.Errorf("direct body too short: %d", len(body))
	}
	m := &Message{
		DestHash:   body[:rns.IdentityHashLen],
		SourceHash: body[rns.IdentityHashLen : 2*rns.IdentityHashLen],
		Signature:  body[2*rns.IdentityHashLen : 2*rns.IdentityHashLen+signatureLen],
		rawPayload: body[2*rns.IdentityHashLen+signatureLen:],
	}
	if err := m.unpackPayload(); err != nil {
		return nil, err
	}
	return m, nil
}

// ParseOpportunisticBody decodes the LXMF body bytes (without dest_hash).
// destHash MUST come from the outer Reticulum packet header — passing the
// raw body bytes alone would let a malicious sender forge sigs against
// arbitrary destinations.
func ParseOpportunisticBody(body, destHash []byte) (*Message, error) {
	if len(body) < minOpportunisticBodyLen {
		return nil, fmt.Errorf("opportunistic body too short: %d", len(body))
	}
	if len(destHash) != rns.IdentityHashLen {
		return nil, fmt.Errorf("dest_hash must be %d bytes, got %d", rns.IdentityHashLen, len(destHash))
	}

	m := &Message{
		DestHash:   destHash,
		SourceHash: body[:rns.IdentityHashLen],
		Signature:  body[rns.IdentityHashLen : rns.IdentityHashLen+signatureLen],
		rawPayload: body[rns.IdentityHashLen+signatureLen:],
	}

	if err := m.unpackPayload(); err != nil {
		return nil, err
	}
	return m, nil
}

// Verify checks the Ed25519 signature using the SPEC §5.6 dual-variant rule:
// try the raw msgpack bytes first; if that fails AND the payload had an
// optional 5th element (stamp), retry with a stripped+re-encoded 4-element
// msgpack. Returns nil on success.
func (m *Message) Verify(senderEd25519Pub []byte) error {
	if len(senderEd25519Pub) != ed25519.PublicKeySize {
		return errors.New("sender Ed25519 public must be 32 bytes")
	}

	// Variant 1: raw bytes as-received.
	signedData := buildSignedData(m.DestHash, m.SourceHash, m.rawPayload)
	if rns.Validate(senderEd25519Pub, signedData, m.Signature) {
		return nil
	}

	// Variant 2: strip optional 5th element (stamp) and re-encode the
	// first 4. Attempt whenever the ELEMENT was present — a stamp we
	// could not decode as bytes still has to be stripped, or a peer's
	// malformed stamp turns into a dropped message.
	if !m.stampElement {
		return errors.New("LXMF signature invalid")
	}
	stripped, err := reencodeFirstFour(m.rawPayload)
	if err != nil {
		return fmt.Errorf("strip stamp re-encode: %w", err)
	}
	signedData = buildSignedData(m.DestHash, m.SourceHash, stripped)
	if rns.Validate(senderEd25519Pub, signedData, m.Signature) {
		return nil
	}
	return errors.New("LXMF signature invalid (both raw and stamp-stripped variants)")
}

// CheckOpportunisticSize returns nil if a SignAndPackOpportunistic call with
// these inputs would fit in a single Reticulum DATA packet, or an error
// wrapping ErrPayloadTooLarge if not. It does the msgpack marshal but no
// crypto and no network I/O — safe to call as a pre-check before iterating
// recipients.
//
// It answers for the UNSTAMPED form. A §5.7 stamp adds 34 bytes to the
// payload, so a message that passes here may still route to link delivery
// for a recipient who demands one — which is per-recipient information
// this call deliberately does not take.
func CheckOpportunisticSize(title, content []byte, fields map[any]any) error {
	if title == nil {
		title = []byte{}
	}
	if content == nil {
		content = []byte{}
	}
	if fields == nil {
		fields = map[any]any{}
	}
	// Same encoder as buildSignedPayload, or this predicts a size the
	// real pack will not produce.
	payload, err := canonicalMarshal([]any{0.0, title, content, fields})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if len(payload) > MaxOpportunisticPayload {
		return fmt.Errorf("%w: msgpack payload is %d bytes, limit is %d",
			ErrPayloadTooLarge, len(payload), MaxOpportunisticPayload)
	}
	return nil
}

func buildSignedData(destHash, sourceHash, msgpackPayload []byte) []byte {
	signedData, _ := buildSignedDataWithID(destHash, sourceHash, msgpackPayload)
	return signedData
}

// buildSignedDataWithID is buildSignedData but also returns the 32-byte
// SHA-256 hash inserted between hashedPart and the signature input — that
// hash IS the LXMF message_id (SPEC §5.4: H(dest||source||payload)). Two
// callers want both pieces; everyone else just calls buildSignedData.
func buildSignedDataWithID(destHash, sourceHash, msgpackPayload []byte) (signedData, msgID []byte) {
	hashedPart := make([]byte, 0, len(destHash)+len(sourceHash)+len(msgpackPayload))
	hashedPart = append(hashedPart, destHash...)
	hashedPart = append(hashedPart, sourceHash...)
	hashedPart = append(hashedPart, msgpackPayload...)
	mh := sha256.Sum256(hashedPart)
	out := make([]byte, 0, len(hashedPart)+len(mh))
	out = append(out, hashedPart...)
	out = append(out, mh[:]...)
	return out, append([]byte(nil), mh[:]...)
}

// ComputeMessageID returns the LXMF message_id for a given (dest_hash,
// source_hash, msgpack_payload) tuple — SPEC §5.4 defines it as
// SHA-256(dest_hash || source_hash || msgpack_payload). Used by the
// forwarding service to register per-recipient message_ids in the
// id-rewrite cache without re-packing the body.
func ComputeMessageID(destHash, sourceHash, msgpackPayload []byte) []byte {
	mh := sha256.Sum256(append(append(append([]byte{}, destHash...), sourceHash...), msgpackPayload...))
	return mh[:]
}

// MessageID returns the LXMF message_id (32 bytes, SPEC §5.4) for a
// parsed inbound message. Only valid after a successful Parse; Verify is
// not required, since message_id is independent of the signature.
//
// NOTE: message_id is MALLEABLE and must not be used to suppress
// replays — see DedupKey. It remains the correct identifier for
// reply-to / reaction binding, because that is what peer clients
// compute and reference.
//
// For a STAMPED message the hash covers the first four payload elements
// only (SPEC §5.5), not the 5-element array on the wire. Hashing the
// stamp too would make message_id depend on the sender's proof-of-work,
// so the id our own SignAndPack*Stamped returned would not match the one
// the recipient derives — and every reaction or reply either side binds
// to it would miss.
func (m *Message) MessageID() []byte {
	payload := m.rawPayload
	if m.stampElement {
		// The error is unreachable in practice — unpackPayload already
		// decoded this array — and MessageID has no way to report one.
		// Falling back to the raw payload keeps the id defined; it would
		// diverge from peers, which is why the splice is tried first.
		if stripped, err := reencodeFirstFour(payload); err == nil {
			payload = stripped
		}
	}
	return ComputeMessageID(m.DestHash, m.SourceHash, payload)
}

// DedupKey returns a replay-resistant identity for a VERIFIED inbound
// message: SHA-256(source_hash || signature).
//
// WHY NOT message_id: SPEC §5.6 lets a stamp be added, changed or
// removed without invalidating the signature, and message_id is
// computed over the raw stamp-inclusive payload. So the same captured,
// genuinely-signed body yields a different message_id for every stamp
// value — an attacker can replay one message unboundedly and every copy
// looks new. Keying on the signature instead is stable under exactly
// the mutation the spec permits, and cannot be forged for different
// content without the sender's private key. Ed25519 is deterministic,
// so a genuine resend of identical content also collapses correctly,
// while two distinct sends differ (the signed payload carries the
// sender's timestamp).
//
// Only meaningful after Verify has succeeded.
func (m *Message) DedupKey() []byte {
	h := sha256.New()
	h.Write(m.SourceHash)
	h.Write(m.Signature)
	return h.Sum(nil)
}

// msgpackNil is the msgpack format byte for nil (0xc0).
const msgpackNil = 0xc0

// decodeFields decodes the LXMF "fields" payload element into a
// map[any]any with interface-keyed maps at EVERY nesting level.
//
// The default msgpack interface-map decoder produces
// map[string]interface{} for nested map *values*, which rejects the
// integer-keyed inner dicts that FIELD_REACTION (0x40), FIELD_COMMENT
// (0x41), and FIELD_CONTINUATION (0x42) use: it fails with "invalid
// code=0 decoding string/bytes length" the moment it hits inner key
// 0x00, and the whole message is dropped before it ever reaches the
// relay logic. (Top-level reply-to 0x30 raw bytes and image arrays
// decode fine, which is why only reactions/comments/continuations
// vanished.) Wiring DecodeUntypedMap in as the map decoder makes every
// level decode into map[any]any regardless of key type.
func decodeFields(raw []byte) (map[any]any, error) {
	// msgpack nil → no fields (tolerated like an empty map).
	if len(raw) == 1 && raw[0] == msgpackNil {
		return nil, nil
	}
	// Guarded separately from safeUnmarshal: this path builds its own
	// Decoder, and DecodeUntypedMap descends into nested ARRAYS via the
	// untyped decodeSlice path — which has the same unclamped
	// make([]interface{}, 0, n) as the typed path. A field value like
	// {6: <array32 claiming 2^32-1>} would otherwise be a remote OOM.
	if err := rns.ValidateMsgpackBounds(raw); err != nil {
		return nil, err
	}
	dec := msgpack.NewDecoder(bytes.NewReader(raw))
	dec.SetMapDecoder(func(d *msgpack.Decoder) (interface{}, error) {
		return d.DecodeUntypedMap()
	})
	return dec.DecodeUntypedMap()
}

// unpackPayload extracts Timestamp/Title/Content/Fields/Stamp from rawPayload.
func (m *Message) unpackPayload() error {
	var elems []msgpack.RawMessage
	if err := safeUnmarshal(m.rawPayload, &elems); err != nil {
		return fmt.Errorf("decode payload array: %w", err)
	}
	if len(elems) < 4 {
		return fmt.Errorf("payload has %d elements, need at least 4", len(elems))
	}

	var tsSeconds float64
	if err := safeUnmarshal(elems[0], &tsSeconds); err != nil {
		return fmt.Errorf("decode timestamp: %w", err)
	}
	whole, frac := splitSeconds(tsSeconds)
	m.Timestamp = time.Unix(whole, frac).UTC()

	if err := safeUnmarshal(elems[1], &m.Title); err != nil {
		// Tolerate msgpack str — some implementations write title as str.
		var titleStr string
		if err2 := safeUnmarshal(elems[1], &titleStr); err2 == nil {
			m.Title = []byte(titleStr)
		} else {
			return fmt.Errorf("decode title: %w", err)
		}
	}
	if err := safeUnmarshal(elems[2], &m.Content); err != nil {
		var contentStr string
		if err2 := safeUnmarshal(elems[2], &contentStr); err2 == nil {
			m.Content = []byte(contentStr)
		} else {
			return fmt.Errorf("decode content: %w", err)
		}
	}
	fields, err := decodeFields(elems[3])
	if err != nil {
		return fmt.Errorf("decode fields: %w", err)
	}
	m.Fields = fields
	if len(elems) >= 5 {
		m.stampElement = true
		_ = safeUnmarshal(elems[4], &m.Stamp) // best-effort; stamp is optional
	}
	return nil
}

// reencodeFirstFour decodes the msgpack array, drops everything past
// element [3], and re-encodes — used to strip an optional stamp before
// signature verification (SPEC §5.6).
// reencodeFirstFour rebuilds the 4-element signed payload from a
// 5-element (stamped) one, for SPEC §5.6 variant-2 verification.
//
// It SPLICES the original element bytes rather than decoding and
// re-marshalling them. The previous decode/re-encode approach was broken
// twice over:
//
//   - It decoded with the DEFAULT map decoder, which cannot handle the
//     integer-keyed inner dicts real LXMF fields use (FIELD_REPLY_TO
//     0x30, FIELD_REACTION 0x40, FIELD_IMAGE 0x06) — exactly the failure
//     decodeFields exists to work around. Every stamped message carrying
//     those fields failed variant 2 and was dropped, so stamp-using
//     senders silently lost all reactions, replies and images.
//   - Re-marshalling a Go map is order-nondeterministic, so even with a
//     working decoder a multi-key fields map would produce different
//     bytes than the sender signed and verify only by luck.
//
// Splicing sidesteps both: the bytes handed to Verify are byte-identical
// to what the sender signed. The 0x94 prefix is msgpack fixarray-of-4,
// which is what any encoder emits for a 4-element array.
func reencodeFirstFour(payload []byte) ([]byte, error) {
	var elems []msgpack.RawMessage
	if err := safeUnmarshal(payload, &elems); err != nil {
		return nil, err
	}
	if len(elems) < 4 {
		return nil, errors.New("payload has fewer than 4 elements")
	}
	out := make([]byte, 0, len(payload))
	out = append(out, 0x94) // fixarray, 4 elements
	for i := 0; i < 4; i++ {
		out = append(out, elems[i]...)
	}
	return out, nil
}

func splitSeconds(secs float64) (whole int64, nanos int64) {
	w := int64(secs)
	frac := secs - float64(w)
	if frac < 0 {
		frac = 0
	}
	return w, int64(frac * 1e9)
}
