package wire

// Derived from rrcd envelope.py, Copyright (c) 2025 S. Miller, KC1AWV, MIT License.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// Envelope is one RRC message.
//
// Optional fields are absent when empty: Room "", Nick "", Dst nil, Body nil.
// rrcd treats an empty room and an empty nick as absent too, and omitting the
// key matters to clients: MeshChatX files a NOTICE that carries any room
// under that room.
type Envelope struct {
	Type Type
	ID   []byte
	TS   uint64 // milliseconds since the Unix epoch
	Src  []byte
	Room string
	Nick string
	Dst  []byte
	// Body is the raw CBOR of the body, relayed as received. Use BodyString
	// or DecodeBody to read it and SetBody or TextBody to fill it.
	Body cbor.RawMessage
	// Ext holds extension fields (keys ≥ ExtensionMin) as raw CBOR, so a hub
	// relays replies, reactions and future extensions without understanding
	// them.
	Ext map[Key]cbor.RawMessage
}

// DecodeLimits bound what Decode accepts. A zero field means no limit. Not
// to be confused with HubLimits, which a hub tells clients about itself in
// WELCOME (its nick and message size limits, rate, and so on) — these are
// bounds on the envelope's own wire encoding, checked before it's decoded.
type DecodeLimits struct {
	MaxFrameBytes int // whole encoded envelope
	MaxIDBytes    int // K_ID
	MaxExtBytes   int // all extension fields together, encoded
}

// DefaultLimits are the limits a hub applies to frames from clients, before
// any of its own (a real hub overrides MaxFrameBytes with its link's MDU;
// see internal/rrcsrv). A function rather than a package-level var, so
// nothing can mutate the default for every caller by assigning to it.
func DefaultLimits() DecodeLimits {
	return DecodeLimits{
		MaxFrameBytes: 256 * 1024, // this package's own ceiling; callers usually set a tighter one
		MaxIDBytes:    64,
		MaxExtBytes:   128,
	}
}

// Error is a validation failure. Its text is what the hub sends back as
// ERROR "bad message: <text>", so it follows rrcd's wording exactly.
type Error struct {
	Text string
}

func (e *Error) Error() string { return e.Text }

func invalid(format string, args ...any) error {
	return &Error{Text: fmt.Sprintf(format, args...)}
}

// IsInvalid reports whether err is a validation failure from this package.
func IsInvalid(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

var (
	decMode = mustDecMode(cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  8,
		MaxArrayElements: 256,
		MaxMapPairs:      256,
		IntDec:           cbor.IntDecConvertNone,
		UTF8:             cbor.UTF8RejectInvalid,
		DefaultMapType:   nil,
	})
	encMode = mustEncMode(cbor.CoreDetEncOptions())
)

func mustDecMode(o cbor.DecOptions) cbor.DecMode {
	m, err := o.DecMode()
	if err != nil {
		panic(err)
	}
	return m
}

func mustEncMode(o cbor.EncOptions) cbor.EncMode {
	m, err := o.EncMode()
	if err != nil {
		panic(err)
	}
	return m
}

// Decode parses and validates one envelope.
//
// Checks run in rrcd's order and fail with rrcd's messages. Differences from
// rrcd, all deliberate:
//   - limits (frame size, ID length, extension size) are checked, with the
//     frame size checked before any decoding;
//   - malformed, indefinite-length, tagged or over-deep CBOR is rejected;
//   - a CBOR bool is not accepted where an integer is expected (Python's
//     True == 1 lets rrcd accept it);
//   - unknown keys 9–63 are dropped; extension keys ≥ 64 are kept in Ext.
func Decode(frame []byte, lim DecodeLimits) (*Envelope, error) {
	if lim.MaxFrameBytes > 0 && len(frame) > lim.MaxFrameBytes {
		return nil, invalid("frame too large: %d bytes > %d bytes", len(frame), lim.MaxFrameBytes)
	}
	if len(frame) == 0 || frame[0]>>5 != 5 { // major type 5: map
		return nil, invalid("envelope must be a CBOR map (dict)")
	}
	var raw map[any]cbor.RawMessage
	if err := decMode.Unmarshal(frame, &raw); err != nil {
		return nil, invalid("malformed CBOR: %v", err)
	}

	fields := make(map[Key]cbor.RawMessage, len(raw))
	negative := false
	for k, v := range raw {
		switch kk := k.(type) {
		case uint64:
			fields[Key(kk)] = v
		case int64:
			negative = true
		default:
			return nil, invalid("envelope keys must be integers")
		}
	}
	if negative {
		return nil, invalid("envelope keys must be unsigned integers")
	}
	for _, k := range []Key{KeyV, KeyT, KeyID, KeyTS, KeySrc} {
		if _, ok := fields[k]; !ok {
			return nil, invalid("missing envelope key %d", k)
		}
	}

	v, ok := decodeInt(fields[KeyV])
	if !ok {
		return nil, invalid("protocol version must be an integer")
	}
	if v != Version {
		return nil, invalid("unsupported version %d", v)
	}
	t, ok := decodeInt(fields[KeyT])
	if !ok || t < 0 {
		return nil, invalid("message type must be an integer")
	}
	env := &Envelope{Type: Type(t)}

	if env.ID, ok = decodeBytes(fields[KeyID]); !ok {
		return nil, invalid("message id must be bytes")
	}
	if lim.MaxIDBytes > 0 && len(env.ID) > lim.MaxIDBytes {
		return nil, invalid("message id too long: %d bytes > %d bytes", len(env.ID), lim.MaxIDBytes)
	}
	ts, ok := decodeInt(fields[KeyTS])
	if !ok {
		return nil, invalid("timestamp must be an integer")
	}
	if ts < 0 {
		return nil, invalid("timestamp must be unsigned")
	}
	env.TS = uint64(ts)
	if env.Src, ok = decodeBytes(fields[KeySrc]); !ok {
		return nil, invalid("sender identity must be bytes")
	}
	if rv, present := fields[KeyRoom]; present {
		if env.Room, ok = decodeString(rv); !ok {
			return nil, invalid("room name must be a string")
		}
	}
	if nv, present := fields[KeyNick]; present {
		if env.Nick, ok = decodeString(nv); !ok {
			return nil, invalid("nickname must be a string")
		}
	}
	if dv, present := fields[KeyDst]; present {
		if env.Dst, ok = decodeBytes(dv); !ok {
			return nil, invalid("destination identity must be bytes")
		}
	}
	if b, present := fields[KeyBody]; present {
		env.Body = b
	}

	extBytes := 0
	for k, val := range fields {
		if k < ExtensionMin {
			continue
		}
		if env.Ext == nil {
			env.Ext = make(map[Key]cbor.RawMessage)
		}
		env.Ext[k] = val
		extBytes += len(val) + 9 // value plus the largest key encoding
	}
	if lim.MaxExtBytes > 0 && extBytes > lim.MaxExtBytes {
		return nil, invalid("extension fields too large: %d bytes > %d bytes", extBytes, lim.MaxExtBytes)
	}
	return env, nil
}

// Encode returns the envelope as deterministic CBOR (RFC 8949 §4.2.1).
func (e *Envelope) Encode() ([]byte, error) {
	m := make(map[Key]any, 8+len(e.Ext))
	for k, v := range e.Ext {
		if k >= ExtensionMin {
			m[k] = v
		}
	}
	m[KeyV] = uint64(Version)
	m[KeyT] = uint64(e.Type)
	m[KeyID] = nonNil(e.ID)
	m[KeyTS] = e.TS
	m[KeySrc] = nonNil(e.Src)
	if e.Room != "" {
		m[KeyRoom] = e.Room
	}
	if len(e.Body) > 0 {
		m[KeyBody] = e.Body
	}
	if e.Nick != "" {
		m[KeyNick] = e.Nick
	}
	if e.Dst != nil {
		m[KeyDst] = e.Dst
	}
	b, err := encMode.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode %s envelope: %w", e.Type, err)
	}
	return b, nil
}

// BodyString returns the body if it is a CBOR text string.
func (e *Envelope) BodyString() (string, bool) {
	if len(e.Body) == 0 {
		return "", false
	}
	return decodeString(e.Body)
}

// DecodeBody decodes the body into v with the same limits as envelopes.
func (e *Envelope) DecodeBody(v any) error {
	if len(e.Body) == 0 {
		return errors.New("no body")
	}
	return decMode.Unmarshal(e.Body, v)
}

// SetBody encodes v as the body. A nil v removes the body.
func (e *Envelope) SetBody(v any) error {
	if v == nil {
		e.Body = nil
		return nil
	}
	b, err := encMode.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	e.Body = b
	return nil
}

// Clone returns a deep copy, so a relayed envelope can be rewritten per
// recipient without touching the original.
func (e *Envelope) Clone() *Envelope {
	c := *e
	c.ID = slices.Clone(e.ID)
	c.Src = slices.Clone(e.Src)
	c.Dst = slices.Clone(e.Dst)
	c.Body = slices.Clone(e.Body)
	if e.Ext != nil {
		c.Ext = make(map[Key]cbor.RawMessage, len(e.Ext))
		for k, v := range e.Ext {
			c.Ext[k] = slices.Clone(v)
		}
	}
	return &c
}

// TextBody returns the CBOR encoding of a text string, for Envelope.Body.
func TextBody(s string) cbor.RawMessage {
	b, err := encMode.Marshal(s)
	if err != nil { // a Go string always encodes; invalid UTF-8 is still a text string
		panic(err)
	}
	return b
}

// NewID returns a fresh random 8-byte message ID, as rrcd's msg_id does.
func NewID() []byte {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return b
}

// NowMS returns the current time in milliseconds since the Unix epoch.
func NowMS() uint64 {
	return uint64(time.Now().UnixMilli()) //nolint:gosec // the clock is after 1970
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// decodeInt accepts CBOR major types 0 and 1 only.
func decodeInt(raw cbor.RawMessage) (int64, bool) {
	if len(raw) == 0 || raw[0]>>5 > 1 {
		return 0, false
	}
	var v any
	if err := decMode.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	switch n := v.(type) {
	case uint64:
		if n > 1<<63-1 {
			return 1<<63 - 1, true
		}
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

func decodeBytes(raw cbor.RawMessage) ([]byte, bool) {
	if len(raw) == 0 || raw[0]>>5 != 2 {
		return nil, false
	}
	var b []byte
	if err := decMode.Unmarshal(raw, &b); err != nil {
		return nil, false
	}
	if b == nil {
		b = []byte{}
	}
	return b, true
}

func decodeString(raw cbor.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0]>>5 != 3 {
		return "", false
	}
	var s string
	if err := decMode.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
