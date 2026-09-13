package rns

import (
	"bytes"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
)

func TestNewRatchetIsAValidClampedX25519Key(t *testing.T) {
	r, err := NewRatchet()
	if err != nil {
		t.Fatalf("NewRatchet: %v", err)
	}
	if len(r.Private) != 32 || len(r.Public) != 32 {
		t.Fatalf("key sizes: priv %d pub %d", len(r.Private), len(r.Public))
	}
	// Clamping is not done by curve25519.X25519; an unclamped scalar
	// yields a key the peer cannot match.
	if r.Private[0]&7 != 0 || r.Private[31]&128 != 0 || r.Private[31]&64 == 0 {
		t.Error("private key is not X25519-clamped")
	}
	want, err := curve25519.X25519(r.Private, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.Public, want) {
		t.Error("public key is not the base-point product of the private key")
	}
	// Two ratchets must differ.
	r2, _ := NewRatchet()
	if bytes.Equal(r.Private, r2.Private) {
		t.Error("two ratchets share a private key")
	}
}

// §7.3.1: rotate_ratchets runs on every announce but is a no-op unless
// RATCHET_INTERVAL has elapsed. Rotating on every announce would burn
// ring slots without buying forward secrecy.
func TestRatchetRotationHonoursTheInterval(t *testing.T) {
	k, err := NewRatchetKeeper()
	if err != nil {
		t.Fatal(err)
	}
	if k.Len() != 1 {
		t.Fatalf("a fresh keeper holds %d ratchets, want 1 so the first announce carries one", k.Len())
	}
	first := k.Current()
	now := time.Now()

	rotated, err := k.Rotate(now.Add(time.Minute), false)
	if err != nil {
		t.Fatal(err)
	}
	if rotated {
		t.Error("rotated inside the interval")
	}
	if !bytes.Equal(k.Current(), first) {
		t.Error("current ratchet changed without a rotation")
	}

	rotated, err = k.Rotate(now.Add(RatchetInterval+time.Minute), false)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Error("did not rotate after the interval elapsed")
	}
	if bytes.Equal(k.Current(), first) {
		t.Error("current ratchet did not change after rotation")
	}
	if k.Len() != 2 {
		t.Errorf("ring holds %d, want both ratchets so in-flight messages still decrypt", k.Len())
	}
}

func TestRatchetRingIsBounded(t *testing.T) {
	k, err := NewRatchetKeeper()
	if err != nil {
		t.Fatal(err)
	}
	k.count = 4 // keep the test quick; the default is RatchetCount
	now := time.Now()
	for i := 0; i < 20; i++ {
		if _, err := k.Rotate(now.Add(time.Duration(i)*time.Hour), true); err != nil {
			t.Fatal(err)
		}
	}
	if got := k.Len(); got != 4 {
		t.Errorf("ring holds %d, want the cap of 4", got)
	}
}

// A ratchet older than RATCHET_EXPIRY is dropped even when the ring is
// not full: holding a month-old private key defeats rotating away from
// it.
func TestRatchetExpiryDropsOldKeys(t *testing.T) {
	k, err := NewRatchetKeeper()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := k.Rotate(now, true); err != nil {
		t.Fatal(err)
	}
	if k.Len() != 2 {
		t.Fatalf("expected 2 ratchets, got %d", k.Len())
	}
	// Rotate far enough ahead that both existing ratchets are expired.
	if _, err := k.Rotate(now.Add(RatchetExpiry+time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if got := k.Len(); got != 1 {
		t.Errorf("ring holds %d after expiry, want only the fresh ratchet", got)
	}
}

// §7.4: a message encrypted to a rotated-out ratchet must still decrypt,
// because rotation races announce propagation — a sender keeps using the
// previous ratchet until the new announce reaches them. Without the ring
// every rotation would lose the messages in flight across it.
func TestRatchetRingDecryptsRotatedOutKeys(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := NewRatchetKeeper()
	if err != nil {
		t.Fatal(err)
	}
	oldPub := k.Current()

	// A sender encrypts to the ratchet we are about to rotate away from.
	msg := []byte("in flight across a rotation")
	ct, err := TokenEncrypt(msg, oldPub, id.Hash())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := k.Rotate(time.Now().Add(RatchetInterval*2), false); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k.Current(), oldPub) {
		t.Fatal("premise broken: the ratchet did not rotate")
	}

	got, err := TokenDecryptWithRatchets(id, ct, k.PrivateKeys())
	if err != nil {
		t.Fatalf("a message encrypted to the previous ratchet did not decrypt: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Error("plaintext mismatch")
	}
	// The long-term key alone must NOT open it — that is the forward
	// secrecy the ratchet buys.
	if _, err := TokenDecrypt(id, ct); err == nil {
		t.Error("a ratchet-encrypted token opened with the long-term key")
	}
}

// §3 permits a sender who has never seen a ratchet announce to encrypt
// to the long-term key, so that path must keep working even when we
// publish ratchets.
func TestLongTermFallbackStillDecrypts(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := NewRatchetKeeper()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("sender never saw our ratchet")
	ct, err := TokenEncrypt(msg, id.X25519Public(), id.Hash())
	if err != nil {
		t.Fatal(err)
	}
	got, err := TokenDecryptWithRatchets(id, ct, k.PrivateKeys())
	if err != nil {
		t.Fatalf("long-term fallback failed: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Error("plaintext mismatch")
	}
}

// The HKDF salt stays the recipient's IDENTITY hash under a ratchet —
// §3 step 3 is explicit that it is not the ratchet hash. Only the ECDH
// input changes between the two cases, which this proves by decrypting
// a ratchet-encrypted token with a plain identity-salted derivation.
func TestRatchetDoesNotChangeTheHKDFSalt(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRatchet()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("salt check")
	// TokenEncrypt salts with the identity hash we pass; if the ratchet
	// changed the salt, decrypting with the same identity hash would
	// fail.
	ct, err := TokenEncrypt(msg, r.Public, id.Hash())
	if err != nil {
		t.Fatal(err)
	}
	got, err := TokenDecryptWithRatchets(id, ct, [][]byte{r.Private})
	if err != nil {
		t.Fatalf("decrypt with identity-hash salt failed: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Error("plaintext mismatch")
	}
}

// A peer's announced ratchet is what we encrypt to; an announce without
// one clears the stored value rather than leaving a stale ratchet in
// place, which would black-hole a peer that stopped publishing them.
func TestKnownIdentityPrefersTheAnnouncedRatchet(t *testing.T) {
	id, _ := NewIdentity()
	k := &KnownIdentity{PublicKey: id.PublicKey()}

	if !bytes.Equal(k.EncryptionPublic(), k.X25519Public()) {
		t.Error("without a ratchet, the long-term key must be used")
	}
	r, _ := NewRatchet()
	k.RatchetPub = r.Public
	if !bytes.Equal(k.EncryptionPublic(), r.Public) {
		t.Error("with a ratchet announced, it must be preferred")
	}
	k.RatchetPub = nil
	if !bytes.Equal(k.EncryptionPublic(), k.X25519Public()) {
		t.Error("clearing the ratchet must fall back to the long-term key")
	}
}
