// Package nomadpage is the NomadNet chat page: the group room rendered as
// Micron for NomadNet and MeshChatX, with posting for visitors who identify,
// and a page for managing your name and LXMF delivery.
//
// Requests are answered from the hub: messages come from the store, and
// posting, names and settings go through the same hub calls as RRC and LXMF,
// so the rules can't differ between ways in.
package nomadpage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// MaxBannerLines and MaxBannerBytes bound a banner file: every page load
// carries it, including over LoRa.
const (
	MaxBannerLines = 24
	MaxBannerBytes = 4096
)

//go:embed banners/saltire.mu
var saltireBanner string

// ParseBanner turns banner file content into lines for Config.Banner. The
// result is never nil: an empty (zero-length) result means "no banner",
// which is different from leaving Config.Banner as nil, which selects the
// built-in default (the Saltire).
func ParseBanner(raw []byte) ([]string, error) {
	if len(raw) > MaxBannerBytes {
		return nil, fmt.Errorf("banner: larger than %d bytes", MaxBannerBytes)
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("banner: not valid UTF-8")
	}
	text := strings.TrimRight(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	lines := []string{}
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	if len(lines) > MaxBannerLines {
		return nil, fmt.Errorf("banner: more than %d lines", MaxBannerLines)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "#!") {
			return nil, errors.New("banner: a line starts with #!, which is a page directive")
		}
	}
	return lines, nil
}

// defaultBanner is the Saltire, embedded in the binary so it's the default
// with no configuration at all.
func defaultBanner() []string {
	lines, err := ParseBanner([]byte(saltireBanner))
	if err != nil {
		panic("nomadpage: embedded default banner: " + err.Error())
	}
	return lines
}

// Paths the page serves.
const (
	PathIndex    = "/page/index.mu"
	PathOlder    = "/page/older.mu"
	PathMessages = "/page/messages.mu" // the conversation alone, for the refreshing partial
	PathProfile  = "/page/profile.mu"
	PathMe       = "/page/me.mu"
	PathHelp     = "/page/help.mu"
)

// Paths lists every path to register.
var Paths = []string{PathIndex, PathOlder, PathMessages, PathProfile, PathMe, PathHelp}

// Hub is what the page needs from the chat core.
type Hub interface {
	Identify(ctx context.Context, id, publicKey []byte) (hub.Person, error)
	PersonOf(ctx context.Context, id []byte) (hub.Person, error)
	ClaimName(ctx context.Context, id []byte, want string) (hub.Person, error)
	Forget(ctx context.Context, id []byte) (hub.Person, error)
	LeaveGroup(ctx context.Context, id []byte) (bool, error)
	Command(ctx context.Context, req hub.CommandRequest) (hub.Reply, error)
	LinkedApps(ctx context.Context, id []byte) (hub.LinkedApps, error)
	SetPrefs(ctx context.Context, id []byte, change func(*store.Prefs)) (hub.Person, error)
	Whispers(ctx context.Context, id []byte, limit int) (hub.WhisperInbox, error)
	SetLXMFMode(ctx context.Context, id []byte, mode store.LXMFMode) (hub.Person, error)
	SetLXMFMine(ctx context.Context, id []byte, on bool) (hub.Person, error)
	Post(ctx context.Context, req hub.PostRequest) (hub.PostResult, error)
	Recent(ctx context.Context, room string, n int, beforeID int64) ([]store.Message, error)
	Members(ctx context.Context, room string) ([]hub.Member, error)
	Room(ctx context.Context, name string) (store.Room, bool, error)
	MarkRead(ctx context.Context, id []byte, room string, via hub.Via, messageID int64) error
	ReadCursor(ctx context.Context, id []byte, room string, via hub.Via) (store.Cursor, bool, error)
	DeleteMessage(ctx context.Context, actor []byte, messageID int64) (hub.Reply, error)
	ProfileOf(ctx context.Context, id []byte) (hub.Profile, error)
	Permissions(ctx context.Context, viewer, target []byte) (map[hub.Action]bool, error)
}

// Config is the page's content.
type Config struct {
	Title        string // "ScotMesh Chat"
	GroupRoom    string
	HubAddress   string // RRC hub, shown on the help page
	GroupAddress string // LXMF group, shown on the help page
	// NodeAddress is this page's own destination, for the refreshing
	// partial's URL; "" makes it relative, which NomadNet understands.
	NodeAddress string
	// WikiURL is a NomadNet page about the chat, linked from the chat page
	// and the help page: the wiki mirror on the ScotMesh RNS Network node.
	WikiURL string
	// Banner is the Micron lines shown at the top of the chat page, each
	// rendered as written (an operator sets its own colours). nil selects
	// the built-in default (the Saltire); an empty, non-nil slice means no
	// banner at all. Use ParseBanner to build this from a file's content.
	Banner []string
}

// Page renders the chat page.
type Page struct {
	cfg      Config
	hub      Hub
	log      *slog.Logger
	now      func() time.Time
	sessions *sessions
}

// Option adjusts a Page.
type Option func(*Page)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(p *Page) { p.log = l } }

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option { return func(p *Page) { p.now = now } }

// New creates the page.
func New(cfg Config, h Hub, opts ...Option) *Page {
	if cfg.Title == "" {
		cfg.Title = "ScotMesh Chat"
	}
	if cfg.GroupRoom == "" {
		cfg.GroupRoom = "scotmesh"
	}
	if cfg.Banner == nil {
		cfg.Banner = defaultBanner()
	}
	p := &Page{cfg: cfg, hub: h, log: slog.Default(), now: time.Now}
	for _, o := range opts {
		o(p)
	}
	p.sessions = newSessions(p.now)
	return p
}

// Request is one page request.
type Request struct {
	Path      string
	Fields    map[string]string // field_* and var_* without their prefix
	Identity  []byte            // the visitor's identity hash if they identified
	PublicKey []byte
	RequestID []byte // stable across a client's retry of the same request
}

// Handler adapts the page to a reticulum-go request handler.
func (p *Page) Handler() rns.RequestHandler {
	return func(rc *rns.RequestContext) (any, error) {
		req := Request{Path: rc.Path, Fields: fields(rc.Data)}
		if rc.RemoteIdentity != nil {
			req.PublicKey = rc.RemoteIdentity
			req.Identity = rns.IdentityHashFromPublicKey(rc.RemoteIdentity)
		}
		sum := sha256.Sum256(fmt.Appendf(nil, "%x|%d|%s|%s", rc.LinkID, rc.Timestamp.UnixNano(), req.Fields["action"], req.Fields["message"]))
		req.RequestID = sum[:16]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return p.Serve(ctx, req), nil
	}
}

func fields(data any) map[string]string {
	out := map[string]string{}
	m, ok := data.(map[any]any)
	if !ok {
		return out
	}
	for k, v := range m {
		ks, _ := k.(string)
		var vs string
		switch x := v.(type) {
		case string:
			vs = x
		case []byte:
			vs = string(x)
		}
		if after, ok := strings.CutPrefix(ks, "field_"); ok {
			out[after] = vs
		} else if after, ok := strings.CutPrefix(ks, "var_"); ok {
			out[after] = vs
		}
	}
	return out
}

// Serve answers one request with a Micron page.
func (p *Page) Serve(ctx context.Context, r Request) []byte {
	var person *hub.Person
	var refusal string
	if r.Identity != nil {
		pp, err := p.hub.Identify(ctx, r.Identity, r.PublicKey)
		if err != nil {
			refusal = p.userText(err)
		} else {
			person = &pp
		}
	}
	var out string
	switch r.Path {
	case PathMe:
		out = p.me(ctx, r, person, refusal)
	case PathHelp:
		out = p.help(p.viewFor(r, person))
	case PathMessages:
		out = p.messages(ctx, r)
	case PathProfile:
		out = p.profile(ctx, r, person, refusal)
	default:
		out = p.index(ctx, r, person, refusal)
	}
	return []byte(out)
}

// sessions let the refreshing conversation know who is looking. NomadNet
// loads a partial over a link of its own that doesn't identify, so the
// identified page hands the partial a random token for its visitor, good for
// two hours. It only changes how the conversation is shown (your own name,
// delete links); anything that acts goes through an identified request.
type sessions struct {
	mu      sync.Mutex
	now     func() time.Time
	byToken map[string]session
	byID    map[string]string
}

type session struct {
	identity []byte
	expires  time.Time
}

const sessionTTL = 2 * time.Hour

func newSessions(now func() time.Time) *sessions {
	return &sessions{now: now, byToken: map[string]session{}, byID: map[string]string{}}
}

// issue returns the visitor's token, renewing it.
func (s *sessions) issue(identity []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for tok, e := range s.byToken {
		if now.After(e.expires) {
			delete(s.byToken, tok)
			delete(s.byID, string(e.identity))
		}
	}
	tok, ok := s.byID[string(identity)]
	if !ok {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return ""
		}
		tok = hex.EncodeToString(b[:])
		s.byID[string(identity)] = tok
	}
	s.byToken[tok] = session{identity: identity, expires: now.Add(sessionTTL)}
	return tok
}

// identity is the visitor a token belongs to, or nil.
func (s *sessions) identity(token string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byToken[token]
	if !ok || s.now().After(e.expires) {
		return nil
	}
	return e.identity
}

// actionTokenField is the link/form field carrying the action token below.
const actionTokenField = "tok"

// actionToken is embedded in every link or form that performs a mutating
// action (kick, ban, rename, redeem a link code, forget your name, and so
// on), so following it proves the request came from a page this Page
// rendered for the identity making it, moments before. A link built
// anywhere else — another node's page, a message, a pasted URL — can't
// know it, so it can't drive an action just because a client auto-
// identifies when it connects; the visitor has to have loaded the page
// themselves first. It reuses sessions, which already exists to let the
// visitor be identified on the refreshing partial's roomless requests.
func (p *Page) actionToken(identity []byte) string {
	return p.sessions.issue(identity)
}

// validAction reports whether r carries the action token issued to
// identity.
func (p *Page) validAction(r Request, identity []byte) bool {
	tok := r.Fields[actionTokenField]
	return tok != "" && bytes.Equal(p.sessions.identity(tok), identity)
}

// --- Micron helpers ---------------------------------------------------------

type builder struct{ strings.Builder }

func (b *builder) line(format string, args ...any) {
	fmt.Fprintf(&b.Builder, format, args...)
	b.WriteByte('\n')
}

func (b *builder) blank() { b.WriteByte('\n') }

// esc makes text safe inside a Micron line: backslash and backtick are
// escaped, so nobody's message can carry formatting or links. Text never
// starts a line on these pages (message continuation lines are indented),
// so Micron's block characters need no escaping.
func esc(s string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`").Replace(s)
}

// escLabel makes text safe as a link's label. NomadNet finds a link
// label's end by scanning for the next backtick or ']', with no regard for
// backslash escapes, so esc's usual escaping doesn't protect this
// position: a backtick or ']' there could still close the label early and
// let the rest of a name be read as Micron formatting, or as more of the
// link's own target and variables. Replaced outright, not escaped.
func escLabel(s string) string {
	return strings.NewReplacer("`", "'", "]", ")").Replace(s)
}

func link(label, path, vars string) string {
	if vars != "" {
		return fmt.Sprintf("`F5af`_`[%s`:%s`%s]`_`f", escLabel(label), path, vars)
	}
	return fmt.Sprintf("`F5af`_`[%s`:%s]`_`f", escLabel(label), path)
}

// fullLink links to a page on another node: "hash:/page/x.mu".
func fullLink(label, url string) string {
	return fmt.Sprintf("`F5af`_`[%s`%s]`_`f", escLabel(label), url)
}

func fieldLink(label, path, field, vars string) string {
	return fmt.Sprintf("`F5af`_`[%s`:%s`%s|%s]`_`f", escLabel(label), path, field, vars)
}

var london = func() *time.Location {
	l, err := time.LoadLocation("Europe/London")
	if err != nil {
		return time.UTC
	}
	return l
}()

func (p *Page) clock(ms int64) string {
	t := time.UnixMilli(ms).In(london)
	now := p.now().In(london)
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

func (p *Page) userText(err error) string {
	var ue *hub.UserError
	if errors.As(err, &ue) {
		return ue.Text
	}
	p.log.Error("page: hub request failed", "err", err)
	return "something went wrong on the hub; please try again"
}
