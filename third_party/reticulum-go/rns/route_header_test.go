package rns

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// A HEADER_2 announce that arrived with hops 0 came from a destination
// attached to the relaying node itself: a local client of its rnsd, such as
// lxmd's propagation node. Python RNS counts that as one hop and addresses
// packets to it plain (HEADER_1). rnsd 1.5 hands a HEADER_2 link request
// addressed to itself to such a local client unstripped, and the LRPROOF
// never comes back, so every link to it timed out.
func TestLinkRequestHeaderFollowsHops(t *testing.T) {
	relay := bytes.Repeat([]byte{0xAB}, IdentityHashLen)
	cases := []struct {
		name     string
		hops     byte
		wantType byte
	}{
		{"local client of our neighbour", 0, HeaderType1},
		{"one relay beyond our neighbour", 1, HeaderType2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			iface := newCaptureIface()
			tr := NewTransport(noopLogger{})
			tr.AddInterface(iface)
			id, _ := NewIdentity()
			dest := id.DestinationHashFor(FullName("lxmf", "propagation"))
			tr.mu.Lock()
			tr.known[hex.EncodeToString(dest)] = &KnownIdentity{
				DestHash: dest, PublicKey: id.PublicKey(), Hops: c.hops,
				TransportID: relay, LastSeen: time.Now(),
			}
			tr.mu.Unlock()

			err := tr.SendOverLink(dest, []byte("x"), 200*time.Millisecond)
			if !errors.Is(err, ErrLinkHandshakeTimeout) {
				t.Fatalf("SendOverLink err = %v, want handshake timeout (nobody answers)", err)
			}
			sent := iface.Snapshot()
			if len(sent) == 0 {
				t.Fatal("no link request was sent")
			}
			lr, err := ParsePacket(sent[0])
			if err != nil {
				t.Fatalf("parse link request: %v", err)
			}
			if lr.HeaderType != c.wantType {
				t.Errorf("link request header = %d, want %d", lr.HeaderType, c.wantType)
			}
			if c.wantType == HeaderType2 && !bytes.Equal(lr.TransportID, relay) {
				t.Errorf("link request transport_id = %x, want %x", lr.TransportID, relay)
			}
		})
	}
}

func TestRouteTransportID(t *testing.T) {
	relay := bytes.Repeat([]byte{0xAB}, IdentityHashLen)
	if got := (&KnownIdentity{Hops: 0, TransportID: relay}).RouteTransportID(); got != nil {
		t.Errorf("hops 0: RouteTransportID = %x, want nil", got)
	}
	if got := (&KnownIdentity{Hops: 3, TransportID: relay}).RouteTransportID(); !bytes.Equal(got, relay) {
		t.Errorf("hops 3: RouteTransportID = %x, want %x", got, relay)
	}
	if got := (&KnownIdentity{Hops: 3}).RouteTransportID(); got != nil {
		t.Errorf("no transport_id: RouteTransportID = %x, want nil", got)
	}
	var nilKnown *KnownIdentity
	if got := nilKnown.RouteTransportID(); got != nil {
		t.Errorf("nil identity: RouteTransportID = %x, want nil", got)
	}
}
