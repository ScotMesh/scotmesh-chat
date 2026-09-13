package rrcsrv

// Per-link session state follows rrcd 0.3.2's session.py, Copyright (c)
// 2025 S. Miller, KC1AWV, MIT License, keyed by identity rather than by
// link — see the package doc in server.go.

import (
	"sync"
	"time"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"
)

// maxPending is how many frames a link may send before it identifies. They
// are replayed once it does: some clients send HELLO right behind
// LINKIDENTIFY, and the two can arrive in either order.
const maxPending = 8

// session is one link. Fields under mu are shared between the link's worker
// and the event fan-out; the writer goroutine only reads the send queue.
type session struct {
	link  []byte
	start time.Time

	mu        sync.Mutex
	identity  []byte // nil until LINKIDENTIFY
	pubKey    []byte
	welcomed  bool
	nick      string          // the name the hub gave this identity
	triedNick map[string]bool // refused nicks already explained this session
	caps      map[wire.Key]bool
	rooms     map[string]bool
	joining   map[string][]buffered // rooms whose JOIN is in progress, and what arrived meanwhile
	lastHeard time.Time
	pingSent  time.Time // zero when no PING is outstanding
	pending   [][]byte  // frames that arrived before identify
	tokens    float64
	tokensAt  time.Time
	errorAt   time.Time // last "rate limited" ERROR, which is itself limited

	out     chan []byte
	done    chan struct{}
	stopped bool
	closing bool // flush the queue, then tear the link down
}

func newSession(link []byte, now time.Time, queue, ratePerMinute int) *session {
	return &session{
		link:      link,
		start:     now,
		triedNick: map[string]bool{},
		rooms:     map[string]bool{},
		joining:   map[string][]buffered{},
		lastHeard: now,
		tokens:    float64(ratePerMinute),
		tokensAt:  now,
		out:       make(chan []byte, queue),
		done:      make(chan struct{}),
	}
}

// buffered is a room message held for a link whose JOIN is in progress.
type buffered struct {
	id     int64
	frames [][]byte
}

// maxBuffered bounds what one joining link holds; a JOIN takes milliseconds.
const maxBuffered = 256

// offerMessage sends a room message if the link is in the room, and holds it
// if the link is joining the room.
func (s *Server) offerMessage(ss *session, room string, id int64, frames [][]byte) {
	ss.mu.Lock()
	if buf, joining := ss.joining[room]; joining {
		if len(buf) < maxBuffered {
			ss.joining[room] = append(buf, buffered{id: id, frames: frames})
		} else {
			s.stats.FramesDropped.Add(uint64(len(frames)))
		}
		ss.mu.Unlock()
		return
	}
	in := ss.welcomed && ss.rooms[room]
	ss.mu.Unlock()
	if in {
		s.sendFrames(ss, frames)
	}
}

func (ss *session) inRoom(room string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.welcomed && ss.rooms[room]
}

func (ss *session) roomList() []string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]string, 0, len(ss.rooms))
	for r := range ss.rooms {
		out = append(out, r)
	}
	return out
}

func (ss *session) id() []byte {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.identity
}

// take spends a token from the link's frame budget.
func (ss *session) take(now time.Time, perMinute int) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	capacity := float64(perMinute)
	ss.tokens = min(capacity, ss.tokens+now.Sub(ss.tokensAt).Minutes()*capacity)
	ss.tokensAt = now
	if ss.tokens < 1 {
		return false
	}
	ss.tokens--
	return true
}

// enqueue queues a frame for the writer. It reports false if the queue is
// full or the session has stopped.
func (ss *session) enqueue(frame []byte) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.stopped || ss.closing {
		return false
	}
	select {
	case ss.out <- frame:
		return true
	default:
		return false
	}
}

// closeAfterFlush lets the writer send what is queued, then close the link.
func (ss *session) closeAfterFlush() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.stopped || ss.closing {
		return
	}
	ss.closing = true
	close(ss.out)
}

// stop ends the writer without flushing.
func (ss *session) stop() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.stopped {
		return
	}
	ss.stopped = true
	close(ss.done)
	if !ss.closing {
		close(ss.out)
	}
}

// writer sends queued frames in order until the session stops. After
// closeAfterFlush it drains the queue and tears the link down.
func (s *Server) writer(ss *session) {
	for {
		select {
		case <-ss.done:
			return
		case frame, ok := <-ss.out:
			if !ok {
				ss.mu.Lock()
				closing := ss.closing
				ss.mu.Unlock()
				if closing {
					s.tr.TeardownLink(ss.link)
				}
				return
			}
			if err := s.tr.SendOnLink(ss.link, frame); err != nil {
				s.log.Debug("rrc: send failed", "link", short(ss.link), "err", err)
				continue
			}
			s.stats.FramesOut.Add(1)
		}
	}
}
