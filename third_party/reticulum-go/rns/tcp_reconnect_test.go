package rns

import (
	"bytes"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeListener exposes a localhost TCP endpoint that captures every
// accepted connection. dropLast() closes the most recently accepted
// conn, simulating a peer-side drop — the same observable behavior a
// long-running fwdsvc sees when an upstream TCPServerInterface or NAT
// silently kills the socket (issue #6).
type fakeListener struct {
	ln       net.Listener
	accepts  chan net.Conn
	mu       sync.Mutex
	accepted []net.Conn
	closed   bool
}

func newFakeListener(t *testing.T) *fakeListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeListener{ln: ln, accepts: make(chan net.Conn, 8)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.accepted = append(f.accepted, c)
			f.closed = false
			f.mu.Unlock()
			select {
			case f.accepts <- c:
			default:
			}
		}
	}()
	return f
}

func (f *fakeListener) addr() string { return f.ln.Addr().String() }

func (f *fakeListener) dropLast() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.accepted) == 0 {
		return
	}
	f.accepted[len(f.accepted)-1].Close()
}

func (f *fakeListener) close() {
	f.ln.Close()
	f.mu.Lock()
	for _, c := range f.accepted {
		c.Close()
	}
	f.mu.Unlock()
}

// TestReconnectingTCPClientSurvivesPeerDrop reproduces issue #6: a TCP
// connection dies and the service must transparently reconnect instead
// of silently sitting on a broken socket forever. The wrapper must:
//   - keep its outer Done() un-fired (Transport.Run's fan-in must not exit)
//   - re-establish the underlying TCP connection to the same address
//   - resume Send() succeeding on the new connection
func TestReconnectingTCPClientSurvivesPeerDrop(t *testing.T) {
	fl := newFakeListener(t)
	defer fl.close()

	logger := log.New(io.Discard, "", 0)
	rc, err := dialReconnectingTCPForTest(fl.addr(), 2*time.Second, logger, fastRetryPolicy())
	if err != nil {
		t.Fatalf("dialReconnectingTCP: %v", err)
	}
	defer rc.Close()

	// First conn arrives at the listener.
	var first net.Conn
	select {
	case first = <-fl.accepts:
	case <-time.After(1 * time.Second):
		t.Fatal("listener never received first accept")
	}

	// Send a packet — peer reads it on the first conn.
	payload := bytes.Repeat([]byte{0xAB}, 20)
	if err := rc.Send(payload); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if frame := readFrameWithTimeout(t, first, 500*time.Millisecond); !bytes.Equal(frame, payload) {
		t.Fatalf("first frame mismatch: got %x want %x", frame, payload)
	}

	// Simulate the peer-side TCP drop from issue #6.
	fl.dropLast()

	// Listener must see a fresh dial from the supervisor.
	var second net.Conn
	select {
	case second = <-fl.accepts:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect never arrived after peer drop")
	}

	// Outer Done() must NOT have fired. If it had, Transport.Run's
	// fan-in goroutine for this interface would have exited and the
	// service would be permanently disconnected (the original bug).
	select {
	case <-rc.Done():
		t.Fatal("outer Done() fired on peer drop; should only fire on explicit Close()")
	default:
	}

	// Send succeeds against the redialed conn. Retry briefly to absorb
	// the tiny window between Accept returning and the supervisor
	// swapping the current inner client under the lock.
	payload = bytes.Repeat([]byte{0xCD}, 20)
	deadline := time.Now().Add(1 * time.Second)
	var sendErr error
	for time.Now().Before(deadline) {
		sendErr = rc.Send(payload)
		if sendErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sendErr != nil {
		t.Fatalf("post-reconnect Send: %v", sendErr)
	}
	if frame := readFrameWithTimeout(t, second, 500*time.Millisecond); !bytes.Equal(frame, payload) {
		t.Fatalf("second frame mismatch: got %x want %x", frame, payload)
	}

	_ = first
}

// TestReconnectingTCPClientReportsConnected covers the Connected() ScotMesh
// patch: true once dialed, false during the reconnect window after a peer
// drop, true again once the supervisor redials.
func TestReconnectingTCPClientReportsConnected(t *testing.T) {
	fl := newFakeListener(t)
	defer fl.close()

	logger := log.New(io.Discard, "", 0)
	rc, err := dialReconnectingTCPForTest(fl.addr(), 2*time.Second, logger, fastRetryPolicy())
	if err != nil {
		t.Fatalf("dialReconnectingTCP: %v", err)
	}
	defer rc.Close()

	if !rc.Connected() {
		t.Fatal("Connected() should be true right after a successful dial")
	}

	select {
	case <-fl.accepts:
	case <-time.After(1 * time.Second):
		t.Fatal("listener never received first accept")
	}
	fl.dropLast()

	deadline := time.Now().Add(1 * time.Second)
	for rc.Connected() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rc.Connected() {
		t.Fatal("Connected() should be false while redialing after a peer drop")
	}

	select {
	case <-fl.accepts:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect never arrived after peer drop")
	}
	deadline = time.Now().Add(1 * time.Second)
	for !rc.Connected() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !rc.Connected() {
		t.Fatal("Connected() should be true again after the supervisor redials")
	}

	rc.Close()
	if rc.Connected() {
		t.Fatal("Connected() should be false after Close()")
	}
}

func TestReconnectingTCPClientOuterDoneFiresOnExplicitClose(t *testing.T) {
	fl := newFakeListener(t)
	defer fl.close()

	logger := log.New(io.Discard, "", 0)
	rc, err := dialReconnectingTCPForTest(fl.addr(), 2*time.Second, logger, fastRetryPolicy())
	if err != nil {
		t.Fatalf("dialReconnectingTCP: %v", err)
	}

	select {
	case <-fl.accepts:
	case <-time.After(1 * time.Second):
		t.Fatal("listener never received accept")
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-rc.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("outer Done() did not fire after explicit Close()")
	}
}

func readFrameWithTimeout(t *testing.T, c net.Conn, d time.Duration) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Time{})
	dec := NewHDLCDecoder(c)
	f, err := dec.NextFrame()
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return f
}

// refusingListener accepts every connection and closes it immediately,
// which is how a peer that has denylisted our address behaves: the TCP
// handshake completes, then the socket shuts before a byte is exchanged.
type refusingListener struct {
	ln      net.Listener
	accepts chan time.Time
}

func newRefusingListener(t *testing.T) *refusingListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &refusingListener{ln: ln, accepts: make(chan time.Time, 64)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case f.accepts <- time.Now():
			default:
			}
			c.Close()
		}
	}()
	return f
}

func (f *refusingListener) addr() string { return f.ln.Addr().String() }
func (f *refusingListener) close()       { f.ln.Close() }

// TestReconnectingTCPClientBacksOffFromARefusingPeer is the regression
// test for the reconnect storm that plausibly earned an operator's
// address a denylisting upstream.
//
// The old supervisor re-initialised its backoff at the top of every
// disconnect cycle, so the exponential ramp only applied to consecutive
// *dial failures*. Against this listener the dial always succeeds, so
// the ramp never advanced: a redial every initialBackoff, forever.
//
// The assertion is on elapsed time rather than the individual gaps,
// because scheduling noise can only ever make a gap longer — so a loaded
// runner cannot turn a passing run into a failure, only the absence of a
// ramp can.
func TestReconnectingTCPClientBacksOffFromARefusingPeer(t *testing.T) {
	fl := newRefusingListener(t)
	defer fl.close()

	policy := fastRetryPolicy()
	logs := &syncBuffer{}
	rc, err := dialReconnectingTCPForTest(fl.addr(), 2*time.Second, log.New(logs, "", 0), policy)
	if err != nil {
		t.Fatalf("dialReconnectingTCP: %v", err)
	}
	defer rc.Close()

	// The initial dial plus four supervised redials: the ramp should be
	// at 20, 40, 80 and 160ms, so ~300ms before jitter. A flat ramp at
	// the floor would deliver the same five accepts in ~80ms.
	const want = 5
	var times []time.Time
	for len(times) < want {
		select {
		case ts := <-fl.accepts:
			times = append(times, ts)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d reconnect attempts arrived", len(times), want)
		}
	}

	elapsed := times[len(times)-1].Sub(times[0])
	floor := 4 * policy.connectFailFloor // what a non-climbing ramp would cost
	if elapsed <= 2*floor {
		t.Fatalf("%d attempts in %v — the connect ramp is not climbing "+
			"(a flat ramp at the %v floor would take ~%v)",
			want, elapsed, policy.connectFailFloor, floor)
	}

	// Assert the classification directly rather than only inferring it
	// from the timing: this is the line the bug was on, and a log check
	// cannot be knocked over by a slow runner.
	if !strings.Contains(logs.String(), "treating as a refusal") {
		t.Fatalf("supervisor did not classify accepted-then-closed as a refusal; log:\n%s", logs.String())
	}

	// A refusing peer must never look like a closed interface: Transport
	// keeps the interface and the supervisor keeps trying, just slowly.
	select {
	case <-rc.Done():
		t.Fatal("outer Done() fired on a refusing peer; should only fire on explicit Close()")
	default:
	}
}

// syncBuffer is a bytes.Buffer safe to read from the test goroutine
// while the supervisor writes to it.
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
