package importer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

func id(b byte) []byte { return bytes.Repeat([]byte{b}, 16) }

func hx(b []byte) string { return hex.EncodeToString(b) }

var now = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func record(t *testing.T, fields map[uint64]any) []byte {
	t.Helper()
	b, err := cbor.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b))) //nolint:gosec // small test records
	return append(n[:], b...)
}

// fixtures builds rrc-hub and bridge 0.1 data directories in their real formats.
func fixtures(t *testing.T) (rrcHub, bridge string) {
	t.Helper()
	dir := t.TempDir()
	rrcHub, bridge = filepath.Join(dir, "rrc-hub"), filepath.Join(dir, "bridge")
	ms := func(d time.Duration) uint64 { return uint64(now.Add(d).UnixMilli()) } //nolint:gosec // after 1970

	write(t, filepath.Join(rrcHub, "peers.toml"), `[peers]
  [peers.`+hx(id(0xa1))+`]
    public_key = "`+strings.Repeat("ab", 64)+`"
    nick = "Alex"
    last_seen_ts = `+formatTS(now.Add(-time.Hour))+`
  [peers.`+hx(id(0xa2))+`]
    nick = "A1ex"
    last_seen_ts = `+formatTS(now.Add(-48*time.Hour))+`
  [peers.`+hx(id(0xb2))+`]
    nick = "Rab Smith"
    last_seen_ts = `+formatTS(now.Add(-2*time.Hour))+`
  [peers.`+hx(id(0xbb))+`]
    nick = "bridge"
    last_seen_ts = `+formatTS(now.Add(-time.Minute))+`
  [peers.nothex]
    nick = "Broken"
`)
	write(t, filepath.Join(rrcHub, "rooms.toml"), `[rooms]
[rooms."ScotMesh"]
founder = "`+hx(id(0xa1))+`"
topic = "ScotMesh - Scotland Reticulum community"
moderated = false
operators = ["`+hx(id(0xa1))+`"]
voiced = []
bans = ["`+hx(id(0xee))+`", "zz"]
topic_ops_only = true
last_used_ts = `+formatTS(now.Add(-time.Hour))+`
`)
	write(t, filepath.Join(rrcHub, "klines.txt"), `banned_identities = ["`+hx(id(0xdd))+`"]`+"\n")

	var log []byte
	log = append(log, record(t, map[uint64]any{1: ms(-3 * time.Hour), 2: []byte("id-one-1"), 3: id(0xa1), 4: "Alex", 5: uint64(20), 6: "hello from rrc", 7: ms(-3 * time.Hour)})...)
	log = append(log, record(t, map[uint64]any{1: ms(-2 * time.Hour), 2: []byte("id-two-2"), 3: id(0xbb), 4: "bridge", 5: uint64(20), 6: "<Ellen> relayed copy", 7: ms(-2 * time.Hour)})...)
	log = append(log, record(t, map[uint64]any{1: ms(-90 * time.Minute), 2: []byte("id-thr-3"), 3: id(0xb2), 4: "Rab Smith", 5: uint64(22), 6: "waves", 7: ms(-90 * time.Minute)})...)
	log = append(log, record(t, map[uint64]any{1: ms(-80 * time.Minute), 2: []byte("id-ntc-4"), 3: id(0xa1), 4: "Alex", 5: uint64(21), 6: "a notice", 7: ms(-80 * time.Minute)})...)
	log = append(log, record(t, map[uint64]any{1: ms(-10 * 24 * time.Hour), 2: []byte("id-old-5"), 3: id(0xa1), 4: "Alex", 5: uint64(20), 6: "too old", 7: ms(-10 * 24 * time.Hour)})...)
	log = append(log, 0, 0, 0, 99, 1, 2) // torn record at the end
	write(t, filepath.Join(rrcHub, "history", hex.EncodeToString([]byte("scotmesh")), "2026-09-13.log"), string(log))

	write(t, filepath.Join(bridge, "log", "state.json"), `{"members":{
  "0be6c276ee0c55b460eeeb983fdada40":{"address":"0be6c276ee0c55b460eeeb983fdada40","identity":"`+hx(id(0xe3))+`","nick":"Ellen","joined_at":1789251421624,"last_seen":1789251526665},
  "1111111111111111111111111111111a":{"address":"1111111111111111111111111111111a","identity":"`+hx(id(0xc4))+`","nick":"","paused":true,"joined_at":1789251421000,"last_seen":1789251526000},
  "2222222222222222222222222222222b":{"address":"2222222222222222222222222222222b","identity":"","nick":"Ghost"}
},"bans":{}}`)
	write(t, filepath.Join(bridge, "log", "messages.jsonl"), strings.Join([]string{
		`{"id":1,"said_at":` + i64(now.Add(-2*time.Hour)) + `,"received_at":` + i64(now.Add(-2*time.Hour)) + `,"source":"lxmf","origin_id":"aa11","author_identity":"` + hx(id(0xe3)) + `","author_name":"Ellen","body":"relayed copy"}`,
		`{"id":2,"said_at":` + i64(now.Add(-100*time.Minute)) + `,"received_at":` + i64(now.Add(-100*time.Minute)) + `,"source":"rrc","origin_id":"bb22","author_identity":"` + hx(id(0xa1)) + `","author_name":"Alex","body":"already in rrc-hub history"}`,
		`{"id":3,"said_at":` + i64(now.Add(-70*time.Minute)) + `,"received_at":` + i64(now.Add(-70*time.Minute)) + `,"source":"page","origin_id":"cc33","author_identity":"` + hx(id(0xe3)) + `","author_name":"Ellen","body":"* dances"}`,
		`not json`,
	}, "\n")+"\n")
	return rrcHub, bridge
}

// formatTS writes Unix seconds as a float, as rrc-hub (Python) does.
func formatTS(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}

func i64(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func open(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestImport(t *testing.T) {
	rrcHub, bridge := fixtures(t)
	st := open(t)
	src := Sources{RRCHub: rrcHub, Bridge: bridge, Skip: [][]byte{id(0xbb)}}

	dry, err := Import(context.Background(), st, src, now, 7*24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.View(context.Background(), func(tx *store.Tx) error {
		if _, ok, _ := tx.Identity(id(0xa1)); ok {
			t.Error("a dry run wrote to the store")
		}
		return nil
	})

	rep, err := Import(context.Background(), st, src, now, 7*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.String() != dry.String() {
		t.Errorf("dry run and real run disagree:\n%s\nvs\n%s", dry, rep)
	}
	t.Log(rep)
	if rep.Names != 3 || rep.Members != 2 || rep.Rooms != 1 || rep.Bans != 1 || rep.Messages != 4 || rep.Old != 1 {
		t.Errorf("report %+v", rep)
	}
	for _, want := range []string{
		`rrc-hub name "A1ex" for a2a2a2a2 not imported: a1a1a1a1 already holds "Alex"`,
		`rrc-hub peer "nothex" skipped`,
		"is not an identity hash; skipped",
		"torn or oversized record at the end",
		"bridge member 2222222222222222222222222222222b skipped",
		"unreadable line skipped",
	} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}

	_ = st.View(context.Background(), func(tx *store.Tx) error {
		check := func(who []byte, want string) {
			n, ok, _ := tx.NameOf(who)
			if want == "" && ok || want != "" && (!ok || n.Name != want) {
				t.Errorf("name of %x = %+v %v, want %q", who[:2], n, ok, want)
			}
		}
		check(id(0xa1), "Alex")
		check(id(0xa2), "")
		check(id(0xb2), "Rab_Smith")
		check(id(0xe3), "Ellen")
		check(id(0xbb), "")

		i, _, _ := tx.Identity(id(0xa1))
		if len(i.PublicKey) != 64 {
			t.Error("public key not imported")
		}
		if pi, _, _ := tx.Identity(id(0xc4)); pi.LXMFMode != store.LXMFOff {
			t.Errorf("paused bridge member has mode %q", pi.LXMFMode)
		}
		r, ok, _ := tx.Room("scotmesh")
		if !ok || !r.Registered || r.Topic != "ScotMesh - Scotland Reticulum community" || r.Modes != "t" || !bytes.Equal(r.Founder, id(0xa1)) {
			t.Errorf("room %+v", r)
		}
		if ok, _ := tx.HasRole("scotmesh", id(0xee), store.RoleBan); !ok {
			t.Error("room ban not imported")
		}
		if ok, _ := tx.IsBanned(id(0xdd)); !ok {
			t.Error("kline not imported")
		}
		msgs, _ := tx.LatestMessages("scotmesh", 0, 10)
		var got []string
		for _, m := range msgs {
			got = append(got, string(m.Kind)+":"+m.AuthorName+":"+m.Body)
		}
		want := "msg:Alex:hello from rrc|msg:Ellen:relayed copy|action:Rab Smith:waves|action:Ellen:dances"
		if strings.Join(got, "|") != want {
			t.Errorf("messages\n got %s\nwant %s", strings.Join(got, "|"), want)
		}
		c, ok, _ := tx.Cursor(id(0xa1), "scotmesh", store.ViaRRC)
		if !ok || c.MessageID != msgs[len(msgs)-1].ID {
			t.Errorf("RRC users' catch-up should start after imported history: %+v %v", c, ok)
		}
		return nil
	})

	again, err := Import(context.Background(), st, src, now, 7*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Messages != 0 || again.Duplicates != 4 || again.Members != 0 {
		t.Errorf("a second import changed things: %+v", again)
	}
}

func TestImportMissingSources(t *testing.T) {
	st := open(t)
	rep, err := Import(context.Background(), st, Sources{RRCHub: t.TempDir(), Bridge: t.TempDir()}, now, time.Hour, false)
	if err != nil || rep.Messages != 0 || rep.Identities != 0 {
		t.Errorf("empty sources: %+v %v", rep, err)
	}
	bad := t.TempDir()
	write(t, filepath.Join(bad, "peers.toml"), "this is = = not toml")
	if _, err := Import(context.Background(), st, Sources{RRCHub: bad}, now, time.Hour, false); err == nil {
		t.Error("broken peers.toml accepted")
	}
}
