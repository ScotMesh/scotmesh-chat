//go:build interop_python

package lxmf

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/thatSFguy/reticulum-go/rns"
)

// TestStampedMessageValidatesInUpstreamLXMF packs a stamped message whose
// fields map came off the wire (so its integer keys are the sized Go
// types a decode yields) and hands the bytes to real upstream LXMF.
//
// This is the test that would have caught the v0.6.0 bug, and the only
// admissible proof per CLAUDE.md cardinal rule 3: the Go-side assertions
// in msgpack_canonical_test.go check our bytes against a fixture of
// umsgpack output, but only upstream's own LXMessage.unpack_from_bytes
// exercises the §5.7.1 strip-and-re-pack that actually breaks. A
// self-round-trip passes either way, because both ends agree on the
// wrong thing.
//
// Run with:
//
//	LXMF_INTEROP_PYTHON=/path/to/venv/bin/python \
//	  go test -tags interop_python ./lxmf/ -run StampedMessageValidatesInUpstream
//
// Skipped (not failed) when no interpreter is configured.
func TestStampedMessageValidatesInUpstreamLXMF(t *testing.T) {
	python := os.Getenv("LXMF_INTEROP_PYTHON")
	if python == "" {
		t.Skip("set LXMF_INTEROP_PYTHON to a venv with rns==1.5.0 lxmf==1.1.1")
	}

	v := loadCanonicalVector(t)
	wire, err := hex.DecodeString(v.FieldsMapHex)
	if err != nil {
		t.Fatal(err)
	}
	// The fields map as a real inbound message yields it — sized integer
	// keys, not Go literals. This is the input that regressed.
	inboundFields, err := decodeFields(wire)
	if err != nil {
		t.Fatal(err)
	}

	sender, err := rns.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	senderDest := sender.DestinationHashFor(FullName())
	destHash := bytes.Repeat([]byte{0x21}, rns.IdentityHashLen)

	const cost = 8
	body, _, err := SignAndPackDirectStamped(sender, senderDest, destHash,
		nil, []byte("canonical interop"), inboundFields, StampOptions{Cost: cost})
	if err != nil {
		t.Fatalf("pack stamped: %v", err)
	}

	in, err := json.Marshal(map[string]any{
		"body_hex":       hex.EncodeToString(body),
		"sender_pub_hex": hex.EncodeToString(sender.PublicKey()),
		"stamp_cost":     cost,
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(python, "testdata/verify_stamped_signature.py")
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("upstream verifier failed: %v\n%s", err, out)
	}

	var verdict struct {
		SignatureValidated bool   `json:"signature_validated"`
		UnverifiedReason   any    `json:"unverified_reason"`
		StampPresent       bool   `json:"stamp_present"`
		StampValid         bool   `json:"stamp_valid"`
		StampValue         int    `json:"stamp_value"`
		MessageIDHex       string `json:"message_id_hex"`
		Error              string `json:"error"`
	}
	if err := json.Unmarshal(out, &verdict); err != nil {
		t.Fatalf("decode verdict %q: %v", out, err)
	}
	if verdict.Error != "" {
		t.Fatalf("upstream raised: %s", verdict.Error)
	}
	if !verdict.StampPresent {
		t.Fatal("upstream saw no stamp — the message was not packed stamped")
	}
	if !verdict.SignatureValidated {
		t.Fatalf("upstream rejected our signature (reason %v).\n"+
			"This is the canonical-encoding bug: upstream strips element [4] and "+
			"re-packs the first four with umsgpack, so non-canonical integers make "+
			"the reconstruction differ from what we signed.", verdict.UnverifiedReason)
	}
	if !verdict.StampValid {
		t.Errorf("upstream rejected our stamp (value %d, cost %d) — validate_stamp "+
			"keys off the same re-packed payload as the signature", verdict.StampValue, cost)
	}
}
