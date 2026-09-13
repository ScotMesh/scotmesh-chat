package rrcsrv

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// fakeTransport records frames per link and closes links like the real
// transport does: TeardownLink fires the closed hook.
type fakeTransport struct {
	mu     sync.Mutex
	frames map[string][][]byte
	torn   map[string]int
	hooks  *rns.LinkHooks
}

func (f *fakeTransport) SendOnLink(link, pt []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.torn[hex.EncodeToString(link)] > 0 {
		return fmt.Errorf("link closed")
	}
	f.frames[hex.EncodeToString(link)] = append(f.frames[hex.EncodeToString(link)], append([]byte(nil), pt...))
	return nil
}

func (f *fakeTransport) TeardownLink(link []byte) {
	f.mu.Lock()
	k := hex.EncodeToString(link)
	f.torn[k]++
	first := f.torn[k] == 1
	f.mu.Unlock()
	if first {
		f.hooks.OnClosed(link, rns.TeardownLocalClosed)
	}
}

type env struct {
	t      *testing.T
	hub    *hub.Hub
	srv    *Server
	ft     *fakeTransport
	hubID  []byte
	cancel context.CancelFunc
	wg     sync.WaitGroup
	clock  *testClock
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

var operator = bytes.Repeat([]byte{0x0f}, 16)

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := &testClock{t: time.Date(2026, 9, 12, 18, 0, 0, 0, time.UTC)}
	hcfg := hub.Defaults()
	hcfg.Admins = [][]byte{operator}
	h, err := hub.New(context.Background(), hcfg, st, hub.WithLogger(quiet), hub.WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, hub: h, ft: &fakeTransport{frames: map[string][][]byte{}, torn: map[string]int{}}, hubID: bytes.Repeat([]byte{0x40}, 16), clock: clk}
	cfg := Defaults()
	cfg.Greeting = []string{"Welcome to the ScotMesh RRC hub."}
	cfg.GroupRoom = "scotmesh"
	for _, m := range mutate {
		m(&cfg)
	}
	e.srv = New(cfg, h, h.Subscribe("rrc", 1024), e.ft, e.hubID, WithLogger(quiet), WithClock(clk.now))
	e.ft.hooks = e.srv.Hooks()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.wg.Add(2)
	go func() { defer e.wg.Done(); _ = h.Run(ctx) }()
	go func() { defer e.wg.Done(); _ = e.srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		e.wg.Wait()
		_ = st.Close()
	})
	return e
}

type client struct {
	e    *env
	link []byte
	id   *rns.Identity
	read int
}

func (e *env) client(t *testing.T) *client {
	t.Helper()
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	link := make([]byte, 16)
	copy(link, wire.NewID())
	copy(link[8:], wire.NewID())
	return &client{e: e, link: link, id: id}
}

func (c *client) hash() []byte { return c.id.Hash() }

func (c *client) connect() {
	c.e.ft.hooks.OnEstablished(c.link)
	c.e.ft.hooks.OnIdentified(c.link, c.id.PublicKey())
}

func (c *client) sendEnv(t wire.Type, room string, body any, nick string) {
	c.e.t.Helper()
	en := &wire.Envelope{Type: t, ID: wire.NewID(), TS: wire.NowMS(), Src: c.hash(), Room: room, Nick: nick}
	if body != nil {
		if s, ok := body.(string); ok {
			en.Body = wire.TextBody(s)
		} else if err := en.SetBody(body); err != nil {
			c.e.t.Fatal(err)
		}
	}
	frame, err := en.Encode()
	if err != nil {
		c.e.t.Fatal(err)
	}
	c.e.ft.hooks.OnData(c.link, frame)
}

func (c *client) hello(nick string) {
	c.e.t.Helper()
	c.sendEnv(wire.TypeHello, "", map[uint64]any{0: "test", 1: "0.1", 2: map[uint64]bool{0: true, 1: true}}, nick)
	c.expect(wire.TypeWelcome, "")
}

// frames returns every frame this link has been sent, decoded.
func (c *client) all() []*wire.Envelope {
	c.e.ft.mu.Lock()
	raw := append([][]byte(nil), c.e.ft.frames[hex.EncodeToString(c.link)]...)
	c.e.ft.mu.Unlock()
	out := make([]*wire.Envelope, 0, len(raw))
	for _, f := range raw {
		if len(f) > rns.LinkMDU {
			c.e.t.Fatalf("frame of %d bytes exceeds the link MDU", len(f))
		}
		en, err := wire.Decode(f, wire.DecodeLimits{})
		if err != nil {
			c.e.t.Fatalf("hub sent an invalid frame %x: %v", f, err)
		}
		out = append(out, en)
	}
	return out
}

// expect waits for the next unread frame of type t whose body contains
// substr, skipping others, and returns it.
func (c *client) expect(t wire.Type, substr string) *wire.Envelope {
	c.e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		frames := c.all()
		for i := c.read; i < len(frames); i++ {
			f := frames[i]
			body, _ := f.BodyString()
			if f.Type == t && strings.Contains(body, substr) {
				c.read = i + 1
				return f
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	var got []string
	for _, f := range c.all()[c.read:] {
		b, _ := f.BodyString()
		got = append(got, fmt.Sprintf("%s room=%q nick=%q %q", f.Type, f.Room, f.Nick, b))
	}
	c.e.t.Fatalf("no %s containing %q; unread frames:\n  %s", t, substr, strings.Join(got, "\n  "))
	return nil
}

// quiet asserts nothing of type t arrives within a short wait.
func (c *client) quiet(t wire.Type) {
	c.e.t.Helper()
	time.Sleep(100 * time.Millisecond)
	for _, f := range c.all()[c.read:] {
		if f.Type == t {
			b, _ := f.BodyString()
			c.e.t.Fatalf("unexpected %s %q", t, b)
		}
	}
}

func TestHandshakeWelcomeAndGreeting(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	c.connect()
	c.sendEnv(wire.TypeHello, "", map[uint64]any{0: "nomadnet", 1: "0.1"}, "Alex")
	w := c.expect(wire.TypeWelcome, "")
	if !bytes.Equal(w.Src, e.hubID) || w.Room != "" {
		t.Errorf("WELCOME src %x room %q", w.Src, w.Room)
	}
	var body map[uint64]any
	if err := w.DecodeBody(&body); err != nil {
		t.Fatal(err)
	}
	limits, _ := body[uint64(wire.WelcomeLimits)].(map[any]any)
	if body[uint64(wire.WelcomeHub)] != "ScotMesh" || limits[uint64(wire.LimitMaxMsgBodyBytes)] != uint64(350) {
		t.Errorf("WELCOME body %v", body)
	}
	greet := c.expect(wire.TypeNotice, "Welcome to the ScotMesh RRC hub.")
	if greet.Room != "" {
		t.Errorf("greeting carries room %q", greet.Room)
	}
	p, err := e.hub.PersonOf(context.Background(), c.hash())
	if err != nil || p.Name != "Alex" {
		t.Errorf("HELLO nick not claimed: %+v %v", p, err)
	}
}

func TestFramesBeforeIdentifyWaitForIt(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	e.ft.hooks.OnEstablished(c.link)
	c.sendEnv(wire.TypeHello, "", nil, "")
	c.quiet(wire.TypeWelcome)
	e.ft.hooks.OnIdentified(c.link, c.id.PublicKey())
	c.expect(wire.TypeWelcome, "")
	c.expect(wire.TypeNotice, "You are guest-")
}

func TestHelloFirstAndBadFrames(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	c.connect()
	c.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	if f := c.expect(wire.TypeError, "send HELLO first"); f.Room != "" {
		t.Errorf("room on ERROR %q", f.Room)
	}
	e.ft.hooks.OnData(c.link, []byte{0x83, 1, 2, 3})
	c.expect(wire.TypeError, "bad message: envelope must be a CBOR map (dict)")
}

func TestJoinCatchUpAndFanOut(t *testing.T) {
	e := newEnv(t)
	alex, rab := e.client(t), e.client(t)
	alex.connect()
	alex.hello("Alex")
	rab.connect()
	rab.hello("Rab")

	rab.sendEnv(wire.TypeJoin, "#ScotMesh", nil, "")
	rab.expect(wire.TypeJoined, "")
	rab.expect(wire.TypeNotice, "room scotmesh: registered; mode=+rt; topic=ScotMesh")
	for i := range 3 {
		rab.sendEnv(wire.TypeMsg, "scotmesh", fmt.Sprintf("early %d", i), "Rab")
		rab.expect(wire.TypeMsg, fmt.Sprintf("early %d", i)) // echoed to the sender, as rrcd does
	}

	alex.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	alex.expect(wire.TypeJoined, "")
	alex.expect(wire.TypeNotice, "— the last 3 messages in #scotmesh —")
	m := alex.expect(wire.TypeMsg, "early 0")
	if m.Nick != "Rab" || !bytes.Equal(m.Src, rab.hash()) || m.Room != "scotmesh" {
		t.Errorf("replayed message src %x nick %q room %q", m.Src, m.Nick, m.Room)
	}
	alex.expect(wire.TypeMsg, "early 2")
	alex.expect(wire.TypeNotice, "— end of history —")

	j := rab.expect(wire.TypeJoined, "")
	if j.Nick != "Alex" || j.Room != "scotmesh" {
		t.Errorf("JOINED fan-out nick %q room %q", j.Nick, j.Room)
	}
	var ids [][]byte
	if err := j.DecodeBody(&ids); err != nil || len(ids) != 1 || !bytes.Equal(ids[0], alex.hash()) {
		t.Errorf("JOINED body %v %v", ids, err)
	}

	// A message with an extension and its original ID reaches the other member intact.
	en := &wire.Envelope{Type: wire.TypeMsg, ID: []byte("orig-id1"), TS: 1234, Src: []byte("spoofed"), Room: "scotmesh",
		Nick: "Alex", Body: wire.TextBody("reply here"), Ext: map[wire.Key]cbor.RawMessage{64: wire.TextBody("target")}}
	frame, _ := en.Encode()
	e.ft.hooks.OnData(alex.link, frame)
	got := rab.expect(wire.TypeMsg, "reply here")
	if !bytes.Equal(got.Src, alex.hash()) || string(got.ID) != "orig-id1" || got.TS != 1234 {
		t.Errorf("relayed src %x id %q ts %d", got.Src, got.ID, got.TS)
	}
	if s, _ := cbor.Marshal("target"); !bytes.Equal(got.Ext[64], s) {
		t.Errorf("extension lost: %x", got.Ext[64])
	}

	// Disconnect, three messages, reconnect: exactly those three.
	e.ft.TeardownLink(alex.link)
	rab.expect(wire.TypeParted, "")
	for i := range 3 {
		rab.sendEnv(wire.TypeMsg, "scotmesh", fmt.Sprintf("while away %d", i), "Rab")
		rab.expect(wire.TypeMsg, fmt.Sprintf("while away %d", i)) // stored before anyone reconnects
	}
	e.clock.advance(time.Minute)
	back := &client{e: e, link: bytes.Repeat([]byte{0x77}, 16), id: alex.id}
	back.connect()
	back.hello("Alex")
	back.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	back.expect(wire.TypeNotice, "— 3 messages since you were here (")
	back.expect(wire.TypeMsg, "while away 0")
	back.expect(wire.TypeMsg, "while away 2")
	back.expect(wire.TypeNotice, "— end of history —")
	for _, f := range back.all() {
		if b, _ := f.BodyString(); f.Type == wire.TypeMsg && (strings.HasPrefix(b, "early") || b == "reply here") {
			t.Errorf("replayed a message from before the disconnect: %q", b)
		}
	}
}

func TestCommandsRepliesAreRoomlessAndPerLine(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	c.connect()
	c.hello("Alex")
	c.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	c.expect(wire.TypeJoined, "")

	c.sendEnv(wire.TypeMsg, "scotmesh", "/who", "Alex")
	who := c.expect(wire.TypeNotice, "members in scotmesh: Alex (")
	if who.Room != "" {
		t.Errorf("/who reply carries room %q", who.Room)
	}
	c.sendEnv(wire.TypeMsg, "scotmesh", "/help", "Alex")
	c.expect(wire.TypeNotice, "· chatting")
	c.expect(wire.TypeNotice, "/nick <name>")
	c.sendEnv(wire.TypeMsg, "scotmesh", "/kline list", "Alex")
	if f := c.expect(wire.TypeError, "not authorized"); f.Room != "" {
		t.Errorf("error room %q", f.Room)
	}
	c.sendEnv(wire.TypeNotice, "", "/frob", "")
	c.expect(wire.TypeError, "There's no /frob. Send /help for the commands.")
	// NomadNet keeps / to itself; ! reaches the hub, and !help works.
	c.sendEnv(wire.TypeMsg, "scotmesh", "!help 2", "Alex")
	c.expect(wire.TypeNotice, "Help 2/")
	// "!" that isn't a command is said, as is.
	c.sendEnv(wire.TypeMsg, "scotmesh", "!!! good news", "Alex")
	c.expect(wire.TypeMsg, "!!! good news")
	// !me is an action.
	c.sendEnv(wire.TypeMsg, "scotmesh", "!me waves", "Alex")
	if f := c.expect(wire.TypeAction, "waves"); f.Room != "scotmesh" {
		t.Errorf("!me action room %q", f.Room)
	}
	// /leave is a PART from this link.
	c.sendEnv(wire.TypeMsg, "scotmesh", "!leave", "Alex")
	c.expect(wire.TypeParted, "")
	c.expect(wire.TypeNotice, "You've left #scotmesh.")
	c.quiet(wire.TypeMsg) // commands are never relayed as chat
	c.sendEnv(wire.TypeMsg, "", "/leave #scotmesh", "Alex")
	c.expect(wire.TypeError, "You're not in #scotmesh.")
}

func TestLongMessagesFromOtherWaysInAreSplit(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	c.connect()
	c.hello("Alex")
	c.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	c.expect(wire.TypeJoined, "")

	ellen := bytes.Repeat([]byte{0xe3}, 16)
	if _, _, err := e.hub.JoinGroup(context.Background(), ellen, "Ellen"); err != nil {
		t.Fatal(err)
	}
	j := c.expect(wire.TypeJoined, "")
	if j.Nick != "Ellen" {
		t.Errorf("LXMF member join shown as %q", j.Nick)
	}
	long := strings.Repeat("the hill node on the Ochils hears Stirling fine ", 20) // ~1000 bytes
	if _, err := e.hub.Post(context.Background(), hub.PostRequest{Via: hub.ViaLXMF, Identity: ellen, Body: long}); err != nil {
		t.Fatal(err)
	}
	first := c.expect(wire.TypeMsg, "(1/")
	if !bytes.Equal(first.Src, ellen) || first.Nick != "Ellen" {
		t.Errorf("LXMF author src %x nick %q, want the real identity and name", first.Src, first.Nick)
	}
	var rebuilt strings.Builder
	parts := 0
	for _, f := range c.all() {
		b, _ := f.BodyString()
		if f.Type == wire.TypeMsg {
			parts++
			rebuilt.WriteString(b[:strings.LastIndex(b, " (")] + " ")
		}
	}
	if parts < 3 || strings.Join(strings.Fields(rebuilt.String()), " ") != strings.Join(strings.Fields(long), " ") {
		t.Errorf("%d parts do not rebuild the message", parts)
	}
}

func TestBadNickAndTakenNickAreExplainedOnce(t *testing.T) {
	e := newEnv(t)
	a, b := e.client(t), e.client(t)
	a.connect()
	a.hello("Alex")
	b.connect()
	b.sendEnv(wire.TypeHello, "", nil, "A1ex")
	b.expect(wire.TypeWelcome, "")
	b.expect(wire.TypeNotice, "\"A1ex\" is taken by someone else.")
	b.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	b.expect(wire.TypeJoined, "")
	b.sendEnv(wire.TypeMsg, "scotmesh", "hello", "A1ex")
	m := b.expect(wire.TypeMsg, "hello")
	if !strings.HasPrefix(m.Nick, "guest-") {
		t.Errorf("a refused nick went out as %q", m.Nick)
	}
	b.quiet(wire.TypeNotice) // the refusal is not repeated on every message
	b.sendEnv(wire.TypeMsg, "scotmesh", "now with a free one", "Morag")
	b.expect(wire.TypeMsg, "now with a free one")
	b.sendEnv(wire.TypeMsg, "scotmesh", "again", "Morag")
	if m := b.expect(wire.TypeMsg, "again"); m.Nick != "Morag" {
		t.Errorf("claimed nick not used: %q", m.Nick)
	}
}

func TestPingPongAndSilence(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	c.connect()
	c.hello("")
	c.sendEnv(wire.TypePing, "", uint64(42), "")
	pong := c.expect(wire.TypePong, "")
	var n uint64
	if err := pong.DecodeBody(&n); err != nil || n != 42 {
		t.Errorf("PONG body %d %v", n, err)
	}

	e.clock.advance(31 * time.Second)
	e.srv.keepalive(context.Background())
	c.expect(wire.TypePing, "")
	e.clock.advance(31 * time.Second)
	e.srv.keepalive(context.Background())
	e.ft.mu.Lock()
	torn := e.ft.torn[hex.EncodeToString(c.link)]
	e.ft.mu.Unlock()
	if torn == 0 {
		t.Error("a link silent after PING was not closed")
	}
}

func TestHelloTimeout(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	c.connect()
	time.Sleep(50 * time.Millisecond)
	e.clock.advance(31 * time.Second)
	e.srv.keepalive(context.Background())
	e.ft.mu.Lock()
	defer e.ft.mu.Unlock()
	if e.ft.torn[hex.EncodeToString(c.link)] == 0 {
		t.Error("a link that never said HELLO was not closed")
	}
}

func TestKlineClosesEveryLink(t *testing.T) {
	e := newEnv(t)
	victim := e.client(t)
	victim.connect()
	victim.hello("Spammer")
	second := &client{e: e, link: bytes.Repeat([]byte{0x55}, 16), id: victim.id}
	second.connect()
	second.hello("")
	reply, err := e.hub.Command(context.Background(), hub.CommandRequest{Via: hub.ViaRRC, Identity: operator, Text: "/kline add Spammer"})
	if err != nil || !strings.HasPrefix(reply.Text(), "Banned Spammer (1 app), for good.") {
		t.Fatalf("kline %q %v", reply.Text(), err)
	}
	victim.expect(wire.TypeError, "banned")
	second.expect(wire.TypeError, "banned")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.ft.mu.Lock()
		n := e.ft.torn[hex.EncodeToString(victim.link)] + e.ft.torn[hex.EncodeToString(second.link)]
		e.ft.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.ft.mu.Lock()
	defer e.ft.mu.Unlock()
	if e.ft.torn[hex.EncodeToString(victim.link)] == 0 || e.ft.torn[hex.EncodeToString(second.link)] == 0 {
		t.Error("klined identity's links were not all closed")
	}
}

func TestRejoinAndStrayPartKeepPresenceRight(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	c.connect()
	c.hello("Alex")
	c.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	c.expect(wire.TypeJoined, "")
	c.sendEnv(wire.TypeJoin, "scotmesh", nil, "") // again on the same link
	c.expect(wire.TypeJoined, "")
	c.sendEnv(wire.TypePart, "elsewhere", nil, "") // a room it was never in
	c.expect(wire.TypeParted, "")
	e.ft.TeardownLink(c.link)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ms, err := e.hub.Members(context.Background(), "scotmesh")
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a double JOIN left a ghost in the room after the link closed")
}

func TestDirectNotice(t *testing.T) {
	e := newEnv(t)
	a, b := e.client(t), e.client(t)
	a.connect()
	a.hello("Alex")
	b.connect()
	b.hello("Rab")
	en := &wire.Envelope{Type: wire.TypeNotice, ID: wire.NewID(), TS: 1, Src: a.hash(), Dst: b.hash(), Body: wire.TextBody("psst")}
	frame, _ := en.Encode()
	e.ft.hooks.OnData(a.link, frame)
	got := b.expect(wire.TypeNotice, "psst")
	if !bytes.Equal(got.Src, a.hash()) || got.Nick != "Alex" || !bytes.Equal(got.Dst, b.hash()) {
		t.Errorf("direct notice %+v", got)
	}
	en.Dst = bytes.Repeat([]byte{9}, 16)
	frame, _ = en.Encode()
	e.ft.hooks.OnData(a.link, frame)
	a.expect(wire.TypeError, "destination not connected")
}

// TestNoticeAppliesTheSameChecksAsMsgAndWhisper locks in the fix for RRC
// NOTICE reaching a room or a person with none of Post's or /whisper's
// rules applied: a moderated room's unvoiced member, and someone who has
// been ignored, are refused exactly as they would be for a MSG or a
// /whisper.
func TestNoticeAppliesTheSameChecksAsMsgAndWhisper(t *testing.T) {
	e := newEnv(t)
	a, b := e.client(t), e.client(t)
	a.connect()
	a.hello("Alex")
	b.connect()
	b.hello("Rab")
	a.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	a.expect(wire.TypeJoined, "")
	a.expect(wire.TypeNotice, "room scotmesh: registered")
	b.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	b.expect(wire.TypeJoined, "")
	b.expect(wire.TypeNotice, "room scotmesh: registered")

	// +m: an unvoiced member's room NOTICE is refused, same as a MSG would be.
	if reply, err := e.hub.Command(context.Background(), hub.CommandRequest{Via: hub.ViaRRC, Identity: operator, Text: "/room mode #scotmesh +m"}); err != nil || reply.Error {
		t.Fatalf("+m: %+v %v", reply, err)
	}
	a.expect(wire.TypeNotice, "mode for scotmesh is now: +m")
	b.expect(wire.TypeNotice, "mode for scotmesh is now: +m")
	b.sendEnv(wire.TypeMsg, "scotmesh", "should be blocked too", "")
	b.expect(wire.TypeError, "room is moderated (+m)")
	b.sendEnv(wire.TypeNotice, "scotmesh", "should be blocked", "")
	b.expect(wire.TypeError, "room is moderated (+m)")
	a.quiet(wire.TypeNotice)

	// Voiced, the same NOTICE now reaches the room.
	if reply, err := e.hub.Command(context.Background(), hub.CommandRequest{Via: hub.ViaRRC, Identity: operator, Text: "/room voice #scotmesh " + hex.EncodeToString(b.hash())}); err != nil || reply.Error {
		t.Fatalf("voice: %+v %v", reply, err)
	}
	b.sendEnv(wire.TypeNotice, "scotmesh", "now allowed", "")
	a.expect(wire.TypeNotice, "now allowed")
	b.expect(wire.TypeNotice, "now allowed") // rrcd echoes back to the sender too

	// A direct NOTICE to someone who has ignored the sender is refused,
	// the same as /whisper is, and without saying why.
	if reply, err := e.hub.Command(context.Background(), hub.CommandRequest{Via: hub.ViaRRC, Identity: b.hash(), Text: "/ignore Alex"}); err != nil || reply.Error {
		t.Fatalf("ignore: %+v %v", reply, err)
	}
	en := &wire.Envelope{Type: wire.TypeNotice, ID: wire.NewID(), TS: 1, Src: a.hash(), Dst: b.hash(), Body: wire.TextBody("psst")}
	frame, _ := en.Encode()
	e.ft.hooks.OnData(a.link, frame)
	a.expect(wire.TypeError, "destination not connected")
	b.quiet(wire.TypeNotice)
}

func TestRateLimitBeforeDecode(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.RatePerMinute = 5 })
	c := e.client(t)
	c.connect()
	c.hello("")
	for range 10 {
		e.ft.hooks.OnData(c.link, []byte{0xff}) // garbage: would be "bad message" if decoded
	}
	c.expect(wire.TypeError, "rate limited")
	time.Sleep(100 * time.Millisecond)
	limited := 0
	for _, f := range c.all() {
		if b, _ := f.BodyString(); f.Type == wire.TypeError && b == "rate limited" {
			limited++
		}
	}
	if limited != 1 {
		t.Errorf("%d rate-limit errors, want 1 (they are limited too)", limited)
	}
}

func TestSplitText(t *testing.T) {
	for _, c := range []struct {
		in    string
		limit int
		want  []string
	}{
		{"short", 10, []string{"short"}},
		{"one two three four", 9, []string{"one two", "three", "four"}},
		{"ééééé", 5, []string{"éé", "éé", "é"}},
		{"nospacesatall", 5, []string{"nospa", "cesat", "all"}},
	} {
		got := splitText(c.in, c.limit)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitText(%q, %d) = %q, want %q", c.in, c.limit, got, c.want)
		}
		for _, p := range got {
			if len(p) > c.limit {
				t.Errorf("piece %q over %d bytes", p, c.limit)
			}
		}
	}
}

func TestMessageRefusalsAndRoomTraffic(t *testing.T) {
	e := newEnv(t)
	a, b := e.client(t), e.client(t)
	a.connect()
	a.hello("Alex")
	b.connect()
	b.hello("Rab")
	a.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	a.expect(wire.TypeJoined, "")
	b.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	b.expect(wire.TypeJoined, "")

	a.sendEnv(wire.TypeMsg, "", "no room", "")
	a.expect(wire.TypeError, "message requires room name")
	a.sendEnv(wire.TypeMsg, "scotmesh", []int{1, 2}, "")
	a.expect(wire.TypeError, "message body must be text")
	a.sendEnv(wire.TypeMsg, "scotmesh", strings.Repeat("x", 351), "")
	a.expect(wire.TypeError, "message too large: 351 bytes > 350 bytes")
	a.sendEnv(wire.TypeMsg, "nowhere", "hi", "")
	a.expect(wire.TypeError, "no such room")
	a.sendEnv(wire.TypeResourceEnvelope, "scotmesh", "not a map", "")
	a.expect(wire.TypeError, "invalid resource envelope body")
	a.sendEnv(wire.TypeAction, "scotmesh", "waves", "")
	if f := b.expect(wire.TypeAction, "waves"); f.Nick != "Alex" {
		t.Errorf("ACTION nick %q", f.Nick)
	}

	// A NOTICE to a room goes to its RRC members, unstored.
	a.sendEnv(wire.TypeNotice, "scotmesh", "heads up", "")
	if f := b.expect(wire.TypeNotice, "heads up"); !bytes.Equal(f.Src, a.hash()) || f.Room != "scotmesh" {
		t.Errorf("room notice src %x room %q", f.Src, f.Room)
	}
	c := e.client(t)
	c.connect()
	c.hello("")
	c.sendEnv(wire.TypeNotice, "scotmesh", "from outside", "")
	c.expect(wire.TypeError, "no outside messages (+n)")

	// Topic changes reach the room; a room ban removes the member with an ERROR.
	reply, err := e.hub.Command(context.Background(), hub.CommandRequest{Via: hub.ViaRRC, Identity: operator, Text: "/topic scotmesh Radios"})
	if err != nil || reply.Error {
		t.Fatalf("topic: %+v %v", reply, err)
	}
	b.expect(wire.TypeNotice, "topic for scotmesh is now: Radios")
	reply, err = e.hub.Command(context.Background(), hub.CommandRequest{Via: hub.ViaRRC, Identity: operator, Text: "/room ban #scotmesh Rab"})
	if err != nil || reply.Text() != "ban added in scotmesh" {
		t.Fatalf("ban: %+v %v", reply, err)
	}
	if f := b.expect(wire.TypeError, "banned from scotmesh"); f.Room != "scotmesh" {
		t.Errorf("room ban ERROR room %q", f.Room)
	}
	a.expect(wire.TypeParted, "")
	a.sendEnv(wire.TypeMsg, "scotmesh", "after the ban", "")
	a.expect(wire.TypeMsg, "after the ban")
	b.quiet(wire.TypeMsg)

	// An invitation is a roomless notice to the invitee.
	reply, err = e.hub.Command(context.Background(), hub.CommandRequest{Via: hub.ViaRRC, Identity: operator, Text: "/room invite #scotmesh " + hex.EncodeToString(c.hash())})
	if err != nil || reply.Error {
		t.Fatalf("invite: %+v %v", reply, err)
	}
	c.expect(wire.TypeNotice, "You have been invited to join scotmesh.")
}

func TestPartAndReHello(t *testing.T) {
	e := newEnv(t)
	a, b := e.client(t), e.client(t)
	a.connect()
	a.hello("Alex")
	b.connect()
	b.hello("Rab")
	for _, c := range []*client{a, b} {
		c.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
		c.expect(wire.TypeJoined, "")
	}
	a.sendEnv(wire.TypePart, "", nil, "")
	a.expect(wire.TypeError, "PART requires room name")
	a.sendEnv(wire.TypePart, "bad room!", nil, "")
	a.expect(wire.TypeError, "room name may only use")
	a.sendEnv(wire.TypePart, "scotmesh", nil, "")
	parted := a.expect(wire.TypeParted, "")
	var ids [][]byte
	if err := parted.DecodeBody(&ids); err != nil || len(ids) != 1 || !bytes.Equal(ids[0], a.hash()) {
		t.Errorf("PARTED to the parter: %v %v", ids, err)
	}
	if f := b.expect(wire.TypeParted, ""); f.Nick != "Alex" {
		t.Errorf("PARTED fan-out nick %q", f.Nick)
	}

	// A re-HELLO leaves rooms properly (rrcd left others thinking you stayed).
	a.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	a.expect(wire.TypeJoined, "")
	b.expect(wire.TypeJoined, "")
	a.hello("Alex")
	b.expect(wire.TypeParted, "")
	b.sendEnv(wire.TypeMsg, "scotmesh", "still here?", "")
	b.expect(wire.TypeMsg, "still here?")
	a.quiet(wire.TypeMsg) // the re-HELLOed link is no longer in the room

	if err := e.srv.Run(context.Background()); err == nil {
		t.Error("Run twice did not fail")
	}
	if e.srv.Stats().LinksAccepted.Load() != 2 {
		t.Errorf("links accepted %d", e.srv.Stats().LinksAccepted.Load())
	}
}

func TestIncludeJoinedMemberList(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.IncludeJoinedMemberList = true })
	a, b := e.client(t), e.client(t)
	a.connect()
	a.hello("Alex")
	b.connect()
	b.hello("Rab")
	a.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	a.expect(wire.TypeJoined, "")
	b.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
	j := b.expect(wire.TypeJoined, "")
	var ids [][]byte
	if err := j.DecodeBody(&ids); err != nil || len(ids) != 2 {
		t.Errorf("JOINED member list %x %v", ids, err)
	}
}

// A message said while a JOIN is being committed reaches the joiner exactly
// once: in the replay or live, never both.
func TestMessagesDuringJoinArriveOnce(t *testing.T) {
	for round := range 20 {
		e := newEnv(t)
		a, b := e.client(t), e.client(t)
		a.connect()
		a.hello("Alex")
		b.connect()
		b.hello("Rab")
		b.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
		b.expect(wire.TypeJoined, "")
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := range 10 {
				b.sendEnv(wire.TypeMsg, "scotmesh", fmt.Sprintf("r%d m%d", round, i), "Rab")
			}
		}()
		a.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
		<-done
		b.expect(wire.TypeMsg, fmt.Sprintf("r%d m9", round))
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			n := 0
			for _, f := range a.all() {
				if body, _ := f.BodyString(); f.Type == wire.TypeMsg && strings.HasPrefix(body, fmt.Sprintf("r%d ", round)) {
					n++
				}
			}
			if n >= 10 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		seen := map[string]int{}
		for _, f := range a.all() {
			if body, _ := f.BodyString(); f.Type == wire.TypeMsg {
				seen[body]++
			}
		}
		for i := range 10 {
			if n := seen[fmt.Sprintf("r%d m%d", round, i)]; n != 1 {
				t.Fatalf("round %d: message %d reached the joiner %d times", round, i, n)
			}
		}
	}
}

// Whispers reach only the recipient's links, as a direct notice from the
// sender, now or after their next HELLO.
func TestWhispersAreDirectNotices(t *testing.T) {
	e := newEnv(t)
	a, b, onlooker := e.client(t), e.client(t), e.client(t)
	for _, c := range []struct {
		c    *client
		nick string
	}{{a, "Alex"}, {b, "Rab"}, {onlooker, "Ellen"}} {
		c.c.connect()
		c.c.hello(c.nick)
		c.c.sendEnv(wire.TypeJoin, "scotmesh", nil, "")
		c.c.expect(wire.TypeJoined, "")
	}
	a.sendEnv(wire.TypeMsg, "scotmesh", "!whisper Rab meet at the mast?", "Alex")
	f := b.expect(wire.TypeNotice, "(whisper) meet at the mast?")
	if !bytes.Equal(f.Src, a.hash()) || f.Nick != "Alex" || !bytes.Equal(f.Dst, b.hash()) || f.Room != "" {
		t.Errorf("whisper frame src=%x nick=%q dst=%x room=%q", f.Src, f.Nick, f.Dst, f.Room)
	}
	a.expect(wire.TypeNotice, "Whispered to Rab (on RRC now).")
	time.Sleep(100 * time.Millisecond)
	for _, f := range onlooker.all() {
		if body, _ := f.BodyString(); strings.Contains(body, "mast") {
			t.Errorf("someone else saw the whisper: %s %q", f.Type, body)
		}
	}

	// Someone who isn't connected gets it after HELLO, once.
	later := e.client(t)
	if _, err := e.hub.Identify(context.Background(), later.hash(), later.id.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hub.ClaimName(context.Background(), later.hash(), "Morag"); err != nil {
		t.Fatal(err)
	}
	a.sendEnv(wire.TypeMsg, "scotmesh", "/w Morag call me", "Alex")
	a.expect(wire.TypeNotice, "Whispered to Morag. They'll see it when they're next on.")
	later.connect()
	later.hello("Morag")
	later.expect(wire.TypeNotice, "(whisper) call me")
	second := &client{e: e, link: bytes.Repeat([]byte{0x77}, 16), id: later.id}
	second.connect()
	second.hello("Morag")
	time.Sleep(100 * time.Millisecond)
	for _, f := range second.all() {
		if body, _ := f.BodyString(); strings.Contains(body, "call me") {
			t.Errorf("a whisper sent once came again: %q", body)
		}
	}
}
