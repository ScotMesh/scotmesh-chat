package rns

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// SPEC §11 REQUEST/RESPONSE — the generic over-Link RPC that NomadNet
// page fetches, LXMF propagation-node retrieval (§5.8.3) and any
// application RPC all ride on. There is no separate "NomadNet wire
// format"; NomadNet is one consumer of this.
const (
	// ContextRequest / ContextResponse are the link-DATA context bytes
	// carrying the two halves (§11.1, §11.2).
	ContextRequest  = 0x09
	ContextResponse = 0x0A

	// RequestIDLen is the truncation both sides key correlation on.
	RequestIDLen = 16
)

// AllowMode is the §11.4 authorization mode a request handler is
// registered under.
type AllowMode byte

const (
	// AllowNone rejects every request — a stub for testing.
	AllowNone AllowMode = 0x00
	// AllowList accepts only requesters that have proved an identity
	// over §6.7.6 LINKIDENTIFY AND whose identity hash is listed.
	AllowList AllowMode = 0x01
	// AllowAll accepts any request arriving on the link.
	AllowAll AllowMode = 0x02
)

var (
	// ErrRequestNoHandler is returned when no handler is registered for
	// the requested path hash. Upstream drops such a request silently;
	// we surface it so the caller can log.
	ErrRequestNoHandler = errors.New("no request handler for path")
	// ErrRequestNotAllowed is returned when the §11.4 allow mode
	// refuses the requester.
	ErrRequestNotAllowed = errors.New("request not allowed by handler policy")
	// ErrResponseUnsolicited is returned for a RESPONSE whose element
	// [0] matches no in-flight request — see RegisterPendingRequest.
	ErrResponseUnsolicited = errors.New("RESPONSE request_id matches no pending request")
	// ErrRequestTimeout is delivered to a receipt whose response never
	// arrived.
	ErrRequestTimeout = errors.New("request timed out")
)

// RequestPathHash is the §11.3 handler key: SHA-256 of the path string,
// truncated to 16 bytes. The path itself never goes on the wire, which
// is what lets a server publish opaque, unenumerable path tokens.
func RequestPathHash(path string) []byte {
	sum := sha256.Sum256([]byte(path))
	return sum[:RequestIDLen]
}

// PackRequest builds the §11.1 REQUEST body:
//
//	msgpack([timestamp float64, request_path_hash bytes(16), data])
//
// `data` is the application value ITSELF — nil, a map, a slice, a
// string, bytes — encoded directly into the outer list.
//
// The whole envelope is packed EXACTLY ONCE. Passing a pre-msgpacked
// blob as `data` produces an envelope whose element [2] decodes back as
// bytes rather than as the map or list the server expects, and every
// upstream handler tests the decoded type (`isinstance(data, dict)` /
// `isinstance(data, list)`) and silently drops the request with no
// error response. §11.1 calls this out as the common implementer
// mistake; it breaks every form submission and every propagation poll.
func PackRequest(pathHash []byte, data any, ts time.Time) ([]byte, error) {
	if len(pathHash) != RequestIDLen {
		return nil, fmt.Errorf("request_path_hash must be %d bytes, got %d", RequestIDLen, len(pathHash))
	}
	tsSeconds := float64(ts.UnixMicro()) / 1_000_000.0
	// `data` is caller-supplied `any` (SPEC §11.1 element [2]) and may
	// well be a decoded inbound value, so this is the one site in this
	// package with a live path to non-canonical integers.
	packed, err := canonicalMarshal([]any{tsSeconds, pathHash, data})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	return packed, nil
}

// ParseRequest decodes a §11.1 REQUEST body. The returned data is the
// decoded application value, with maps at every nesting level decoded
// as map[any]any so integer-keyed dicts survive — the same decoder
// concern LXMF fields have.
func ParseRequest(plaintext []byte) (ts time.Time, pathHash []byte, data any, err error) {
	elems, err := decodeRPCEnvelope(plaintext, 3)
	if err != nil {
		return time.Time{}, nil, nil, err
	}
	var tsSeconds float64
	if err := safeUnmarshalAnnounce(elems[0], &tsSeconds); err != nil {
		return time.Time{}, nil, nil, fmt.Errorf("decode request timestamp: %w", err)
	}
	whole := int64(tsSeconds)
	frac := int64((tsSeconds - float64(whole)) * 1e9)
	if frac < 0 {
		frac = 0
	}
	if err := safeUnmarshalAnnounce(elems[1], &pathHash); err != nil {
		return time.Time{}, nil, nil, fmt.Errorf("decode request_path_hash: %w", err)
	}
	if len(pathHash) != RequestIDLen {
		return time.Time{}, nil, nil, fmt.Errorf("request_path_hash is %d bytes, want %d", len(pathHash), RequestIDLen)
	}
	data, err = decodeRPCValue(elems[2])
	if err != nil {
		return time.Time{}, nil, nil, fmt.Errorf("decode request data: %w", err)
	}
	return time.Unix(whole, frac).UTC(), pathHash, data, nil
}

// PackResponse builds the §11.2 RESPONSE body:
//
//	msgpack([request_id bytes(16), response])
//
// Element [0] is what lets an initiator match this to the right
// outbound REQUEST when several are in flight on one link.
func PackResponse(requestID []byte, response any) ([]byte, error) {
	if len(requestID) != RequestIDLen {
		return nil, fmt.Errorf("request_id must be %d bytes, got %d", RequestIDLen, len(requestID))
	}
	// `response` is application-supplied; same reasoning as PackRequest.
	packed, err := canonicalMarshal([]any{requestID, response})
	if err != nil {
		return nil, fmt.Errorf("marshal response: %w", err)
	}
	return packed, nil
}

// ParseResponse decodes a §11.2 RESPONSE body.
func ParseResponse(plaintext []byte) (requestID []byte, response any, err error) {
	elems, err := decodeRPCEnvelope(plaintext, 2)
	if err != nil {
		return nil, nil, err
	}
	if err := safeUnmarshalAnnounce(elems[0], &requestID); err != nil {
		return nil, nil, fmt.Errorf("decode response request_id: %w", err)
	}
	if len(requestID) != RequestIDLen {
		return nil, nil, fmt.Errorf("response request_id is %d bytes, want %d", len(requestID), RequestIDLen)
	}
	response, err = decodeRPCValue(elems[1])
	if err != nil {
		return nil, nil, fmt.Errorf("decode response value: %w", err)
	}
	return requestID, response, nil
}

// decodeRPCEnvelope splits an inbound §11 envelope into raw elements,
// through the same bounds guard every other attacker-controlled decode
// uses.
func decodeRPCEnvelope(plaintext []byte, want int) ([]msgpack.RawMessage, error) {
	if err := ValidateMsgpackBounds(plaintext); err != nil {
		return nil, err
	}
	var elems []msgpack.RawMessage
	if err := safeUnmarshalAnnounce(plaintext, &elems); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if len(elems) < want {
		return nil, fmt.Errorf("envelope has %d elements, want at least %d", len(elems), want)
	}
	return elems, nil
}

// decodeRPCValue decodes one application-defined element. msgpack nil
// yields a nil interface. Maps decode untyped at every level so an
// integer-keyed dict does not blow up the way LXMF's fields map did.
//
// A nil element reaches us as an EMPTY RawMessage under this msgpack
// library, not as a one-byte 0xc0 — the same shape that made the
// announce stamp_cost decoder report a malformed field for an ordinary
// "nothing here". Both forms mean nil; decoding either would otherwise
// fail with EOF and drop every plain GET, whose element [2] is exactly
// this.
func decodeRPCValue(raw []byte) (any, error) {
	if len(raw) == 0 || (len(raw) == 1 && raw[0] == msgpackNilByte) {
		return nil, nil
	}
	dec := msgpack.NewDecoder(bytes.NewReader(raw))
	dec.SetMapDecoder(func(d *msgpack.Decoder) (any, error) {
		return d.DecodeUntypedMap()
	})
	return dec.DecodeInterface()
}

// RequestIDFromPacket computes the §11.1 request_id for a SINGLE-PACKET
// REQUEST: SHA-256 of the packet's hashable part, truncated to 16.
//
// This is the trap §11.2 spends a callout on. It is the hash of the
// ON-THE-WIRE packet bytes (low nibble of flags || raw[2:], skipping
// the transport_id slot for HEADER_2), NOT of the inner plaintext and
// NOT of the msgpack envelope. Upstream's server side is literally
// `packet.getTruncatedHash()` (RNS/Link.py:1286). An implementation
// that hashes the plaintext computes an id the server never sends, so
// every response is dropped as unsolicited and every page fetch and
// /get round times out in silence.
func RequestIDFromPacket(p *Packet) ([]byte, error) {
	hashable, err := p.HashablePart()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(hashable)
	return sum[:RequestIDLen], nil
}

// RequestIDFromPacked computes the request_id for a RESOURCE-form
// REQUEST: SHA-256 of the packed envelope, truncated to 16 (§11.1,
// RNS/Link.py:504). The Resource path uses the plaintext-hash form
// because there is no single packet to hash; the id travels explicitly
// in the advertisement's `q` field.
func RequestIDFromPacked(packedRequest []byte) []byte {
	sum := sha256.Sum256(packedRequest)
	return sum[:RequestIDLen]
}

// RequestContext is what a handler is given for one inbound request.
type RequestContext struct {
	// Path is the registered path string this handler was found under.
	Path string
	// PathHash is its §11.3 hash, as it arrived on the wire.
	PathHash []byte
	// Data is the decoded application value from element [2].
	Data any
	// Timestamp is the requester's clock reading from element [0].
	// Attacker-controlled: do not use it to make trust decisions.
	Timestamp time.Time
	// LinkID identifies the link the request arrived on.
	LinkID []byte
	// RemoteIdentity is the requester's 64-byte public key when they
	// have identified over §6.7.6, else nil. AllowList is enforced
	// against its identity hash before the handler runs, so a handler
	// registered AllowList can rely on this being non-nil.
	RemoteIdentity []byte
}

// RequestHandler answers one path. Returning an error means no response
// is sent — upstream's generator returning None behaves the same way.
type RequestHandler func(*RequestContext) (any, error)

type requestHandlerEntry struct {
	path        string
	allow       AllowMode
	allowedList [][]byte // identity hashes, AllowList only
	handler     RequestHandler
}

// RegisterRequestHandler registers a §11 handler at `path`, keyed by
// its §11.3 hash. Re-registering a path replaces the entry.
//
// allowed is consulted only for AllowList, and holds 16-byte identity
// hashes (not public keys) — matching upstream, whose allowed_list is
// compared against `identity.hash`.
func (t *Transport) RegisterRequestHandler(path string, allow AllowMode, allowed [][]byte, h RequestHandler) error {
	if path == "" {
		return errors.New("request handler path must not be empty")
	}
	if h == nil {
		return errors.New("nil request handler")
	}
	if allow == AllowList && len(allowed) == 0 {
		return errors.New("AllowList handler registered with an empty allowed list; it would refuse everyone")
	}
	entry := &requestHandlerEntry{path: path, allow: allow, handler: h}
	for _, a := range allowed {
		entry.allowedList = append(entry.allowedList, append([]byte(nil), a...))
	}
	t.requestMu.Lock()
	defer t.requestMu.Unlock()
	if t.requestHandlers == nil {
		t.requestHandlers = make(map[string]*requestHandlerEntry)
	}
	t.requestHandlers[hex.EncodeToString(RequestPathHash(path))] = entry
	return nil
}

// UnregisterRequestHandler removes the handler at `path`, if any.
func (t *Transport) UnregisterRequestHandler(path string) {
	t.requestMu.Lock()
	defer t.requestMu.Unlock()
	delete(t.requestHandlers, hex.EncodeToString(RequestPathHash(path)))
}

func (t *Transport) lookupRequestHandler(pathHash []byte) *requestHandlerEntry {
	t.requestMu.Lock()
	defer t.requestMu.Unlock()
	return t.requestHandlers[hex.EncodeToString(pathHash)]
}

// permits applies the §11.4 allow mode. remoteIdentity is the link's
// proved public key, or nil when the peer never identified.
func (e *requestHandlerEntry) permits(remoteIdentity []byte) bool {
	switch e.allow {
	case AllowAll:
		return true
	case AllowList:
		if remoteIdentity == nil {
			return false
		}
		want := IdentityHashFromPublicKey(remoteIdentity)
		for _, a := range e.allowedList {
			if bytes.Equal(a, want) {
				return true
			}
		}
		return false
	default: // AllowNone
		return false
	}
}

// RequestReceipt is the initiator-side handle for one outbound request
// (§11.5). Response delivers exactly once.
type RequestReceipt struct {
	// ID is the 16-byte request_id this receipt correlates on.
	ID []byte
	// Path is the path string requested.
	Path string
	// LinkID is the link the request went out on.
	LinkID []byte

	ch   chan requestResult
	once sync.Once
}

type requestResult struct {
	response any
	err      error
}

// Response blocks until the response arrives, the request times out, or
// ctx-less timeout elapses. Safe to call once; subsequent calls return
// the same outcome only if it has not been consumed.
func (r *RequestReceipt) Response(timeout time.Duration) (any, error) {
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-r.ch:
		return res.response, res.err
	case <-timer.C:
		return nil, fmt.Errorf("%w: %x after %s", ErrRequestTimeout, r.ID[:4], timeout)
	}
}

func (r *RequestReceipt) deliver(res requestResult) {
	r.once.Do(func() {
		select {
		case r.ch <- res:
		default:
		}
	})
}

// DefaultRequestTimeout is how long Response waits when the caller
// passes 0. Upstream's default is derived from link RTT plus a
// Resource grace period; this is a flat, generous stand-in.
const DefaultRequestTimeout = 30 * time.Second
