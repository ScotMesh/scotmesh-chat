// Package rrcsrv is the RRC hub server: the rrc.hub destination that RRC
// clients (MeshChatX, NomadNet, rrc-tui, …) link to.
//
// It owns links and sessions and speaks the wire protocol; the chat itself
// (names, rooms, permissions, history) is the hub core's. Transport
// callbacks never do work: they queue it for a small pool of workers, each
// link always on the same worker so its frames stay in order. Each session
// has its own bounded send queue and writer, so a slow link only delays
// itself.
//
// Protocol behaviour follows rrcd 0.3.2 (router.py, session.py,
// messages.py, service.py), Copyright (c) 2025 S. Miller, KC1AWV, MIT
// License, with lessons learned from running rrc-hub in production applied:
// identify before anything, rate limit before decoding, command replies as
// roomless NOTICEs sized to the link, ping by silence.
package rrcsrv

import (
	"context"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// Transport is what the server needs from Reticulum.
type Transport interface {
	SendOnLink(linkID, plaintext []byte) error
	TeardownLink(linkID []byte)
}

// Hub is what the server needs from the chat core.
type Hub interface {
	Identify(ctx context.Context, id, publicKey []byte) (hub.Person, error)
	PersonOf(ctx context.Context, id []byte) (hub.Person, error)
	ClaimName(ctx context.Context, id []byte, want string) (hub.Person, error)
	Join(ctx context.Context, req hub.JoinRequest) (hub.JoinResult, error)
	Part(ctx context.Context, id []byte, room string) (string, error)
	Disconnect(ctx context.Context, id []byte, rooms []string) error
	Post(ctx context.Context, req hub.PostRequest) (hub.PostResult, error)
	Command(ctx context.Context, req hub.CommandRequest) (hub.Reply, error)
	MayNotice(ctx context.Context, id []byte, room string) error
	MayNotifyDirect(ctx context.Context, from, to []byte) error
	UnsentRRCWhispers(ctx context.Context, id []byte) ([]store.Whisper, error)
	WhisperSentRRC(ctx context.Context, whisperID int64) error
}

// Config is the server's protocol behaviour.
type Config struct {
	HubName   string
	Version   string
	Greeting  []string // lines sent as a NOTICE after WELCOME
	GroupRoom string   // where LXMF group members appear

	MaxNickBytes            int
	MaxRoomNameBytes        int
	MaxMsgBodyBytes         int
	MaxRoomsPerSession      int
	RatePerMinute           int // frames per link, before decoding
	IncludeJoinedMemberList bool

	PingInterval time.Duration // silence before we PING
	PingTimeout  time.Duration // further silence before we close the link
	HelloTimeout time.Duration // from link to WELCOME
	Workers      int
	SendQueue    int // frames per session
}

// Defaults are rrcd's limits with rrc-hub's keepalive.
func Defaults() Config {
	return Config{
		HubName:            "ScotMesh",
		Version:            "dev",
		MaxNickBytes:       wire.DefaultMaxNickBytes,
		MaxRoomNameBytes:   64,
		MaxMsgBodyBytes:    350,
		MaxRoomsPerSession: 32,
		RatePerMinute:      240,
		PingInterval:       30 * time.Second,
		PingTimeout:        30 * time.Second,
		HelloTimeout:       30 * time.Second,
		Workers:            4,
		SendQueue:          256,
	}
}

// Server is the RRC hub server.
type Server struct {
	cfg     Config
	hub     Hub
	events  *hub.Subscription
	tr      Transport
	hubHash []byte // the hub identity's hash, the src of the hub's own envelopes
	log     *slog.Logger
	now     func() time.Time

	mu         sync.Mutex
	sessions   map[string]*session            // link hex -> session
	byIdentity map[string]map[string]*session // identity hex -> link hex -> session

	jobs    []chan job
	started atomic.Bool
	stats   Stats
}

// Stats are the server's counters.
type Stats struct {
	FramesIn       atomic.Uint64
	FramesOut      atomic.Uint64
	FramesDropped  atomic.Uint64 // send queue full
	JobsDropped    atomic.Uint64 // worker queue full
	RateLimited    atomic.Uint64
	BadFrames      atomic.Uint64
	LinksAccepted  atomic.Uint64
	LinksClosed    atomic.Uint64
	InternalErrors atomic.Uint64
}

// Option adjusts a Server.
type Option func(*Server)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// New creates a server. events must be a subscription to the hub the server
// talks to; hubHash is the hub identity's 16-byte hash.
func New(cfg Config, h Hub, events *hub.Subscription, tr Transport, hubHash []byte, opts ...Option) *Server {
	d := Defaults()
	fill := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	fillDur := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	if cfg.HubName == "" {
		cfg.HubName = d.HubName
	}
	if cfg.Version == "" {
		cfg.Version = d.Version
	}
	fill(&cfg.MaxNickBytes, d.MaxNickBytes)
	fill(&cfg.MaxRoomNameBytes, d.MaxRoomNameBytes)
	fill(&cfg.MaxMsgBodyBytes, d.MaxMsgBodyBytes)
	fill(&cfg.MaxRoomsPerSession, d.MaxRoomsPerSession)
	fill(&cfg.RatePerMinute, d.RatePerMinute)
	fill(&cfg.Workers, d.Workers)
	fill(&cfg.SendQueue, d.SendQueue)
	fillDur(&cfg.PingInterval, d.PingInterval)
	fillDur(&cfg.PingTimeout, d.PingTimeout)
	fillDur(&cfg.HelloTimeout, d.HelloTimeout)

	s := &Server{
		cfg: cfg, hub: h, events: events, tr: tr, hubHash: hubHash,
		log: slog.Default(), now: time.Now,
		sessions: map[string]*session{}, byIdentity: map[string]map[string]*session{},
	}
	for _, o := range opts {
		o(s)
	}
	s.jobs = make([]chan job, cfg.Workers)
	for i := range s.jobs {
		s.jobs[i] = make(chan job, 1024)
	}
	return s
}

// Stats returns the server's counters.
func (s *Server) Stats() *Stats { return &s.stats }

// Hooks returns the link hooks to install on the rrc.hub LocalDestination.
func (s *Server) Hooks() *rns.LinkHooks {
	return &rns.LinkHooks{
		OnEstablished: func(linkID []byte) { s.enqueue(job{kind: jobEstablished, link: linkID}, true) },
		OnIdentified:  func(linkID, pubKey []byte) { s.enqueue(job{kind: jobIdentified, link: linkID, data: pubKey}, true) },
		OnData:        func(linkID, pt []byte) { s.enqueue(job{kind: jobData, link: linkID, data: pt}, false) },
		OnResource:    func(linkID, body []byte) { s.enqueue(job{kind: jobResource, link: linkID, data: body}, false) },
		OnClosed:      func(linkID []byte, _ byte) { s.enqueue(job{kind: jobClosed, link: linkID}, true) },
	}
}

type jobKind int

const (
	jobEstablished jobKind = iota
	jobIdentified
	jobData
	jobResource
	jobClosed
)

type job struct {
	kind jobKind
	link []byte
	data []byte
}

// enqueue hands a transport callback to the link's worker without blocking
// the dispatcher. Data may be dropped under overload (the client's own
// retries and the rate limit cover it); lifecycle events never are, because
// a lost close leaves a ghost in the room.
func (s *Server) enqueue(j job, mustDeliver bool) {
	j.link = append([]byte(nil), j.link...)
	if j.data != nil {
		j.data = append([]byte(nil), j.data...)
	}
	h := fnv.New32a()
	_, _ = h.Write(j.link)
	q := s.jobs[h.Sum32()%uint32(len(s.jobs))] //nolint:gosec // len(s.jobs) is small and positive
	select {
	case q <- j:
		return
	default:
	}
	if !mustDeliver {
		s.stats.JobsDropped.Add(1)
		return
	}
	go func() { q <- j }()
}

// Run processes links and hub events until ctx is done, then closes every link.
func (s *Server) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("rrcsrv: Run called twice")
	}
	var wg sync.WaitGroup
	for _, q := range s.jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.work(ctx, q)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.fanOut(ctx)
	}()
	tick := time.NewTicker(time.Second * 5)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.closeAll()
			wg.Wait()
			return nil
		case <-tick.C:
			s.keepalive(ctx)
		}
	}
}

func (s *Server) work(ctx context.Context, q chan job) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-q:
			s.handleJob(ctx, j)
		}
	}
}

func (s *Server) handleJob(ctx context.Context, j job) {
	defer func() {
		if p := recover(); p != nil {
			s.stats.InternalErrors.Add(1)
			s.log.Error("rrc: panic handling link event; closing the link", "link", short(j.link), "panic", p)
			s.tr.TeardownLink(j.link)
		}
	}()
	switch j.kind {
	case jobEstablished:
		s.established(j.link)
	case jobIdentified:
		s.identified(ctx, j.link, j.data)
	case jobData:
		s.data(ctx, j.link, j.data)
	case jobResource:
		s.resource(j.link, j.data)
	case jobClosed:
		s.closed(ctx, j.link)
	}
}

func (s *Server) session(link []byte) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[hex.EncodeToString(link)]
}

// sessionsOf returns the sessions of an identity.
func (s *Server) sessionsOf(id []byte) []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*session
	for _, ss := range s.byIdentity[hex.EncodeToString(id)] {
		out = append(out, ss)
	}
	return out
}

func (s *Server) allSessions() []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		all = append(all, ss)
	}
	return all
}

// sessionsIn returns the welcomed sessions joined to a room.
func (s *Server) sessionsIn(room string) []*session {
	var out []*session
	for _, ss := range s.allSessions() {
		if ss.inRoom(room) {
			out = append(out, ss)
		}
	}
	return out
}

func (s *Server) closeAll() {
	s.mu.Lock()
	all := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		all = append(all, ss)
	}
	s.mu.Unlock()
	for _, ss := range all {
		ss.stop()
		s.tr.TeardownLink(ss.link)
	}
}

func short(b []byte) string {
	h := hex.EncodeToString(b)
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
