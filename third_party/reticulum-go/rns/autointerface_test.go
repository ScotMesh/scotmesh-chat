package rns

import (
	"encoding/hex"
	"testing"
	"time"
)

// The multicast group and discovery token are mirrored from upstream
// AutoInterface.py, not from the spec — §8 is explicit that the
// discovery protocol is AutoInterface's own. These values were computed
// with upstream RNS 1.5.0 for the default group id, so they pin the
// derivation against the thing we have to interoperate with.
func TestAutoInterfaceDerivationsMatchUpstream(t *testing.T) {
	const wantGroupHash = "eac4d70bfb1c16e45e39485e31e1f5ccb18cedf878e0310d9a96100168f89f0d"
	if got := hex.EncodeToString(AutoInterfaceGroupHash(DefaultGroupID)); got != wantGroupHash {
		t.Errorf("group hash = %s, upstream = %s", got, wantGroupHash)
	}

	// ff12:0:... — the first group is the literal "0"; upstream computes
	// one from the hash and then discards it (the line is commented out
	// in its source). Deriving it instead puts us on an address no
	// upstream peer is listening to.
	const wantAddr = "ff12:0:d70b:fb1c:16e4:5e39:485e:31e1"
	got := MulticastDiscoveryAddress(DefaultGroupID, MulticastTemporaryAddressType, MulticastScopeLink)
	if got != wantAddr {
		t.Errorf("multicast address = %s, upstream = %s", got, wantAddr)
	}

	const wantToken = "97b25576749ea936b0d8a8536ffaf442d157cf47d460dcf13c48b7bd18b6c163"
	if got := hex.EncodeToString(DiscoveryToken(DefaultGroupID, "fe80::1")); got != wantToken {
		t.Errorf("discovery token = %s, upstream = %s", got, wantToken)
	}
}

// A different group id must land on a different address, which is what
// makes group separation a real partition rather than a label.
func TestAutoInterfaceGroupSeparation(t *testing.T) {
	a := MulticastDiscoveryAddress("reticulum", MulticastTemporaryAddressType, MulticastScopeLink)
	b := MulticastDiscoveryAddress("something-else", MulticastTemporaryAddressType, MulticastScopeLink)
	if a == b {
		t.Error("two group ids derived the same multicast address")
	}
	if DiscoveryToken("a", "fe80::1") == nil {
		t.Fatal("nil token")
	}
	if hex.EncodeToString(DiscoveryToken("a", "fe80::1")) == hex.EncodeToString(DiscoveryToken("b", "fe80::1")) {
		t.Error("two group ids produced the same discovery token")
	}
}

// The token binds the sender to the address it announced from. A token
// observed on the wire and replayed from elsewhere must not peer.
func TestDiscoveryTokenIsBoundToTheSourceAddress(t *testing.T) {
	a := NewAutoInterface(DefaultGroupID)
	now := time.Now()

	good := a.Token("fe80::abcd")
	if !a.HandleDiscovery(good, "fe80::abcd", now) {
		t.Fatal("a valid token from its own address was rejected")
	}
	if len(a.Peers(now)) != 1 {
		t.Fatalf("peer count = %d, want 1", len(a.Peers(now)))
	}

	// Same token, different source: this is the replay case.
	if a.HandleDiscovery(good, "fe80::9999", now) {
		t.Error("a token replayed from another address was accepted")
	}
	// Wrong group id entirely.
	if a.HandleDiscovery(DiscoveryToken("other-group", "fe80::1234"), "fe80::1234", now) {
		t.Error("a token from a different group was accepted")
	}
	if a.HandleDiscovery([]byte{0x01, 0x02}, "fe80::1234", now) {
		t.Error("a truncated token was accepted")
	}
	if len(a.Peers(now)) != 1 {
		t.Errorf("peer count = %d after the rejected attempts, want 1", len(a.Peers(now)))
	}
}

// Peers expire after PEERING_TIMEOUT, and a re-announce refreshes them.
func TestAutoInterfacePeerExpiry(t *testing.T) {
	a := NewAutoInterface(DefaultGroupID)
	now := time.Now()
	addr := "fe80::dead"

	if !a.HandleDiscovery(a.Token(addr), addr, now) {
		t.Fatal("discovery rejected")
	}
	if got := len(a.Peers(now.Add(PeeringTimeout - time.Second))); got != 1 {
		t.Errorf("peer count = %d inside the timeout, want 1", got)
	}
	if got := len(a.Peers(now.Add(PeeringTimeout + time.Second))); got != 0 {
		t.Errorf("peer count = %d past the timeout, want 0", got)
	}

	// A re-announce brings it back and refreshes the clock.
	later := now.Add(PeeringTimeout * 2)
	if !a.HandleDiscovery(a.Token(addr), addr, later) {
		t.Fatal("re-announce rejected")
	}
	if got := len(a.Peers(later.Add(time.Second))); got != 1 {
		t.Errorf("peer count = %d after re-announce, want 1", got)
	}
}

// A repeated announce from a live peer must refresh rather than
// duplicate.
func TestAutoInterfaceDeduplicatesPeers(t *testing.T) {
	a := NewAutoInterface(DefaultGroupID)
	now := time.Now()
	addr := "fe80::beef"
	for i := 0; i < 5; i++ {
		if !a.HandleDiscovery(a.Token(addr), addr, now.Add(time.Duration(i)*time.Second)) {
			t.Fatal("discovery rejected")
		}
	}
	if got := len(a.Peers(now.Add(5 * time.Second))); got != 1 {
		t.Errorf("peer count = %d after five announces from one peer, want 1", got)
	}
}

func TestAutoInterfaceInboxAndClose(t *testing.T) {
	a := NewAutoInterface(DefaultGroupID)
	a.Deliver([]byte("packet"))
	select {
	case got := <-a.Inbox():
		if string(got) != "packet" {
			t.Errorf("inbox delivered %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing arrived on the inbox")
	}

	if err := a.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	select {
	case <-a.Done():
	case <-time.After(time.Second):
		t.Error("Done was not closed")
	}
	if err := a.Send([]byte("x")); err == nil {
		t.Error("Send succeeded on a closed interface")
	}
}

// It satisfies the Interface contract, so it can be handed to a
// Transport like any other bearer.
func TestAutoInterfaceSatisfiesInterface(t *testing.T) {
	var _ Interface = NewAutoInterface(DefaultGroupID)
}
