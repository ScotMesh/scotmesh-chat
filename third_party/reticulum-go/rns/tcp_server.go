package rns

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// TCPServer is a Reticulum TCPServerInterface: it accepts inbound TCP
// connections and exchanges HDLC-framed packets (SPEC §8.2) with each
// one. Until this existed the stack could only dial out, so it could
// never host anything — no propagation node, no §11 request handlers
// reachable by anyone, no link a peer could open to us.
//
// It presents itself to the Transport as ONE Interface. Upstream spawns
// a separate interface per connecting client (§7.6) and broadcasts to
// each; the observable behaviour is the same — a Send reaches every
// connected peer, and inbound frames from all of them are merged — with
// the peer set handled here rather than in the Transport.
//
// Per §7.6 a spawned client interface can forward in practice, despite
// the `OUT = False` in upstream's constructor: it is overridden post-init
// for any configured interface. There is nothing to mirror here, but do
// not read that constructor default as "servers are receive-only".
type TCPServer struct {
	listener net.Listener
	logger   Logger

	inbox chan []byte
	done  chan struct{}

	mu    sync.Mutex
	peers map[*TCPClient]struct{}

	closed   atomic.Bool
	maxPeers int

	// dropped counts inbound frames discarded because the merged inbox
	// was full. Surfaced by Stats so a wedged consumer is visible
	// instead of silently lossy.
	dropped atomic.Uint64
	// accepted counts connections ever accepted, for the same reason.
	accepted atomic.Uint64
}

// DefaultMaxTCPPeers bounds concurrent inbound connections. Each peer
// costs a goroutine, a read buffer, and a slot in the fan-out, and the
// listener is reachable pre-authentication by anyone who can route to
// us — so it needs a ceiling. Generous for a hub; a leaf will never
// approach it.
const DefaultMaxTCPPeers = 128

// ListenTCP binds addr (e.g. "0.0.0.0:4965", or "127.0.0.1:0" to let the
// kernel choose) and starts accepting. Addr() reports the bound address,
// which is how a test finds an ephemeral port.
func ListenTCP(addr string, logger Logger) (*TCPServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return NewTCPServer(ln, logger), nil
}

// NewTCPServer wraps an existing listener, so a caller can set socket
// options or supply their own.
func NewTCPServer(ln net.Listener, logger Logger) *TCPServer {
	if logger == nil {
		logger = noopLogger{}
	}
	s := &TCPServer{
		listener: ln,
		logger:   logger,
		inbox:    make(chan []byte, 128),
		done:     make(chan struct{}),
		peers:    make(map[*TCPClient]struct{}),
		maxPeers: DefaultMaxTCPPeers,
	}
	go s.acceptLoop()
	return s
}

// SetMaxPeers overrides DefaultMaxTCPPeers. Takes effect for
// connections accepted after the call.
func (s *TCPServer) SetMaxPeers(n int) {
	s.mu.Lock()
	s.maxPeers = n
	s.mu.Unlock()
}

// Addr returns the bound listen address.
func (s *TCPServer) Addr() net.Addr { return s.listener.Addr() }

func (s *TCPServer) acceptLoop() {
	defer close(s.done)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.closed.Load() {
				return // ordinary shutdown
			}
			// A transient accept error (fd exhaustion, for one) should
			// not kill the listener permanently.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			s.logger.Printf("tcp server accept: %v", err)
			return
		}

		s.mu.Lock()
		atCap := s.maxPeers > 0 && len(s.peers) >= s.maxPeers
		s.mu.Unlock()
		if atCap {
			// Refusing beats accepting-then-dropping: the peer sees the
			// close immediately and can try elsewhere, and we do not
			// spend a goroutine on a connection we will not serve.
			s.logger.Printf("tcp server: refusing %s, at the %d-peer cap", conn.RemoteAddr(), s.maxPeers)
			_ = conn.Close()
			continue
		}

		client := NewTCPClient(conn)
		s.mu.Lock()
		s.peers[client] = struct{}{}
		n := len(s.peers)
		s.mu.Unlock()
		s.accepted.Add(1)
		s.logger.Printf("tcp server: accepted %s (%d peers)", conn.RemoteAddr(), n)
		go s.pump(client, conn.RemoteAddr())
	}
}

// pump forwards one peer's inbound frames into the merged inbox and
// reaps the peer when its connection drops.
//
// Backpressure is deliberately PER-PEER: a full merged inbox blocks only
// the peer whose frame is waiting, never the accept loop or the other
// peers. Dropping instead would make a slow consumer lose traffic
// silently, and blocking globally would let one peer stall the service.
func (s *TCPServer) pump(client *TCPClient, addr net.Addr) {
	defer func() {
		s.mu.Lock()
		delete(s.peers, client)
		n := len(s.peers)
		s.mu.Unlock()
		_ = client.Close()
		s.logger.Printf("tcp server: %s disconnected (%d peers): %v", addr, n, client.Err())
	}()

	for {
		select {
		case <-s.done:
			return
		case frame, ok := <-client.Inbox():
			if !ok {
				return
			}
			select {
			case s.inbox <- frame:
			case <-s.done:
				return
			case <-client.Done():
				return
			}
		}
	}
}

// Send broadcasts a packet to every connected peer.
//
// One peer's failure must not stop the others: a write error closes that
// connection (TCPClient.Send does this, because a partial HDLC frame
// leaves the stream corrupt) and the pump reaps it, while the packet
// still reaches everyone else. The returned error reports how many peers
// failed, for logging — it is not a signal to retry, since the peers
// that did receive it would get a duplicate.
func (s *TCPServer) Send(packet []byte) error {
	if s.closed.Load() {
		return errors.New("tcp server closed")
	}
	s.mu.Lock()
	targets := make([]*TCPClient, 0, len(s.peers))
	for c := range s.peers {
		targets = append(targets, c)
	}
	s.mu.Unlock()

	var failed int
	for _, c := range targets {
		if err := c.Send(packet); err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("tcp server: %d of %d peers failed to receive", failed, len(targets))
	}
	return nil
}

// Inbox returns the merged stream of inbound packets from all peers.
func (s *TCPServer) Inbox() <-chan []byte { return s.inbox }

// Done is closed when the listener stops accepting.
func (s *TCPServer) Done() <-chan struct{} { return s.done }

// Peers reports the current connection count.
func (s *TCPServer) Peers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers)
}

// Stats reports lifetime counters for observability.
func (s *TCPServer) Stats() (accepted, dropped uint64, peers int) {
	return s.accepted.Load(), s.dropped.Load(), s.Peers()
}

// Close stops the listener and disconnects every peer. Idempotent.
func (s *TCPServer) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	err := s.listener.Close()
	s.mu.Lock()
	peers := make([]*TCPClient, 0, len(s.peers))
	for c := range s.peers {
		peers = append(peers, c)
	}
	s.mu.Unlock()
	for _, c := range peers {
		_ = c.Close()
	}
	// Give the accept loop a moment to observe the closed listener so
	// Done is closed by the time Close returns in the common case.
	select {
	case <-s.done:
	case <-time.After(100 * time.Millisecond):
	}
	return err
}
