package hub

// The room operator commands (/room kick, ban, invite, op, voice, mode,
// register) and the /who and /list reply texts are ported from rrcd 0.3.2
// commands.py, Copyright (c) 2025 S. Miller, KC1AWV, MIT License. NomadNet
// parses the /who and /list replies, so those texts are exact.
//
// One command table serves RRC, LXMF and the page: the same names,
// arguments, meaning and reply text wherever a command is typed, so nobody
// moving between apps types the wrong thing. Only how the reply travels
// differs.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// OnePacketText is the most text that fits in one LXMF message sent as a
// single Reticulum packet. /help pages are kept within it, and the LXMF
// group cuts anything longer into parts.
//
// A message that doesn't fit one packet travels as a Resource, and
// MeshChatX's "Block Attachments from Strangers" setting (on by default)
// cancels every inbound delivery Resource from anyone who isn't a contact,
// before it transfers — which meant /help never reached a MeshChatX user
// who hadn't added the group as a contact.
//
// The budget is a link DATA packet: LinkMDU (431 bytes) holds the
// destination and source hashes (32), the signature (64) and the msgpack
// payload, which frames the content with a timestamp, an empty title and no
// fields (about 16 bytes). A recipient that asks for a stamp adds 34 more.
// That leaves 285 bytes of content in the worst case; 280 keeps a margin.
const OnePacketText = 280

// CommandRequest is a command someone typed.
type CommandRequest struct {
	Via      Via
	Identity []byte
	Room     string // the RRC room it was typed in, if any
	Text     string
}

// Reply is the answer to a command, for the person who sent it.
type Reply struct {
	Lines []string
	Error bool // RRC sends a one-line error as ERROR rather than NOTICE
	// Whole asks RRC to send the lines as one NOTICE when they fit: clients
	// parse the /list reply as a block, header and rooms together.
	Whole bool
	// History is set by /history: messages to show, oldest first, with
	// HistoryNote as the opening line.
	History     []store.Message
	HistoryNote string
	// PartRoom, over RRC, is a room the sender's link should leave: /leave.
	PartRoom string
}

// Text joins the reply lines.
func (r Reply) Text() string { return strings.Join(r.Lines, "\n") }

// IsCommand reports whether text is a command. Anything starting with "/"
// is. So is "!" followed by a command the hub knows, for clients that keep
// "/" to themselves (NomadNet's RRC client), while "!!!" and "!important"
// are still said.
func IsCommand(text string) bool {
	t := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(t, "/"):
		return true
	case strings.HasPrefix(t, "!"):
		name, _, _ := strings.Cut(t[1:], " ")
		return lookupCommand(strings.ToLower(name)) != nil
	}
	return false
}

// ActionText returns the action in "/me waves" or "!me waves".
func ActionText(text string) (string, bool) {
	t := strings.TrimSpace(text)
	for _, prefix := range []string{"/me ", "!me "} {
		if len(t) > len(prefix) && strings.EqualFold(t[:len(prefix)], prefix) {
			if action := strings.TrimSpace(t[len(prefix):]); action != "" {
				return action, true
			}
		}
	}
	return "", false
}

type access int

const (
	anyone access = iota
	mods
	admins
	owners
)

// rank is the lowest rank that passes an access level.
func (a access) rank() Rank {
	switch a {
	case mods:
		return RankMod
	case admins:
		return RankAdmin
	case owners:
		return RankOwner
	}
	return RankMember
}

type command struct {
	name    string
	aliases []string
	args    string // usage after the name, the same everywhere
	short   string // its line on a /help page
	help    string // /help <command>
	page    int    // the /help section listing it (helpSections); 0 for none
	access  access
	run     func(*cmd) error
	// expand runs the command as this line instead: /pause is "lxmf off".
	expand string
	// oldForm spots rrcd's form of a command, which runs with rrcd's
	// permissions (room operators) rather than access.
	oldForm func(*cmd) bool
}

// helpSections name the groups of commands /help lists, in order. Each
// section fills as many pages as it needs, and every page fits one LXMF
// packet, so /help reaches every client one message at a time.
var helpSections = []string{1: "chatting", 2: "you", 3: "whispers", 4: "people and rooms", 5: "moderation", 6: "ranks", 7: "running the hub"}

// commandsOnce and commandsList back commandTable, below.
var (
	commandsOnce sync.Once
	commandsList []*command
)

// commandTable is the one table behind dispatch, permissions and /help,
// built once on first use rather than in init() (which CONTRIBUTING rules
// out for its hidden ordering and its cost on every importer, whether or
// not they ever run a command). It can't be a plain
// "var commandTable = sync.OnceValue(func() []*command { ... })" instead:
// the "help" command's own entry here refers to cmdHelp, whose body calls
// helpPages, which calls commandTable — a genuine cycle to the compiler if
// that call sits in a var initializer, even though it's fine once deferred
// into an ordinary function's body, as sync.Once.Do's argument is here.
func commandTable() []*command {
	commandsOnce.Do(func() { commandsList = buildCommandTable() })
	return commandsList
}

func buildCommandTable() []*command {
	commands := []*command{
		{name: "help", args: "[page|command]", help: "commands a page at a time (/help 2), or one command explained (/help nick)", run: cmdHelp},

		{name: "join", args: "[#room|lxmf]", short: "join", page: 1, run: cmdJoin,
			help: "join a room, or the LXMF group. Rooms are joined from your RRC app; the group is joined by sending /join from your LXMF app. Over LXMF, /join Name also picks your name."},
		{name: "leave", args: "[#room|lxmf]", short: "leave", page: 1, run: cmdLeave,
			help: "leave the room you're in over RRC, or the LXMF group from LXMF or the page; /leave lxmf leaves the group from anywhere. Your name stays yours (see /forget)."},
		{name: "nick", args: "<name>", short: "take a name", page: 1, run: cmdNick,
			help: "take a name, the same on RRC, the LXMF group and the page; your old name is freed"},
		{name: "me", args: "<action>", short: "an action", page: 1, run: cmdMe, help: "say an action: /me waves shows as * Name waves"},
		{name: "who", aliases: []string{"names"}, args: "[#room]", short: "who's here", page: 1, run: cmdWho,
			help: "who is in a room, on RRC and in the LXMF group"},
		{name: "list", short: "rooms", page: 1, run: cmdList, help: "the registered public rooms"},
		{name: "history", args: "[#room] [n]", short: "older messages", page: 1, run: cmdHistory,
			help: "the last n messages in a room (20 if you don't say)"},
		{name: "topic", args: "[#room] [text]", short: "the topic", page: 1, run: cmdTopic,
			help: "show a room's topic, or set it (room operators, in +t rooms). /topic #room text sets a topic that starts with a room's name."},

		{name: "whoami", short: "your name and settings", page: 2, run: cmdWhoami, help: "your name, identity, where you're connected and your settings"},
		{name: "lxmf", args: "on|off|auto|mine on|off", short: "LXMF delivery", page: 2, run: cmdLXMF,
			help: "LXMF delivery: on (every message), off (none; you stay in the group), auto (paused while you're in the room over RRC). /lxmf mine on also sends you your own messages from RRC and the page."},
		{name: "pause", expand: "lxmf off", help: "the same as /lxmf off"},
		{name: "resume", expand: "lxmf on", help: "the same as /lxmf on"},
		{name: "share", args: "on|off", short: "show your LXMF address", page: 2, run: cmdShare,
			help: "show your LXMF address on your profile (on, the default) or hide it (off); it's always shown for mods and up"},
		{name: "whispers", args: "[on|off|lxmf on|off]", short: "whisper settings", page: 3, run: cmdWhispers,
			help: "whether people can whisper to you (on, the default), and whether whispers come to your LXMF app (on, even while group delivery is off)"},
		{name: "ignore", args: "<name>", short: "block their whispers", page: 3, run: cmdIgnore, help: "stop someone's whispers reaching you; they aren't told"},
		{name: "unignore", args: "<name>", short: "undo /ignore", page: 3, run: cmdUnignore, help: "let someone whisper to you again"},
		{name: "ignored", short: "who you ignore", page: 3, run: cmdIgnored, help: "the people you're ignoring"},
		{name: "page", args: "[lines|flag|refresh …]", short: "chat page view", page: 2, run: cmdPage,
			help: "how the chat page looks for you: /page lines 10|20|30|50|100, /page flag on|off (the banner), /page refresh off|10|30|60 (seconds)"},
		{name: "forget", short: "free your name, leave all", page: 2, run: cmdForget,
			help: "free your name for anyone and leave the LXMF group. Messages you've sent keep your name."},

		{name: "whisper", aliases: []string{"w", "msg", "tell"}, args: "<name> <text>", short: "only they see it", page: 3, run: cmdWhisper,
			help: "say something only one person sees: on RRC, in their LXMF app and on the chat page, now or when they're next on. Also /w, /msg and /tell."},
		{name: "r", args: "<text>", short: "reply to a whisper", page: 3, run: cmdReply, help: "whisper back to whoever whispered to you last"},
		{name: "profile", aliases: []string{"whois"}, args: "[name]", short: "someone's profile", page: 4, run: cmdProfile,
			help: "a person's profile: their name, whether they're here, and their LXMF address if they share it"},
		{name: "link", args: "[code [confirm]|approve|deny <app>]", short: "link another app", page: 4, run: cmdLink,
			help: "share your name with another app: /link here gives a code, /link CODE on the other app links it. People with a role approve with /link approve <app>."},
		{name: "devices", args: "[name]", short: "your apps", page: 4, run: cmdDevices, help: "the apps sharing your name, or (mods and up) someone else's"},
		{name: "unlink", args: "[app]", short: "unlink an app", page: 4, run: cmdUnlink, help: "take this app, or another of yours (by the start of its identity), out of your name"},
		{name: "room", args: "<verb> [#room] …", short: "for room operators", page: 4, run: cmdRoom, help: roomHelp},

		{name: "kick", args: "<name> [reason]", short: "put out, name freed", page: 5, access: mods, run: cmdKick, oldForm: isLegacyRoomKick,
			help: "put someone out of every room and the group; their name is freed and their apps unlinked, and they can come back as a guest"},
		{name: "ban", args: "<name> [1h|1d|7d|perm] [reason]", short: "ban, name freed", page: 5, access: mods, run: cmdBan, oldForm: isLegacyRoomBan,
			help: "ban every app of someone from the hub, for a while (30m, 1h, 1d, 7d) or for good (perm, the default); their name is freed"},
		{name: "unban", args: "<name|hash>", short: "lift a ban", page: 5, access: mods, run: cmdUnban, help: "lift a ban, by the name they had or their identity"},
		{name: "bans", short: "who's banned", page: 5, access: mods, run: cmdBans, help: "current bans, with the reason, who banned them and until when"},
		{name: "rename", args: "<name> <new name>", short: "change a name", page: 5, access: mods, run: cmdRename, help: "change someone's name; the usual name rules apply"},
		{name: "delete", args: "<name> [n]", short: "remove messages", page: 2, run: cmdDelete,
			help: "remove someone's last n messages (1 if you don't say) in the room you're in, from history and the chat page. Anyone can remove their own; mods and up can remove others'. Copies already delivered stay in people's apps."},
		{name: "modlog", args: "[n]", short: "recent actions", page: 5, access: mods, run: cmdModlog, help: "the last n moderation actions, newest first"},
		{name: "mod", args: "<name>", short: "make a mod", page: 6, access: admins, run: cmdMod, help: "make someone a mod: they can kick, ban, rename and remove messages"},
		{name: "admin", args: "<name>", short: "make an admin", page: 6, access: admins, run: cmdAdmin, help: "make someone an admin: a mod who can also make mods and admins"},
		{name: "demote", args: "<name>", short: "take a role away", page: 6, access: admins, run: cmdDemote, help: "take away someone's mod or admin role"},
		{name: "kline", args: "add|del|list [name|hashprefix|hash]", access: mods, run: cmdKline, help: "rrcd's form of /ban, /unban and /bans, kept for now"},
		{name: "release", args: "<name>", short: "free a name", page: 7, access: owners, run: cmdRelease, help: "free a name someone holds"},
		{name: "stats", short: "hub counters", page: 7, access: owners, run: cmdStats, help: "hub counters"},
	}
	return append(commands, legacyRoomCommands()...)
}

func lookupCommand(name string) *command {
	for _, c := range commandTable() {
		if c.name == name || slices.Contains(c.aliases, name) {
			return c
		}
	}
	return nil
}

// cmd is one command being run.
type cmd struct {
	h     *Hub
	tx    *store.Tx
	out   *outbox
	req   CommandRequest
	def   *command
	args  []string
	me    Person
	reply *Reply
	nowMS int64
}

func (c *cmd) say(format string, args ...any) {
	c.reply.Lines = append(c.reply.Lines, fmt.Sprintf(format, args...))
}

func (c *cmd) sayLines(text string) {
	c.reply.Lines = append(c.reply.Lines, strings.Split(text, "\n")...)
}

func (c *cmd) fail(format string, args ...any) error {
	c.reply.Error = true
	c.say(format, args...)
	return nil
}

func (c *cmd) usage() error {
	return c.fail("%s", strings.TrimSpace("usage: /"+c.def.name+" "+c.def.args))
}

// here is the room a command is about when none is given: the RRC room it
// was typed in, or the group room on LXMF and the page.
func (c *cmd) here() string {
	if c.req.Via == ViaRRC && c.req.Room != "" {
		return c.req.Room
	}
	return c.h.cfg.GroupRoom
}

// target is what a command's first argument names: a room, or the LXMF
// group.
type target struct {
	room  string
	lxmf  bool
	given bool // named in the command rather than taken from here
}

// takeTarget takes an optional "#room" or "lxmf" from the front of the
// arguments. With bareRooms, a word naming a room that exists counts too
// ("/topic scotmesh"), as in rrcd's forms. Without one, it is here. A bad
// room name fails the command; ok is false then.
func (c *cmd) takeTarget(bareRooms bool) (t target, ok bool, err error) {
	if len(c.args) > 0 {
		a := c.args[0]
		switch {
		case strings.EqualFold(a, "lxmf"):
			c.args = c.args[1:]
			return target{room: c.h.cfg.GroupRoom, lxmf: true, given: true}, true, nil
		case strings.HasPrefix(a, "#"):
			r, err := c.h.normRoom(a)
			var ue *UserError
			if errors.As(err, &ue) {
				return target{}, false, c.fail("bad room: %s", ue.Text)
			} else if err != nil {
				return target{}, false, err
			}
			c.args = c.args[1:]
			return target{room: r, given: true}, true, nil
		case bareRooms:
			if r, err := c.h.normRoom(a); err == nil {
				if _, exists, err := c.tx.Room(r); err != nil {
					return target{}, false, err
				} else if exists {
					c.args = c.args[1:]
					return target{room: r, given: true}, true, nil
				}
			}
		}
	}
	return target{room: c.here()}, true, nil
}

// Command runs a command. Refusals and usage errors come back in the Reply;
// the error is only for internal failures.
func (h *Hub) Command(ctx context.Context, req CommandRequest) (Reply, error) {
	var reply Reply
	h.stats.Commands.Add(1)
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		reply = Reply{}
		return h.command(tx, out, req, &reply)
	})
	var ue *UserError
	if errors.As(err, &ue) {
		return Reply{Lines: []string{ue.Text}, Error: true}, nil
	}
	return reply, err
}

func (h *Hub) command(tx *store.Tx, out *outbox, req CommandRequest, reply *Reply) error {
	text := strings.TrimSpace(req.Text)
	var fields []string
	if text != "" && (text[0] == '/' || text[0] == '!') {
		fields = strings.Fields(text[1:])
	}
	if len(fields) == 0 {
		reply.Error = true
		reply.Lines = []string{"Send /help for the commands."}
		return nil
	}
	def := lookupCommand(strings.ToLower(fields[0]))
	if def == nil {
		reply.Error = true
		reply.Lines = []string{fmt.Sprintf("There's no /%s. Send /help for the commands.", strings.ToLower(fields[0]))}
		return nil
	}
	if def.expand != "" {
		fields = append(strings.Fields(def.expand), fields[1:]...)
		def = lookupCommand(fields[0])
	}
	if err := h.checkHubBan(tx, req.Identity); err != nil {
		return err
	}
	if err := tx.TouchIdentity(req.Identity, nil, h.nowMS()); err != nil {
		return err
	}
	me, err := h.person(tx, req.Identity)
	if err != nil {
		return err
	}
	c := &cmd{h: h, tx: tx, out: out, req: req, def: def, args: fields[1:], me: me, reply: reply, nowMS: h.nowMS()}
	if def.oldForm != nil && def.oldForm(c) {
		return legacyRoom(c, def.name)
	}
	if me.Rank < def.access.rank() {
		return c.fail("not authorized")
	}
	return def.run(c)
}

// --- help ---------------------------------------------------------------------

func cmdHelp(c *cmd) error {
	if len(c.args) > 1 {
		return c.usage()
	}
	n := 1
	if len(c.args) == 1 {
		v, err := strconv.Atoi(c.args[0])
		if err != nil {
			return helpOne(c, c.args[0])
		}
		n = v
	}
	pages := c.helpPages()
	if n < 1 || n > len(pages) {
		return c.fail("There are %d help pages: /help 1 to /help %d.", len(pages), len(pages))
	}
	c.reply.Lines = append(c.reply.Lines, renderHelpPage(pages, n)...)
	return nil
}

// compactArgs shortens usage for a help page; /help <command> has it in full.
func compactArgs(args string) string {
	args = strings.ReplaceAll(args, "name|hashprefix|hash", "name")
	args = strings.ReplaceAll(args, "[code [confirm]|approve|deny <app>]", "[code]")
	args = strings.ReplaceAll(args, "[1h|1d|7d|perm] ", "[time] ")
	args = strings.ReplaceAll(args, "on|off|auto|mine on|off", "on|off|auto|mine")
	args = strings.ReplaceAll(args, "[lines|flag|refresh …]", "[lines|flag|refresh]")
	args = strings.ReplaceAll(args, "[on|off|lxmf on|off]", "[on|off|lxmf]")
	return strings.ReplaceAll(args, "add|del|list [name]", "add|del|list")
}

type helpPage struct {
	title string
	lines []string // one per command
}

func helpLine(def *command) string {
	line := "/" + def.name
	if def.args != "" {
		line += " " + compactArgs(def.args)
	}
	return line + " — " + def.short
}

// renderHelpPage is page n of pages, with its heading and its last line.
func renderHelpPage(pages []helpPage, n int) []string {
	page := pages[n-1]
	out := []string{fmt.Sprintf("Help %d/%d · %s", n, len(pages), page.title)}
	out = append(out, page.lines...)
	if n < len(pages) {
		return append(out, fmt.Sprintf("More: /help %d", n+1))
	}
	return append(out, "Details: /help <command> · ! works in place of /")
}

// helpPages packs the commands this person can use into pages, a section at
// a time, starting a new page when the next line would take the page past
// one LXMF packet.
func (c *cmd) helpPages() []helpPage {
	var pages []helpPage
	for section := 1; section < len(helpSections); section++ {
		started := false
		for _, def := range commandTable() {
			if def.page != section || c.me.Rank < def.access.rank() {
				continue
			}
			line := helpLine(def)
			if started {
				last := &pages[len(pages)-1]
				if trial := append(append([]string{}, last.lines...), line); helpPageBytes(last.title, trial, false) <= OnePacketText {
					last.lines = trial
					continue
				}
			}
			pages = append(pages, helpPage{title: helpSections[section], lines: []string{line}})
			started = true
		}
	}
	// The last page ends with the longer "Details" line instead of "More".
	for len(pages) > 0 {
		last := &pages[len(pages)-1]
		if len(last.lines) < 2 || helpPageBytes(last.title, last.lines, true) <= OnePacketText {
			break
		}
		moved := last.lines[len(last.lines)-1]
		last.lines = last.lines[:len(last.lines)-1]
		pages = append(pages, helpPage{title: last.title, lines: []string{moved}})
	}
	return pages
}

// helpPageBytes is the size a page would have, with two-digit page numbers
// to be safe, ending in "More: /help NN" or, on the last page, "Details".
func helpPageBytes(title string, lines []string, last bool) int {
	n := len("Help 10/10 · ") + len(title)
	for _, l := range lines {
		n += 1 + len(l)
	}
	if last {
		return n + 1 + len("Details: /help <command> · ! works in place of /")
	}
	return n + 1 + len("More: /help 10")
}

func helpOne(c *cmd, name string) error {
	name = strings.ToLower(strings.TrimLeft(name, "/!"))
	def := lookupCommand(name)
	if def == nil {
		return c.fail("There's no /%s. Send /help for the commands.", name)
	}
	line := "/" + def.name
	if def.args != "" {
		line += " " + def.args
	}
	c.say("%s — %s", line, def.help)
	return nil
}

// HelpWiki is the command table as a MediaWiki page body, for the wiki's
// "Chat commands" page, so the page can't drift from the hub.
func HelpWiki() string {
	var b strings.Builder
	b.WriteString("Every command works the same in the RRC room, the LXMF group and on the chat page. Start it with <code>/</code> or, where your app keeps <code>/</code> for itself (NomadNet's RRC screen), with <code>!</code>. Rooms are written <code>#room</code>; leave them out to mean the room you're in, or <code>#scotmesh</code> in the group and on the page. <code>/help</code> lists the commands a page at a time.\n")
	ranks := map[access]string{anyone: "everyone", mods: "mods and up", admins: "admins and up", owners: "hub owners"}
	for section := 1; section < len(helpSections); section++ {
		var rows []*command
		for _, def := range commandTable() {
			if def.page == section {
				rows = append(rows, def)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n== %s ==\n\n{| class=\"wikitable\"\n! Command !! Who !! What it does\n", capitalise(helpSections[section]))
		for _, def := range rows {
			usage := "/" + def.name
			if def.args != "" {
				usage += " " + def.args
			}
			names := "<code>" + wikiEscape(usage) + "</code>"
			for _, a := range def.aliases {
				names += " (<code>/" + a + "</code>)"
			}
			fmt.Fprintf(&b, "|-\n| %s || %s || %s\n", names, ranks[def.access], wikiEscape(def.help))
		}
		b.WriteString("|}\n")
	}
	var aliases []string
	for _, def := range commandTable() {
		if def.expand != "" {
			aliases = append(aliases, fmt.Sprintf("<code>/%s</code> is <code>/%s</code>", def.name, def.expand))
		}
	}
	fmt.Fprintf(&b, "\n== Shortcuts and older forms ==\n\n%s. rrcd's room commands (<code>/kick room name</code>, <code>/ban room add name</code>, <code>/op</code>, <code>/voice</code>, <code>/mode</code>, <code>/invite</code>, <code>/register</code>) and <code>/kline</code> still work, and say the form to use next time.\n", strings.Join(aliases, "; "))
	return b.String()
}

// wikiEscape keeps table cells from being read as wiki markup.
func wikiEscape(s string) string {
	return strings.NewReplacer("|", "&#124;", "<", "&lt;", ">", "&gt;").Replace(s)
}
