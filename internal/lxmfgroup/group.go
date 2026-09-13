// Package lxmfgroup is the LXMF group: an lxmf.delivery address people
// message from Sideband, MeshChatX, NomadNet or Columba. Members get every
// message in the group room as "Name: message", and anything they send is
// said in the room.
//
// Commands go to the hub's command table. Deliveries come from the hub's
// durable queue (ADR 0006): this package takes due ones, sends them, and
// reports each outcome. LXMF sends block until the recipient's proof or a
// timeout, so a pool of senders does the work, command replies go ahead of
// fan-out, and a member whose direct delivery keeps failing is marked away
// and sent to via the propagation node instead.
package lxmfgroup

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// Sender sends LXMF messages; *lxmf.Delivery satisfies it.
type Sender interface {
	SendWithID(to, title, content []byte, fields map[any]any) ([]byte, error)
	SendPropagated(node, to, title, content []byte, fields map[any]any) ([]byte, error)
}

// Network is what the group needs from the transport.
type Network interface {
	Recall(destHash []byte) *rns.KnownIdentity
	RequestPath(destHash []byte) error
}

// Hub is what the group needs from the chat core.
type Hub interface {
	PersonOf(ctx context.Context, id []byte) (hub.Person, error)
	Identify(ctx context.Context, id, publicKey []byte) (hub.Person, error)
	Post(ctx context.Context, req hub.PostRequest) (hub.PostResult, error)
	Command(ctx context.Context, req hub.CommandRequest) (hub.Reply, error)
	Recent(ctx context.Context, room string, n int, beforeID int64) ([]store.Message, error)
	DueDeliveries(ctx context.Context, limit int) ([]hub.PendingDelivery, error)
	RecordDelivery(ctx context.Context, d store.Delivery, outcome hub.DeliveryOutcome, propagated bool, retryAt, awayUntil time.Time) error
}

// Config is the group's behaviour.
type Config struct {
	Name            string // shown in replies: "ScotMesh Chat"
	GroupRoom       string
	PropagationNode []byte // 16 bytes, or nil for none
	Senders         int
	JoinDigest      int           // recent messages sent to a new member
	DirectAttempts  int           // before a member is marked away
	AwayFor         time.Duration // how long a member stays away
	GiveUpAfter     time.Duration // a delivery older than this is abandoned
}

// Defaults are the settings bridge 0.1 ran with.
func Defaults() Config {
	return Config{
		Name: "ScotMesh Chat", GroupRoom: "scotmesh", Senders: 16, JoinDigest: 10,
		DirectAttempts: 2, AwayFor: 10 * time.Minute, GiveUpAfter: 24 * time.Hour,
	}
}

// Group is the LXMF group adapter.
type Group struct {
	cfg    Config
	hub    Hub
	events *hub.Subscription
	send   Sender
	net    Network
	log    *slog.Logger
	now    func() time.Time

	inbox      chan *lxmf.Message
	replies    chan outgoing
	replySlots chan struct{}
	wake       chan struct{}
	seen       *seenSet

	mu       sync.Mutex
	inFlight map[string]bool   // message id + identity, being sent now
	finished map[string]uint64 // when (in seq) an attempt last finished
	seq      uint64

	stats Stats
}

// Stats are the group's counters.
type Stats struct {
	Received      atomic.Uint64
	Duplicates    atomic.Uint64
	UnknownSender atomic.Uint64
	Sent          atomic.Uint64
	Propagated    atomic.Uint64
	Retries       atomic.Uint64
	GaveUp        atomic.Uint64
	InboxDropped  atomic.Uint64
}

type outgoing struct {
	to      []byte // identity hash
	content string
}

// Option adjusts a Group.
type Option func(*Group)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(g *Group) { g.log = l } }

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option { return func(g *Group) { g.now = now } }

// New creates the group adapter.
func New(cfg Config, h Hub, events *hub.Subscription, send Sender, net Network, opts ...Option) *Group {
	d := Defaults()
	if cfg.Name == "" {
		cfg.Name = d.Name
	}
	if cfg.GroupRoom == "" {
		cfg.GroupRoom = d.GroupRoom
	}
	if cfg.Senders <= 0 {
		cfg.Senders = d.Senders
	}
	if cfg.JoinDigest <= 0 {
		cfg.JoinDigest = d.JoinDigest
	}
	if cfg.DirectAttempts <= 0 {
		cfg.DirectAttempts = d.DirectAttempts
	}
	if cfg.AwayFor <= 0 {
		cfg.AwayFor = d.AwayFor
	}
	if cfg.GiveUpAfter <= 0 {
		cfg.GiveUpAfter = d.GiveUpAfter
	}
	if len(cfg.PropagationNode) != 16 {
		cfg.PropagationNode = nil
	}
	g := &Group{
		cfg: cfg, hub: h, events: events, send: send, net: net, log: slog.Default(), now: time.Now,
		inbox: make(chan *lxmf.Message, 512), replies: make(chan outgoing, 512), replySlots: make(chan struct{}, 8), wake: make(chan struct{}, 1),
		seen: newSeenSet(4096), inFlight: map[string]bool{}, finished: map[string]uint64{},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Stats returns the counters.
func (g *Group) Stats() *Stats { return &g.stats }

// OnMessage receives inbound LXMF messages; set it as the Delivery's
// OnMessage. It never blocks the transport.
func (g *Group) OnMessage(m *lxmf.Message) {
	select {
	case g.inbox <- m:
	default:
		g.stats.InboxDropped.Add(1)
	}
}

// Run handles inbound messages, hub events and deliveries until ctx is done.
func (g *Group) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	start := func(fn func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); fn(ctx) }()
	}
	start(g.receive)
	start(g.watchEvents)
	jobs := make(chan hub.PendingDelivery)
	for range g.cfg.Senders {
		start(func(ctx context.Context) { g.sender(ctx, jobs) })
	}
	start(func(ctx context.Context) { g.schedule(ctx, jobs) })
	wg.Wait()
	return nil
}

func (g *Group) nudge() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// --- inbound -----------------------------------------------------------------

func (g *Group) receive(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-g.inbox:
			g.handle(ctx, m)
		}
	}
}

func (g *Group) handle(ctx context.Context, m *lxmf.Message) {
	g.stats.Received.Add(1)
	known := g.net.Recall(m.SourceHash)
	if known == nil || len(known.PublicKey) == 0 {
		// Without their announce we can neither tell who they are nor
		// encrypt a reply. Ask for it; their client will retry.
		g.stats.UnknownSender.Add(1)
		if err := g.net.RequestPath(m.SourceHash); err != nil {
			g.log.Debug("lxmf: path request", "err", err)
		}
		g.log.Info("lxmf: message from an address we have no announce for; ignored", "address", short(m.SourceHash))
		return
	}
	// Only now is the message counted as seen: one we could not read must
	// still be handled when the client sends it again.
	if !g.seen.add(string(m.DedupKey())) {
		g.stats.Duplicates.Add(1)
		return
	}
	id := rns.IdentityHashFromPublicKey(known.PublicKey)
	if _, err := g.hub.Identify(ctx, id, known.PublicKey); err != nil {
		var ue *hub.UserError
		if !errors.As(err, &ue) {
			g.log.Error("lxmf: identify", "err", err)
		}
		return // banned: say nothing
	}
	text := strings.TrimSpace(string(m.Content))
	if text == "" {
		return
	}
	if action, ok := hub.ActionText(text); ok {
		g.post(ctx, id, m, action, true)
	} else if hub.IsCommand(text) {
		g.command(ctx, id, known, text)
	} else {
		g.post(ctx, id, m, text, false)
	}
}

func (g *Group) post(ctx context.Context, id []byte, m *lxmf.Message, text string, action bool) {
	_, err := g.hub.Post(ctx, hub.PostRequest{
		Via: hub.ViaLXMF, Identity: id, Action: action, Body: text,
		OriginID: m.MessageID(), SaidAt: m.Timestamp.UnixMilli(),
	})
	var ue *hub.UserError
	switch {
	case errors.As(err, &ue) && strings.HasPrefix(ue.Text, "send /join first"):
		g.reply(id, fmt.Sprintf("This is %s. Send /join to take part: you'll get every message from #%s on RRC, this group and the chat page, and yours will reach them all.", g.cfg.Name, g.cfg.GroupRoom))
	case errors.As(err, &ue):
		g.reply(id, "Not sent: "+ue.Text)
	case err != nil:
		g.log.Error("lxmf: post", "err", err)
		g.reply(id, "Not sent: something went wrong on the hub. Please try again.")
	}
	g.nudge()
}

func (g *Group) command(ctx context.Context, id []byte, known *rns.KnownIdentity, text string) {
	before, err := g.hub.PersonOf(ctx, id)
	if err != nil {
		g.log.Error("lxmf: person", "err", err)
		return
	}
	fields := strings.Fields(text)
	isJoin := strings.EqualFold(fields[0][1:], "join")
	if isJoin && len(fields) == 1 && !before.Claimed {
		if name := announcedName(known); name != "" {
			text = fields[0] + " " + name
		}
	}
	reply, err := g.hub.Command(ctx, hub.CommandRequest{Via: hub.ViaLXMF, Identity: id, Room: g.cfg.GroupRoom, Text: text})
	if err != nil {
		g.log.Error("lxmf: command", "err", err)
		g.reply(id, "That didn't work: something went wrong on the hub. Please try again.")
		return
	}
	var out []string
	if len(reply.Lines) > 0 {
		out = append(out, reply.Text())
	}
	if len(reply.History) > 0 {
		out = append(out, formatHistory(reply.HistoryNote, reply.History))
	}
	if isJoin && !before.Member && !reply.Error {
		if recent, err := g.hub.Recent(ctx, g.cfg.GroupRoom, g.cfg.JoinDigest, 0); err == nil && len(recent) > 0 {
			out = append(out, formatHistory(fmt.Sprintf("— the last %d messages —", len(recent)), recent))
		}
	}
	if len(out) > 0 {
		g.reply(id, strings.Join(out, "\n\n"))
	}
}

// announcedName turns an LXMF display name into a name the hub could accept
// ("Alex Smith" → "Alex_Smith", "Casey 🐈" → "Casey").
func announcedName(k *rns.KnownIdentity) string {
	if k == nil || len(k.AppData) == 0 {
		return ""
	}
	n, err := rns.DecodeLXMFAppDataDisplayName(k.AppData)
	if err != nil {
		return ""
	}
	return hub.SuggestName(string(n))
}

func formatHistory(note string, msgs []store.Message) string {
	lines := []string{note}
	for i := range msgs {
		lines = append(lines, fmt.Sprintf("[%s] %s", clock(msgs[i].SaidAt), Line(&msgs[i])))
	}
	return strings.Join(lines, "\n")
}

// Line is how a message reads in the group: "Name: message", or
// "* Name waves" for an action.
func Line(m *store.Message) string { return hub.LXMFLine(m) }

var london = func() *time.Location {
	l, err := time.LoadLocation("Europe/London")
	if err != nil {
		return time.UTC
	}
	return l
}()

func clock(ms int64) string {
	return time.UnixMilli(ms).In(london).Format("Mon 15:04")
}

func (g *Group) reply(id []byte, content string) {
	select {
	case g.replies <- outgoing{to: id, content: content}:
		g.nudge()
	default:
		g.log.Warn("lxmf: reply queue full; reply dropped", "to", short(id))
	}
}

// --- events ------------------------------------------------------------------

func (g *Group) watchEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-g.events.Events():
			if !ok {
				return
			}
			switch e := ev.(type) {
			case hub.MessageEvent:
				if len(e.LXMFTo) > 0 {
					g.nudge()
				}
			case hub.WhisperEvent:
				g.nudge() // its LXMF copies, if any, are queued
			case hub.LXMFResumeEvent:
				g.reply(e.Identity, fmt.Sprintf("LXMF delivery is back on. %s while it was paused: send /history %d to read them.",
					plural(e.Missed, "message was", "messages were"), min(e.Missed, 100)))
			case hub.NoticeEvent:
				if e.ToLXMF {
					g.reply(e.Identity, e.Text)
				} else {
					g.replyIfMember(ctx, e.Identity, e.Text)
				}
			}
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one + " said"
	}
	return fmt.Sprintf("%d %s said", n, many)
}

func (g *Group) replyIfMember(ctx context.Context, id []byte, text string) {
	p, err := g.hub.PersonOf(ctx, id)
	if err == nil && p.Member {
		g.reply(id, text)
	}
}

// --- outbound ----------------------------------------------------------------

// schedule feeds the senders: replies first, then due deliveries.
func (g *Group) schedule(ctx context.Context, jobs chan<- hub.PendingDelivery) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		// Replies jump the queue, on their own bounded pool so a slow
		// member's proof timeout never holds up anyone's reply.
		for drained := false; !drained; {
			select {
			case r := <-g.replies:
				g.replySlots <- struct{}{}
				go func() {
					defer func() { <-g.replySlots }()
					g.sendReply(r)
				}()
			default:
				drained = true
			}
		}
		g.mu.Lock()
		snapshotSeq := g.seq
		g.mu.Unlock()
		due, err := g.hub.DueDeliveries(ctx, 256)
		if err != nil && ctx.Err() == nil {
			g.log.Error("lxmf: due deliveries", "err", err)
		}
		for _, d := range due {
			key := deliveryKey(&d.Delivery)
			g.mu.Lock()
			// An attempt that finished after this snapshot began has already
			// recorded a newer state than the row we are looking at.
			busy := g.inFlight[key] || g.finished[key] > snapshotSeq
			if !busy {
				g.inFlight[key] = true
			}
			g.mu.Unlock()
			if busy {
				continue
			}
			dispatched := false
			select {
			case jobs <- d:
				dispatched = true
			default: // every sender is busy; the rest wait for the next round
			}
			if !dispatched {
				g.mu.Lock()
				delete(g.inFlight, key)
				g.mu.Unlock()
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-g.wake:
		case <-tick.C:
		}
	}
}

func deliveryKey(d *store.Delivery) string {
	return fmt.Sprintf("%d/%x", d.MessageID, d.Identity)
}

func (g *Group) sender(ctx context.Context, jobs <-chan hub.PendingDelivery) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-jobs:
			g.deliver(ctx, d)
			key := deliveryKey(&d.Delivery)
			g.mu.Lock()
			delete(g.inFlight, key)
			g.seq++
			g.finished[key] = g.seq
			if len(g.finished) > 4096 {
				for k, at := range g.finished {
					if at+2048 < g.seq {
						delete(g.finished, k)
					}
				}
			}
			g.mu.Unlock()
		}
	}
}

// LXMFAddress is an identity's lxmf.delivery address.
func LXMFAddress(identity []byte) []byte { return hub.LXMFAddress(identity) }

// sendReply sends a reply in as many one-packet parts as it needs (see
// MaxContent). A part that can't go directly goes to the propagation node,
// with the parts after it, so they stay in order.
func (g *Group) sendReply(r outgoing) {
	addr := LXMFAddress(r.to)
	parts := Split(r.content, MaxContent)
	for i, part := range parts {
		_, err := g.send.SendWithID(addr, nil, []byte(part), nil)
		if err == nil {
			continue
		}
		if g.cfg.PropagationNode == nil {
			g.log.Info("lxmf: reply not delivered", "to", short(r.to), "part", i+1, "parts", len(parts), "err", err)
			return
		}
		for j := i; j < len(parts); j++ {
			if _, perr := g.send.SendPropagated(g.cfg.PropagationNode, addr, nil, []byte(parts[j]), nil); perr != nil {
				g.log.Info("lxmf: reply not delivered", "to", short(r.to), "part", j+1, "parts", len(parts), "err", err, "propagation_err", perr)
				return
			}
			g.stats.Propagated.Add(1)
		}
		g.log.Debug("lxmf: reply left on the propagation node", "to", short(r.to), "from_part", i+1, "parts", len(parts), "direct_err", err)
		return
	}
}

// deliver makes one attempt at one delivery and records what happened. A
// message longer than one packet goes in parts; the delivery keeps count of
// the parts that arrived, so a retry carries on where the last one stopped.
func (g *Group) deliver(ctx context.Context, d hub.PendingDelivery) {
	now := g.now()
	addr := LXMFAddress(d.Identity)
	if now.Sub(time.UnixMilli(d.SaidAt())) > g.cfg.GiveUpAfter {
		g.stats.GaveUp.Add(1)
		g.record(ctx, d, hub.GaveUp, false, time.Time{}, time.Time{})
		return
	}
	away := d.AwayUntil > now.UnixMilli()
	propagate := away && g.cfg.PropagationNode != nil
	parts := Split(d.Text(), MaxContent)

	var err error
	for d.PartsDone < len(parts) {
		content := []byte(parts[d.PartsDone])
		if propagate {
			_, err = g.send.SendPropagated(g.cfg.PropagationNode, addr, nil, content, nil)
		} else {
			_, err = g.send.SendWithID(addr, nil, content, nil)
		}
		if err != nil {
			break
		}
		d.PartsDone++
	}
	if err == nil {
		if propagate {
			g.stats.Propagated.Add(1)
		} else {
			g.stats.Sent.Add(1)
		}
		g.record(ctx, d, hub.Delivered, propagate, time.Time{}, time.Time{})
		return
	}

	g.stats.Retries.Add(1)
	d.Attempts++
	g.log.Debug("lxmf: delivery attempt failed", "message", d.MessageID, "to", short(d.Identity), "propagated", propagate,
		"attempt", d.Attempts, "part", d.PartsDone+1, "parts", len(parts), "err", err)
	switch {
	case errors.Is(err, lxmf.ErrStampCostTooHigh):
		g.stats.GaveUp.Add(1)
		g.log.Info("lxmf: member asks for a stamp we won't make; not sent", "to", short(d.Identity))
		g.record(ctx, d, hub.GaveUp, false, time.Time{}, time.Time{})
	case errors.Is(err, lxmf.ErrRecipientUnknown) || errors.Is(err, rns.ErrLinkPeerUnknown):
		if perr := g.net.RequestPath(addr); perr != nil {
			g.log.Debug("lxmf: path request", "err", perr)
		}
		g.record(ctx, d, hub.RetryAt, false, now.Add(30*time.Second), time.Time{})
	case errors.Is(err, lxmf.ErrPropagationNodeUnknown):
		if perr := g.net.RequestPath(g.cfg.PropagationNode); perr != nil {
			g.log.Debug("lxmf: path request", "err", perr)
		}
		g.record(ctx, d, hub.RetryAt, false, now.Add(time.Minute), time.Time{})
	case !propagate && d.Attempts >= g.cfg.DirectAttempts:
		// Not answering directly: away for a while, and this message goes
		// to the propagation node on its next attempt, which is now.
		g.record(ctx, d, hub.RetryAt, false, now, now.Add(g.cfg.AwayFor))
	default:
		backoff := time.Duration(d.Attempts) * 30 * time.Second
		g.record(ctx, d, hub.RetryAt, propagate, now.Add(min(backoff, 10*time.Minute)), time.Time{})
	}
}

func (g *Group) record(ctx context.Context, d hub.PendingDelivery, outcome hub.DeliveryOutcome, propagated bool, retryAt, awayUntil time.Time) {
	if err := g.hub.RecordDelivery(ctx, d.Delivery, outcome, propagated, retryAt, awayUntil); err != nil && ctx.Err() == nil {
		g.log.Error("lxmf: record delivery", "err", err)
	}
	if outcome == hub.RetryAt && !retryAt.After(g.now()) {
		g.nudge()
	}
}

func short(b []byte) string {
	h := hex.EncodeToString(b)
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// seenSet is a bounded set of recent LXMF message keys.
type seenSet struct {
	mu    sync.Mutex
	order []string
	set   map[string]bool
	max   int
}

func newSeenSet(n int) *seenSet { return &seenSet{set: map[string]bool{}, max: n} }

func (s *seenSet) add(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set[k] {
		return false
	}
	s.set[k] = true
	s.order = append(s.order, k)
	if len(s.order) > s.max {
		delete(s.set, s.order[0])
		s.order = s.order[1:]
	}
	return true
}
