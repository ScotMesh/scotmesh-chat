package rns

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ReconnectingTCPClient wraps an Interface around a self-healing
// *TCPClient. It looks like a single long-lived interface to Transport:
// the outer Done() only fires when Close() is called, and the outer
// Inbox() yields a single merged stream across reconnects, so
// Transport.Run's fan-in goroutine never needs to be torn down.
//
// Background (issue #6): the original DialTCP was one-shot. When the
// upstream peer's TCP connection died (NAT timeout, peer restart, etc.)
// the service kept writing into the dead socket forever — the link
// sweeper logged "send failed: broken pipe" every 30s with no recovery
// until manual restart. This wrapper supervises the inner client,
// redials with capped exponential backoff after every drop, and
// proactively closes the inner client on any Send write error so the
// supervisor reconnects ASAP instead of waiting for read-side EOF.
type ReconnectingTCPClient struct {
	addr        string
	dialTimeout time.Duration
	logger      Logger
	policy      retryPolicy

	mu  sync.RWMutex
	cur *TCPClient // nil during the reconnect window

	inbox  chan []byte
	done   chan struct{}
	closed atomic.Bool
}

const (
	// tcpKeepAlivePeriod sets the SO_KEEPALIVE probe interval on dialed
	// TCP sockets. Without this, a silent peer drop (no FIN, no RST —
	// classic NAT idle eviction) sits undetected until the next local
	// write fails. 30s gets us detection within ~2 minutes on Linux
	// defaults (9 probes * default interval).
	tcpKeepAlivePeriod = 30 * time.Second
)

// DialReconnectingTCP performs the initial dial synchronously — so a
// misconfigured address fails service startup fast — then spawns a
// supervisor that handles every subsequent reconnect transparently.
func DialReconnectingTCP(addr string, dialTimeout time.Duration, logger Logger) (*ReconnectingTCPClient, error) {
	return dialReconnectingTCPForTest(addr, dialTimeout, logger, defaultRetryPolicy())
}

// dialReconnectingTCPForTest is the testable form: same as
// DialReconnectingTCP but lets tests supply a scaled-down policy so
// reconnect tests run in milliseconds instead of minutes.
func dialReconnectingTCPForTest(addr string, dialTimeout time.Duration, logger Logger, policy retryPolicy) (*ReconnectingTCPClient, error) {
	if logger == nil {
		logger = noopLogger{}
	}
	r := &ReconnectingTCPClient{
		addr:        addr,
		dialTimeout: dialTimeout,
		logger:      logger,
		policy:      policy,
		inbox:       make(chan []byte, 64),
		done:        make(chan struct{}),
	}
	inner, err := r.dial()
	if err != nil {
		return nil, err
	}
	r.cur = inner
	go r.supervise(inner)
	return r, nil
}

func (r *ReconnectingTCPClient) dial() (*TCPClient, error) {
	d := &net.Dialer{Timeout: r.dialTimeout, KeepAlive: tcpKeepAlivePeriod}
	conn, err := d.Dial("tcp", r.addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", r.addr, err)
	}
	// Belt-and-braces — net.Dialer.KeepAlive should already have set
	// this, but be explicit in case the dialer ever returns a wrapped
	// conn that hides the *net.TCPConn from the dialer's reflection.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(tcpKeepAlivePeriod)
	}
	return NewTCPClient(conn), nil
}

// supervise is the lifecycle owner. It pumps the inner client's inbox to
// the outer inbox until the inner client disconnects, then redials under
// retryPolicy until either a connection lands or Close() is called.
//
// One policy decision is made per failed attempt: the first uses the
// attachment's measured lifetime, and each dial that never lands after
// that is a connect failure with no lifetime at all. The ramps live
// across attempts — this loop must not re-initialise them per cycle,
// which is precisely the bug that made a refusing peer see a redial
// every second forever.
func (r *ReconnectingTCPClient) supervise(initial *TCPClient) {
	inner := initial
	connectedAt := time.Now()
	readBackoff := r.policy.readFailFloor
	connectBackoff := r.policy.connectFailFloor

	for {
		r.pump(inner)
		if r.closed.Load() {
			return
		}

		// Clear cur so concurrent Send calls during the reconnect
		// window get a clear "reconnecting" error rather than
		// trying to write to a closed client.
		r.mu.Lock()
		r.cur = nil
		r.mu.Unlock()
		_ = inner.Close()

		survived := time.Since(connectedAt)
		r.logger.Printf("tcp interface %s disconnected after %v: %v — reconnecting",
			r.addr, survived.Round(time.Millisecond), inner.Err())

		lifetime := &survived
		for {
			if r.closed.Load() {
				return
			}

			plan := r.policy.decide(lifetime, readBackoff, connectBackoff)
			if lifetime != nil && !plan.wasReadFailure {
				r.logger.Printf("tcp interface %s accepted then closed us after %v — "+
					"treating as a refusal, not a dropped session",
					r.addr, survived.Round(time.Millisecond))
			}
			readBackoff, connectBackoff = plan.nextReadFailBackoff, plan.nextConnectFailBackoff

			wait := jitter(plan.delayBase)
			select {
			case <-time.After(wait):
			case <-r.done:
				return
			}

			next, err := r.dial()
			if err != nil {
				r.logger.Printf("tcp reconnect %s: %v — retrying in ~%v",
					r.addr, err, connectBackoff.Round(time.Second))
				lifetime = nil
				continue
			}

			r.mu.Lock()
			r.cur = next
			r.mu.Unlock()
			r.logger.Printf("tcp interface %s reconnected", r.addr)
			inner = next
			connectedAt = time.Now()
			break
		}
	}
}

func (r *ReconnectingTCPClient) pump(inner *TCPClient) {
	for {
		select {
		case raw, ok := <-inner.Inbox():
			if !ok {
				return
			}
			select {
			case r.inbox <- raw:
			case <-r.done:
				return
			}
		case <-inner.Done():
			return
		case <-r.done:
			return
		}
	}
}

// Send writes a Reticulum packet through the current inner client. On
// any write error it proactively closes the inner client so the
// supervisor reconnects on the next loop iteration instead of waiting
// for the read side to notice. The caller (Transport.Broadcast) treats
// per-interface Send errors as non-fatal.
func (r *ReconnectingTCPClient) Send(packet []byte) error {
	if r.closed.Load() {
		return errors.New("tcp client closed")
	}
	r.mu.RLock()
	inner := r.cur
	r.mu.RUnlock()
	if inner == nil {
		return errors.New("tcp interface reconnecting")
	}
	if err := inner.Send(packet); err != nil {
		_ = inner.Close()
		return err
	}
	return nil
}

func (r *ReconnectingTCPClient) Inbox() <-chan []byte  { return r.inbox }
func (r *ReconnectingTCPClient) Done() <-chan struct{} { return r.done }

// Connected reports whether the backbone TCP connection is up right now. It
// is false during the reconnect window between a drop and the next
// successful dial, so a caller can surface "the backbone is down" (a health
// check, a status page) instead of relying on log lines alone. ScotMesh
// patch: see SCOTMESH-PATCHES.md.
func (r *ReconnectingTCPClient) Connected() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur != nil
}

// Close shuts the wrapper down: stops the supervisor, closes the inner
// client if any, and fires the outer Done(). Idempotent.
func (r *ReconnectingTCPClient) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	r.mu.Lock()
	inner := r.cur
	r.cur = nil
	r.mu.Unlock()
	close(r.done)
	if inner != nil {
		return inner.Close()
	}
	return nil
}
