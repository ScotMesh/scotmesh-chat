package rns

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// --- the packet -------------------------------------------------------

// SPEC §6.7.3: body is the 16-byte link_id, link-encrypted.
func TestLinkCloseRoundTrips(t *testing.T) {
	link, _, _ := makeActiveTestLink(t)
	pkt, err := BuildLinkClose(link.ID, link.Signing, link.Encryption)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if pkt.Context != ContextLinkClose {
		t.Errorf("context = 0x%02x, want 0x%02x", pkt.Context, ContextLinkClose)
	}
	if pkt.PacketType != PacketData || pkt.DestinationType != DestinationLink {
		t.Errorf("packet_type=%d dest_type=%d, want DATA/LINK", pkt.PacketType, pkt.DestinationType)
	}
	if !bytes.Equal(pkt.DestHash, link.ID) {
		t.Errorf("dest_hash = %x, want the link_id %x", pkt.DestHash, link.ID)
	}
	if bytes.Equal(pkt.Data, link.ID) {
		t.Error("body is the plaintext link_id; it must be link-encrypted")
	}
	if err := ParseLinkClosePacket(pkt, link.ID, link.Signing, link.Encryption); err != nil {
		t.Errorf("parse of our own packet: %v", err)
	}
}

// The encrypted body is the ONLY authentication a LINKCLOSE has. Its
// dest_hash is the link_id in cleartext, so without this check anyone
// who has observed a single packet on a link could tear it down — a
// trivial denial of service against every link they can see.
func TestLinkCloseFromSomebodyWithoutTheSessionKeyIsRefused(t *testing.T) {
	link, _, _ := makeActiveTestLink(t)

	// An attacker who knows the link_id (it is on the wire in clear)
	// but not the session keys.
	forged, err := BuildLinkClose(link.ID,
		bytes.Repeat([]byte{0x99}, 32), bytes.Repeat([]byte{0x88}, 32))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := ParseLinkClosePacket(forged, link.ID, link.Signing, link.Encryption); err == nil {
		t.Fatal("a LINKCLOSE encrypted under the wrong key was accepted")
	}
}

// A well-formed LINKCLOSE for a DIFFERENT link, replayed onto this one.
func TestLinkCloseForAnotherLinkIsRefused(t *testing.T) {
	link, _, _ := makeActiveTestLink(t)
	other := bytes.Repeat([]byte{0xCD}, IdentityHashLen)

	pkt, err := BuildLinkClose(other, link.Signing, link.Encryption)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	pkt.DestHash = link.ID // addressed here, but the sealed body says otherwise
	if err := ParseLinkClosePacket(pkt, link.ID, link.Signing, link.Encryption); err == nil {
		t.Fatal("a LINKCLOSE whose sealed body names another link was accepted")
	}
}

func TestLinkCloseRejectsTheWrongContext(t *testing.T) {
	link, _, _ := makeActiveTestLink(t)
	pkt, _ := BuildLinkClose(link.ID, link.Signing, link.Encryption)
	pkt.Context = ContextLinkIdentify
	if err := ParseLinkClosePacket(pkt, link.ID, link.Signing, link.Encryption); err == nil {
		t.Fatal("accepted a packet whose context is not LINKCLOSE")
	}
}

// --- the manager ------------------------------------------------------

func TestHandleLinkCloseClosesTheLinkAndReportsTheReason(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	pkt, _ := BuildLinkClose(link.ID, link.Signing, link.Encryption)

	_, reason, err := tp.linkManager.HandleLinkClose(pkt)
	if err != nil {
		t.Fatalf("HandleLinkClose: %v", err)
	}
	// makeActiveTestLink has no responderIdentity and no peerDestHash,
	// so it is not an initiator: SPEC §6.7.4 INITIATOR_CLOSED.
	if reason != TeardownInitiatorClosed {
		t.Errorf("reason = 0x%02x, want 0x%02x", reason, TeardownInitiatorClosed)
	}
	if tp.linkManager.Get(link.ID) != nil {
		t.Error("the link is still in the manager after a valid LINKCLOSE")
	}
}

func TestAnUnauthenticatedLinkCloseClosesNothing(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	forged, _ := BuildLinkClose(link.ID,
		bytes.Repeat([]byte{0x99}, 32), bytes.Repeat([]byte{0x88}, 32))

	if _, _, err := tp.linkManager.HandleLinkClose(forged); err == nil {
		t.Fatal("HandleLinkClose accepted a forged packet")
	}
	if tp.linkManager.Get(link.ID) == nil {
		t.Error("a forged LINKCLOSE tore the link down anyway")
	}
}

// --- the callback -----------------------------------------------------

// The callback is the whole point: without it a consumer holding
// per-link state has no way to learn the link is gone.
func TestTheLinkClosedHandlerFiresOnAPeerTeardown(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	var mu sync.Mutex
	var gotID []byte
	var gotReason byte
	var calls int
	tp.linkManager.SetLinkClosedHandler(func(id []byte, reason byte) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		gotID, gotReason = id, reason
	})

	pkt, _ := BuildLinkClose(link.ID, link.Signing, link.Encryption)
	tp.handleLinkClose(pkt)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("handler fired %d times, want 1", calls)
	}
	if !bytes.Equal(gotID, link.ID) {
		t.Errorf("handler got link_id %x, want %x", gotID, link.ID)
	}
	if gotReason != TeardownInitiatorClosed {
		t.Errorf("handler got reason 0x%02x, want 0x%02x", gotReason, TeardownInitiatorClosed)
	}
}

func TestTheLinkClosedHandlerFiresOnALocalClose(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	var mu sync.Mutex
	var calls int
	tp.linkManager.SetLinkClosedHandler(func([]byte, byte) {
		mu.Lock()
		defer mu.Unlock()
		calls++
	})

	tp.linkManager.CloseLink(link.ID)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("handler fired %d times on a local close, want 1", calls)
	}
}

// SPEC §12, pitfall 3: link-closed fires from two paths, so it must be
// idempotent. One link closing must not be reported twice.
func TestOneLinkClosingIsReportedOnce(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	var mu sync.Mutex
	var calls int
	tp.linkManager.SetLinkClosedHandler(func([]byte, byte) {
		mu.Lock()
		defer mu.Unlock()
		calls++
	})

	pkt, _ := BuildLinkClose(link.ID, link.Signing, link.Encryption)
	tp.handleLinkClose(pkt)           // peer tears it down
	tp.linkManager.CloseLink(link.ID) // and we close it again
	tp.handleLinkClose(pkt)           // and the LINKCLOSE is retransmitted

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("one link closing produced %d callbacks, want 1", calls)
	}
}

// A handler that calls back into the LinkManager must not deadlock —
// and a handler is exactly the kind of code that wants to.
func TestAHandlerMayCallBackIntoTheManager(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	done := make(chan struct{})
	tp.linkManager.SetLinkClosedHandler(func(id []byte, _ byte) {
		_ = tp.linkManager.Get(id)
		tp.linkManager.CloseLink(id)
		close(done)
	})

	tp.linkManager.CloseLink(link.ID)
	select {
	case <-done:
	default:
		t.Fatal("handler did not run to completion")
	}
}

// --- the outbound half ------------------------------------------------

// SPEC §6.7.2 step 4: closing locally must tell the peer, so it does not
// sit through its own watchdog to reach the same conclusion.
func TestTeardownLinkSendsALinkClosePacket(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	signing := append([]byte(nil), link.Signing...)
	encryption := append([]byte(nil), link.Encryption...)
	linkID := append([]byte(nil), link.ID...)

	tp.TeardownLink(linkID)

	var found *Packet
	for _, raw := range iface.Snapshot() {
		p, err := ParsePacket(raw)
		if err != nil {
			continue
		}
		if p.Context == ContextLinkClose {
			found = p
			break
		}
	}
	if found == nil {
		t.Fatal("TeardownLink sent no LINKCLOSE")
	}
	// The peer must be able to authenticate it.
	if err := ParseLinkClosePacket(found, linkID, signing, encryption); err != nil {
		t.Errorf("the LINKCLOSE we sent does not authenticate: %v", err)
	}
	if tp.linkManager.Get(linkID) != nil {
		t.Error("TeardownLink sent the packet but left the link open")
	}
}

// --- which closures a consumer hears about, and as what --------------

// A link that never reached Active must not be reported as closed. The
// internal cleanup paths — a LINKREQUEST whose broadcast failed, a
// handshake that timed out — all close a link that was registered at
// StartLinkAsInitiator but never established. A consumer tracking
// presence would otherwise be told a peer left when none ever arrived.
func TestClosingALinkThatNeverEstablishedReportsNothing(t *testing.T) {
	_, tp, _ := makeActiveTestLink(t)

	pending := &Link{
		ID:           bytes.Repeat([]byte{0xCD}, IdentityHashLen),
		State:        LinkPending,
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	}
	tp.linkManager.mu.Lock()
	tp.linkManager.links[bytesHexEncode(pending.ID)] = pending
	tp.linkManager.mu.Unlock()

	var mu sync.Mutex
	var calls int
	tp.linkManager.SetLinkClosedHandler(func([]byte, byte) {
		mu.Lock()
		defer mu.Unlock()
		calls++
	})

	tp.linkManager.CloseLink(pending.ID)

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("handler fired %d times for a link that never established, want 0", calls)
	}
}

// SPEC §6.7.2 gives the teardown reason one job: let the application
// "distinguish 'the peer went dark' from 'the peer cleanly closed'".
// A deliberate local teardown is not a timeout and must not say it is.
func TestTeardownLinkReportsALocalClose(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)

	var mu sync.Mutex
	var got []byte
	tp.linkManager.SetLinkClosedHandler(func(_ []byte, reason byte) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, reason)
	})

	tp.TeardownLink(append([]byte(nil), link.ID...))

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("handler fired %d times, want 1", len(got))
	}
	if got[0] != TeardownLocalClosed {
		t.Errorf("local teardown reported reason 0x%02x, want TeardownLocalClosed (0x%02x)",
			got[0], TeardownLocalClosed)
	}
}

// The other half of the same distinction: the idle sweep IS the
// watchdog, so it reports TIMEOUT — and per SPEC §6.7.2 it also emits
// the teardown packet rather than letting the peer time out too.
func TestTheIdleSweepReportsTimeoutAndTellsThePeer(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	linkID := append([]byte(nil), link.ID...)
	signing := append([]byte(nil), link.Signing...)
	encryption := append([]byte(nil), link.Encryption...)

	// Explicit, so the test does not ride on the default thresholds.
	// sweepLinks reads t.lifetime, which is lazily built — a Transport
	// that has never run the sweeper has none.
	tp.SetLinkLifetime(30*time.Second, time.Minute, time.Minute)

	link.mu.Lock()
	link.LastActivity = time.Now().Add(-24 * time.Hour)
	link.mu.Unlock()

	var mu sync.Mutex
	var got []byte
	tp.linkManager.SetLinkClosedHandler(func(_ []byte, reason byte) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, reason)
	})

	tp.sweepLinks()

	mu.Lock()
	if len(got) != 1 {
		mu.Unlock()
		t.Fatalf("handler fired %d times on the idle sweep, want 1", len(got))
	}
	if got[0] != TeardownTimeout {
		t.Errorf("idle sweep reported reason 0x%02x, want TeardownTimeout (0x%02x)",
			got[0], TeardownTimeout)
	}
	mu.Unlock()

	var found *Packet
	for _, raw := range iface.Snapshot() {
		p, err := ParsePacket(raw)
		if err != nil {
			continue
		}
		if p.Context == ContextLinkClose {
			found = p
			break
		}
	}
	if found == nil {
		t.Fatal("the idle sweep sent no LINKCLOSE (SPEC §6.7.2)")
	}
	if err := ParseLinkClosePacket(found, linkID, signing, encryption); err != nil {
		t.Errorf("the sweep's LINKCLOSE does not authenticate: %v", err)
	}
}
