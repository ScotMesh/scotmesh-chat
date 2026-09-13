package rns

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
)

// SPEC §7.3 / §7.4 — ratchets.
//
// A destination periodically rotates an X25519 keypair and publishes the
// public half in its announces (§4.2, context_flag = 1). Senders that
// have seen the announce encrypt to the ratchet instead of the long-term
// key, so a leaked long-term key does not retroactively decrypt traffic
// that used a rotated-out ratchet. That forward secrecy is the whole
// purpose — §7.3 is explicit that ratchets are NOT what makes announces
// visible to the mesh (random_hash is, §4.5).
//
// Interop does not depend on any of this: §3 permits a sender that has
// not seen a ratchet to fall back to the long-term key, so a client with
// no ratchets talks to everyone. What it loses is forward secrecy, in
// both directions.
const (
	// RatchetInterval is upstream's RATCHET_INTERVAL: rotate_ratchets()
	// runs on every announce but is a no-op unless this has elapsed
	// (RNS/Destination.py:90). At a 5-15 minute announce cadence that is
	// 2-6 announces per ratchet.
	RatchetInterval = 30 * time.Minute

	// RatchetCount is upstream's Destination.RATCHET_COUNT — the ring
	// size for inbound decrypt tolerance (§7.4). Generous, and not an
	// interop requirement: it bounds an in-memory try-list.
	RatchetCount = 512

	// RatchetExpiry is upstream's Identity.RATCHET_EXPIRY. A ratchet
	// older than this is dropped from the ring even if the ring is not
	// full — holding a month-old private key defeats the point of
	// rotating away from it.
	RatchetExpiry = 30 * 24 * time.Hour

	ratchetPrivLen = 32
)

// Ratchet is one rotation's X25519 keypair.
type Ratchet struct {
	Private []byte
	Public  []byte
	Created time.Time
}

// NewRatchet generates a fresh ratchet keypair.
func NewRatchet() (*Ratchet, error) {
	priv := make([]byte, ratchetPrivLen)
	if _, err := rand.Read(priv); err != nil {
		return nil, fmt.Errorf("generate ratchet: %w", err)
	}
	// Clamp exactly as X25519 requires; curve25519.X25519 does not do it
	// for us and an unclamped scalar yields a key the peer cannot match.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("derive ratchet public: %w", err)
	}
	return &Ratchet{Private: priv, Public: pub, Created: time.Now()}, nil
}

// RatchetKeeper holds one destination's ratchet ring: the current key to
// announce, and recent ones kept only so in-flight messages encrypted to
// a just-rotated-out ratchet still decrypt (§7.4).
type RatchetKeeper struct {
	mu   sync.Mutex
	ring []*Ratchet // newest first

	// interval and count are overridable for tests.
	interval time.Duration
	count    int
	expiry   time.Duration
}

// NewRatchetKeeper returns a keeper with upstream's defaults and one
// ratchet already generated, so the first announce carries one.
func NewRatchetKeeper() (*RatchetKeeper, error) {
	k := &RatchetKeeper{interval: RatchetInterval, count: RatchetCount, expiry: RatchetExpiry}
	if _, err := k.Rotate(time.Now(), true); err != nil {
		return nil, err
	}
	return k, nil
}

// SetInterval overrides the rotation cadence (tests, or a deployment
// that wants tighter forward secrecy). §7.3.3: rotating per announce is
// interop-correct, it just consumes ring slots faster.
func (k *RatchetKeeper) SetInterval(d time.Duration) {
	k.mu.Lock()
	k.interval = d
	k.mu.Unlock()
}

// Rotate generates a new ratchet if the interval has elapsed, mirroring
// upstream's rotate_ratchets() no-op-if-recent gate
// (RNS/Destination.py:227-235). force ignores the gate.
//
// Returns whether a rotation happened. Callers announce the Current()
// key either way — a no-op rotation is the normal case, not a failure.
func (k *RatchetKeeper) Rotate(now time.Time, force bool) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if !force && len(k.ring) > 0 && now.Before(k.ring[0].Created.Add(k.interval)) {
		return false, nil
	}
	r, err := NewRatchet()
	if err != nil {
		return false, err
	}
	r.Created = now
	k.ring = append([]*Ratchet{r}, k.ring...)
	k.pruneLocked(now)
	return true, nil
}

// pruneLocked drops expired ratchets and truncates to the ring size.
func (k *RatchetKeeper) pruneLocked(now time.Time) {
	live := k.ring[:0]
	for _, r := range k.ring {
		if k.expiry > 0 && now.Sub(r.Created) > k.expiry {
			continue
		}
		live = append(live, r)
	}
	k.ring = live
	if k.count > 0 && len(k.ring) > k.count {
		k.ring = k.ring[:k.count]
	}
}

// Current returns the public half to publish in announces, or nil when
// the keeper holds nothing.
func (k *RatchetKeeper) Current() []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.ring) == 0 {
		return nil
	}
	return append([]byte(nil), k.ring[0].Public...)
}

// PrivateKeys returns the ring's private halves, newest first, for the
// §7.4 inbound decrypt try-list.
func (k *RatchetKeeper) PrivateKeys() [][]byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([][]byte, 0, len(k.ring))
	for _, r := range k.ring {
		out = append(out, append([]byte(nil), r.Private...))
	}
	return out
}

// Len reports the ring size.
func (k *RatchetKeeper) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.ring)
}

// ErrRatchetDecryptFailed is returned when no ratchet in the ring, nor
// the long-term key, could open a token.
var ErrRatchetDecryptFailed = errors.New("token did not decrypt under any ratchet or the long-term key")
