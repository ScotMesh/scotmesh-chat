package lxmf

import (
	"bytes"

	"github.com/vmihailenco/msgpack/v5"
)

// canonicalMarshal encodes v the way RNS.vendor.umsgpack would — the
// encoder upstream LXMF actually uses for every signing input.
//
// SPEC §5.6.1 states this as a sender SHOULD ("produce signing-input
// bytes that match umsgpack's output"). For a STAMPED message it is a
// hard MUST, and that is what this function exists for. §5.7.1: a
// recipient that finds a 5th payload element strips it and RE-PACKS the
// first four with umsgpack before checking the signature and deriving
// message_id. The re-pack is unconditional, so if our bytes are not what
// umsgpack would have emitted, the recipient's reconstruction differs
// from what we signed, signature_validated comes back False, and the
// message is dropped — after the entire resource transfer has completed
// and been proved. validate_stamp fails for the same reason, since it
// keys off the same re-packed payload.
//
// The specific trap this closes: vmihailenco chooses an integer envelope
// from the Go STATIC type, not from the value. A Go `int` happens to
// encode compactly, so anything built from our own literals looked
// correct. A value that came off the wire does not — msgpack decodes a
// positive fixint into int8, and re-encoding that int8 emits 0xd0 0x06
// where umsgpack emits 0x06. Valid msgpack, one byte longer, and fatal
// the moment a stamp is involved. So the bug only appeared when relaying
// a decoded fields map to a stamp-requiring recipient, which is why it
// looked like a size-boundary problem.
//
// UseCompactInts restores "smallest envelope that fits", reproducing
// umsgpack's _pack_integer at every boundary — asserted byte-for-byte
// against upstream in TestCanonicalIntegerEncodingMatchesUpstream.
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
