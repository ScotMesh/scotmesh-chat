package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

func TestLoadRejectsUnknownKeysAndBadValues(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := Load(write("typo.toml", "[rrc]\ngreting = [\"x\"]\n")); err == nil || !strings.Contains(err.Error(), "unknown keys: rrc.greting") {
		t.Errorf("typo: %v", err)
	}
	bad := "admins = [\"xyz\"]\nlog_level = \"loud\"\n[rrc]\nannounce_interval = \"1m\"\n[group]\npropagation_node = \"abc\"\n"
	_, err := Load(write("bad.toml", bad))
	for _, want := range []string{"admins", "log_level", "rrc.announce_interval", "group.propagation_node"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("bad config error %v lacks %q", err, want)
		}
	}
	good := write("good.toml", "data_dir = \"/tmp/x\"\nbackbone = \"rns.example.net:4242\"\nadmins = [\"178c1f390b8f332f9c8681515c12107b\"]\n[hub]\nretention = \"72h\"\n")
	cfg, err := Load(good)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hub.Retention.Duration != 72*time.Hour || cfg.RRC.AnnounceInterval.Duration != 30*time.Minute || len(cfg.Hub.Rooms) != 1 {
		t.Errorf("config %+v", cfg)
	}
	admins, err := cfg.AdminHashes()
	if err != nil || len(admins) != 1 || len(admins[0]) != 16 {
		t.Errorf("admins %x %v", admins, err)
	}
	if cfg.path("identities/x") != "/tmp/x/identities/x" || cfg.path("/abs") != "/abs" {
		t.Error("path resolution")
	}
	if b, err := (Duration{90 * time.Second}).MarshalText(); err != nil || string(b) != "1m30s" {
		t.Errorf("duration text %q %v", b, err)
	}
}

func TestBackupsAndHealth(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, loc)
	b := &backups{st: st, dir: filepath.Join(dir, "backups"), keep: 3, log: quiet, now: func() time.Time { return day }, location: loc}
	// A manual copy kept alongside the daily backups (as docs/ops.md tells
	// operators to, before a migration) must never be counted or pruned.
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.dir, "hub-before-v1.0.0.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := b.once(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := b.once(context.Background()); err != nil { // twice in a day is one backup
			t.Fatal(err)
		}
		day = day.Add(24 * time.Hour)
	}
	entries, _ := os.ReadDir(b.dir)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	if len(names) != 4 || !slices.Contains(names, "hub-before-v1.0.0.db") || !slices.Contains(names, "hub-2026-09-03.db") || slices.Contains(names, "hub-2026-09-02.db") {
		t.Errorf("backups kept: %v", names)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "hub-2026-09-05.db.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a temp backup file was left behind: %v", err)
	}
	b.now = func() time.Time { return time.Date(2026, 9, 1, 3, 0, 0, 0, loc) }
	if w := b.untilNext(); w != 30*time.Minute {
		t.Errorf("until next backup %v", w)
	}
	b.now = func() time.Time { return time.Date(2026, 9, 1, 4, 0, 0, 0, loc) }
	if w := b.untilNext(); w != 23*time.Hour+30*time.Minute {
		t.Errorf("until next backup %v", w)
	}

	hw := &healthWriter{path: filepath.Join(dir, "health.json"), version: "test", started: time.Now(), log: quiet,
		addresses: map[string]string{"rrc_hub": "abc"},
		sources:   map[string]func() map[string]uint64{"hub": func() map[string]uint64 { return map[string]uint64{"posts": 3} }}}
	hw.write()
	raw, err := os.ReadFile(hw.path)
	if err != nil {
		t.Fatal(err)
	}
	var h health
	if err := json.Unmarshal(raw, &h); err != nil || h.Counters["hub"]["posts"] != 3 || h.Addresses["rrc_hub"] != "abc" {
		t.Errorf("health %s %v", raw, err)
	}
	if !h.BackboneConnected {
		t.Error("BackboneConnected should default to true when no connected func is given")
	}

	hw.connected = func() bool { return false }
	hw.write()
	raw, err = os.ReadFile(hw.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &h); err != nil || h.BackboneConnected {
		t.Errorf("health after disconnect: %s %v", raw, err)
	}
}

// fakeBackbone is a minimal interface{ Connected() bool } for watchBackbone.
type fakeBackbone struct {
	mu        sync.Mutex
	connected bool
}

func (f *fakeBackbone) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeBackbone) set(c bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = c
}

// syncBuffer is a bytes.Buffer safe for one writer goroutine and one reader
// goroutine, for tests that watch a logger's output as it's written.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWatchBackboneLogsTransitions(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	fb := &fakeBackbone{connected: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); watchBackboneEvery(ctx, fb, log, 5*time.Millisecond) }()
	time.Sleep(20 * time.Millisecond) // let watchBackboneEvery capture its initial state before we change it

	fb.set(false)
	waitFor(t, func() bool { return strings.Contains(buf.String(), "backbone connection lost") })
	fb.set(true)
	waitFor(t, func() bool { return strings.Contains(buf.String(), "msg=\"backbone connected\"") })

	cancel()
	<-done
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
