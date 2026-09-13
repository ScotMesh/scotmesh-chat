package rns

import (
	"bytes"

	"github.com/vmihailenco/msgpack/v5"
)

// canonicalMarshal encodes v the way RNS.vendor.umsgpack would — the
// encoder upstream RNS actually uses for every msgpack it emits.
//
// Nothing in this package re-packs our output the way LXMF's stamp path
// does (see lxmf/msgpack_canonical.go for the bug that cost us), so no
// single call site here is load-bearing today. It is used uniformly
// anyway, because the failure mode is invisible: vmihailenco chooses an
// integer envelope from the Go STATIC type, not the value, so a struct
// field or literal typed `int` encodes compactly and looks correct,
// while any value that came off the wire decodes to int8/uint8/int64
// and re-encodes wide — 0x06 becomes 0xd0 0x06. Every call site that
// takes caller-supplied `any` (§11 request/response payloads most of
// all) is one decoded value away from emitting non-canonical bytes, and
// "this one doesn't matter" is exactly the reasoning that let the LXMF
// bug in. Emitting what umsgpack emits, everywhere, costs nothing.
//
// UseCompactFloats is deliberately NOT set: §5.6.1 requires float64
// always, and compact floats would emit float32 for values that fit.
func canonicalMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseCompactInts(true)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
