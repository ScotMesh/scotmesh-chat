package lxmfgroup

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

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

type sent struct {
	to         []byte
	content    string
	propagated bool
}

// fakeSender records sends; fail decides whether an attempt to an address fails.
type fakeSender struct {
	mu   sync.Mutex
	log  []sent
	fail func(to []byte, propagated bool) error
}

func (f *fakeSender) record(to, content []byte, propagated bool) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		if err := f.fail(to, propagated); err != nil {
			return nil, err
		}
	}
	f.log = append(f.log, sent{to: to, content: string(content), propagated: propagated})
	return []byte{1}, nil
}

func (f *fakeSender) SendWithID(to, _, content []byte, _ map[any]any) ([]byte, error) {
	return f.record(to, content, false)
}

func (f *fakeSender) SendPropagated(_, to, _, content []byte, _ map[any]any) ([]byte, error) {
	return f.record(to, content, true)
}

func (f *fakeSender) to(addr []byte) []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sent
	for _, s := range f.log {
		if bytes.Equal(s.to, addr) {
			out = append(out, s)
		}
	}
	return out
}

type fakeNet struct {
	mu    sync.Mutex
	known map[string]*rns.KnownIdentity
	asked [][]byte
}

func (n *fakeNet) Recall(dest []byte) *rns.KnownIdentity {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.known[string(dest)]
}

func (n *fakeNet) RequestPath(dest []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.asked = append(n.asked, dest)
	return nil
}

type person struct {
	id   *rns.Identity
	addr []byte
}

type env struct {
	t     *testing.T
	hub   *hub.Hub
	g     *Group
	send  *fakeSender
	net   *fakeNet
	clock *testClock
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &testClock{t: time.Now()}
	h, err := hub.New(context.Background(), hub.Defaults(), st, hub.WithLogger(quiet), hub.WithClock(c.now))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, hub: h, send: &fakeSender{}, net: &fakeNet{known: map[string]*rns.KnownIdentity{}}, clock: c}
	cfg := Defaults()
	cfg.PropagationNode = bytes.Repeat([]byte{0x9a}, 16)
	cfg.Senders = 2
	e.g = New(cfg, h, h.Subscribe("lxmf", 1024), e.send, e.net, WithLogger(quiet), WithClock(c.now))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = h.Run(ctx) }()
	go func() { defer wg.Done(); _ = e.g.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		_ = st.Close()
	})
	return e
}

func (e *env) person(displayName string) *person {
	e.t.Helper()
	id, err := rns.NewIdentity()
	if err != nil {
		e.t.Fatal(err)
	}
	p := &person{id: id, addr: LXMFAddress(id.Hash())}
	var appData []byte
	if displayName != "" {
		if appData, err = rns.EncodeLXMFAppData([]byte(displayName), nil); err != nil {
			e.t.Fatal(err)
		}
	}
	e.net.mu.Lock()
	e.net.known[string(p.addr)] = &rns.KnownIdentity{PublicKey: id.PublicKey(), AppData: appData}
	e.net.mu.Unlock()
	return p
}

var groupAddr = bytes.Repeat([]byte{0x62}, 16)

// signed builds a real signed LXMF message, as the transport hands it over:
// message IDs and de-duplication keys come from the signature and payload.
func (e *env) signed(p *person, text string) *lxmf.Message {
	e.t.Helper()
	e.clock.advance(time.Millisecond)
	body, _, err := lxmf.SignAndPackDirectStamped(p.id, p.addr, groupAddr, nil, []byte(text), nil, lxmf.StampOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := lxmf.ParseDirectBody(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) from(p *person, text string) {
	e.t.Helper()
	e.g.OnMessage(e.signed(p, text))
	time.Sleep(2 * time.Millisecond) // LXMF timestamps are in seconds with a fraction; keep them apart
}

// waitFor waits until some message to p contains needle and returns it.
func (e *env) waitFor(p *person, needle string) sent {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range e.send.to(p.addr) {
			if strings.Contains(s.content, needle) {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	var got []string
	for _, s := range e.send.to(p.addr) {
		got = append(got, s.content)
	}
	e.t.Fatalf("nothing to %x containing %q; got %q", p.addr[:4], needle, got)
	return sent{}
}

func (e *env) count(p *person, needle string) int {
	n := 0
	for _, s := range e.send.to(p.addr) {
		if strings.Contains(s.content, needle) {
			n++
		}
	}
	return n
}

func TestJoinUsesTheAnnouncedNameAndSendsADigest(t *testing.T) {
	e := newEnv(t)
	rab := e.person("Rab")
	e.from(rab, "/join")
	e.waitFor(rab, "Welcome to ScotMesh, Rab.")
	e.from(rab, "first words")

	ellen := e.person("Ellen Smith")
	e.from(ellen, "/join")
	welcome := e.waitFor(ellen, "Welcome to ScotMesh, Ellen_Smith.")
	if !strings.Contains(welcome.content, "— the last 1 messages —") && !strings.Contains(welcome.content, "Rab: first words") {
		// the digest may arrive in the same reply
		e.waitFor(ellen, "Rab: first words")
	}
	e.waitFor(ellen, "Rab: first words")

	stranger := e.person("")
	e.from(stranger, "hello?")
	e.waitFor(stranger, "Send /join to take part")
}

func TestFanOutIsNameColonMessageAndSkipsTheAuthor(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.person("Alex"), e.person("Rab"), e.person("Ellen")
	for _, p := range []*person{a, b, c} {
		e.from(p, "/join")
		e.waitFor(p, "Welcome to ScotMesh")
	}
	e.from(a, "evening all")
	e.waitFor(b, "Alex: evening all")
	e.waitFor(c, "Alex: evening all")
	e.from(a, "/me waves")
	e.waitFor(b, "* Alex waves")
	time.Sleep(100 * time.Millisecond)
	if n := e.count(a, "Alex: evening all"); n != 0 {
		t.Errorf("the author got their own message %d times", n)
	}

	// The same LXMF message delivered twice is posted once.
	msg := e.signed(b, "only once")
	e.g.OnMessage(msg)
	e.g.OnMessage(msg)
	e.waitFor(c, "Rab: only once")
	time.Sleep(200 * time.Millisecond)
	if n := e.count(c, "Rab: only once"); n != 1 {
		t.Errorf("duplicate LXMF message reached a member %d times", n)
	}
}

func TestSettingsAndResumeNote(t *testing.T) {
	e := newEnv(t)
	a, b := e.person("Alex"), e.person("Rab")
	for _, p := range []*person{a, b} {
		e.from(p, "/join")
		e.waitFor(p, "Welcome to ScotMesh")
	}
	e.from(b, "/lxmf off")
	e.waitFor(b, "LXMF delivery is off.")
	e.from(a, "one")
	e.from(a, "two")
	time.Sleep(300 * time.Millisecond)
	if e.count(b, "Alex: one") != 0 {
		t.Fatal("a paused member got a message")
	}
	e.from(b, "/lxmf on")
	e.waitFor(b, "2 messages were said while it was paused: send /history 2 to read them.")
	e.from(b, "/history 2")
	h := e.waitFor(b, "— the last 2 messages in #scotmesh —")
	if !strings.Contains(h.content, "Alex: one") || !strings.Contains(h.content, "Alex: two") {
		t.Errorf("history reply %q", h.content)
	}
}

func TestUnreachableMemberGoesToThePropagationNode(t *testing.T) {
	e := newEnv(t)
	a, b := e.person("Alex"), e.person("Rab")
	for _, p := range []*person{a, b} {
		e.from(p, "/join")
		e.waitFor(p, "Welcome to ScotMesh")
	}
	e.send.mu.Lock()
	e.send.fail = func(to []byte, propagated bool) error {
		if bytes.Equal(to, b.addr) && !propagated {
			return lxmf.ErrDeliveryProofTimeout
		}
		return nil
	}
	e.send.mu.Unlock()

	e.from(a, "are you there")
	// First failure retries after a backoff; move the clock past it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range e.send.to(b.addr) {
			if s.propagated && s.content == "Alex: are you there" {
				goto propagated
			}
		}
		e.clock.advance(31 * time.Second)
		e.g.nudge()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never sent via the propagation node: %+v", e.send.to(b.addr))
propagated:
	// While away, the next message goes straight to the propagation node.
	before := len(e.send.to(b.addr))
	e.from(a, "second")
	got := e.waitFor(b, "Alex: second")
	if !got.propagated {
		t.Error("a message to an away member was tried directly first")
	}
	if len(e.send.to(b.addr)) != before+1 {
		t.Errorf("extra attempts: %+v", e.send.to(b.addr)[before:])
	}
}

func TestStampCostTooHighGivesUp(t *testing.T) {
	e := newEnv(t)
	a, b := e.person("Alex"), e.person("Rab")
	for _, p := range []*person{a, b} {
		e.from(p, "/join")
		e.waitFor(p, "Welcome to ScotMesh")
	}
	var mu sync.Mutex
	attempts := 0
	e.send.mu.Lock()
	e.send.fail = func(to []byte, _ bool) error {
		if bytes.Equal(to, b.addr) {
			mu.Lock()
			attempts++
			mu.Unlock()
			return lxmf.ErrStampCostTooHigh
		}
		return nil
	}
	e.send.mu.Unlock()
	e.from(a, "costly")
	time.Sleep(300 * time.Millisecond)
	e.clock.advance(time.Hour)
	e.g.nudge()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("%d attempts at a member demanding too costly a stamp, want 1", attempts)
	}
}

func TestUnknownSenderIsAskedForAndHandledOnRetry(t *testing.T) {
	e := newEnv(t)
	p := e.person("Morag")
	e.net.mu.Lock()
	k := e.net.known[string(p.addr)]
	delete(e.net.known, string(p.addr))
	e.net.mu.Unlock()
	msg := e.signed(p, "/join")
	e.g.OnMessage(msg)
	time.Sleep(200 * time.Millisecond)
	e.net.mu.Lock()
	asked := len(e.net.asked)
	e.net.known[string(p.addr)] = k
	e.net.mu.Unlock()
	if asked == 0 {
		t.Error("no path request for an unknown sender")
	}
	e.g.OnMessage(msg) // the client retries the same message
	e.waitFor(p, "Welcome to ScotMesh, Morag.")
	if e.g.Stats().UnknownSender.Load() != 1 {
		t.Errorf("unknown sender count %d", e.g.Stats().UnknownSender.Load())
	}
}

func TestLineFormats(t *testing.T) {
	if got := Line(&store.Message{AuthorName: "Rab", Body: "hi", Kind: store.KindMsg}); got != "Rab: hi" {
		t.Errorf("msg %q", got)
	}
	if got := Line(&store.Message{AuthorName: "Rab", Body: "waves", Kind: store.KindAction}); got != "* Rab waves" {
		t.Errorf("action %q", got)
	}
	if !errors.Is(lxmf.ErrStampCostTooHigh, lxmf.ErrStampCostTooHigh) {
		t.Fatal("sanity")
	}
}

func TestUnknownRecipientPathAndGiveUp(t *testing.T) {
	e := newEnv(t)
	a, b := e.person("Alex"), e.person("Rab")
	for _, p := range []*person{a, b} {
		e.from(p, "/join")
		e.waitFor(p, "Welcome to ScotMesh")
	}
	e.send.mu.Lock()
	e.send.fail = func(to []byte, _ bool) error {
		if bytes.Equal(to, b.addr) {
			return lxmf.ErrRecipientUnknown
		}
		return nil
	}
	e.send.mu.Unlock()
	e.from(a, "hello?")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.net.mu.Lock()
		n := 0
		for _, d := range e.net.asked {
			if bytes.Equal(d, b.addr) {
				n++
			}
		}
		e.net.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A day later the delivery is abandoned rather than retried for ever.
	e.clock.advance(25 * time.Hour)
	e.g.nudge()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.g.Stats().GaveUp.Load() == 0 {
		e.g.nudge()
		time.Sleep(10 * time.Millisecond)
	}
	if e.g.Stats().GaveUp.Load() != 1 {
		t.Errorf("gave up %d times, want 1", e.g.Stats().GaveUp.Load())
	}
}

func TestRepliesFallBackToPropagationAndNoticesReachMembers(t *testing.T) {
	e := newEnv(t)
	a := e.person("Alex")
	e.from(a, "/join")
	e.waitFor(a, "Welcome to ScotMesh")
	e.send.mu.Lock()
	e.send.fail = func(_ []byte, propagated bool) error {
		if !propagated {
			return lxmf.ErrDeliveryProofTimeout
		}
		return nil
	}
	e.send.mu.Unlock()
	e.from(a, "/whoami")
	if r := e.waitFor(a, "You are Alex"); !r.propagated {
		t.Error("a reply that failed directly was not left on the propagation node")
	}
	reply, err := e.hub.Command(context.Background(), hub.CommandRequest{Via: hub.ViaRRC, Identity: bytes.Repeat([]byte{1}, 16), Text: "/whoami"})
	if err != nil || reply.Error {
		t.Fatalf("%+v %v", reply, err)
	}
}

func TestNoticesAndNames(t *testing.T) {
	e := newEnv(t)
	a := e.person("Alex")
	e.from(a, "/join")
	e.waitFor(a, "Welcome to ScotMesh")
	// A notice from the hub reaches a member; a non-member hears nothing.
	stranger := e.person("")
	if _, err := e.hub.Identify(context.Background(), stranger.id.Hash(), stranger.id.PublicKey()); err != nil {
		t.Fatal(err)
	}
	e.g.replyIfMember(context.Background(), a.id.Hash(), "hello member")
	e.g.replyIfMember(context.Background(), stranger.id.Hash(), "hello stranger")
	e.waitFor(a, "hello member")
	time.Sleep(100 * time.Millisecond)
	if e.count(stranger, "hello stranger") != 0 {
		t.Error("a non-member got a member notice")
	}

	for in, want := range map[string]string{"Ellen Smith": "Ellen_Smith", "  Rab  ": "Rab", "7 of 9": "of_9", "": ""} {
		var ad []byte
		if in != "" {
			var err error
			if ad, err = rns.EncodeLXMFAppData([]byte(in), nil); err != nil {
				t.Fatal(err)
			}
		}
		if got := announcedName(&rns.KnownIdentity{AppData: ad}); got != want {
			t.Errorf("announcedName(%q) = %q, want %q", in, got, want)
		}
	}
	if announcedName(nil) != "" || announcedName(&rns.KnownIdentity{AppData: []byte{0xff}}) != "" {
		t.Error("announcedName on nil or garbage")
	}
	// An empty message is ignored; a banned sender hears nothing.
	e.from(a, "   ")
}

func TestLongMessagesGoInOnePacketPartsAndResumeAfterAFailure(t *testing.T) {
	e := newEnv(t)
	a, b := e.person("Alex"), e.person("Rab")
	for _, p := range []*person{a, b} {
		e.from(p, "/join")
		e.waitFor(p, "Welcome to ScotMesh")
	}
	// Rab's second part fails once; the retry must not send part one again.
	var mu sync.Mutex
	failed := false
	e.send.mu.Lock()
	e.send.fail = func(to []byte, _ bool) error {
		mu.Lock()
		defer mu.Unlock()
		if bytes.Equal(to, b.addr) && !failed && len(e.send.log) > 0 && strings.HasPrefix(e.send.log[len(e.send.log)-1].content, "(1/") {
			failed = true
			return lxmf.ErrDeliveryProofTimeout
		}
		return nil
	}
	e.send.mu.Unlock()

	long := strings.Repeat("the quick brown fox jumps over the lazy dog ", 15) // about 660 bytes
	e.from(a, long)
	e.waitFor(b, "(1/3) Alex: the quick")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && e.count(b, "(3/3) ") == 0 {
		e.clock.advance(31 * time.Second)
		e.g.nudge()
		time.Sleep(20 * time.Millisecond)
	}
	for _, mark := range []string{"(1/3) ", "(2/3) ", "(3/3) "} {
		if n := e.count(b, mark); n != 1 {
			t.Errorf("%s arrived %d times, want once", mark, n)
		}
	}
	mu.Lock()
	if !failed {
		t.Error("the injected failure never happened")
	}
	mu.Unlock()
	for _, s := range e.send.to(b.addr) {
		if len(s.content) > MaxContent {
			t.Errorf("a message of %d bytes went out", len(s.content))
		}
	}
}

func TestLongRepliesAreSplitAndFallBackInOrder(t *testing.T) {
	e := newEnv(t)
	a := e.person("Alex")
	e.from(a, "/join")
	e.waitFor(a, "Welcome to ScotMesh")
	for i := range 12 {
		e.from(a, fmt.Sprintf("message number %d with a bit of padding to make it longer", i))
	}
	e.send.mu.Lock()
	e.send.fail = func(_ []byte, propagated bool) error {
		if !propagated {
			return lxmf.ErrDeliveryProofTimeout
		}
		return nil
	}
	e.send.mu.Unlock()
	e.from(a, "/history 12")
	last := e.waitFor(a, "message number 11")
	if !last.propagated {
		t.Error("a reply part that failed directly didn't go to the propagation node")
	}
	var parts []string
	for _, s := range e.send.to(a.addr) {
		if s.propagated && partMark.MatchString(s.content) {
			parts = append(parts, s.content)
		}
	}
	if len(parts) < 2 {
		t.Fatalf("history reply wasn't split: %q", parts)
	}
	for i, p := range parts {
		if !strings.HasPrefix(p, fmt.Sprintf("(%d/%d) ", i+1, len(parts))) || len(p) > MaxContent {
			t.Errorf("part %d out of order or too long: %q", i, p)
		}
	}
}

func TestWhispersComeByLXMFEvenWithDeliveryOff(t *testing.T) {
	e := newEnv(t)
	a, b := e.person("Alex"), e.person("Rab")
	for _, p := range []*person{a, b} {
		e.from(p, "/join")
		e.waitFor(p, "Welcome to ScotMesh")
	}
	e.from(b, "/lxmf off")
	e.waitFor(b, "LXMF delivery is off.")
	e.from(a, "/whisper Rab only for you")
	e.waitFor(a, "Whispered to Rab (by LXMF).")
	e.waitFor(b, "Alex whispers: only for you")
	e.from(b, "!r got it")
	e.waitFor(a, "Rab whispers: got it")
	time.Sleep(100 * time.Millisecond)
	if e.count(a, "only for you") != 0 || e.count(b, "got it") != 0 {
		t.Error("a whisper came back to its sender")
	}
}
