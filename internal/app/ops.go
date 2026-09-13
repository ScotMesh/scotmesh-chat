package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// health is written to data_dir/health.json every 30 seconds, for the status
// page, the "scotmesh-chat healthcheck" subcommand (used as a container
// HEALTHCHECK) and anyone checking the service by hand.
type health struct {
	Version           string                       `json:"version"`
	StartedAt         time.Time                    `json:"started_at"`
	UpdatedAt         time.Time                    `json:"updated_at"`
	BackboneConnected bool                         `json:"backbone_connected"`
	Addresses         map[string]string            `json:"addresses"`
	Counters          map[string]map[string]uint64 `json:"counters"`
}

// healthWriter gathers counters from each component.
type healthWriter struct {
	path      string
	version   string
	started   time.Time
	addresses map[string]string
	sources   map[string]func() map[string]uint64
	connected func() bool // nil treated as always connected, for tests that don't care
	log       *slog.Logger
}

func (w *healthWriter) write() {
	h := health{Version: w.version, StartedAt: w.started, UpdatedAt: time.Now().UTC(), Addresses: w.addresses, Counters: map[string]map[string]uint64{}, BackboneConnected: true}
	if w.connected != nil {
		h.BackboneConnected = w.connected()
	}
	for name, fn := range w.sources {
		h.Counters[name] = fn()
	}
	h.Counters["process"] = processCounters()
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		w.log.Error("health encode", "err", err)
		return
	}
	if err := writeAtomic(w.path, append(b, '\n'), 0o644); err != nil {
		w.log.Error("health write", "err", err)
	}
}

func (w *healthWriter) run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	w.write()
	for {
		select {
		case <-ctx.Done():
			w.write()
			return
		case <-tick.C:
			w.write()
		}
	}
}

// processCounters are the numbers a soak test watches for leaks.
func processCounters() map[string]uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	c := map[string]uint64{
		"goroutines": uint64(runtime.NumGoroutine()), //nolint:gosec // non-negative
		"heap_bytes": ms.HeapAlloc,
		"sys_bytes":  ms.Sys,
		"gc_cycles":  uint64(ms.NumGC),
	}
	// Resident memory, from /proc on Linux; absent elsewhere.
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 1 {
			if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				c["rss_bytes"] = pages * uint64(os.Getpagesize()) //nolint:gosec // page size is positive
			}
		}
	}
	return c
}

// writeAtomic writes via a temp file, fsync and rename.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) //nolint:gosec // operator-configured path
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dailyBackupName matches only the nightly backup's own filename, not any
// other hub-*.db copy an operator keeps in the same folder (such as
// hub-before-<version>.db, taken by hand before a migration): prune must
// never count or remove those.
var dailyBackupName = regexp.MustCompile(`^hub-\d{4}-\d{2}-\d{2}\.db$`)

// backups writes data_dir/backups/hub-YYYY-MM-DD.db once a day, soon after
// 03:30 in location, and keeps the newest keep.
type backups struct {
	st       *store.Store
	dir      string
	keep     int
	log      *slog.Logger
	now      func() time.Time
	location *time.Location
}

func (b *backups) run(ctx context.Context) {
	for {
		wait := b.untilNext()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			if err := b.once(ctx); err != nil && ctx.Err() == nil {
				b.log.Error("backup failed", "err", err)
			}
		}
	}
}

func (b *backups) untilNext() time.Duration {
	now := b.now().In(b.location)
	next := time.Date(now.Year(), now.Month(), now.Day(), 3, 30, 0, 0, b.location)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1) // a calendar day, not a fixed 24h: DST-safe
	}
	return next.Sub(now)
}

// once backs up to a temp file and renames it into place, so a backup
// interrupted partway (a shutdown, a full disk) never leaves a truncated
// file where prune, or an operator restoring it, would take it for a good
// one.
func (b *backups) once(ctx context.Context) error {
	if err := ensurePrivateDir(b.dir); err != nil {
		return err
	}
	name := filepath.Join(b.dir, "hub-"+b.now().In(b.location).Format("2006-01-02")+".db")
	if _, err := os.Stat(name); err == nil {
		return nil // already made today
	}
	tmp := name + ".tmp"
	_ = os.Remove(tmp) // a previous, interrupted attempt
	if err := b.st.Backup(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	b.log.Info("backup written", "path", name)
	return b.prune()
}

func (b *backups) prune() error {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if dailyBackupName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var errs []error
	for len(names) > b.keep {
		if err := os.Remove(filepath.Join(b.dir, names[0])); err != nil {
			errs = append(errs, fmt.Errorf("remove old backup: %w", err))
		}
		names = names[1:]
	}
	return errors.Join(errs...)
}
