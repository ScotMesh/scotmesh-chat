// Package hub is the chat core: the one owner of chat state (ADR 0003).
//
// Adapters (the RRC server, the LXMF group, the NomadNet page) call the Hub's
// methods from their own goroutines. Every method runs its work on the hub's
// single loop goroutine, in one store transaction, and returns a result the
// adapter turns into its own protocol. Things other people need to know
// about — a message, a join, a topic change — go out as Events on each
// adapter's Subscription.
//
// The core never does network I/O and never blocks on an adapter: a
// subscriber that falls behind loses events from its own queue, counted and
// logged, and nobody else waits.
package hub

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// Via is a way in.
type Via = store.Via

// Ways in.
const (
	ViaRRC  = store.ViaRRC
	ViaLXMF = store.ViaLXMF
	ViaPage = store.ViaPage
)

// ErrClosed is returned by calls made after the hub has stopped.
var ErrClosed = errors.New("hub stopped")

// UserError is a refusal meant for the person who asked: bad input, no
// permission, rate limited. Adapters show Text as it is. Any other error
// from a Hub method is an internal failure, logged, and shown as a generic
// apology.
type UserError struct {
	Text string
}

func (e *UserError) Error() string { return e.Text }

func refuse(format string, args ...any) error {
	return &UserError{Text: fmt.Sprintf(format, args...)}
}

// Hub is the chat core. Create it with New, subscribe the adapters, then Run.
type Hub struct {
	cfg  Config
	st   *store.Store
	log  *slog.Logger
	now  func() time.Time
	reqs chan func()
	done chan struct{}
	subs []*Subscription

	// Owned by the loop goroutine.
	presence  map[string]map[string]*presenceEntry // room -> identity hex -> entry
	rate      map[string]*bucket                   // identity hex -> post bucket
	historyAt map[string]time.Time                 // identity hex -> last /history
	// /link limits: codes issued per person, wrong codes per identity.
	linkIssued   map[int64][]time.Time
	linkFailures map[string][]time.Time
	stats        Stats
}

// Option adjusts a Hub at construction.
type Option func(*Hub)

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option { return func(h *Hub) { h.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(h *Hub) { h.log = l } }

// New creates a hub on an open store. It creates the configured default
// rooms if they don't exist.
func New(ctx context.Context, cfg Config, st *store.Store, opts ...Option) (*Hub, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	h := &Hub{
		cfg:       cfg,
		st:        st,
		log:       slog.Default(),
		now:       time.Now,
		reqs:      make(chan func(), 256),
		done:      make(chan struct{}),
		presence:  map[string]map[string]*presenceEntry{},
		rate:      map[string]*bucket{},
		historyAt: map[string]time.Time{},

		linkIssued:   map[int64][]time.Time{},
		linkFailures: map[string][]time.Time{},
	}
	for _, o := range opts {
		o(h)
	}
	if err := h.ensureDefaultRooms(ctx); err != nil {
		return nil, err
	}
	return h, nil
}

// Subscribe registers an adapter for events. It must be called before Run.
// buf bounds the queue; when it is full, further events for this subscriber
// are dropped and counted.
func (h *Hub) Subscribe(name string, buf int) *Subscription {
	s := &Subscription{name: name, ch: make(chan Event, buf)}
	h.subs = append(h.subs, s)
	return s
}

// Run processes requests until ctx is cancelled, with periodic maintenance.
func (h *Hub) Run(ctx context.Context) error {
	defer close(h.done)
	defer func() {
		for _, s := range h.subs {
			close(s.ch)
		}
	}()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	h.maintain(ctx)
	for {
		select {
		case <-ctx.Done():
			h.saveAllCursors(context.WithoutCancel(ctx))
			return nil
		case fn := <-h.reqs:
			fn()
		case <-tick.C:
			h.maintain(ctx)
		}
	}
}

// do runs fn on the loop goroutine and waits for it.
func (h *Hub) do(ctx context.Context, fn func()) error {
	finished := make(chan struct{})
	job := func() {
		defer close(finished)
		fn()
	}
	select {
	case h.reqs <- job:
	case <-h.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-finished:
		return nil
	case <-h.done:
		// The loop may have finished the job just before stopping.
		select {
		case <-finished:
			return nil
		default:
			return ErrClosed
		}
	}
}

// update runs fn in a store transaction on the loop goroutine. Events that
// fn queues with emit are delivered only if the transaction commits.
func (h *Hub) update(ctx context.Context, fn func(tx *store.Tx, out *outbox) error) error {
	var err error
	if derr := h.do(ctx, func() {
		out := &outbox{}
		err = h.st.Update(ctx, func(tx *store.Tx) error { return fn(tx, out) })
		if err == nil {
			for _, fn := range out.after {
				fn()
			}
			h.publish(out.events)
		}
	}); derr != nil {
		return derr
	}
	var ue *UserError
	if err != nil && !errors.As(err, &ue) {
		h.stats.InternalErrors.Add(1)
		h.log.Error("hub request failed", "err", err)
	}
	return err
}

// outbox collects what a request produces, applied after commit.
type outbox struct {
	events []Event
	after  []func() // in-memory state changes that must follow the commit
}

func (o *outbox) emit(e Event)   { o.events = append(o.events, e) }
func (o *outbox) then(fn func()) { o.after = append(o.after, fn) }

func (h *Hub) publish(events []Event) {
	for _, e := range events {
		for _, s := range h.subs {
			select {
			case s.ch <- e:
			default:
				n := s.dropped.Add(1)
				if n == 1 || n%100 == 0 {
					h.log.Warn("subscriber is not keeping up; events dropped", "subscriber", s.name, "dropped", n)
				}
			}
		}
	}
}

// Subscription is one adapter's event queue.
type Subscription struct {
	name    string
	ch      chan Event
	dropped atomic.Uint64
}

// Events returns the channel of events. It is closed when the hub stops.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped returns how many events this subscriber has lost.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Stats are counters for /stats and the health file.
type Stats struct {
	Posts          atomic.Uint64
	Duplicates     atomic.Uint64
	Refusals       atomic.Uint64
	Commands       atomic.Uint64
	InternalErrors atomic.Uint64
}

func (h *Hub) nowMS() int64 { return h.now().UnixMilli() }

func hexID(id []byte) string { return hex.EncodeToString(id) }

// Counters returns the hub's counters and each subscriber's drops, for the
// health file.
func (h *Hub) Counters() map[string]uint64 {
	c := map[string]uint64{
		"posts":           h.stats.Posts.Load(),
		"duplicates":      h.stats.Duplicates.Load(),
		"refusals":        h.stats.Refusals.Load(),
		"commands":        h.stats.Commands.Load(),
		"internal_errors": h.stats.InternalErrors.Load(),
	}
	for _, s := range h.subs {
		c["dropped_"+s.name] = s.Dropped()
	}
	return c
}
