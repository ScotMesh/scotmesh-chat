package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t      *testing.T
	h      *Hub
	st     *store.Store
	clock  *clock
	sub    *Subscription
	cancel context.CancelFunc
	done   chan struct{}
	path   string
}

var (
	alex  = bytes.Repeat([]byte{0xa1}, 16)
	rab   = bytes.Repeat([]byte{0xb2}, 16)
	ellen = bytes.Repeat([]byte{0xe3}, 16)
	oper  = bytes.Repeat([]byte{0x0f}, 16) // a hub operator
)

func start(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	return startAt(t, filepath.Join(t.TempDir(), "hub.db"), &clock{t: time.Date(2026, 9, 12, 18, 0, 0, 0, time.UTC)}, mutate...)
}

func startAt(t *testing.T, path string, c *clock, mutate ...func(*Config)) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Admins = [][]byte{oper}
	for _, m := range mutate {
		m(&cfg)
	}
	h, err := New(context.Background(), cfg, st, WithClock(c.now), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	hs := &harness{t: t, h: h, st: st, clock: c, sub: h.Subscribe("test", 4096), done: make(chan struct{}), path: path}
	ctx, cancel := context.WithCancel(context.Background())
	hs.cancel = cancel
	go func() {
		defer close(hs.done)
		if err := h.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(hs.stop)
	return hs
}

func (hs *harness) stop() {
	if hs.cancel == nil {
		return
	}
	hs.cancel()
	<-hs.done
	hs.cancel = nil
	if err := hs.st.Close(); err != nil {
		hs.t.Errorf("close store: %v", err)
	}
}

// events drains the events published so far.
func (hs *harness) events() []Event {
	var out []Event
	for {
		select {
		case e, ok := <-hs.sub.Events():
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func eventsOf[T Event](events []Event) []T {
	var out []T
	for _, e := range events {
		if v, ok := e.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

var bg = context.Background()

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func userErr(t *testing.T, err error, wantPrefix string) {
	t.Helper()
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want a UserError starting %q", err, err, wantPrefix)
	}
	if !strings.HasPrefix(ue.Text, wantPrefix) {
		t.Fatalf("refusal %q, want it to start %q", ue.Text, wantPrefix)
	}
}

func (hs *harness) identify(ids ...[]byte) {
	hs.t.Helper()
	for _, id := range ids {
		if _, err := hs.h.Identify(bg, id, nil); err != nil {
			hs.t.Fatal(err)
		}
	}
}

func (hs *harness) claim(id []byte, name string) {
	hs.t.Helper()
	if _, err := hs.h.ClaimName(bg, id, name); err != nil {
		hs.t.Fatal(err)
	}
}

func (hs *harness) join(id []byte, room string) JoinResult {
	hs.t.Helper()
	res, err := hs.h.Join(bg, JoinRequest{Identity: id, Room: room})
	if err != nil {
		hs.t.Fatal(err)
	}
	return res
}

func (hs *harness) say(via Via, id []byte, body string) store.Message {
	hs.t.Helper()
	hs.clock.advance(time.Second)
	res, err := hs.h.Post(bg, PostRequest{Via: via, Identity: id, Body: body})
	if err != nil {
		hs.t.Fatalf("post %q: %v", body, err)
	}
	return res.Message
}

func (hs *harness) cmd(via Via, id []byte, room, text string) Reply {
	hs.t.Helper()
	r, err := hs.h.Command(bg, CommandRequest{Via: via, Identity: id, Room: room, Text: text})
	if err != nil {
		hs.t.Fatal(err)
	}
	return r
}

func bodies(ms []store.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Body
	}
	return out
}

// --- names ------------------------------------------------------------------

func TestNamesAreBoundToOneIdentity(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	p := must[Person](t)(hs.h.ClaimName(bg, alex, "Alex"))
	if p.Name != "Alex" || !p.Claimed {
		t.Fatalf("claim: %+v", p)
	}
	ev := eventsOf[NameEvent](hs.events())
	if len(ev) != 1 || ev[0].Old != "guest-a1a1" || ev[0].New != "Alex" {
		t.Errorf("name events %+v", ev)
	}

	// Another identity, any way in, any lookalike: refused, and told what they're called.
	for _, attempt := range []string{"Alex", "alex", "A1ex", "Al.ex"} {
		_, err := hs.h.ClaimName(bg, rab, attempt)
		userErr(t, err, fmt.Sprintf("%q is taken by someone else. You are shown as guest-b2b2", attempt))
	}
	// Asking again for your own name is fine and emits nothing.
	hs.claim(alex, "Alex")
	if ev := eventsOf[NameEvent](hs.events()); len(ev) != 0 {
		t.Errorf("re-claim emitted %+v", ev)
	}
	// A bad name is explained.
	_, err := hs.h.ClaimName(bg, rab, "Rab Smith")
	userErr(t, err, "A name can't contain spaces. You are shown as guest-b2b2")

	// /nick swaps in one step and frees the old name.
	r := hs.cmd(ViaRRC, alex, "", "/nick Sandy")
	if r.Error || r.Text() != "You are now Sandy. Alex is free for anyone." {
		t.Fatalf("/nick: %+v", r)
	}
	hs.claim(rab, "Alex")
	_, err = hs.h.ClaimName(bg, rab, "sandy")
	userErr(t, err, "\"sandy\" is taken by someone else. You are still Alex.")
}

func TestForgetFreesTheNameAndTheGroup(t *testing.T) {
	hs := start(t)
	p, note, err := hs.h.JoinGroup(bg, ellen, "Ellen")
	if err != nil || note != "" || !p.Member || p.Name != "Ellen" {
		t.Fatalf("join group: %+v %q %v", p, note, err)
	}
	hs.events()
	r := hs.cmd(ViaLXMF, ellen, "", "/forget")
	if r.Text() != "Done: Ellen is free again and you are shown as guest-e3e3, and you have left the LXMF group." {
		t.Errorf("/forget reply %q", r.Text())
	}
	evs := hs.events()
	if g := eventsOf[GroupEvent](evs); len(g) != 1 || g[0].Joined {
		t.Errorf("group events %+v", g)
	}
	if n := eventsOf[NameEvent](evs); len(n) != 1 || n[0].New != "guest-e3e3" {
		t.Errorf("name events %+v", n)
	}
	hs.identify(rab)
	hs.claim(rab, "Ellen")
	// History keeps the name things were said under.
	if r := hs.cmd(ViaLXMF, rab, "", "!forget"); !strings.HasPrefix(r.Text(), "Done: Ellen is free again") {
		t.Errorf("second forget %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, rab, "", "/forget"); r.Text() != "You have no name and aren't in the group, so there's nothing to forget." {
		t.Errorf("empty forget %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, rab, "", "/forget now"); !r.Error || r.Text() != "usage: /forget" {
		t.Errorf("forget with an argument %q", r.Text())
	}
}

func TestJoinGroupWithATakenNameStillJoins(t *testing.T) {
	hs := start(t)
	hs.identify(alex)
	hs.claim(alex, "Alex")
	p, note, err := hs.h.JoinGroup(bg, rab, "A1ex")
	if err != nil || !p.Member || p.Claimed {
		t.Fatalf("join: %+v %v", p, err)
	}
	if !strings.HasPrefix(note, "\"A1ex\" is taken by someone else.") {
		t.Errorf("note %q", note)
	}
}

// --- presence and catch-up --------------------------------------------------

func TestPresenceIsPerIdentityAcrossLinks(t *testing.T) {
	hs := start(t)
	hs.identify(alex)
	hs.claim(alex, "Alex")
	hs.events()
	first := hs.join(alex, "#ScotMesh")
	second := hs.join(alex, "scotmesh")
	if !first.FirstLink || second.FirstLink || first.Room.Name != "scotmesh" {
		t.Fatalf("first %+v second %+v", first.FirstLink, second.FirstLink)
	}
	if j := eventsOf[JoinedEvent](hs.events()); len(j) != 1 || j[0].Name != "Alex" {
		t.Fatalf("joined events %+v", j)
	}
	must[string](t)(hs.h.Part(bg, alex, "scotmesh"))
	if p := eventsOf[PartedEvent](hs.events()); len(p) != 0 {
		t.Fatalf("parted after first of two links: %+v", p)
	}
	if err := hs.h.Disconnect(bg, alex, []string{"scotmesh"}); err != nil {
		t.Fatal(err)
	}
	if p := eventsOf[PartedEvent](hs.events()); len(p) != 1 {
		t.Fatalf("parted events %+v", p)
	}
	ms := must[[]Member](t)(hs.h.Members(bg, "scotmesh"))
	if len(ms) != 0 {
		t.Errorf("members after leaving: %+v", ms)
	}
}

func TestCatchUpIsSinceYouLastDisconnected(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	hs.join(rab, "scotmesh")
	for i := range 14 {
		hs.say(ViaRRC, rab, fmt.Sprintf("early %d", i))
	}

	// First visit: the last 10.
	res := hs.join(alex, "scotmesh")
	if got := bodies(res.Replay); len(got) != 10 || got[0] != "early 4" || got[9] != "early 13" {
		t.Fatalf("first visit replay %v", got)
	}
	if res.ReplayNote != "— the last 10 messages in #scotmesh —" {
		t.Errorf("first visit note %q", res.ReplayNote)
	}
	hs.say(ViaRRC, alex, "hello")
	hs.clock.advance(time.Hour)
	if err := hs.h.Disconnect(bg, alex, []string{"scotmesh"}); err != nil {
		t.Fatal(err)
	}

	// 12 messages while away, some by other ways in: exactly those 12.
	must[Person](t)(hs.h.Identify(bg, ellen, nil))
	for i := range 12 {
		via := ViaRRC
		if i%3 == 0 {
			via = ViaPage
		}
		id := rab
		if via == ViaPage {
			id = ellen
		}
		hs.say(via, id, fmt.Sprintf("missed %d", i))
	}
	res = hs.join(alex, "scotmesh")
	if got := bodies(res.Replay); len(got) != 12 || got[0] != "missed 0" || got[11] != "missed 11" {
		t.Fatalf("catch-up %v", got)
	}
	if res.ReplayNote != "— 12 messages since you were here (Sat 20:00) —" {
		t.Errorf("catch-up note %q", res.ReplayNote)
	}

	// Reconnect with nothing missed: no replay at all.
	must[string](t)(hs.h.Part(bg, alex, "scotmesh"))
	res = hs.join(alex, "scotmesh")
	if len(res.Replay) != 0 || res.ReplayNote != "" {
		t.Errorf("replay with nothing missed: %v %q", bodies(res.Replay), res.ReplayNote)
	}

	// More than ReplayMax missed: the latest ReplayMax and a pointer to /history.
	must[string](t)(hs.h.Part(bg, alex, "scotmesh"))
	for i := range 55 {
		hs.say(ViaRRC, rab, fmt.Sprintf("flood %d", i))
		hs.clock.advance(2 * time.Second) // stay under the post rate
	}
	res = hs.join(alex, "scotmesh")
	if got := bodies(res.Replay); len(got) != 50 || got[0] != "flood 5" {
		t.Fatalf("capped replay has %d, first %q", len(got), got[0])
	}
	if !strings.HasPrefix(res.ReplayNote, "— 55 messages since you were here (") || !strings.HasSuffix(res.ReplayNote, "the latest 50 follow, /history 55 shows more —") {
		t.Errorf("capped note %q", res.ReplayNote)
	}
}

func TestCatchUpSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	c := &clock{t: time.Date(2026, 9, 12, 18, 0, 0, 0, time.UTC)}
	hs := startAt(t, path, c)
	hs.identify(alex, rab)
	hs.join(rab, "scotmesh")
	hs.join(alex, "scotmesh")
	hs.say(ViaRRC, rab, "before restart")
	hs.stop() // the hub saves cursors for everyone present on the way down

	hs = startAt(t, path, c)
	hs.identify(alex, rab)
	hs.join(rab, "scotmesh")
	hs.say(ViaRRC, rab, "after restart")
	res := hs.join(alex, "scotmesh")
	if got := bodies(res.Replay); len(got) != 1 || got[0] != "after restart" {
		t.Fatalf("replay after restart %v", got)
	}
}

// --- posting ------------------------------------------------------------------

func TestPostRules(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	hs.join(alex, "scotmesh")

	m := hs.say(ViaRRC, alex, "hello  ")
	if m.Body != "hello" || m.AuthorName != "guest-a1a1" || m.Room != "scotmesh" {
		t.Errorf("stored %+v", m)
	}
	cases := []struct {
		req  PostRequest
		want string
	}{
		{PostRequest{Via: ViaRRC, Identity: alex, Body: "   "}, "message is empty"},
		{PostRequest{Via: ViaRRC, Identity: alex, Body: strings.Repeat("x", 2001)}, "message too large: 2001 bytes > 2000 bytes"},
		{PostRequest{Via: ViaRRC, Identity: alex, Room: "nowhere", Body: "hi"}, "no such room"},
		{PostRequest{Via: ViaRRC, Identity: alex, Room: "bad room", Body: "hi"}, "room name may only use"},
		{PostRequest{Via: ViaLXMF, Identity: rab, Body: "hi"}, "send /join first"},
		{PostRequest{Via: ViaPage, Identity: rab, Room: "other", Body: "hi"}, "no such room"},
	}
	for _, c := range cases {
		_, err := hs.h.Post(bg, c.req)
		userErr(t, err, c.want)
	}

	// Duplicates by origin ID are stored once and sent once.
	hs.events()
	req := PostRequest{Via: ViaRRC, Identity: alex, Body: "once", OriginID: []byte{1, 2, 3}}
	first := must[PostResult](t)(hs.h.Post(bg, req))
	again := must[PostResult](t)(hs.h.Post(bg, req))
	if first.Duplicate || !again.Duplicate || again.Message.ID != first.Message.ID || again.Message.Body != "once" {
		t.Errorf("duplicate handling: %+v / %+v", first, again)
	}
	if n := len(eventsOf[MessageEvent](hs.events())); n != 1 {
		t.Errorf("%d message events for a duplicate", n)
	}
}

func TestRoomModesOnPosting(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	hs.join(alex, "den") // alex founds #den
	userErrPost := func(id []byte, want string) {
		t.Helper()
		_, err := hs.h.Post(bg, PostRequest{Via: ViaRRC, Identity: id, Room: "den", Body: "hi"})
		userErr(t, err, want)
	}
	// Without +n an outsider may post to an existing room, as in rrcd.
	if _, err := hs.h.Post(bg, PostRequest{Via: ViaRRC, Identity: rab, Room: "den", Body: "outside"}); err != nil {
		t.Fatalf("outside post without +n: %v", err)
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room mode #den +n"); r.Error || r.Text() != "mode for den is now: +n" {
		t.Fatalf("+n: %+v", r)
	}
	userErrPost(rab, "no outside messages (+n)")
	hs.join(rab, "den")
	hs.cmd(ViaRRC, alex, "den", "/room mode #den +m")
	userErrPost(rab, "room is moderated (+m)")
	if r := hs.cmd(ViaRRC, alex, "den", "/room voice #den b2b2b2b2"); r.Text() != "voice granted in den" {
		t.Fatalf("voice: %+v", r)
	}
	if _, err := hs.h.Post(bg, PostRequest{Via: ViaRRC, Identity: rab, Room: "den", Body: "voiced"}); err != nil {
		t.Fatal(err)
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room ban #den b2b2b2b2"); r.Text() != "ban added in den" {
		t.Fatalf("ban: %+v", r)
	}
	userErrPost(rab, "no outside messages (+n)") // rrcd checks +n before the ban
	hs.cmd(ViaRRC, alex, "den", "/room mode #den -n")
	userErrPost(rab, "banned from room")
	if ev := eventsOf[RemovedEvent](hs.events()); len(ev) != 1 || ev[0].Reason != "banned from den" {
		t.Errorf("removed events %+v", ev)
	}
}

func TestRateLimit(t *testing.T) {
	hs := start(t, func(c *Config) { c.PostsPerMinute = 3 })
	hs.identify(alex)
	hs.join(alex, "scotmesh")
	for i := range 3 {
		if _, err := hs.h.Post(bg, PostRequest{Via: ViaRRC, Identity: alex, Body: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := hs.h.Post(bg, PostRequest{Via: ViaLXMF, Identity: alex, Body: "too many"})
	userErr(t, err, "send /join first") // membership is checked before the rate
	_, err = hs.h.Post(bg, PostRequest{Via: ViaRRC, Identity: alex, Body: "too many"})
	userErr(t, err, "rate limited")
	hs.clock.advance(20 * time.Second) // one token back
	if _, err := hs.h.Post(bg, PostRequest{Via: ViaRRC, Identity: alex, Body: "ok again"}); err != nil {
		t.Fatalf("after refill: %v", err)
	}
}

// --- LXMF delivery settings -----------------------------------------------------

func TestLXMFRecipientsFollowSettings(t *testing.T) {
	hs := start(t)
	for _, p := range []struct {
		id   []byte
		name string
	}{{alex, "Alex"}, {rab, "Rab"}, {ellen, "Ellen"}} {
		must[Person](t)(func() (Person, error) { p, _, err := hs.h.JoinGroup(bg, p.id, p.name); return p, err }())
	}
	recipients := func() []string {
		t.Helper()
		hs.events()
		hs.say(ViaLXMF, alex, "ping")
		ev := eventsOf[MessageEvent](hs.events())
		if len(ev) != 1 {
			t.Fatalf("%d message events", len(ev))
		}
		var names []string
		for _, id := range ev[0].LXMFTo {
			names = append(names, map[string]string{hexID(rab): "Rab", hexID(ellen): "Ellen", hexID(alex): "Alex"}[hexID(id)])
		}
		return names
	}
	if got := recipients(); strings.Join(got, ",") != "Rab,Ellen" {
		t.Fatalf("everyone on: %v (the author never gets their own)", got)
	}

	r := hs.cmd(ViaLXMF, rab, "", "/lxmf off")
	if r.Text() != "LXMF delivery is off. You stay in the group; /lxmf on starts it again." {
		t.Errorf("/lxmf off: %q", r.Text())
	}
	if got := recipients(); strings.Join(got, ",") != "Ellen" {
		t.Fatalf("rab off: %v", got)
	}
	hs.say(ViaLXMF, alex, "while paused 2")
	r = hs.cmd(ViaLXMF, rab, "", "/resume")
	if r.Text() != "LXMF delivery is on." {
		t.Errorf("/resume: %q", r.Text())
	}
	if res := eventsOf[LXMFResumeEvent](hs.events()); len(res) != 1 || res[0].Missed != 2 {
		t.Errorf("resume events %+v, want 2 missed", res)
	}

	// auto: paused only while in the group room over RRC.
	hs.cmd(ViaRRC, ellen, "", "/lxmf auto")
	if got := recipients(); strings.Join(got, ",") != "Rab,Ellen" {
		t.Fatalf("ellen auto, not on RRC: %v", got)
	}
	hs.join(ellen, "scotmesh")
	if got := recipients(); strings.Join(got, ",") != "Rab" {
		t.Fatalf("ellen auto, on RRC: %v", got)
	}
	if r := hs.cmd(ViaRRC, ellen, "scotmesh", "/lxmf"); r.Text() != "LXMF delivery is auto, paused while you're on RRC, and your own messages from RRC and the page: on. Change it with /lxmf on, /lxmf off, /lxmf auto, or /lxmf mine on|off." {
		t.Errorf("/lxmf status %q", r.Text())
	}
	if err := hs.h.Disconnect(bg, ellen, []string{"scotmesh"}); err != nil {
		t.Fatal(err)
	}
	if res := eventsOf[LXMFResumeEvent](hs.events()); len(res) != 1 || res[0].Missed != 1 {
		t.Errorf("auto resume events %+v, want 1 missed", res)
	}
	if got := recipients(); strings.Join(got, ",") != "Rab,Ellen" {
		t.Fatalf("ellen auto, left RRC: %v", got)
	}
}

// --- commands ---------------------------------------------------------------------

func TestWhoAndListTextsForNomadNet(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	hs.claim(alex, "Alex")
	hs.join(alex, "scotmesh")
	hs.join(rab, "scotmesh")
	must[Person](t)(func() (Person, error) { p, _, err := hs.h.JoinGroup(bg, ellen, "Ellen"); return p, err }())

	// NomadNet parses the first line; it is a line of its own, as RRC sends
	// each line as a notice.
	r := hs.cmd(ViaRRC, rab, "scotmesh", "/who")
	want := "members in scotmesh: Alex (a1a1a1a1a1a1), Ellen (e3e3e3e3e3e3), guest-b2b2 (b2b2b2b2b2b2)\non RRC: Alex, guest-b2b2 · in the LXMF group: Ellen"
	if r.Error || r.Text() != want {
		t.Errorf("/who over RRC:\n got %q\nwant %q", r.Text(), want)
	}
	if r := hs.cmd(ViaLXMF, ellen, "", "/who"); r.Text() != want {
		t.Errorf("/who over LXMF %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "", "/who #empty"); r.Text() != "members in empty: (none)" {
		t.Errorf("/who #empty %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "", "/who empty"); !r.Error || r.Text() != "usage: /who [#room]" {
		t.Errorf("/who with a word that isn't a room %q", r.Text())
	}
	r = hs.cmd(ViaRRC, rab, "", "/list")
	if r.Text() != "Registered public rooms:\n  scotmesh - ScotMesh - Scotland's Reticulum community" {
		t.Errorf("/list %q", r.Text())
	}
}

func TestUnknownCommandsAndHelp(t *testing.T) {
	hs := start(t)
	hs.identify(alex)
	for _, via := range []Via{ViaRRC, ViaLXMF, ViaPage} {
		if r := hs.cmd(via, alex, "", "/frobnicate"); !r.Error || r.Text() != "There's no /frobnicate. Send /help for the commands." {
			t.Errorf("%s unknown %+v", via, r)
		}
		if r := hs.cmd(via, alex, "", "/"); !r.Error || r.Text() != "Send /help for the commands." {
			t.Errorf("%s bare slash %q", via, r.Text())
		}
		if r := hs.cmd(via, alex, "", "hello"); !r.Error || r.Text() != "Send /help for the commands." {
			t.Errorf("%s not a command %q", via, r.Text())
		}
	}
	help := hs.cmd(ViaLXMF, alex, "", "/help 1").Text() + "\n" + hs.cmd(ViaLXMF, alex, "", "/help 2").Text() + "\n" + hs.cmd(ViaLXMF, alex, "", "/help 3").Text() + "\n" + hs.cmd(ViaLXMF, alex, "", "/help 4").Text()
	for _, want := range []string{"/join [#room|lxmf]", "/lxmf on|off|auto", "/room <verb>", "/topic [#room] [text]", "/forget"} {
		if !strings.Contains(help, want) {
			t.Errorf("/help lacks %q:\n%s", want, help)
		}
	}
	for _, not := range []string{"/kline", "/release", "/stats"} {
		if strings.Contains(help, not) {
			t.Errorf("/help for a member shows %q", not)
		}
	}
	var ownerHelp string
	for n := 1; n <= 8; n++ {
		ownerHelp += hs.cmd(ViaRRC, oper, "", fmt.Sprintf("/help %d", n)).Text() + "\n"
	}
	for _, want := range []string{"/ban <name> [time] [reason]", "/mod <name>", "/stats"} {
		if !strings.Contains(ownerHelp, want) {
			t.Errorf("owners' /help lacks %q:\n%s", want, ownerHelp)
		}
	}
	if r := hs.cmd(ViaRRC, alex, "", "/help nick"); r.Text() != "/nick <name> — take a name, the same on RRC, the LXMF group and the page; your old name is freed" {
		t.Errorf("/help nick %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "", "/kline list"); !r.Error || r.Text() != "not authorized" {
		t.Errorf("/kline by a non-operator %+v", r)
	}
}

func TestWhoami(t *testing.T) {
	hs := start(t)
	hs.identify(alex)
	hs.claim(alex, "Alex")
	hs.join(alex, "scotmesh")
	must[Person](t)(func() (Person, error) { p, _, err := hs.h.JoinGroup(bg, alex, ""); return p, err }())
	want := "You are Alex (yours since 12 Sep 2026).\nIdentity a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1.\nHere via RRC in #scotmesh and the LXMF group.\nLXMF delivery: on; your own messages: on."
	if r := hs.cmd(ViaRRC, alex, "scotmesh", "/whoami"); r.Text() != want {
		t.Errorf("/whoami\n got %q\nwant %q", r.Text(), want)
	}
}

func TestTopicKickInviteRegister(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab, ellen)
	hs.claim(rab, "Rab")
	hs.join(alex, "den")
	hs.join(rab, "den")

	if r := hs.cmd(ViaRRC, rab, "den", "/topic den"); r.Text() != "topic for den: (none)" {
		t.Errorf("topic view %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "den", "/topic den sheep and radios"); r.Error {
		t.Errorf("topic set without +t refused: %q", r.Text())
	}
	hs.events()
	hs.cmd(ViaRRC, alex, "den", "/room mode #den +t")
	if r := hs.cmd(ViaRRC, rab, "den", "/topic den no"); !r.Error || r.Text() != "not authorized (+t)" {
		t.Errorf("topic with +t %+v", r)
	}
	if n := eventsOf[RoomNoticeEvent](hs.events()); len(n) != 1 || n[0].Text != "mode for den is now: +t" {
		t.Errorf("room notices %+v", n)
	}

	if r := hs.cmd(ViaRRC, rab, "den", "/room kick #den alex"); r.Text() != "not authorized" {
		t.Errorf("kick by non-op %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room kick #den r.a.b"); r.Text() != "kicked r.a.b from den" {
		t.Errorf("kick by lookalike name %q", r.Text())
	}
	evs := hs.events()
	if rm := eventsOf[RemovedEvent](evs); len(rm) != 1 || !bytes.Equal(rm[0].Identity, rab) || rm[0].Reason != "kicked from den" {
		t.Errorf("removed %+v", rm)
	}
	if p := eventsOf[PartedEvent](evs); len(p) != 1 {
		t.Errorf("parted %+v", p)
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room kick #den rab"); r.Text() != "target not in room" {
		t.Errorf("kick absent %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room kick #den zz"); r.Text() != "target 'zz' not found" {
		t.Errorf("kick unknown %q", r.Text())
	}

	hs.cmd(ViaRRC, alex, "den", "/room mode #den +i")
	_, err := hs.h.Join(bg, JoinRequest{Identity: ellen, Room: "den"})
	userErr(t, err, "invite-only (+i)")
	if r := hs.cmd(ViaRRC, alex, "den", "/room invite #den "+hexID(ellen)); r.Text() != "invite added in den (expires in 900s)" {
		t.Errorf("invite %q", r.Text())
	}
	if n := eventsOf[NoticeEvent](hs.events()); len(n) != 1 || n[0].Text != "You have been invited to join den." {
		t.Errorf("invite notice %+v", n)
	}
	hs.join(ellen, "den")
	must[string](t)(hs.h.Part(bg, ellen, "den"))
	_, err = hs.h.Join(bg, JoinRequest{Identity: ellen, Room: "den"})
	userErr(t, err, "invite-only (+i)") // the invite was used up

	hs.cmd(ViaRRC, alex, "den", "/room mode #den +k sesame")
	hs.cmd(ViaRRC, alex, "den", "/room mode #den -i")
	_, err = hs.h.Join(bg, JoinRequest{Identity: ellen, Room: "den", Key: "wrong"})
	userErr(t, err, "bad key (+k)")
	if _, err := hs.h.Join(bg, JoinRequest{Identity: ellen, Room: "den", Key: "sesame"}); err != nil {
		t.Errorf("join with key: %v", err)
	}

	if r := hs.cmd(ViaRRC, ellen, "den", "/room register #den"); r.Text() != "only the room founder can register" {
		t.Errorf("register by non-founder %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "", "/room register #den"); r.Text() != "must be present in the room to register it" {
		t.Errorf("register from outside %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room register #den"); r.Text() != "registered room den" {
		t.Errorf("register %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room mode #den -m"); r.Text() != "mode for den is now: +iknrt" && r.Text() != "mode for den is now: +knrt" {
		t.Errorf("mode after register %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "scotmesh", "/unregister scotmesh"); !r.Error {
		t.Errorf("unregistering a default room: %q", r.Text())
	}
}

func TestKlineIsBanAndRelease(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	hs.claim(rab, "Rab")
	hs.join(rab, "scotmesh")
	must[Person](t)(func() (Person, error) { p, _, err := hs.h.JoinGroup(bg, rab, ""); return p, err }())
	hs.events()

	if r := hs.cmd(ViaRRC, oper, "", "/kline add Rab spamming"); r.Text() != "Banned Rab (1 app), for good. Their name is free.\n(next time: /ban Rab spamming)" {
		t.Fatalf("kline %q", r.Text())
	}
	evs := hs.events()
	if b := eventsOf[BannedEvent](evs); len(b) != 1 || !bytes.Equal(b[0].Identity, rab) || b[0].Reason != "You were banned from ScotMesh by guest-0f0f, for good: spamming." {
		t.Errorf("banned events %+v", b)
	}
	if p := eventsOf[PartedEvent](evs); len(p) != 1 {
		t.Errorf("banned identity not parted: %+v", p)
	}
	if n := eventsOf[NoticeEvent](evs); len(n) != 1 || !n[0].ToLXMF {
		t.Errorf("the banned member isn't told by LXMF: %+v", n)
	}
	_, err := hs.h.Identify(bg, rab, nil)
	userErr(t, err, "you are banned from this hub")
	_, err = hs.h.Post(bg, PostRequest{Via: ViaRRC, Identity: rab, Body: "still here?"})
	userErr(t, err, "you are banned from this hub")
	if r := hs.cmd(ViaRRC, oper, "", "/kline list"); r.Text() != "Banned (1):\n  Rab (1 app) — spamming — by guest-0f0f, Sat 19:00\n(next time: /bans)" {
		t.Errorf("kline list %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/kline add "+hexID(oper)); !r.Error {
		t.Errorf("an owner klined themselves: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/kline frob"); r.Text() != "usage: /kline add|del|list [name|hashprefix|hash]" {
		t.Errorf("kline usage %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/kline del "+hexID(rab)); r.Text() != "Unbanned Rab (1 app).\n(next time: /unban "+hexID(rab)+")" {
		t.Errorf("kline del %q", r.Text())
	}
	if _, err := hs.h.Identify(bg, rab, nil); err != nil {
		t.Errorf("identify after unban: %v", err)
	}

	hs.claim(rab, "Rab")
	hs.events()
	if r := hs.cmd(ViaLXMF, oper, "", "/release rab"); r.Text() != "released Rab (held by "+hexID(rab)+")" {
		t.Errorf("release %q", r.Text())
	}
	if n := eventsOf[NoticeEvent](hs.events()); len(n) != 1 || !strings.HasPrefix(n[0].Text, "A hub operator has released the name Rab.") {
		t.Errorf("release notice %+v", n)
	}
	var entries []store.AuditEntry
	_ = hs.st.View(bg, func(tx *store.Tx) error {
		var err error
		entries, err = tx.AuditLog(10)
		return err
	})
	if len(entries) != 3 || entries[0].Action != "release" || entries[1].Action != "unban" || entries[2].Action != "ban" || entries[2].Detail != "for good: spamming" {
		t.Errorf("audit %+v", entries)
	}
}

func TestHistoryCommand(t *testing.T) {
	hs := start(t)
	hs.identify(alex)
	hs.join(alex, "scotmesh")
	for i := range 5 {
		hs.say(ViaRRC, alex, fmt.Sprintf("m%d", i))
	}
	r := hs.cmd(ViaRRC, alex, "scotmesh", "/history 3")
	if got := bodies(r.History); strings.Join(got, ",") != "m2,m3,m4" || r.HistoryNote != "— the last 3 messages in #scotmesh —" {
		t.Errorf("/history 3: %v %q", got, r.HistoryNote)
	}
	if r := hs.cmd(ViaRRC, alex, "scotmesh", "/history"); !r.Error || r.Text() != "please wait 15 seconds before asking for history again" {
		t.Errorf("cooldown %+v", r)
	}
	hs.clock.advance(15 * time.Second)
	if r := hs.cmd(ViaRRC, alex, "scotmesh", "/history x"); r.Text() != "usage: /history [#room] [n]" {
		t.Errorf("bad count %q", r.Text())
	}
}

func TestUnregisteredRoomIsForgottenWhenEmpty(t *testing.T) {
	hs := start(t)
	hs.identify(alex)
	hs.join(alex, "temp")
	if err := hs.h.Disconnect(bg, alex, []string{"temp"}); err != nil {
		t.Fatal(err)
	}
	_ = hs.st.View(bg, func(tx *store.Tx) error {
		if _, ok, _ := tx.Room("temp"); ok {
			t.Error("empty unregistered room kept")
		}
		if _, ok, _ := tx.Room("scotmesh"); !ok {
			t.Error("default room missing")
		}
		return nil
	})
}

func TestSlowSubscriberDropsOnlyItsOwnEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(bg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	h, err := New(bg, Defaults(), st, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	slow := h.Subscribe("slow", 1)
	fast := h.Subscribe("fast", 100)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	if _, err := h.Identify(bg, alex, nil); err != nil {
		t.Fatal(err)
	}
	for _, room := range []string{"a", "b", "c"} {
		if _, err := h.Join(bg, JoinRequest{Identity: alex, Room: room}); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	<-done
	if slow.Dropped() != 2 || fast.Dropped() != 0 {
		t.Errorf("dropped slow=%d fast=%d", slow.Dropped(), fast.Dropped())
	}
	if _, err := h.Identify(bg, alex, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("call after stop: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	st, err := store.Open(bg, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	bad := []func(*Config){
		func(c *Config) { c.Admins = [][]byte{{1, 2}} },
		func(c *Config) { c.GroupRoom = "not listed" },
		func(c *Config) { c.GroupRoom = "elsewhere" },
		func(c *Config) { c.Rooms = []RoomConfig{{Name: ""}} },
	}
	for i, m := range bad {
		cfg := Defaults()
		m(&cfg)
		if _, err := New(bg, cfg, st); err == nil {
			t.Errorf("bad config %d accepted", i)
		}
	}
	cfg := Config{} // all defaults
	if _, err := New(bg, cfg, st); err != nil {
		t.Errorf("empty config: %v", err)
	}
}

func TestDeliveriesAreQueuedWithTheMessage(t *testing.T) {
	hs := start(t)
	for _, p := range []struct {
		id   []byte
		name string
	}{{alex, "Alex"}, {rab, "Rab"}, {ellen, "Ellen"}} {
		if _, _, err := hs.h.JoinGroup(bg, p.id, p.name); err != nil {
			t.Fatal(err)
		}
	}
	hs.cmd(ViaLXMF, ellen, "", "/lxmf off")
	hs.cmd(ViaLXMF, alex, "", "/lxmf mine off")
	m := hs.say(ViaLXMF, alex, "for the group")
	due, err := hs.h.DueDeliveries(bg, 10)
	if err != nil || len(due) != 1 || !bytes.Equal(due[0].Identity, rab) || due[0].Message.Body != "for the group" {
		t.Fatalf("due %+v %v; want only Rab (Ellen is off, Alex wrote it)", due, err)
	}

	later := hs.clock.now().Add(time.Minute)
	away := hs.clock.now().Add(10 * time.Minute)
	if err := hs.h.RecordDelivery(bg, due[0].Delivery, RetryAt, false, later, away); err != nil {
		t.Fatal(err)
	}
	if due, _ := hs.h.DueDeliveries(bg, 10); len(due) != 0 {
		t.Fatalf("a delivery set to retry later is due now: %+v", due)
	}
	hs.clock.advance(2 * time.Minute)
	due, _ = hs.h.DueDeliveries(bg, 10)
	if len(due) != 1 || due[0].AwayUntil != away.UnixMilli() {
		t.Fatalf("retry %+v", due)
	}
	if err := hs.h.RecordDelivery(bg, due[0].Delivery, Delivered, true, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if due, _ := hs.h.DueDeliveries(bg, 10); len(due) != 0 {
		t.Fatalf("delivered but still due: %+v", due)
	}
	m2 := hs.say(ViaRRC, alex, "not in the group room? yes it is")
	_ = m2
	due, _ = hs.h.DueDeliveries(bg, 10)
	if len(due) != 1 {
		t.Fatalf("RRC post to the group room queued %d deliveries, want 1", len(due))
	}
	if err := hs.h.RecordDelivery(bg, due[0].Delivery, GaveUp, false, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := hs.h.RecordDelivery(bg, due[0].Delivery, Delivered, false, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	_ = hs.st.View(bg, func(tx *store.Tx) error {
		ms, _ := tx.Members()
		for _, mem := range ms {
			if bytes.Equal(mem.Identity, rab) && mem.AwayUntil != 0 {
				t.Errorf("a direct delivery did not clear away: %d", mem.AwayUntil)
			}
		}
		return nil
	})
	_ = m
}

func TestReadCursorsAndRoomLookup(t *testing.T) {
	hs := start(t)
	if _, ok, err := hs.h.ReadCursor(bg, alex, "scotmesh", ViaPage); err != nil || ok {
		t.Fatalf("cursor before any read: %v %v", ok, err)
	}
	if err := hs.h.MarkRead(bg, alex, "scotmesh", ViaPage, 7); err != nil {
		t.Fatal(err)
	}
	c, ok, err := hs.h.ReadCursor(bg, alex, "scotmesh", ViaPage)
	if err != nil || !ok || c.MessageID != 7 {
		t.Fatalf("cursor %+v %v %v", c, ok, err)
	}
	if _, ok, _ := hs.h.ReadCursor(bg, alex, "scotmesh", ViaRRC); ok {
		t.Error("a page read moved the RRC cursor")
	}
	r, ok, err := hs.h.Room(bg, "scotmesh")
	if err != nil || !ok || !r.Registered {
		t.Fatalf("room %+v %v %v", r, ok, err)
	}
	if _, ok, _ := hs.h.Room(bg, "nowhere"); ok {
		t.Error("found a room that doesn't exist")
	}
}

func TestHistoryMethodRefusals(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	hs.join(alex, "den")
	hs.cmd(ViaRRC, alex, "den", "/room mode #den +p")
	_, err := hs.h.History(bg, rab, "den", 5, 0)
	userErr(t, err, "room den is private")
	hs.clock.advance(time.Minute)
	_, err = hs.h.History(bg, rab, "nowhere", 5, 0)
	userErr(t, err, "no such room")
	hs.clock.advance(time.Minute)
	_, err = hs.h.History(bg, rab, "bad room", 5, 0)
	userErr(t, err, "room name may only use")
	hs.cmd(ViaRRC, alex, "den", "/room ban #den "+hexID(rab))
	hs.clock.advance(time.Minute)
	_, err = hs.h.History(bg, rab, "den", 5, 0)
	userErr(t, err, "banned from room")
	if msgs, err := hs.h.History(bg, alex, "den", 0, 0); err != nil || len(msgs) != 0 {
		t.Errorf("empty history %v %v", msgs, err)
	}
}

// Your own messages from RRC and the page should still arrive in your LXMF
// conversation, when you've asked for that.
func TestLXMFMineDeliversYourOwnRRCAndPageMessages(t *testing.T) {
	hs := start(t)
	for _, p := range []struct {
		id   []byte
		name string
	}{{alex, "Alex"}, {rab, "Rab"}} {
		if _, _, err := hs.h.JoinGroup(bg, p.id, p.name); err != nil {
			t.Fatal(err)
		}
	}
	hs.join(alex, "scotmesh")
	to := func(via Via, body string) []string {
		t.Helper()
		hs.events()
		hs.say(via, alex, body)
		var names []string
		for _, e := range eventsOf[MessageEvent](hs.events()) {
			for _, id := range e.LXMFTo {
				names = append(names, map[string]string{hexID(alex): "Alex", hexID(rab): "Rab"}[hexID(id)])
			}
		}
		return names
	}
	// Joining turned it on; start from off.
	hs.cmd(ViaLXMF, alex, "", "/lxmf mine off")
	if got := strings.Join(to(ViaRRC, "off"), ","); got != "Rab" {
		t.Errorf("off: %s", got)
	}
	r := hs.cmd(ViaRRC, alex, "scotmesh", "/lxmf mine on")
	if !strings.HasPrefix(r.Text(), "Your own messages from RRC and the chat page will now come to you by LXMF too") {
		t.Errorf("/lxmf mine on: %q", r.Text())
	}
	if got := strings.Join(to(ViaRRC, "from rrc"), ","); got != "Alex,Rab" {
		t.Errorf("mine on, RRC: %s", got)
	}
	if got := strings.Join(to(ViaPage, "from the page"), ","); got != "Alex,Rab" {
		t.Errorf("mine on, page: %s", got)
	}
	if got := strings.Join(to(ViaLXMF, "from lxmf"), ","); got != "Rab" {
		t.Errorf("mine on, LXMF (the app already shows it): %s", got)
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/lxmf"); !strings.Contains(r.Text(), "your own messages from RRC and the page: on") {
		t.Errorf("/lxmf status %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/lxmf mine maybe"); !r.Error {
		t.Errorf("bad mine value accepted: %q", r.Text())
	}
	hs.cmd(ViaLXMF, alex, "", "/lxmf off")
	if got := strings.Join(to(ViaRRC, "paused"), ","); got != "Rab" {
		t.Errorf("mine on but LXMF off: %s", got)
	}
	hs.cmd(ViaLXMF, alex, "", "/lxmf on")
	hs.cmd(ViaLXMF, alex, "", "/lxmf mine off")
	if got := strings.Join(to(ViaRRC, "mine off again"), ","); got != "Rab" {
		t.Errorf("mine off: %s", got)
	}
	if p, err := hs.h.SetLXMFMine(bg, alex, true); err != nil || !p.LXMFMine {
		t.Errorf("SetLXMFMine %+v %v", p, err)
	}
}
