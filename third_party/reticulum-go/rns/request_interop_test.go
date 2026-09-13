package rns

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

type requestVector struct {
	RNSVersion             string         `json:"rns_version_at_generation"`
	Timestamp              float64        `json:"timestamp"`
	GetPath                string         `json:"get_path"`
	GetPathHashHex         string         `json:"get_path_hash_hex"`
	GetEnvelopeHex         string         `json:"get_envelope_hex"`
	FormPath               string         `json:"form_path"`
	FormPathHashHex        string         `json:"form_path_hash_hex"`
	FormData               map[string]any `json:"form_data"`
	FormEnvelopeHex        string         `json:"form_envelope_hex"`
	FormEnvelopeDoubleHex  string         `json:"form_envelope_double_packed_hex"`
	GetMessagesEnvelopeHex string         `json:"get_messages_envelope_hex"`
	ResponseRequestIDHex   string         `json:"response_request_id_hex"`
	ResponseEnvelopeHex    string         `json:"response_envelope_hex"`
}

func loadRequestVector(t *testing.T) requestVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/request_response_upstream.json")
	if err != nil {
		t.Fatalf("read vector: %v", err)
	}
	var v requestVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	return v
}

// TestRequestPathHashMatchesUpstream pins §11.3: the handler key is
// SHA-256 of the path string truncated to 16 bytes. Get this wrong and
// every request lands on no handler at all.
func TestRequestPathHashMatchesUpstream(t *testing.T) {
	v := loadRequestVector(t)
	for _, c := range []struct{ path, want string }{
		{v.GetPath, v.GetPathHashHex},
		{v.FormPath, v.FormPathHashHex},
	} {
		if got := hex.EncodeToString(RequestPathHash(c.path)); got != c.want {
			t.Errorf("RequestPathHash(%q) = %s, upstream = %s", c.path, got, c.want)
		}
	}
}

// TestRequestEnvelopeMatchesUpstream is byte-equality against envelopes
// packed by upstream RNS's own bundled umsgpack — the encoder every RNS
// packing input uses (CLAUDE.md cardinal rule 3).
func TestRequestEnvelopeMatchesUpstream(t *testing.T) {
	v := loadRequestVector(t)
	ts := time.UnixMicro(int64(v.Timestamp * 1_000_000)).UTC()

	// Plain GET: element [2] is msgpack nil.
	got, err := PackRequest(RequestPathHash(v.GetPath), nil, ts)
	if err != nil {
		t.Fatalf("PackRequest: %v", err)
	}
	if want := mustHex(t, v.GetEnvelopeHex); !bytes.Equal(got, want) {
		t.Errorf("GET envelope\n got %x\nwant %x", got, want)
	}

	// Form post: element [2] is a map, encoded in the SAME pack call.
	// Go map iteration is randomised, so a two-key map would not be
	// byte-reproducible; the single-key case pins the shape, and
	// TestFormEnvelopeIsNotDoublePacked below covers the trap itself.
	single := map[string]any{"field_message": "hello"}
	gotSingle, err := PackRequest(RequestPathHash(v.FormPath), single, ts)
	if err != nil {
		t.Fatalf("PackRequest form: %v", err)
	}
	_, _, data, err := ParseRequest(gotSingle)
	if err != nil {
		t.Fatalf("ParseRequest form: %v", err)
	}
	m, ok := data.(map[any]any)
	if !ok {
		t.Fatalf("element [2] decoded as %T, want a map — the §11.1 double-pack trap", data)
	}
	if m["field_message"] != "hello" {
		t.Errorf("field_message = %v, want hello", m["field_message"])
	}
}

// TestFormEnvelopeIsNotDoublePacked is the §11.1 trap. An envelope whose
// element [2] is a PRE-MSGPACKED blob decodes as bytes, not as a map,
// and every upstream handler tests the decoded type and silently drops
// the request with no error response — breaking every form submission
// and every propagation poll. Both shapes are in the vector; they must
// not be confusable.
func TestFormEnvelopeIsNotDoublePacked(t *testing.T) {
	v := loadRequestVector(t)

	_, _, good, err := ParseRequest(mustHex(t, v.FormEnvelopeHex))
	if err != nil {
		t.Fatalf("parse upstream form envelope: %v", err)
	}
	if _, ok := good.(map[any]any); !ok {
		t.Fatalf("upstream form data decoded as %T, want map[any]any", good)
	}

	_, _, bad, err := ParseRequest(mustHex(t, v.FormEnvelopeDoubleHex))
	if err != nil {
		t.Fatalf("parse double-packed envelope: %v", err)
	}
	if _, ok := bad.([]byte); !ok {
		t.Fatalf("double-packed data decoded as %T, want []byte — the vector no longer demonstrates the trap", bad)
	}
	if bytes.Equal(mustHex(t, v.FormEnvelopeHex), mustHex(t, v.FormEnvelopeDoubleHex)) {
		t.Fatal("the two envelope shapes are identical; the trap vector is broken")
	}
}

// A propagation /get round sends a LIST as element [2]. It must survive
// the round trip as a list, for the same reason the form dict must.
func TestGetMessagesEnvelopeDecodesAsList(t *testing.T) {
	v := loadRequestVector(t)
	_, _, data, err := ParseRequest(mustHex(t, v.GetMessagesEnvelopeHex))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	list, ok := data.([]any)
	if !ok {
		t.Fatalf("element [2] decoded as %T, want []any", data)
	}
	if len(list) != 2 {
		t.Fatalf("list has %d elements, want 2", len(list))
	}
	if list[0] != nil {
		t.Errorf("element [0] = %v, want nil", list[0])
	}
}

// TestResponseEnvelopeMatchesUpstream covers §11.2's shape, and that we
// read back the request_id an initiator must verify.
func TestResponseEnvelopeMatchesUpstream(t *testing.T) {
	v := loadRequestVector(t)
	wantID := mustHex(t, v.ResponseRequestIDHex)

	gotID, resp, err := ParseResponse(mustHex(t, v.ResponseEnvelopeHex))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if !bytes.Equal(gotID, wantID) {
		t.Errorf("request_id = %x, want %x", gotID, wantID)
	}
	m, ok := resp.(map[any]any)
	if !ok {
		t.Fatalf("response decoded as %T, want map[any]any", resp)
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok", m["status"])
	}

	// And our own packing round-trips.
	packed, err := PackResponse(wantID, "pong")
	if err != nil {
		t.Fatalf("PackResponse: %v", err)
	}
	id2, r2, err := ParseResponse(packed)
	if err != nil {
		t.Fatalf("ParseResponse round trip: %v", err)
	}
	if !bytes.Equal(id2, wantID) || r2 != "pong" {
		t.Errorf("round trip gave (%x, %v)", id2, r2)
	}
}
