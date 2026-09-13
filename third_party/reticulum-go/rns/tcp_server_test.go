package rns

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Frames must be at least header1MinLen bytes: TCPClient.readLoop drops
// anything shorter as a malformed Reticulum header, so a toy payload
// never reaches the far side. Tests here use packet-sized bytes.
func testFrame(tag byte) []byte {
	f := bytes.Repeat([]byte{tag}, header1MinLen+8)
	return f
}

func dialToServer(t *testing.T, s *TCPServer) *TCPClient {
	t.Helper()
	c, err := DialTCP(s.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitPeers(t *testing.T, s *TCPServer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Peers() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("peer count is %d, want %d", s.Peers(), want)
}

func TestTCPServerAcceptsAndReceives(t *testing.T) {
	s, err := ListenTCP("127.0.0.1:0", noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	client := dialToServer(t, s)
	waitPeers(t, s, 1)

	payload := testFrame(0xA1)
	if err := client.Send(payload); err != nil {
		t.Fatalf("client send: %v", err)
	}
	select {
	case got := <-s.Inbox():
		if !bytes.Equal(got, payload) {
			t.Errorf("server received %x, want %x", got, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the frame")
	}
}

// A Send reaches every connected peer — upstream spawns one interface
// per client and broadcasts to each; we present one Interface and fan
// out internally, and the observable behaviour must match.
func TestTCPServerBroadcastsToAllPeers(t *testing.T) {
	s, err := ListenTCP("127.0.0.1:0", noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a := dialToServer(t, s)
	b := dialToServer(t, s)
	waitPeers(t, s, 2)

	payload := testFrame(0xB2)
	if err := s.Send(payload); err != nil {
		t.Fatalf("server send: %v", err)
	}
	for name, c := range map[string]*TCPClient{"a": a, "b": b} {
		select {
		case got := <-c.Inbox():
			if !bytes.Equal(got, payload) {
				t.Errorf("peer %s received %x", name, got)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("peer %s never received the broadcast", name)
		}
	}
}

// One dead peer must not stop the others from receiving. A write error
// closes that connection and the pump reaps it, while the packet still
// reaches everyone live.
func TestTCPServerSurvivesADeadPeer(t *testing.T) {
	s, err := ListenTCP("127.0.0.1:0", noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	dead := dialToServer(t, s)
	live := dialToServer(t, s)
	waitPeers(t, s, 2)
	_ = dead.Close()

	// Send until the server has noticed the drop; the live peer must
	// receive every attempt regardless.
	deadline := time.Now().Add(2 * time.Second)
	for s.Peers() > 1 && time.Now().Before(deadline) {
		_ = s.Send(testFrame(0xC3))
		time.Sleep(10 * time.Millisecond)
	}
	if s.Peers() != 1 {
		t.Fatalf("dead peer was not reaped: %d peers", s.Peers())
	}
	if err := s.Send(testFrame(0xC4)); err != nil {
		t.Errorf("send with only live peers: %v", err)
	}
	select {
	case <-live.Inbox():
	case <-time.After(2 * time.Second):
		t.Error("live peer received nothing while a dead peer was present")
	}
}

// The listener is reachable pre-authentication by anyone who can route
// to us, so the peer count needs a ceiling.
func TestTCPServerEnforcesPeerCap(t *testing.T) {
	s, err := ListenTCP("127.0.0.1:0", noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetMaxPeers(1)

	dialToServer(t, s)
	waitPeers(t, s, 1)

	// The second connection is accepted by the kernel and then closed
	// by us, so the dial succeeds but the peer set does not grow.
	over, err := DialTCP(s.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer over.Close()

	time.Sleep(200 * time.Millisecond)
	if got := s.Peers(); got != 1 {
		t.Errorf("peer count is %d, want the cap of 1", got)
	}
}

// End to end through the Transport: a packet a peer sends over TCP must
// reach the announce handler, which is what "we can host something"
// actually means.
func TestTCPServerFeedsTheTransport(t *testing.T) {
	s, err := ListenTCP("127.0.0.1:0", noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tp := NewTransport(noopLogger{})
	tp.AddInterface(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tp.Run(ctx)

	id, _ := NewIdentity()
	name := FullName("vectors", "tcpserver")
	appData, _ := EncodeLXMFAppData([]byte("peer"), nil)
	pkt, err := BuildAnnounce(id, name, appData, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pkt.Pack()
	if err != nil {
		t.Fatal(err)
	}

	client := dialToServer(t, s)
	waitPeers(t, s, 1)
	if err := client.Send(raw); err != nil {
		t.Fatal(err)
	}

	destHash := id.DestinationHashFor(name)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tp.Recall(destHash) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("announce arriving over the TCP server never reached the Transport")
}

func TestTCPServerCloseIsIdempotent(t *testing.T) {
	s, err := ListenTCP("127.0.0.1:0", noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	dialToServer(t, s)
	waitPeers(t, s, 1)

	if err := s.Close(); err != nil {
		t.Errorf("first close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Error("Done was not closed after Close")
	}
	if err := s.Send(testFrame(0xD5)); err == nil {
		t.Error("Send succeeded on a closed server")
	}
}
