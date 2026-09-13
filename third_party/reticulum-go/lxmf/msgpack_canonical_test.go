package lxmf

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

// canonicalVector is testdata/msgpack_canonical_upstream.json, generated
// by RNS.vendor.umsgpack at the pinned rns==1.5.0 — the encoder upstream
// LXMF uses for every signing input, and therefore the definition of
// canonical for SPEC §5.6.1.
type canonicalVector struct {
	Source       string            `json:"_source"`
	Ints         map[string]string `json:"ints"`
	FloatZeroHex string            `json:"float_zero_hex"`
	FloatFracHex string            `json:"float_frac_hex"`
	BytesABHex   string            `json:"bytes_ab_hex"`
	FieldsMapHex string            `json:"fields_map_hex"`
	PayloadHex   string            `json:"payload_hex"`
}

func loadCanonicalVector(t *testing.T) canonicalVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/msgpack_canonical_upstream.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var v canonicalVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return v
}

// Every integer envelope boundary, byte-compared against umsgpack.
//
// This is the table that would have caught the bug: vmihailenco picks an
// envelope from the Go STATIC type, so the same logical 6 encodes as
// 0x06 from an `int` and 0xd0 0x06 from the int8 a decode produces. Each
// value is asserted from the literal Go int AND from every sized type an
// inbound decode can yield, because only the latter was ever wrong.
func TestCanonicalIntegerEncodingMatchesUpstream(t *testing.T) {
	v := loadCanonicalVector(t)
	if len(v.Ints) == 0 {
		t.Fatal("fixture has no integer vectors")
	}
	for key, wantHex := range v.Ints {
		n, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			t.Fatalf("bad fixture key %q: %v", key, err)
		}
		variants := map[string]any{"int": int(n), "int64": int64(n)}
		if n >= -128 && n <= 127 {
			variants["int8"] = int8(n)
		}
		if n >= -32768 && n <= 32767 {
			variants["int16"] = int16(n)
		}
		if n >= 0 && n <= 255 {
			variants["uint8"] = uint8(n)
		}
		if n >= 0 {
			variants["uint64"] = uint64(n)
		}
		for typeName, val := range variants {
			got, err := canonicalMarshal(val)
			if err != nil {
				t.Fatalf("%s(%d): %v", typeName, n, err)
			}
			if hex.EncodeToString(got) != wantHex {
				t.Errorf("%s(%d) = %s, umsgpack emits %s",
					typeName, n, hex.EncodeToString(got), wantHex)
			}
		}
	}
}

// §5.6.1: floats are always float64, never float32 — so UseCompactFloats
// must stay off. Bytes go in the bin family, matching Python `bytes`.
func TestCanonicalFloatAndBytesMatchUpstream(t *testing.T) {
	v := loadCanonicalVector(t)
	for _, tc := range []struct {
		name string
		val  any
		want string
	}{
		{"float 0.0", 0.0, v.FloatZeroHex},
		{"float 1.5", 1.5, v.FloatFracHex},
		{"bytes ab", []byte("ab"), v.BytesABHex},
	} {
		got, err := canonicalMarshal(tc.val)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if hex.EncodeToString(got) != tc.want {
			t.Errorf("%s = %s, umsgpack emits %s", tc.name, hex.EncodeToString(got), tc.want)
		}
	}
}

// The exact shape that broke in the field: a fields map whose integer key
// came off the wire rather than from a Go literal. Decoding umsgpack's
// own bytes gives us the real inbound Go types, not a hand-built map.
func TestCanonicalFieldsMapFromDecodedKeyMatchesUpstream(t *testing.T) {
	v := loadCanonicalVector(t)
	wire, err := hex.DecodeString(v.FieldsMapHex)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeFields(wire)
	if err != nil {
		t.Fatalf("decode upstream fields map: %v", err)
	}
	got, err := canonicalMarshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != v.FieldsMapHex {
		t.Errorf("re-encoded decoded fields map = %s, want %s (umsgpack)\n"+
			"a stamp-requiring recipient re-packs this before verifying our signature",
			hex.EncodeToString(got), v.FieldsMapHex)
	}
}

// End to end on a full 4-element payload: decode an upstream-produced
// payload and re-encode it value-by-value, mirroring what
// LXMessage.unpack_from_bytes does to a stamped message before it checks
// the signature. Byte-identical, or the message is dropped.
func TestCanonicalPayloadSurvivesValueLevelReencode(t *testing.T) {
	v := loadCanonicalVector(t)
	wire, err := hex.DecodeString(v.PayloadHex)
	if err != nil {
		t.Fatal(err)
	}
	m := &Message{rawPayload: wire}
	if err := m.unpackPayload(); err != nil {
		t.Fatalf("unpack upstream payload: %v", err)
	}
	tsSeconds := float64(m.Timestamp.UnixMicro()) / 1_000_000.0
	got, err := canonicalMarshal([]any{tsSeconds, m.Title, m.Content, m.Fields})
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != v.PayloadHex {
		t.Errorf("re-encoded payload = %s\n              want = %s (umsgpack)",
			hex.EncodeToString(got), v.PayloadHex)
	}
}

// The regression proper: take a fields map off the wire, pack it into an
// outbound message, then do to that message exactly what a stamped
// recipient does (§5.7.1 — decode, re-pack the first four elements) and
// require the result to equal what we signed.
//
// Before the fix the decoded integer key re-encoded one byte wider, the
// recipient's mandatory re-pack no longer matched the signed bytes, and
// every stamped message we sent was dropped as signature_validated =
// False — after the whole resource transfer had completed.
func TestStampedMessageFromDecodedFieldsSurvivesRecipientRepack(t *testing.T) {
	v := loadCanonicalVector(t)
	wire, err := hex.DecodeString(v.FieldsMapHex)
	if err != nil {
		t.Fatal(err)
	}
	inboundFields, err := decodeFields(wire)
	if err != nil {
		t.Fatal(err)
	}

	sender, err := rns.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dest := make([]byte, rns.IdentityHashLen)
	src := make([]byte, rns.IdentityHashLen)
	for i := range dest {
		dest[i], src[i] = byte(i), byte(0x40+i)
	}

	payload, _, _, err := buildSignedPayload(sender, src, dest,
		[]byte(""), []byte("hi"), inboundFields, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}

	m := &Message{rawPayload: payload}
	if err := m.unpackPayload(); err != nil {
		t.Fatal(err)
	}
	reFields, err := canonicalMarshal(m.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(reFields) != v.FieldsMapHex {
		t.Errorf("fields map inside a packed message = %s, umsgpack emits %s",
			hex.EncodeToString(reFields), v.FieldsMapHex)
	}

	tsSeconds := float64(m.Timestamp.UnixMicro()) / 1_000_000.0
	repacked, err := canonicalMarshal([]any{tsSeconds, m.Title, m.Content, m.Fields})
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(repacked) != hex.EncodeToString(payload) {
		t.Errorf("the recipient's re-pack differs from what we signed:\n"+
			"   signed = %s\nre-packed = %s\nthis is the drop a stamped recipient reports as signature_validated=False",
			hex.EncodeToString(payload), hex.EncodeToString(repacked))
	}
}
