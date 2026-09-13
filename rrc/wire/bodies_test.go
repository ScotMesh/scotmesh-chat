package wire

import (
	"bytes"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

func mustCBOR(t *testing.T, v any) cbor.RawMessage {
	t.Helper()
	b, err := cbor.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseHello(t *testing.T) {
	h := ParseHello(mustCBOR(t, map[uint64]any{0: "nomadnet", 1: "0.1", 2: map[uint64]any{0: true, 1: false, 2: 1}, 64: "legacy"}))
	if h.Name != "nomadnet" || h.Version != "0.1" || h.LegacyNick != "legacy" {
		t.Errorf("parsed %+v", h)
	}
	if !h.Caps[CapResourceEnvelope] || h.Caps[CapAction] || !h.Caps[CapDirectNotice] {
		t.Errorf("caps %v: want resource on, action off, direct notice on (non-bool counts as on)", h.Caps)
	}
	for _, body := range []cbor.RawMessage{nil, mustCBOR(t, "hello"), mustCBOR(t, map[uint64]any{2: "nope"})} {
		if h := ParseHello(body); h.Name != "" || h.Caps != nil {
			t.Errorf("ParseHello(%x) = %+v, want zero", body, h)
		}
	}
}

// TestHelloEncode round-trips through ParseHello, the client side of
// TestParseHello above.
func TestHelloEncode(t *testing.T) {
	h := Hello{Name: "nomadnet", Version: "1.4.3", Caps: map[Key]bool{CapAction: true, CapDirectNotice: false}, LegacyNick: "Alex"}
	b, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got := ParseHello(b)
	if got.Name != h.Name || got.Version != h.Version || got.LegacyNick != h.LegacyNick {
		t.Errorf("round-tripped %+v, want %+v", got, h)
	}
	if !got.Caps[CapAction] || got.Caps[CapDirectNotice] || len(got.Caps) != 1 {
		t.Errorf("caps %v: a false capability must be omitted", got.Caps)
	}
	empty, err := Hello{}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got := ParseHello(empty); got.Name != "" || got.Caps != nil {
		t.Errorf("an empty Hello round-tripped to %+v, want zero", got)
	}
}

// TestParseWelcome round-trips through Welcome.Encode, the client side of
// TestWelcomeEncode below.
func TestParseWelcome(t *testing.T) {
	w := Welcome{Hub: "ScotMesh", Version: "1.0.0",
		Caps:   map[Key]bool{CapAction: true, CapDirectNotice: true, CapResourceEnvelope: false},
		Limits: HubLimits{32, 64, 350, 32, 240}}
	b, err := w.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got := ParseWelcome(b)
	if got.Hub != w.Hub || got.Version != w.Version || got.Limits != w.Limits {
		t.Errorf("round-tripped %+v, want %+v", got, w)
	}
	if !got.Caps[CapAction] || !got.Caps[CapDirectNotice] || got.Caps[CapResourceEnvelope] || len(got.Caps) != 2 {
		t.Errorf("caps %v: a false capability must be omitted", got.Caps)
	}
	for _, body := range []cbor.RawMessage{nil, mustCBOR(t, "hello"), mustCBOR(t, map[uint64]any{2: "nope"})} {
		if got := ParseWelcome(body); got.Hub != "" || got.Caps != nil {
			t.Errorf("ParseWelcome(%x) = %+v, want zero", body, got)
		}
	}
}

func TestWelcomeEncode(t *testing.T) {
	w := Welcome{Hub: "ScotMesh", Version: "1.0.0",
		Caps:   map[Key]bool{CapAction: true, CapDirectNotice: true, CapResourceEnvelope: false},
		Limits: HubLimits{32, 64, 350, 32, 240}}
	b, err := w.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var m map[uint64]any
	if err := cbor.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	caps, ok := m[uint64(WelcomeCaps)].(map[any]any)
	if !ok {
		t.Fatalf("caps is %T", m[uint64(WelcomeCaps)])
	}
	if len(caps) != 2 {
		t.Errorf("caps %v: a false capability must be omitted", caps)
	}
	lim, ok := m[uint64(WelcomeLimits)].(map[any]any)
	if !ok {
		t.Fatalf("limits is %T", m[uint64(WelcomeLimits)])
	}
	if lim[uint64(LimitMaxMsgBodyBytes)] != uint64(350) || lim[uint64(LimitRatePerMinute)] != uint64(240) {
		t.Errorf("limits %v", lim)
	}
	if m[uint64(WelcomeHub)] != "ScotMesh" || m[uint64(WelcomeVersion)] != "1.0.0" {
		t.Errorf("hub/version %v %v", m[uint64(WelcomeHub)], m[uint64(WelcomeVersion)])
	}
}

func TestResourceEnvelope(t *testing.T) {
	re := ResourceEnvelope{ID: []byte{1, 2}, Kind: ResourceKindMOTD, Size: 900, SHA256: bytes.Repeat([]byte{7}, 32), Encoding: "utf-8"}
	b, err := re.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseResourceEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.ID, re.ID) || got.Kind != re.Kind || got.Size != re.Size || !bytes.Equal(got.SHA256, re.SHA256) || got.Encoding != re.Encoding {
		t.Errorf("round trip %+v, want %+v", got, re)
	}
	minimal, err := ResourceEnvelope{ID: []byte{1}, Kind: "blob"}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var m map[uint64]any
	_ = cbor.Unmarshal(minimal, &m)
	if _, ok := m[uint64(ResSHA256)]; ok {
		t.Error("optional sha256 encoded when unset")
	}

	bad := []struct {
		body any
		want string
	}{
		{"not a map", "invalid resource envelope body"},
		{map[uint64]any{1: "motd", 2: 1}, "resource envelope missing id"},
		{map[uint64]any{0: []byte{1}, 2: 1}, "resource envelope missing kind"},
		{map[uint64]any{0: []byte{1}, 1: "", 2: 1}, "resource envelope missing kind"},
		{map[uint64]any{0: []byte{1}, 1: "motd"}, "resource envelope invalid size"},
		{map[uint64]any{0: []byte{1}, 1: "motd", 2: -1}, "resource envelope invalid size"},
		{map[uint64]any{0: []byte{1}, 1: "motd", 2: 1, 3: "abc"}, "resource envelope invalid sha256"},
	}
	for _, c := range bad {
		if _, err := ParseResourceEnvelope(mustCBOR(t, c.body)); err == nil || err.Error() != c.want {
			t.Errorf("ParseResourceEnvelope(%v) err = %v, want %q", c.body, err, c.want)
		}
	}
	if got, err := ParseResourceEnvelope(mustCBOR(t, map[uint64]any{0: []byte{1}, 1: "motd", 2: 1, 4: 7})); err != nil || got.Encoding != "" {
		t.Errorf("a non-string encoding must be ignored: %+v, %v", got, err)
	}
}
