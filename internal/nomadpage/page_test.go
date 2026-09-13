package nomadpage

import (
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

var update = flag.Bool("update", false, "rewrite golden pages in testdata")

var (
	alex  = bytes.Repeat([]byte{0xa1}, 16)
	rab   = bytes.Repeat([]byte{0xb2}, 16)
	ellen = bytes.Repeat([]byte{0xe3}, 16)
	owner = bytes.Repeat([]byte{0x0f}, 16) // in the config's admins
)

type env struct {
	t   *testing.T
	hub *hub.Hub
	p   *Page
	now time.Time
	mu  sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, now: time.Date(2026, 9, 12, 18, 40, 0, 0, time.UTC)}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	hcfg := hub.Defaults()
	hcfg.Admins = [][]byte{owner}
	h, err := hub.New(context.Background(), hcfg, st, hub.WithLogger(quiet), hub.WithClock(e.clock))
	if err != nil {
		t.Fatal(err)
	}
	e.hub = h
	e.p = New(Config{GroupAddress: "625ad393ca4df62a1382f9d936322a0a", HubAddress: "004e5a15da221d9a236b0e8d74195f13", WikiURL: "10a839df2c50635bcb0c8a299b9ca0f8:/page/wiki/The_chat_room.mu"}, h, WithLogger(quiet), WithClock(e.clock))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})
	return e
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) tick(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

var bg = context.Background()

func (e *env) get(path string, id []byte, fields map[string]string) string {
	e.t.Helper()
	if fields == nil {
		fields = map[string]string{}
	}
	e.tick(time.Second)
	return string(e.p.Serve(bg, Request{Path: path, Identity: id, Fields: fields, RequestID: []byte(e.clock().String() + fields["message"])}))
}

func (e *env) say(via hub.Via, id []byte, body string) store.Message {
	e.t.Helper()
	e.tick(time.Minute)
	res, err := e.hub.Post(bg, hub.PostRequest{Via: via, Identity: id, Body: body})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Message
}

// actionTokenPlaceholder stands in for the action token in golden files: the
// real token is random per session, so it can never be fixed in a fixture.
const actionTokenPlaceholder = "0123456789abcdef0123456789abcdef"

var actionTokenRE = regexp.MustCompile(`tok=[0-9a-f]{32}`)

func golden(t *testing.T, name, got string) {
	t.Helper()
	got = actionTokenRE.ReplaceAllString(got, "tok="+actionTokenPlaceholder)
	path := filepath.Join("testdata", name+".mu")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s:\n--- got ---\n%s\n--- want ---\n%s", name, path, got, want)
	}
}

func TestAnonymousVisitorReadsButCannotPost(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.hub.JoinGroup(bg, rab, "Rab"); err != nil {
		t.Fatal(err)
	}
	e.say(hub.ViaLXMF, rab, "anyone near Stirling on 868?")
	e.say(hub.ViaLXMF, rab, "`B900injected`b and\n> a heading")
	page := e.get(PathIndex, nil, nil)
	golden(t, "anonymous", page)
	posted := e.get(PathIndex, nil, map[string]string{"action": "send", "message": "hi"})
	if !strings.Contains(posted, "Identify to post:") {
		t.Errorf("anonymous post not refused:\n%s", posted)
	}
}

func TestIdentifiedVisitorPostsAndSeesTheMarker(t *testing.T) {
	e := newEnv(t)
	if _, err := e.hub.ClaimName(bg, alex, "Alex"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.hub.JoinGroup(bg, rab, "Rab"); err != nil {
		t.Fatal(err)
	}
	e.get(PathIndex, alex, map[string]string{"action": "view", "lines": "30", "flag": "1", "refresh": "0"}) // the conversation inline
	e.say(hub.ViaLXMF, rab, "first")
	e.get(PathIndex, alex, nil) // Alex reads up to "first"
	e.say(hub.ViaLXMF, rab, "second")
	page := e.indexAction(alex, map[string]string{"action": "send", "message": "/me waves"})
	golden(t, "identified", page)
	if !strings.Contains(page, "── new since your last visit") {
		t.Error("no marker")
	}
	again := e.get(PathIndex, alex, nil)
	if strings.Contains(again, "── new since your last visit") {
		t.Error("marker shown with nothing new")
	}
	// A stale or forged token is refused, and doesn't reach the hub.
	before, err := e.hub.Recent(bg, "scotmesh", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	noTok := e.get(PathIndex, alex, map[string]string{"action": "send", "message": "should not post"})
	if !strings.Contains(noTok, "That link has expired") {
		t.Errorf("send with no token should be refused:\n%s", noTok)
	}
	after, err := e.hub.Recent(bg, "scotmesh", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("a post with no token still landed: %d -> %d messages", len(before), len(after))
	}
	// Commands run for the visitor, with a reply only they see, and are
	// never said.
	cmd := e.indexAction(alex, map[string]string{"action": "send", "message": "/nick Sandy"})
	if !strings.Contains(cmd, "Only you see this:") || !strings.Contains(cmd, "You are now Sandy. Alex is free for anyone.") {
		t.Errorf("command post:\n%s", cmd)
	}
	cmd = e.indexAction(alex, map[string]string{"action": "send", "message": "!help"})
	if !strings.Contains(cmd, "· chatting") || !strings.Contains(cmd, "Help 1/") {
		t.Errorf("!help on the page:\n%s", cmd)
	}
	cmd = e.indexAction(alex, map[string]string{"action": "send", "message": "/frob"})
	if !strings.Contains(cmd, "`Ffb3  There's no /frob.") {
		t.Errorf("unknown command on the page:\n%s", cmd)
	}
	e.tick(time.Minute)
	cmd = e.indexAction(alex, map[string]string{"action": "send", "message": "/history 1"})
	if !strings.Contains(cmd, "— the last 1 message in #scotmesh —") {
		t.Errorf("/history on the page:\n%s", cmd)
	}
	recent, err := e.hub.Recent(bg, "scotmesh", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range recent {
		if strings.HasPrefix(m.Body, "/") || strings.HasPrefix(m.Body, "!") {
			t.Errorf("a command was said: %q", m.Body)
		}
	}
	older := e.get(PathOlder, alex, map[string]string{"before": "2"})
	if !strings.Contains(older, "— older messages —") || strings.Contains(older, "second") || !strings.Contains(older, "first") {
		t.Errorf("older page:\n%s", older)
	}
}

func TestNameAndSettingsPage(t *testing.T) {
	e := newEnv(t)
	if _, err := e.hub.ClaimName(bg, rab, "Rab"); err != nil {
		t.Fatal(err)
	}
	anon := e.get(PathMe, nil, nil)
	if !strings.Contains(anon, "Identify to this node first") {
		t.Errorf("settings page for an anonymous visitor:\n%s", anon)
	}

	page := e.meAction(ellen, map[string]string{"action": "nick", "nick": "R.a.b"})
	if !strings.Contains(page, "\"R.a.b\" is taken by someone else. You are shown as guest-e3e3") {
		t.Errorf("lookalike claim:\n%s", page)
	}
	// A stale or forged token is refused, action and all.
	page = e.get(PathMe, ellen, map[string]string{"action": "nick", "nick": "Ellen"})
	if !strings.Contains(page, "That link has expired") || strings.Contains(page, "You are now Ellen") {
		t.Errorf("nick with no token should be refused:\n%s", page)
	}
	page = e.meAction(ellen, map[string]string{"action": "nick", "nick": "Ellen"})
	golden(t, "me-not-member", page)

	if _, _, err := e.hub.JoinGroup(bg, ellen, ""); err != nil {
		t.Fatal(err)
	}
	page = e.meAction(ellen, map[string]string{"action": "lxmf", "mode": "auto"})
	if !strings.Contains(page, "LXMF delivery is now auto.") || !strings.Contains(page, "`F5d8●`f `F5af`_`[Auto:") {
		t.Errorf("lxmf auto:\n%s", page)
	}
	page = e.get(PathMe, ellen, map[string]string{"action": "leave_confirm"})
	if !strings.Contains(page, "Yes, leave the group") {
		t.Errorf("leave confirm:\n%s", page)
	}
	page = e.meAction(ellen, map[string]string{"action": "leave"})
	if !strings.Contains(page, "You have left the LXMF group. Your name stays yours.") || strings.Contains(page, "Leave the LXMF group…") {
		t.Errorf("leave:\n%s", page)
	}
	if p, err := e.hub.PersonOf(bg, ellen); err != nil || !p.Claimed || p.Member {
		t.Errorf("after leaving the group %+v %v", p, err)
	}
	page = e.meAction(ellen, map[string]string{"action": "leave"})
	if !strings.Contains(page, "You weren't in the LXMF group.") {
		t.Errorf("leave again:\n%s", page)
	}
	page = e.get(PathMe, ellen, map[string]string{"action": "forget_confirm"})
	if !strings.Contains(page, "Yes, forget me") {
		t.Errorf("forget confirm:\n%s", page)
	}
	page = e.meAction(ellen, map[string]string{"action": "forget"})
	if !strings.Contains(page, "Ellen is free again.") {
		t.Errorf("forget:\n%s", page)
	}
	p, err := e.hub.PersonOf(bg, ellen)
	if err != nil || p.Claimed || p.Member {
		t.Errorf("after forget %+v %v", p, err)
	}
	page = e.meAction(ellen, map[string]string{"action": "forget"})
	if !strings.Contains(page, "There was nothing to forget.") {
		t.Errorf("forget again:\n%s", page)
	}
}

func TestHelpPage(t *testing.T) {
	e := newEnv(t)
	golden(t, "help", e.get(PathHelp, nil, nil))
}

func TestBannedVisitor(t *testing.T) {
	oper := bytes.Repeat([]byte{0x0f}, 16)
	st, err := store.Open(bg, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	hcfg := hub.Defaults()
	hcfg.Admins = [][]byte{oper}
	h, err := hub.New(bg, hcfg, st, hub.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	defer func() { cancel(); <-done }()
	if _, err := h.Identify(bg, rab, nil); err != nil {
		t.Fatal(err)
	}
	if r, err := h.Command(bg, hub.CommandRequest{Via: hub.ViaRRC, Identity: oper, Text: "/kline add " + strings.Repeat("b2", 16)}); err != nil || r.Error {
		t.Fatalf("%+v %v", r, err)
	}
	p := New(Config{}, h)
	page := string(p.Serve(bg, Request{Path: PathIndex, Identity: rab, Fields: map[string]string{"action": "send", "message": "spam"}}))
	if !strings.Contains(page, "you are banned from this hub") || !strings.Contains(page, "To post, identify yourself") {
		t.Errorf("banned visitor:\n%s", page)
	}
}

func TestFieldsFromNomadNetRequestData(t *testing.T) {
	got := fields(map[any]any{"field_message": []byte("hi"), "var_action": "send", "other": "x", 7: "y"})
	if got["message"] != "hi" || got["action"] != "send" || len(got) != 2 {
		t.Errorf("fields %v", got)
	}
	if len(fields("not a map")) != 0 {
		t.Error("non-map data")
	}
}

func FuzzEsc(f *testing.F) {
	for _, s := range []string{"hi", "`B900red`b", "> heading", "-", "a\\`b", "#!c=0", "line\n<back"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := esc(s)
		// No unescaped backtick survives, and no line starts a Micron block.
		for i := 0; i < len(out); i++ {
			if out[i] == '`' {
				bs := 0
				for j := i - 1; j >= 0 && out[j] == '\\'; j-- {
					bs++
				}
				if bs%2 == 0 {
					t.Fatalf("unescaped backtick in %q -> %q", s, out)
				}
			}
		}
	})
}

// After posting from the page, your own message must not sit under
// "new since your last visit".
func TestYourOwnMessagesAreNotNew(t *testing.T) {
	e := newEnv(t)
	if _, err := e.hub.ClaimName(bg, alex, "Alex"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.hub.JoinGroup(bg, rab, "Rab"); err != nil {
		t.Fatal(err)
	}
	e.get(PathIndex, alex, map[string]string{"action": "view", "lines": "30", "flag": "1", "refresh": "0"})
	e.say(hub.ViaLXMF, rab, "hello")
	e.get(PathIndex, alex, nil)
	e.say(hub.ViaRRC, alex, "from my other app")
	page := e.indexAction(alex, map[string]string{"action": "send", "message": "test"})
	if strings.Contains(page, "new since your last visit") {
		t.Errorf("own messages marked as new:\n%s", page)
	}
	e.say(hub.ViaLXMF, rab, "reply")
	e.say(hub.ViaRRC, alex, "and mine after it")
	page = e.get(PathIndex, alex, nil)
	marker := strings.Index(page, "new since your last visit")
	if marker < 0 || strings.Index(page, "reply") < marker || strings.Index(page, "`F5d8Alex`f  test") > marker {
		t.Errorf("marker should sit just above Rab's reply:\n%s", page)
	}
}

func TestLXMFMineToggleOnThePage(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.hub.JoinGroup(bg, alex, "Alex"); err != nil {
		t.Fatal(err)
	}
	// Joining turns own messages on; the toggle turns them off and on.
	page := e.get(PathMe, alex, nil)
	if !strings.Contains(page, "`[Stop`") {
		t.Errorf("no toggle:\n%s", page)
	}
	page = e.meAction(alex, map[string]string{"action": "mine", "on": "0"})
	if !strings.Contains(page, "Send them to me too") {
		t.Errorf("toggle off:\n%s", page)
	}
	page = e.meAction(alex, map[string]string{"action": "mine", "on": "1"})
	if !strings.Contains(page, "will come to you by LXMF too") || !strings.Contains(page, "`[Stop`") {
		t.Errorf("toggle on:\n%s", page)
	}
	page = e.meAction(alex, map[string]string{"action": "mine", "on": "0"})
	if !strings.Contains(page, "won't be sent to you by LXMF any more") {
		t.Errorf("toggle off:\n%s", page)
	}
}

// The page offers every personal setting the commands do: name, LXMF
// delivery, own messages by LXMF, and leaving.
func TestSettingsPageOffersEverySetting(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.hub.JoinGroup(bg, alex, "Alex"); err != nil {
		t.Fatal(err)
	}
	page := e.get(PathMe, alex, nil)
	for _, want := range []string{"action=nick", "action=lxmf|mode=on", "action=lxmf|mode=auto", "action=lxmf|mode=off", "action=mine|on=0", "action=share|on=0", "action=link", "action=leave_confirm", "action=forget_confirm", "Your identity:"} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
}

func TestShareToggleOnThePage(t *testing.T) {
	e := newEnv(t)
	page := e.meAction(alex, map[string]string{"action": "share", "on": "0"})
	if !strings.Contains(page, "hidden from your profile now") || !strings.Contains(page, "action=share|on=1") {
		t.Errorf("hide:\n%s", page)
	}
	page = e.meAction(alex, map[string]string{"action": "share", "on": "1"})
	if !strings.Contains(page, "shown on your profile now") || !strings.Contains(page, "action=share|on=0") {
		t.Errorf("show:\n%s", page)
	}
}

// Linking another app from the settings page: a code on one app, entered on
// the other, then both listed with Unlink links; people with a role approve
// new apps there too.
func TestLinkAnotherAppOnThePage(t *testing.T) {
	e := newEnv(t)
	if _, err := e.hub.ClaimName(bg, alex, "Alex"); err != nil {
		t.Fatal(err)
	}
	page := e.meAction(alex, map[string]string{"action": "link"})
	m := regexp.MustCompile(`send /link ([A-Z2-7]{4}-[A-Z2-7]{4})`).FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, "Only you see this:") {
		t.Fatalf("no code:\n%s", page)
	}
	page = e.meAction(rab, map[string]string{"action": "redeem", "code": m[1]})
	if !strings.Contains(page, "This app is now Alex.") || !strings.Contains(page, "`[Unlink`:/page/me.mu`action=unlink|app=b2b2b2b2") {
		t.Errorf("redeemed:\n%s", page)
	}
	page = e.get(PathMe, alex, nil)
	if !strings.Contains(page, "`!a1a1a1a1`!  this app") || !strings.Contains(page, "`!b2b2b2b2`!") {
		t.Errorf("apps listed:\n%s", page)
	}
	page = e.meAction(alex, map[string]string{"action": "unlink", "app": "b2b2b2b2"})
	if !strings.Contains(page, "Unlinked b2b2b2b2 from Alex.") || strings.Contains(page, "`!b2b2b2b2`!") {
		t.Errorf("unlinked:\n%s", page)
	}
	if got := linkCommand(map[string]string{"action": "redeem", "code": "AB", "confirm": "1"}); got != "/link AB confirm" {
		t.Errorf("confirm form %q", got)
	}
	for _, f := range []map[string]string{{"action": "redeem"}, {"action": "approve"}, {"action": "nick"}} {
		if got := linkCommand(f); got != "" {
			t.Errorf("%v -> %q", f, got)
		}
	}
	if got := linkCommand(map[string]string{"action": "deny", "app": "5252"}); got != "/link deny 5252" {
		t.Errorf("deny %q", got)
	}
}

func TestWhispersOnTheSettingsPage(t *testing.T) {
	e := newEnv(t)
	for id, name := range map[string]string{string(alex): "Alex", string(rab): "Rab"} {
		if _, err := e.hub.ClaimName(bg, []byte(id), name); err != nil {
			t.Fatal(err)
		}
	}
	page := e.indexAction(alex, map[string]string{"action": "send", "message": "/whisper Rab see you at the mast"})
	if !strings.Contains(page, "Whispered to Rab. They'll see it when they're next on.") {
		t.Fatalf("whisper from the page:\n%s", page)
	}
	page = e.get(PathMe, rab, nil)
	if !strings.Contains(page, "`Fe93── new ──`f") || !strings.Contains(page, "`!Alex`! whispers: see you at the mast") {
		t.Errorf("inbox:\n%s", page)
	}
	page = e.get(PathMe, rab, nil)
	if strings.Contains(page, "── new ──") {
		t.Errorf("still new on the second look:\n%s", page)
	}
	page = e.meAction(rab, map[string]string{"action": "whispers", "set": "lxmf-off"})
	if !strings.Contains(page, "won't come to your LXMF app") || !strings.Contains(page, "action=whispers|set=lxmf-on") {
		t.Errorf("lxmf toggle:\n%s", page)
	}
	page = e.meAction(rab, map[string]string{"action": "whispers", "set": "off"})
	if !strings.Contains(page, "Whispers are off") || !strings.Contains(page, "action=whispers|set=on") {
		t.Errorf("toggle off:\n%s", page)
	}
	e.indexAction(rab, map[string]string{"action": "send", "message": "/ignore Alex"})
	page = e.get(PathMe, rab, nil)
	if !strings.Contains(page, "Ignoring `!Alex`!") || !strings.Contains(page, "action=unignore|app="+hex.EncodeToString(alex)) {
		t.Errorf("ignored list:\n%s", page)
	}
	page = e.meAction(rab, map[string]string{"action": "unignore", "app": hex.EncodeToString(alex)})
	if !strings.Contains(page, "No longer ignoring Alex.") {
		t.Errorf("unignore:\n%s", page)
	}
	if got := linkCommand(map[string]string{"action": "whispers", "set": "sideways"}); got != "" {
		t.Errorf("bad whisper setting became %q", got)
	}
	if got := linkCommand(map[string]string{"action": "unignore"}); got != "" {
		t.Errorf("unignore nobody became %q", got)
	}
}

// The conversation refreshes as a partial. Its request doesn't identify, so
// a token from the identified page says who is looking; the marker stays
// where the full page load put it.
func TestTheConversationRefreshesAsAPartial(t *testing.T) {
	e := newEnv(t)
	if _, err := e.hub.ClaimName(bg, alex, "Alex"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.hub.JoinGroup(bg, rab, "Rab"); err != nil {
		t.Fatal(err)
	}
	e.say(hub.ViaLXMF, rab, "before")
	e.get(PathIndex, alex, nil)
	e.say(hub.ViaLXMF, rab, "after")
	page := e.get(PathIndex, alex, nil)
	m := regexp.MustCompile("`\\{:/page/messages.mu`30`pid=chat\\|lines=30\\|m=(\\d+)\\|t=([0-9a-f]{32})\\}").FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, "`[Refresh now`p:chat]") || strings.Contains(page, "after") {
		t.Fatalf("no partial:\n%s", page)
	}
	e.say(hub.ViaRRC, alex, "mine")
	part := e.get(PathMessages, nil, map[string]string{"lines": "30", "m": m[1], "t": m[2]})
	if !strings.Contains(part, "── new since your last visit") || !strings.Contains(part, "`F5d8Alex`f  mine") || !strings.Contains(part, "action=delete_confirm") {
		t.Errorf("partial for Alex:\n%s", part)
	}
	if strings.Contains(part, "#!c=") || strings.Contains(part, "Say:") {
		t.Errorf("the partial carries more than the conversation:\n%s", part)
	}
	// Without a good token it's the anonymous view.
	anon := e.get(PathMessages, nil, map[string]string{"lines": "10", "t": "nope"})
	if strings.Contains(anon, "F5d8Alex") || strings.Contains(anon, "delete_confirm") || !strings.Contains(anon, "`[Alex`:/page/profile.mu`id="+hex.EncodeToString(alex)+"|lines=10|flag=1|refresh=30]") {
		t.Errorf("anonymous partial:\n%s", anon)
	}
	// The token lasts two hours.
	e.tick(3 * time.Hour)
	if e.p.sessions.identity(m[2]) != nil {
		t.Error("a token outlived its two hours")
	}
}

// newEnvBanner is newEnv with the page's banner overridden, for the banner
// tests below. nil means the built-in default (the Saltire); an empty,
// non-nil slice means no banner.
func newEnvBanner(t *testing.T, banner []string) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, now: time.Date(2026, 9, 12, 18, 40, 0, 0, time.UTC)}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	hcfg := hub.Defaults()
	hcfg.Admins = [][]byte{owner}
	h, err := hub.New(context.Background(), hcfg, st, hub.WithLogger(quiet), hub.WithClock(e.clock))
	if err != nil {
		t.Fatal(err)
	}
	e.hub = h
	e.p = New(Config{GroupAddress: "625ad393ca4df62a1382f9d936322a0a", HubAddress: "004e5a15da221d9a236b0e8d74195f13", Banner: banner}, h, WithLogger(quiet), WithClock(e.clock))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})
	return e
}

func TestCustomBanner(t *testing.T) {
	custom := []string{"`B234`Fdef  HIGHLAND MESH  `f`b"}
	e := newEnvBanner(t, custom)
	page := e.get(PathIndex, nil, map[string]string{"lines": "10", "flag": "1", "refresh": "0"})
	if !strings.Contains(page, custom[0]) || strings.Contains(page, "████") {
		t.Errorf("custom banner not shown in place of the Saltire:\n%s", page)
	}
	// Hiding it still works, the same as the built-in banner.
	hidden := e.get(PathIndex, nil, map[string]string{"lines": "10", "flag": "0", "refresh": "0"})
	if strings.Contains(hidden, custom[0]) || !strings.Contains(hidden, "`[show`:/page/index.mu`action=view|lines=10|flag=1|refresh=0]") {
		t.Errorf("custom banner didn't hide:\n%s", hidden)
	}
}

func TestNoBanner(t *testing.T) {
	e := newEnvBanner(t, []string{})
	page := e.get(PathIndex, nil, map[string]string{"lines": "10", "refresh": "0"})
	if strings.Contains(page, "████") || strings.Contains(page, "`F9abFlag:`f") {
		t.Errorf("a banner toggle was shown with no banner configured:\n%s", page)
	}
	me := e.get(PathMe, alex, nil)
	if strings.Contains(me, "Flag:") {
		t.Errorf("settings page still mentions the banner with none configured:\n%s", me)
	}
}

func TestViewButtonsAndChoices(t *testing.T) {
	e := newEnv(t)
	// Anonymous choices ride in the links.
	page := e.get(PathIndex, nil, map[string]string{"lines": "10", "flag": "0", "refresh": "0"})
	if strings.Contains(page, "████") || !strings.Contains(page, "`!`F2c8[10]`f`!") || !strings.Contains(page, "`[Reload`:/page/index.mu`lines=10|flag=0|refresh=0]") || !strings.Contains(page, "identify to keep them") {
		t.Errorf("anonymous view:\n%s", page)
	}
	if !strings.Contains(page, "`[About the chat (wiki)`10a839df2c50635bcb0c8a299b9ca0f8:/page/wiki/The_chat_room.mu]") {
		t.Errorf("no wiki link:\n%s", page)
	}
	// Identified choices are saved, and /page sees them.
	page = e.get(PathIndex, alex, map[string]string{"action": "view", "lines": "50", "flag": "0", "refresh": "60"})
	if strings.Contains(page, "████") || !strings.Contains(page, "`!`F2c8[50]`f`!") || !strings.Contains(page, "`!`F2c8[60s]`f`!") || !strings.Contains(page, "`[show`:/page/index.mu`action=view|lines=50|flag=1|refresh=60]") {
		t.Errorf("identified view:\n%s", page)
	}
	r, err := e.hub.Command(bg, hub.CommandRequest{Via: hub.ViaRRC, Identity: alex, Text: "/page"})
	if err != nil || r.Text() != "Chat page: 50 messages, flag hidden, refresh every 60s. Change it with /page lines 10|20|30|50|100, /page flag on|off or /page refresh off|10|30|60." {
		t.Errorf("/page %q %v", r.Text(), err)
	}
	page = e.get(PathMe, alex, nil)
	if !strings.Contains(page, "Messages shown: 50  ·  Flag: hidden  ·  Refresh: every 60s") {
		t.Errorf("settings page view:\n%s", page)
	}
	// Choices outside the allowed ones are ignored.
	page = e.get(PathIndex, nil, map[string]string{"lines": "7", "refresh": "5"})
	if !strings.Contains(page, "`!`F2c8[30]`f`!") || !strings.Contains(page, "`!`F2c8[30s]`f`!") {
		t.Errorf("bad choices:\n%s", page)
	}
}

func TestDeleteFromThePage(t *testing.T) {
	e := newEnv(t)
	if _, err := e.hub.ClaimName(bg, alex, "Alex"); err != nil {
		t.Fatal(err)
	}
	e.get(PathIndex, alex, map[string]string{"action": "view", "lines": "30", "flag": "1", "refresh": "0"})
	msg := e.say(hub.ViaRRC, alex, "oops, wrong room")
	id := strconv.FormatInt(msg.ID, 10)
	page := e.get(PathIndex, alex, map[string]string{"action": "delete_confirm", "msg": id})
	if !strings.Contains(page, "Remove this message from Alex? \"oops, wrong room\"") || !strings.Contains(page, "action=delete|msg="+id) {
		t.Fatalf("confirm:\n%s", page)
	}
	page = e.indexAction(alex, map[string]string{"action": "delete", "msg": id})
	if !strings.Contains(page, "Removed Alex's last message") || strings.Contains(page, "oops, wrong room") {
		t.Errorf("deleted:\n%s", page)
	}
	page = e.get(PathIndex, nil, map[string]string{"action": "delete", "msg": id})
	if !strings.Contains(page, "Identify first to remove messages.") {
		t.Errorf("anonymous delete:\n%s", page)
	}
}

func TestProfilePage(t *testing.T) {
	e := newEnv(t)
	mod := bytes.Repeat([]byte{0x6d}, 16)
	for id, name := range map[string]string{string(alex): "Alex", string(rab): "Rab", string(mod): "Moira"} {
		if _, err := e.hub.ClaimName(bg, []byte(id), name); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e.hub.JoinGroup(bg, rab, ""); err != nil {
		t.Fatal(err)
	}
	e.say(hub.ViaLXMF, rab, "hello from the hills")
	rabHex := hex.EncodeToString(rab)
	addr := hex.EncodeToString(hub.LXMFAddress(rab))

	// Anyone can read a profile; the message link opens a conversation.
	page := e.get(PathProfile, nil, map[string]string{"id": rabHex})
	for _, want := range []string{">Rab", "Name held since", "`[Message on LXMF`lxmf@" + addr + "]", "hello from the hills"} {
		if !strings.Contains(page, want) {
			t.Errorf("anonymous profile lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "Whisper:") || strings.Contains(page, ">>Moderation") {
		t.Errorf("anonymous visitors get a whisper box or moderation:\n%s", page)
	}
	if page := e.get(PathProfile, nil, map[string]string{"id": "nothex"}); !strings.Contains(page, "There's nobody here by that link.") {
		t.Errorf("bad id:\n%s", page)
	}

	// A member whispers from the profile, and sees no moderation. The
	// button carries the action token this page issued alex; without it
	// (a link built anywhere else) the whisper is refused.
	page = e.get(PathProfile, alex, map[string]string{"id": rabHex})
	tok := tokenFromPage(t, page)
	page = e.get(PathProfile, alex, map[string]string{"id": rabHex, "action": "whisper", "whisper": "fancy a walk?"})
	if !strings.Contains(page, "That link has expired") {
		t.Errorf("whisper with no token should be refused:\n%s", page)
	}
	page = e.get(PathProfile, alex, map[string]string{"id": rabHex, "action": "whisper", "whisper": "fancy a walk?", "tok": tok})
	if !strings.Contains(page, "Whispered to Rab (by LXMF).") || strings.Contains(page, ">>Moderation") {
		t.Errorf("whisper from the profile:\n%s", page)
	}

	// A mod: buttons, each asking first.
	makeMod(t, e, mod)
	page = e.get(PathProfile, mod, map[string]string{"id": rabHex})
	for _, want := range []string{">>Moderation", "action=kick_confirm", "action=ban_confirm", "action=rename_confirm", "action=deletelast_confirm"} {
		if !strings.Contains(page, want) {
			t.Errorf("mod's profile view lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "action=mod_confirm") {
		t.Errorf("a mod offered to make mods:\n%s", page)
	}
	page = e.get(PathProfile, mod, map[string]string{"id": rabHex, "action": "ban_confirm"})
	if !strings.Contains(page, "Ban Rab?") || !strings.Contains(page, "`[1 day`:/page/profile.mu`reason|action=ban|len=1d|id="+rabHex+"|tok=") {
		t.Errorf("ban confirm:\n%s", page)
	}
	page = e.get(PathProfile, mod, map[string]string{"id": rabHex, "action": "rename_confirm"})
	tok = tokenFromPage(t, page)
	page = e.get(PathProfile, mod, map[string]string{"id": rabHex, "action": "rename", "newname": "Robert"})
	if !strings.Contains(page, "That link has expired") {
		t.Errorf("rename with no token should be refused:\n%s", page)
	}
	page = e.get(PathProfile, mod, map[string]string{"id": rabHex, "action": "rename", "newname": "Robert", "tok": tok})
	if !strings.Contains(page, "Renamed Rab to Robert.") || !strings.Contains(page, ">Robert") {
		t.Errorf("rename:\n%s", page)
	}
	page = e.get(PathProfile, mod, map[string]string{"id": rabHex, "action": "kick_confirm"})
	if !strings.Contains(page, "Kick Robert?") || !strings.Contains(page, "reason|action=kick|tok=") {
		t.Errorf("kick confirm:\n%s", page)
	}
	tok = tokenFromPage(t, page)
	page = e.get(PathProfile, mod, map[string]string{"id": rabHex, "action": "kick", "reason": "testing", "tok": tok})
	if !strings.Contains(page, "Kicked Robert. Their name is free.") {
		t.Errorf("kick:\n%s", page)
	}
	// Your own profile: nothing to moderate, no whisper box.
	page = e.get(PathProfile, mod, map[string]string{"id": hex.EncodeToString(mod)})
	if strings.Contains(page, "Whisper:") || strings.Contains(page, "action=kick_confirm") {
		t.Errorf("own profile:\n%s", page)
	}
	for f, want := range map[string]string{
		"action=ban|len=forever": "/ban " + rabHex + " perm",
		"action=deletelast":      "/delete " + rabHex,
		"action=admin":           "/admin " + rabHex,
		"action=whisper":         "",
		"action=rename":          "",
		"action=nothing":         "",
	} {
		fields := map[string]string{}
		for _, kv := range strings.Split(f, "|") {
			k, v, _ := strings.Cut(kv, "=")
			fields[k] = v
		}
		if got := profileCommand(fields, rabHex); got != want {
			t.Errorf("profileCommand(%s) = %q, want %q", f, got, want)
		}
	}
}

// TestLinkLabelEscaping locks in the fix for link labels: NomadNet finds a
// label's end by scanning for the next backtick or ']' with no regard for
// backslash escapes, so esc's usual escaping (meant for message bodies)
// doesn't protect a label — a name carrying either character could close
// the label early and be read as Micron formatting, or extend the link's
// own target and variables. link, fieldLink and fullLink must never emit
// either character from a label.
func TestLinkLabelEscaping(t *testing.T) {
	if got := escLabel("a`b]c"); got != "a'b)c" {
		t.Errorf("escLabel(%q) = %q", "a`b]c", got)
	}
	// A name carrying both characters must not let NomadNet's label-parsing
	// (which stops at the first backtick or ']', ignoring backslashes)
	// read the rest of the line as formatting or as more of the link.
	dangerous := "A`B900evil`f]bogus`:elsewhere"
	for _, got := range []string{
		link(dangerous, PathProfile, "id=abcd"),
		fieldLink(dangerous, PathProfile, "field", "id=abcd"),
		fullLink(dangerous, "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f:/page/x.mu"),
	} {
		if !strings.Contains(got, escLabel(dangerous)) {
			t.Errorf("label wasn't cleaned: %s", got)
		}
		if strings.Contains(got, "`B900evil`f]bogus") {
			t.Errorf("raw dangerous label characters survived: %s", got)
		}
	}
}

func TestParseBanner(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{"empty file means no banner", "", []string{}, false},
		{"trailing newline is trimmed", "one\ntwo\n", []string{"one", "two"}, false},
		{"CRLF is normalised", "one\r\ntwo\r\n", []string{"one", "two"}, false},
		{"a page directive is refused", "#!c=1\nhello", nil, true},
		{"invalid UTF-8 is refused", "\xff\xfe", nil, true},
		{"too many lines is refused", strings.Repeat("x\n", MaxBannerLines+1), nil, true},
		{"too many bytes is refused", strings.Repeat("x", MaxBannerBytes+1), nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBanner([]byte(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseBanner(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if err == nil && !slices.Equal(got, tt.want) {
				t.Errorf("ParseBanner(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

// makeMod gives an identity the mod role through the owner's command.
// tokenFromPage extracts the action token embedded in a rendered page's
// links, the way a real client following one of them would carry it.
func tokenFromPage(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`tok=([0-9a-f]{32})`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no action token in page:\n%s", page)
	}
	return m[1]
}

// actionToken is the token a real client would have picked up from any
// earlier page this Page rendered for id — fetched directly here (the test
// is in-package) so building one doesn't itself render a page and trip
// index's read-marker bookkeeping.
func (e *env) actionToken(id []byte) string { return e.p.actionToken(id) }

// meAction performs a settings-page action as a real client would: with the
// token a prior page load would have carried.
func (e *env) meAction(id []byte, fields map[string]string) string {
	e.t.Helper()
	f := map[string]string{"tok": e.actionToken(id)}
	for k, v := range fields {
		f[k] = v
	}
	return e.get(PathMe, id, f)
}

// indexAction performs a chat-page action (send, delete) as a real client
// would: with the token a prior page load would have carried. id must be
// identified: an anonymous visitor's page carries no token, since it can't
// post or delete anyway.
func (e *env) indexAction(id []byte, fields map[string]string) string {
	e.t.Helper()
	f := map[string]string{"tok": e.actionToken(id)}
	for k, v := range fields {
		f[k] = v
	}
	return e.get(PathIndex, id, f)
}

func makeMod(t *testing.T, e *env, id []byte) {
	t.Helper()
	r, err := e.hub.Command(bg, hub.CommandRequest{Via: hub.ViaRRC, Identity: owner, Text: "/mod " + hex.EncodeToString(id)})
	if err != nil || r.Error {
		t.Fatalf("/mod: %q %v", r.Text(), err)
	}
}
