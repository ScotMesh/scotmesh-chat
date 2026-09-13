package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// FuzzDecode: no input may panic, and anything accepted must re-encode to a
// frame that decodes to the same envelope.
func FuzzDecode(f *testing.F) {
	if b, err := os.ReadFile("testdata/golden.json"); err == nil {
		var g golden
		if json.Unmarshal(b, &g) == nil {
			for _, c := range g.Valid {
				if frame, err := hex.DecodeString(c.Frame); err == nil {
					f.Add(frame)
				}
			}
			for _, c := range g.Invalid {
				if frame, err := hex.DecodeString(c.Frame); err == nil {
					f.Add(frame)
				}
			}
		}
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		env, err := Decode(frame, DefaultLimits())
		if err != nil {
			if !IsInvalid(err) {
				t.Fatalf("non-validation error %T: %v", err, err)
			}
			return
		}
		out, err := env.Encode()
		if err != nil {
			t.Fatalf("accepted frame does not re-encode: %v", err)
		}
		again, err := Decode(out, DecodeLimits{})
		if err != nil {
			t.Fatalf("re-encoded frame does not decode: %v", err)
		}
		if again.Type != env.Type || again.TS != env.TS || !bytes.Equal(again.ID, env.ID) ||
			!bytes.Equal(again.Src, env.Src) || again.Room != env.Room || again.Nick != env.Nick ||
			!bytes.Equal(again.Body, env.Body) || len(again.Ext) != len(env.Ext) {
			t.Fatalf("round trip changed the envelope:\n%+v\n%+v", env, again)
		}
		_ = ParseHello(env.Body)
		_, _ = ParseResourceEnvelope(env.Body)
	})
}

func FuzzNormalizeRoom(f *testing.F) {
	for _, s := range []string{"scotmesh", "#ScotMesh", "", "a b", "ümlaut"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := NormalizeRoom(s, 64)
		if err != nil {
			return
		}
		if again, err := NormalizeRoom(r, 64); err != nil || again != r {
			t.Fatalf("not idempotent: %q -> %q -> %q, %v", s, r, again, err)
		}
	})
}
