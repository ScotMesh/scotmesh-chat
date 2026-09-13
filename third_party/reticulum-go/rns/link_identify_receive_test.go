package rns

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

type linkIdentifyVector struct {
	LinkIDHex                  string `json:"link_id_hex"`
	PublicKeyHex               string `json:"public_key_hex"`
	SignatureHex               string `json:"signature_hex"`
	BodyHex                    string `json:"body_hex"`
	SignatureOverLinkIDOnlyHex string `json:"signature_over_link_id_only_hex"`
	RNSVersionAtGeneration     string `json:"rns_version_at_generation"`
}

func loadLinkIdentifyVector(t *testing.T) linkIdentifyVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "link_identify_upstream.json"))
	if err != nil {
		t.Fatalf("read vector: %v", err)
	}
	var v linkIdentifyVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	return v
}

// The §6.7.6 body is public_key(64) || signature(64), and the signature
// covers link_id || public_key. These bytes came out of upstream Python
// RNS, not out of this package, so agreement here is interop rather than
// a self-round-trip (CLAUDE.md cardinal rule 3).
func TestLinkIdentifyUpstreamVectorVerifies(t *testing.T) {
	v := loadLinkIdentifyVector(t)
	linkID := mustHex(t, v.LinkIDHex)
	pub := mustHex(t, v.PublicKeyHex)
	sig := mustHex(t, v.SignatureHex)
	body := mustHex(t, v.BodyHex)

	if len(body) != LinkIdentifyBodyLen {
		t.Fatalf("upstream body is %d bytes, our LinkIdentifyBodyLen is %d", len(body), LinkIdentifyBodyLen)
	}
	if !bytes.Equal(body[:PublicKeyLen], pub) || !bytes.Equal(body[PublicKeyLen:], sig) {
		t.Fatal("body is not public_key || signature in that order")
	}
	if !VerifyLinkIdentify(linkID, pub, sig) {
		t.Fatalf("rejected a signature upstream RNS %s produced", v.RNSVersionAtGeneration)
	}
}

// §6.7.6 calls out signing link_id alone as the mistake that makes every
// allow-listed request fail. A conforming verifier must reject it —
// otherwise we would interoperate with our own bug.
func TestLinkIdentifyRejectsSignatureOverLinkIDAlone(t *testing.T) {
	v := loadLinkIdentifyVector(t)
	if VerifyLinkIdentify(mustHex(t, v.LinkIDHex), mustHex(t, v.PublicKeyHex),
		mustHex(t, v.SignatureOverLinkIDOnlyHex)) {
		t.Fatal("accepted a signature over link_id alone; §6.7.6 forbids this")
	}
}

func TestLinkIdentifyRejectsTamperedInputs(t *testing.T) {
	v := loadLinkIdentifyVector(t)
	linkID := mustHex(t, v.LinkIDHex)
	pub := mustHex(t, v.PublicKeyHex)
	sig := mustHex(t, v.SignatureHex)

	t.Run("wrong link_id", func(t *testing.T) {
		other := append([]byte(nil), linkID...)
		other[0] ^= 0xFF
		if VerifyLinkIdentify(other, pub, sig) {
			t.Fatal("accepted a signature bound to a different link")
		}
	})
	t.Run("flipped signature bit", func(t *testing.T) {
		bad := append([]byte(nil), sig...)
		bad[len(bad)-1] ^= 0x01
		if VerifyLinkIdentify(linkID, pub, bad) {
			t.Fatal("accepted a corrupted signature")
		}
	})
	t.Run("substituted public key", func(t *testing.T) {
		other, err := NewIdentity()
		if err != nil {
			t.Fatalf("NewIdentity: %v", err)
		}
		if VerifyLinkIdentify(linkID, other.PublicKey(), sig) {
			t.Fatal("accepted a signature under somebody else's key")
		}
	})
	t.Run("malformed lengths", func(t *testing.T) {
		if VerifyLinkIdentify(linkID[:8], pub, sig) ||
			VerifyLinkIdentify(linkID, pub[:32], sig) ||
			VerifyLinkIdentify(linkID, pub, sig[:32]) {
			t.Fatal("accepted a malformed input")
		}
	})
}

// activeLinkPair runs a real handshake and returns the initiator manager
// and link, the responder manager and link, and the initiator identity.
func activeLinkPair(t *testing.T) (*LinkManager, *Link, *LinkManager, *Link, *Identity) {
	t.Helper()
	alice, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity(alice): %v", err)
	}
	bob, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity(bob): %v", err)
	}
	sig := &LinkSignalling{MTU: 500, Mode: LinkModeAES256CBC}

	aliceMgr, bobMgr := NewLinkManager(), NewLinkManager()
	aliceLink, lrReq, err := aliceMgr.StartLinkAsInitiator(bob.DestinationHashFor(FullName("vectors", "link")), sig)
	if err != nil {
		t.Fatalf("StartLinkAsInitiator: %v", err)
	}
	bobLink, lrProof, err := bobMgr.AcceptIncomingLinkRequest(lrReq, bob, sig)
	if err != nil {
		t.Fatalf("AcceptIncomingLinkRequest: %v", err)
	}
	if _, err := aliceMgr.HandleLRProof(lrProof, bob.PublicKey()[32:]); err != nil {
		t.Fatalf("HandleLRProof: %v", err)
	}
	return aliceMgr, aliceLink, bobMgr, bobLink, alice
}

// The responder path this whole change exists for: an initiator's
// LINKIDENTIFY binds its long-term identity to the link (SPEC §6.7.6).
func TestResponderBindsPeerIdentityFromLinkIdentify(t *testing.T) {
	_, aliceLink, bobMgr, bobLink, alice := activeLinkPair(t)

	if bobLink.RemoteIdentity() != nil {
		t.Fatal("responder starts with a remote identity before any LINKIDENTIFY")
	}
	pkt, err := BuildLinkIdentify(aliceLink.ID, aliceLink.Signing, aliceLink.Encryption, alice)
	if err != nil {
		t.Fatalf("BuildLinkIdentify: %v", err)
	}
	if pkt.Context != ContextLinkIdentify {
		t.Fatalf("context is 0x%02x, want 0x%02x", pkt.Context, ContextLinkIdentify)
	}

	link, pub, err := bobMgr.HandleLinkIdentify(pkt)
	if err != nil {
		t.Fatalf("HandleLinkIdentify: %v", err)
	}
	if !bytes.Equal(pub, alice.PublicKey()) {
		t.Error("bound the wrong public key")
	}
	if !bytes.Equal(link.RemoteIdentity(), alice.PublicKey()) {
		t.Error("RemoteIdentity() does not report the bound key")
	}
}

// Upstream assigns remote_identity only while it is None, so a peer
// cannot re-identify as somebody else part-way through a link.
func TestLinkIdentifyDoesNotRebindAnIdentifiedLink(t *testing.T) {
	_, aliceLink, bobMgr, bobLink, alice := activeLinkPair(t)
	first, _ := BuildLinkIdentify(aliceLink.ID, aliceLink.Signing, aliceLink.Encryption, alice)
	if _, _, err := bobMgr.HandleLinkIdentify(first); err != nil {
		t.Fatalf("first identify: %v", err)
	}

	mallory, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	second, _ := BuildLinkIdentify(aliceLink.ID, aliceLink.Signing, aliceLink.Encryption, mallory)
	if _, _, err := bobMgr.HandleLinkIdentify(second); err == nil {
		t.Error("a second LINKIDENTIFY was accepted; upstream binds only once")
	}
	if !bytes.Equal(bobLink.RemoteIdentity(), alice.PublicKey()) {
		t.Error("the link was rebound to a different identity")
	}
}

// Upstream guards the whole branch on `not self.initiator`.
func TestLinkIdentifyIsIgnoredOnAnInitiatorLink(t *testing.T) {
	aliceMgr, aliceLink, _, _, alice := activeLinkPair(t)
	pkt, _ := BuildLinkIdentify(aliceLink.ID, aliceLink.Signing, aliceLink.Encryption, alice)

	if _, _, err := aliceMgr.HandleLinkIdentify(pkt); err == nil {
		t.Error("an initiator accepted a LINKIDENTIFY; upstream ignores it")
	}
	if aliceLink.RemoteIdentity() != nil {
		t.Error("initiator bound a remote identity")
	}
}

// A body that is not exactly public_key(64) || signature(64) is refused
// before any crypto work.
func TestLinkIdentifyRejectsWrongBodyLength(t *testing.T) {
	_, aliceLink, bobMgr, _, _ := activeLinkPair(t)
	short, err := BuildLinkDataPacket(aliceLink.ID, aliceLink.Signing, aliceLink.Encryption, []byte("too short"))
	if err != nil {
		t.Fatalf("BuildLinkDataPacket: %v", err)
	}
	short.Context = ContextLinkIdentify
	if _, _, err := bobMgr.HandleLinkIdentify(short); err == nil {
		t.Error("accepted a LINKIDENTIFY whose body is the wrong length")
	}
}

// TestLinkIdentifyAssignOnceUnderConcurrency exercises the assign-once
// rule from several goroutines at once. Two valid identifications
// racing must leave exactly one winner: the invariant is what stops a
// peer re-identifying as somebody else mid-link, and it must hold in
// this function rather than depending on the Transport happening to
// dispatch on a single goroutine.
func TestLinkIdentifyAssignOnceUnderConcurrency(t *testing.T) {
	const racers = 8
	_, aliceLink, bobMgr, bobLink, _ := activeLinkPair(t)

	// Every racer presents a DIFFERENT valid identity, so a lost race
	// is visible as a changed binding rather than an idempotent rewrite.
	pkts := make([]*Packet, 0, racers)
	pubs := make([][]byte, 0, racers)
	for i := 0; i < racers; i++ {
		id, err := NewIdentity()
		if err != nil {
			t.Fatal(err)
		}
		pkt, err := BuildLinkIdentify(aliceLink.ID, aliceLink.Signing, aliceLink.Encryption, id)
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, pkt)
		pubs = append(pubs, id.PublicKey())
	}

	var wg sync.WaitGroup
	var wins atomic.Int32
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(pkt *Packet) {
			defer wg.Done()
			<-start
			if _, pub, err := bobMgr.HandleLinkIdentify(pkt); err == nil && pub != nil {
				wins.Add(1)
			}
		}(pkts[i])
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Errorf("%d identifications were accepted, want exactly 1", got)
	}
	bound := bobLink.RemoteIdentity()
	if bound == nil {
		t.Fatal("no identity bound after the race")
	}
	matches := 0
	for _, p := range pubs {
		if bytes.Equal(bound, p) {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("bound identity matches %d of the racers, want exactly 1", matches)
	}
}
