package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

// loadOrCreateIdentity reads an identity file, creating it (0600) if absent.
func loadOrCreateIdentity(path string, log *slog.Logger) (*rns.Identity, error) {
	id, err := rns.IdentityFromFile(path)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("identity %s: %w", path, err)
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if id, err = rns.NewIdentity(); err != nil {
		return nil, err
	}
	if err := id.Save(path); err != nil {
		return nil, fmt.Errorf("save identity %s: %w", path, err)
	}
	log.Info("created identity", "path", path, "hash", fmt.Sprintf("%x", id.Hash()))
	return id, nil
}

// announceCache keeps what the transport has heard across restarts, so a
// restarted hub can answer people without waiting for their next announce.
type announceCache struct {
	t    *rns.Transport
	path string
	log  *slog.Logger
}

func (a *announceCache) restore() {
	b, err := os.ReadFile(a.path)
	if err != nil {
		return
	}
	var entries []*rns.KnownIdentity
	if err := json.Unmarshal(b, &entries); err != nil {
		a.log.Warn("announce cache unreadable; starting empty", "err", err)
		return
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	n := 0
	for _, e := range entries {
		// Never restore a stub (no public key) over what the network tells us.
		if e == nil || len(e.PublicKey) == 0 || e.LastSeen.Before(cutoff) {
			continue
		}
		a.t.Restore(e)
		n++
	}
	a.log.Info("announce cache restored", "entries", n)
}

func (a *announceCache) save() {
	b, err := json.Marshal(a.t.KnownSnapshot())
	if err != nil {
		a.log.Error("announce cache encode", "err", err)
		return
	}
	tmp := a.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // operator-configured path
	if err != nil {
		a.log.Error("announce cache save", "err", err)
		return
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		a.log.Error("announce cache save", "err", err)
		return
	}
	if err := os.Rename(tmp, a.path); err != nil {
		a.log.Error("announce cache save", "err", err)
	}
}

func (a *announceCache) run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			a.save()
			return
		case <-tick.C:
			a.save()
		}
	}
}

// announcer sends one destination's announce once at start (after delay, so
// several destinations don't announce back to back) and then every interval
// with ±10% jitter. Upstream's startup burst of three is deliberately not
// used: rrc-hub's history shows announce floods hurt radio users.
func announcer(ctx context.Context, t *rns.Transport, log *slog.Logger, name string, delay, interval time.Duration, build func() (*rns.Packet, error)) {
	emit := func() {
		p, err := build()
		if err != nil {
			log.Error("announce build", "destination", name, "err", err)
			return
		}
		if err := t.Broadcast(p); err != nil {
			log.Warn("announce send", "destination", name, "err", err)
			return
		}
		log.Debug("announced", "destination", name)
	}
	wait := delay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			emit()
		}
		jitter := time.Duration((rand.Float64()*0.2 - 0.1) * float64(interval)) //nolint:gosec // jitter needs no crypto
		wait = interval + jitter
	}
}
