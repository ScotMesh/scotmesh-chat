package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

type golden struct {
	Valid []struct {
		Name      string `json:"name"`
		Encoder   string `json:"encoder"`
		Frame     string `json:"frame"`
		Canonical string `json:"canonical"`
		Expect    struct {
			Type uint64            `json:"type"`
			ID   string            `json:"id"`
			TS   uint64            `json:"ts"`
			Src  string            `json:"src"`
			Room string            `json:"room"`
			Nick string            `json:"nick"`
			Dst  *string           `json:"dst"`
			Body string            `json:"body"`
			Ext  map[string]string `json:"ext"`
		} `json:"expect"`
	} `json:"valid"`
	Invalid []struct {
		Name  string `json:"name"`
		Frame string `json:"frame"`
		Error string `json:"error"`
	} `json:"invalid"`
	Announce struct {
		Hub     string `json:"hub"`
		AppData string `json:"app_data"`
	} `json:"announce"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Valid) == 0 || len(g.Invalid) == 0 {
		t.Fatal("golden.json has no cases")
	}
	return g
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sameCBOR compares two encodings by value, so an encoder's choice of map
// order or float width does not matter.
func sameCBOR(t *testing.T, a, b []byte) bool {
	t.Helper()
	var va, vb any
	if err := cbor.Unmarshal(a, &va); err != nil {
		t.Fatalf("decode %x: %v", a, err)
	}
	if err := cbor.Unmarshal(b, &vb); err != nil {
		t.Fatalf("decode %x: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}

// Frames produced by rrcd's encoder (cbor2) and NomadNet's decode to the
// expected fields, and re-encode to exactly the deterministic form.
func TestGoldenValidFrames(t *testing.T) {
	for _, c := range loadGolden(t).Valid {
		t.Run(c.Name, func(t *testing.T) {
			env, err := Decode(unhex(t, c.Frame), DefaultLimits())
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			x := c.Expect
			if uint64(env.Type) != x.Type || hex.EncodeToString(env.ID) != x.ID || env.TS != x.TS ||
				hex.EncodeToString(env.Src) != x.Src || env.Room != x.Room || env.Nick != x.Nick {
				t.Fatalf("decoded %+v, want %+v", env, x)
			}
			if (x.Dst == nil) != (env.Dst == nil) || x.Dst != nil && hex.EncodeToString(env.Dst) != *x.Dst {
				t.Errorf("dst = %x, want %v", env.Dst, x.Dst)
			}
			if x.Body == "" {
				if env.Body != nil {
					t.Errorf("unexpected body %x", env.Body)
				}
			} else if !sameCBOR(t, env.Body, unhex(t, x.Body)) {
				t.Errorf("body %x, want %s", env.Body, x.Body)
			}
			if len(env.Ext) != len(x.Ext) {
				t.Errorf("ext has %d keys, want %d", len(env.Ext), len(x.Ext))
			}
			for ks, vs := range x.Ext {
				k, _ := strconv.ParseUint(ks, 10, 64)
				if !sameCBOR(t, env.Ext[Key(k)], unhex(t, vs)) {
					t.Errorf("ext[%d] = %x, want %s", k, env.Ext[Key(k)], vs)
				}
			}

			// Re-encode with canonical nested values: our output must match the
			// reference encoder's canonical form byte for byte.
			if x.Body != "" {
				env.Body = unhex(t, x.Body)
			}
			for ks, vs := range x.Ext {
				k, _ := strconv.ParseUint(ks, 10, 64)
				env.Ext[Key(k)] = unhex(t, vs)
			}
			out, err := env.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if hex.EncodeToString(out) != c.Canonical {
				t.Errorf("Encode = %x\n        want %s", out, c.Canonical)
			}
		})
	}
}

// Invalid frames fail with rrcd's exact message, because clients show it.
func TestGoldenInvalidFrames(t *testing.T) {
	for _, c := range loadGolden(t).Invalid {
		t.Run(c.Name, func(t *testing.T) {
			_, err := Decode(unhex(t, c.Frame), DefaultLimits())
			if err == nil {
				t.Fatal("accepted")
			}
			if !IsInvalid(err) {
				t.Errorf("error %T is not a validation error", err)
			}
			if err.Error() != c.Error {
				t.Errorf("error %q, want %q", err, c.Error)
			}
		})
	}
}

func TestAnnounceAppDataMatchesRrcd(t *testing.T) {
	g := loadGolden(t)
	got, err := AnnounceAppData(g.Announce.Hub)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != g.Announce.AppData {
		t.Errorf("app_data %x, want %s", got, g.Announce.AppData)
	}
	name, ok := ParseAnnounceAppData(got)
	if !ok || name != g.Announce.Hub {
		t.Errorf("ParseAnnounceAppData = %q, %v", name, ok)
	}
	for _, bad := range []any{
		map[string]any{"proto": "lxmf", "v": 1, "hub": "x"},
		map[string]any{"proto": "rrc", "v": 2, "hub": "x"},
		[]any{"rrc"},
	} {
		b, _ := cbor.Marshal(bad)
		if _, ok := ParseAnnounceAppData(b); ok {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestDecodeLimits(t *testing.T) {
	base := &Envelope{Type: TypeMsg, ID: NewID(), TS: 1, Src: bytes.Repeat([]byte{1}, 16), Room: "scotmesh", Body: TextBody("hi")}
	frame, err := base.Encode()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  func() []byte
		lim  DecodeLimits
		want string
	}{
		{"frame", func() []byte { return frame }, DecodeLimits{MaxFrameBytes: len(frame) - 1}, "frame too large: "},
		{"id", func() []byte {
			e := base.Clone()
			e.ID = make([]byte, 65)
			b, _ := e.Encode()
			return b
		}, DefaultLimits(), "message id too long: 65 bytes > 64 bytes"},
		{"ext", func() []byte {
			e := base.Clone()
			e.Ext = map[Key]cbor.RawMessage{64: TextBody(strings.Repeat("x", 200))}
			b, _ := e.Encode()
			return b
		}, DefaultLimits(), "extension fields too large: "},
		{"empty", func() []byte { return nil }, DefaultLimits(), "envelope must be a CBOR map (dict)"},
		{"trailing bytes", func() []byte { return append(slicesClone(frame), 0x00) }, DefaultLimits(), "malformed CBOR: "},
		{"indefinite map", func() []byte { return []byte{0xbf, 0x00, 0x01, 0xff} }, DefaultLimits(), "malformed CBOR: "},
		{"duplicate key", func() []byte {
			return unhexNoT("a6000101140248010203040506070803010450" + strings.Repeat("01", 16) + "0101")
		}, DefaultLimits(), "malformed CBOR: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(tt.env(), tt.lim)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("err = %v, want prefix %q", err, tt.want)
			}
		})
	}
	if _, err := Decode(frame, DecodeLimits{}); err != nil {
		t.Errorf("zero limits: %v", err)
	}
}

func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

func unhexNoT(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestDeepNestingIsRejected(t *testing.T) {
	body := []byte{}
	for range 20 {
		body = append(body, 0x81) // array of 1
	}
	body = append(body, 0x00)
	e := &Envelope{Type: TypeMsg, ID: NewID(), TS: 1, Src: []byte{1}, Body: body}
	frame, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(frame, DefaultLimits()); err == nil {
		t.Error("a body nested 20 deep was accepted")
	}
}

func TestEncodeOmitsEmptyOptionalFields(t *testing.T) {
	e := &Envelope{Type: TypeNotice, ID: []byte{1}, TS: 5, Src: []byte{2}}
	b, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var m map[uint64]any
	if err := cbor.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []Key{KeyRoom, KeyBody, KeyNick, KeyDst} {
		if _, ok := m[uint64(k)]; ok {
			t.Errorf("key %d present on an envelope that does not set it", k)
		}
	}
	if len(m) != 5 {
		t.Errorf("%d keys, want 5", len(m))
	}
	// Nil ID and Src still encode as byte strings, so the frame validates.
	e2 := &Envelope{Type: TypePing}
	b2, err := e2.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(b2, DefaultLimits()); err != nil {
		t.Errorf("nil ID/Src frame does not validate: %v", err)
	}
}

func TestEncodeDropsNonExtensionKeysFromExt(t *testing.T) {
	e := &Envelope{Type: TypeMsg, ID: []byte{1}, TS: 1, Src: []byte{1},
		Ext: map[Key]cbor.RawMessage{KeySrc: TextBody("spoof"), 64: TextBody("ok")}}
	b, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decode(b, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.Src, []byte{1}) {
		t.Errorf("Ext overrode K_SRC: %x", d.Src)
	}
	if s, _ := decodeString(d.Ext[64]); s != "ok" {
		t.Errorf("ext 64 = %x", d.Ext[64])
	}
}

func TestCloneIsDeep(t *testing.T) {
	e := &Envelope{ID: []byte{1}, Src: []byte{2}, Dst: []byte{3}, Body: TextBody("x"),
		Ext: map[Key]cbor.RawMessage{64: TextBody("y")}}
	c := e.Clone()
	c.ID[0], c.Src[0], c.Dst[0], c.Body[1] = 9, 9, 9, 'z'
	c.Ext[64][1] = 'z'
	if e.ID[0] != 1 || e.Src[0] != 2 || e.Dst[0] != 3 {
		t.Error("Clone shares byte slices")
	}
	if s, _ := e.BodyString(); s != "x" {
		t.Errorf("Clone shares the body: %q", s)
	}
	if s, _ := decodeString(e.Ext[64]); s != "y" {
		t.Errorf("Clone shares ext values: %q", s)
	}
}

func TestBodyHelpers(t *testing.T) {
	e := &Envelope{}
	if _, ok := e.BodyString(); ok {
		t.Error("BodyString ok on no body")
	}
	if err := e.DecodeBody(new(any)); err == nil {
		t.Error("DecodeBody on no body: no error")
	}
	if err := e.SetBody([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.BodyString(); ok {
		t.Error("BodyString ok on an array body")
	}
	var got []string
	if err := e.DecodeBody(&got); err != nil || len(got) != 2 {
		t.Errorf("DecodeBody = %v, %v", got, err)
	}
	if err := e.SetBody(nil); err != nil || e.Body != nil {
		t.Error("SetBody(nil) did not clear")
	}
	if err := e.SetBody(make(chan int)); err == nil {
		t.Error("SetBody of an unencodable value: no error")
	}
}

func TestTypeString(t *testing.T) {
	if TypeResourceEnvelope.String() != "RESOURCE_ENVELOPE" || Type(99).String() != "TYPE(99)" {
		t.Errorf("got %q and %q", TypeResourceEnvelope, Type(99))
	}
}

func TestNewIDAndNow(t *testing.T) {
	a, b := NewID(), NewID()
	if len(a) != 8 || bytes.Equal(a, b) {
		t.Errorf("NewID gave %x and %x", a, b)
	}
	if NowMS() < 1_700_000_000_000 {
		t.Error("NowMS is not in milliseconds")
	}
}
