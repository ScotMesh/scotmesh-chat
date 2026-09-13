package nomadpage

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// view is how the chat page looks for one visitor: saved with their
// identity once they identify, or carried in the page's links until then,
// since NomadNet requests have no cookies.
type view struct {
	lines   int
	flag    bool
	refresh int
	carried bool // an anonymous visitor's choices, repeated in every link
}

func (p *Page) viewFor(r Request, person *hub.Person) view {
	if person != nil {
		return view{lines: person.Prefs.PageLines, flag: person.Prefs.PageFlag, refresh: person.Prefs.PageRefresh}
	}
	d := store.DefaultPrefs()
	v := view{lines: d.PageLines, flag: d.PageFlag, refresh: d.PageRefresh, carried: true}
	if n, err := strconv.Atoi(r.Fields["lines"]); err == nil && slices.Contains(store.PageLineChoices, n) {
		v.lines = n
	}
	if f := r.Fields["flag"]; f == "0" || f == "1" {
		v.flag = f == "1"
	}
	if n, err := strconv.Atoi(r.Fields["refresh"]); err == nil && slices.Contains(store.PageRefreshChoices, n) {
		v.refresh = n
	}
	return v
}

// vars are the view choices to repeat in links, for an anonymous visitor.
func (v view) vars() string {
	if !v.carried {
		return ""
	}
	return fmt.Sprintf("lines=%d|flag=%d|refresh=%d", v.lines, b2i(v.flag), v.refresh)
}

// with joins link variables, leaving out empty ones.
func with(vars ...string) string {
	var out []string
	for _, v := range vars {
		if v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, "|")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- the conversation --------------------------------------------------------

func (p *Page) index(ctx context.Context, r Request, person *hub.Person, refusal string) string {
	notice := refusal
	var reply *hub.Reply
	if refusal == "" {
		const expired = "That link has expired; reload the page and try again."
		switch r.Fields["action"] {
		case "send":
			if person != nil && !p.validAction(r, person.Identity) {
				notice = expired
			} else {
				notice, reply = p.post(ctx, r, person)
			}
		case "view":
			notice = p.changeView(ctx, r, person)
		case "delete":
			if person != nil && !p.validAction(r, person.Identity) {
				reply = &hub.Reply{Lines: []string{expired}, Error: true}
			} else {
				reply = p.deleteMessage(ctx, r, person)
			}
		}
		if person != nil {
			if fresh, err := p.hub.PersonOf(ctx, person.Identity); err == nil {
				person = &fresh
			}
		}
	}
	v := p.viewFor(r, person)
	var before int64
	if r.Path == PathOlder {
		before, _ = strconv.ParseInt(r.Fields["before"], 10, 64)
	}
	msgs, err := p.hub.Recent(ctx, p.cfg.GroupRoom, v.lines, before)
	if err != nil {
		p.log.Error("page: recent messages", "err", err)
		notice = "The conversation couldn't be loaded just now. Please reload."
	}

	// The marker sits after the last message this visitor had seen here. It
	// moves only on a full page load, not when the conversation refreshes.
	var marker int64
	if person != nil && before == 0 {
		if c, ok, err := p.hub.ReadCursor(ctx, person.Identity, p.cfg.GroupRoom, hub.ViaPage); err == nil && ok {
			marker = c.MessageID
		}
		if len(msgs) > 0 {
			if err := p.hub.MarkRead(ctx, person.Identity, p.cfg.GroupRoom, hub.ViaPage, msgs[len(msgs)-1].ID); err != nil {
				p.log.Error("page: mark read", "err", err)
			}
		}
	}

	var b builder
	p.header(ctx, &b, person, v)
	p.viewButtons(&b, person, v)
	if notice != "" {
		b.line("`Ffb3%s`f", esc(notice))
		b.blank()
	}
	if r.Fields["action"] == "delete_confirm" {
		p.confirmDelete(ctx, &b, r, v, person)
	}
	if reply != nil {
		p.replyBox(&b, reply)
	}
	if before > 0 {
		b.line("`F9ab— older messages —`f")
		b.blank()
	}
	if v.refresh > 0 && before == 0 {
		// The conversation reloads on its own; the header, the buttons and
		// the Say box stay put, so half-typed text isn't lost. The partial's
		// request doesn't identify the visitor, so a short-lived token
		// carries who they are (see sessions).
		token := ""
		if person != nil {
			token = "t=" + p.sessions.issue(person.Identity)
		}
		b.line("`{%s:%s`%d`%s}", p.cfg.NodeAddress, PathMessages, v.refresh, with("pid=chat", fmt.Sprintf("lines=%d", v.lines), fmt.Sprintf("m=%d", marker), token))
	} else {
		p.conversation(&b, msgs, person, marker, before == 0, v)
	}
	b.blank()
	b.line("-")
	p.postForm(&b, person)
	b.blank()
	nav := []string{link("Reload", PathIndex, v.vars())}
	if v.refresh > 0 && before == 0 {
		nav = append(nav, "`F5af`_`[Refresh now`p:chat]`_`f")
	}
	if len(msgs) > 0 {
		nav = append(nav, link("Older", PathOlder, with(fmt.Sprintf("before=%d", msgs[0].ID), v.vars())))
	}
	nav = append(nav, link("Your name & settings", PathMe, ""), link("How to join", PathHelp, v.vars()))
	if p.cfg.WikiURL != "" {
		nav = append(nav, fullLink("About the chat (wiki)", p.cfg.WikiURL))
	}
	b.line("%s", strings.Join(nav, "  ·  "))
	return b.String()
}

// messages serves the conversation on its own, for the refreshing partial.
func (p *Page) messages(ctx context.Context, r Request) string {
	var person *hub.Person
	if id := p.sessions.identity(r.Fields["t"]); id != nil {
		if pp, err := p.hub.PersonOf(ctx, id); err == nil {
			person = &pp
		}
	}
	v := p.viewFor(Request{Fields: map[string]string{"lines": r.Fields["lines"]}}, nil)
	if person != nil {
		v = p.viewFor(r, person)
	}
	msgs, err := p.hub.Recent(ctx, p.cfg.GroupRoom, v.lines, 0)
	if err != nil {
		p.log.Error("page: recent messages", "err", err)
		return "`Ffb3The conversation couldn't be loaded just now.`f"
	}
	marker, _ := strconv.ParseInt(r.Fields["m"], 10, 64)
	var b builder
	p.conversation(&b, msgs, person, marker, true, v)
	return strings.TrimRight(b.String(), "\n")
}

// conversation writes the message lines, with the "new since your last
// visit" line after marker.
func (p *Page) conversation(b *builder, msgs []store.Message, person *hub.Person, marker int64, latest bool, v view) {
	if len(msgs) == 0 && latest {
		b.line("`F9abNothing said in the last 7 days. Say hello!`f")
		return
	}
	// Your own messages are never news to you: they move the marker along
	// rather than sitting under it.
	if person != nil && marker > 0 {
		for i := range msgs {
			if msgs[i].ID > marker && person.Owns(msgs[i].Author) {
				marker = msgs[i].ID
			} else if msgs[i].ID > marker {
				break
			}
		}
	}
	markerShown := false
	for i := range msgs {
		m := &msgs[i]
		if marker > 0 && !markerShown && m.ID > marker && i > 0 {
			b.line("`Fe93── new since your last visit ──────────────`f")
			markerShown = true
		}
		b.line("%s", p.messageLine(m, person, v))
	}
}

func (p *Page) messageLine(m *store.Message, visitor *hub.Person, v view) string {
	var name string
	if visitor != nil && visitor.Owns(m.Author) {
		name = "`F5d8" + esc(m.AuthorName) + "`f" // your own messages
	} else {
		name = "`!" + link(m.AuthorName, PathProfile, with("id="+hex.EncodeToString(m.Author), v.vars())) + "`!"
	}
	body := esc(m.Body)
	body = strings.ReplaceAll(body, "\n", "\n          ")
	clock := p.clock(m.SaidAt)
	line := fmt.Sprintf("`F9ab%s`f  %s  %s", clock, name, body)
	if m.Kind == store.KindAction {
		line = fmt.Sprintf("`F9ab%s`f  * %s %s", clock, name, body)
	}
	// Your own messages, and (for mods and up) anyone's, can be removed; the
	// hub still applies the rank rule when the link is followed.
	if visitor != nil && (visitor.Owns(m.Author) || visitor.Rank >= hub.RankMod) {
		line += "  " + fmt.Sprintf("`F667`[×`:%s`action=delete_confirm|msg=%d]`f", PathIndex, m.ID)
	}
	return line
}

// changeView applies a view button: saved for an identified visitor, and
// carried in the links (by viewFor) for anyone else.
func (p *Page) changeView(ctx context.Context, r Request, person *hub.Person) string {
	if person == nil {
		return ""
	}
	_, err := p.hub.SetPrefs(ctx, person.Identity, func(pr *store.Prefs) {
		if n, err := strconv.Atoi(r.Fields["lines"]); err == nil && slices.Contains(store.PageLineChoices, n) {
			pr.PageLines = n
		}
		if f := r.Fields["flag"]; f == "0" || f == "1" {
			pr.PageFlag = f == "1"
		}
		if n, err := strconv.Atoi(r.Fields["refresh"]); err == nil && slices.Contains(store.PageRefreshChoices, n) {
			pr.PageRefresh = n
		}
	})
	if err != nil {
		return p.userText(err)
	}
	return ""
}

// viewButtons is the line of view choices under the title.
func (p *Page) viewButtons(b *builder, person *hub.Person, v view) {
	choice := func(label string, current bool, vars string) string {
		if current {
			return "`!`F2c8[" + esc(label) + "]`f`!"
		}
		return link(label, PathIndex, vars)
	}
	// Each button sets one choice and keeps the others.
	set := func(lines, flag, refresh int) string {
		return fmt.Sprintf("action=view|lines=%d|flag=%d|refresh=%d", lines, flag, refresh)
	}
	var lines []string
	for _, n := range store.PageLineChoices {
		lines = append(lines, choice(strconv.Itoa(n), n == v.lines, set(n, b2i(v.flag), v.refresh)))
	}
	var refresh []string
	for _, n := range store.PageRefreshChoices {
		label := "off"
		if n > 0 {
			label = fmt.Sprintf("%ds", n)
		}
		refresh = append(refresh, choice(label, n == v.refresh, set(v.lines, b2i(v.flag), n)))
	}
	hasBanner := len(p.cfg.Banner) > 0
	if hasBanner {
		flag := link("hide", PathIndex, set(v.lines, 0, v.refresh))
		if !v.flag {
			flag = link("show", PathIndex, set(v.lines, 1, v.refresh))
		}
		b.line("`F9abShow:`f %s  `F9abFlag:`f %s  `F9abRefresh:`f %s", strings.Join(lines, " "), flag, strings.Join(refresh, " "))
	} else {
		b.line("`F9abShow:`f %s  `F9abRefresh:`f %s", strings.Join(lines, " "), strings.Join(refresh, " "))
	}
	changed := v.lines != store.DefaultPrefs().PageLines || v.refresh != store.DefaultPrefs().PageRefresh || (hasBanner && !v.flag)
	if person == nil && changed {
		b.line("`F9abThese choices last for this visit; identify to keep them.`f")
	}
	b.blank()
}

// post says what the visitor typed, or runs it as their command. A command's
// reply is for them alone and shows above the conversation.
func (p *Page) post(ctx context.Context, r Request, person *hub.Person) (string, *hub.Reply) {
	if person == nil {
		return "Identify to post: the fingerprint button in MeshChatX, or \"Identify when connecting\" for this node in NomadNet.", nil
	}
	msg := strings.TrimSpace(r.Fields["message"])
	if msg == "" {
		return "", nil
	}
	action := false
	if a, ok := hub.ActionText(msg); ok {
		msg, action = a, true
	} else if hub.IsCommand(msg) {
		reply, err := p.hub.Command(ctx, hub.CommandRequest{Via: hub.ViaPage, Identity: person.Identity, Text: msg})
		if err != nil {
			return p.userText(err), nil
		}
		return "", &reply
	}
	_, err := p.hub.Post(ctx, hub.PostRequest{Via: hub.ViaPage, Identity: person.Identity, Body: msg, Action: action, OriginID: r.RequestID})
	if err != nil {
		return "Not sent: " + p.userText(err), nil
	}
	return "", nil
}

// confirmDelete asks before a message is removed.
func (p *Page) confirmDelete(ctx context.Context, b *builder, r Request, v view, person *hub.Person) {
	id, err := strconv.ParseInt(r.Fields["msg"], 10, 64)
	if err != nil || person == nil {
		return
	}
	msgs, err := p.hub.Recent(ctx, p.cfg.GroupRoom, 100, 0)
	if err != nil {
		p.log.Error("page: recent messages", "err", err)
		return
	}
	for i := range msgs {
		if msgs[i].ID != id {
			continue
		}
		excerpt := msgs[i].Body
		if len(excerpt) > 60 {
			excerpt = strings.ToValidUTF8(excerpt[:60], "") + "…"
		}
		b.line("`B223`Ffb3  Remove this message from %s? \"%s\"`f`b", esc(msgs[i].AuthorName), esc(excerpt))
		yes := with(fmt.Sprintf("action=delete|msg=%d|tok=%s", id, p.actionToken(person.Identity)), v.vars())
		b.line("`B223  %s  ·  %s`b", link("Yes, remove it", PathIndex, yes), link("No, keep it", PathIndex, v.vars()))
		b.blank()
		return
	}
}

func (p *Page) deleteMessage(ctx context.Context, r Request, person *hub.Person) *hub.Reply {
	if person == nil {
		return &hub.Reply{Lines: []string{"Identify first to remove messages."}, Error: true}
	}
	id, err := strconv.ParseInt(r.Fields["msg"], 10, 64)
	if err != nil {
		return nil
	}
	reply, err := p.hub.DeleteMessage(ctx, person.Identity, id)
	if err != nil {
		return &hub.Reply{Lines: []string{p.userText(err)}, Error: true}
	}
	return &reply
}

// replyBox shows a command's reply, which only this visitor sees.
func (p *Page) replyBox(b *builder, reply *hub.Reply) {
	colour := "9df"
	if reply.Error {
		colour = "fb3"
	}
	b.line("`B223`F%s  Only you see this:`f`b", colour)
	for _, l := range reply.Lines {
		b.line("`B223`F%s  %s`f`b", colour, esc(l))
	}
	if len(reply.History) > 0 {
		b.line("`B223`F9ab  %s`f`b", esc(reply.HistoryNote))
		for i := range reply.History {
			b.line("`B223  %s`b", p.messageLine(&reply.History[i], nil, view{}))
		}
	}
	if len(reply.Lines) == 0 && len(reply.History) == 0 {
		b.line("`B223`F%s  Done.`f`b", colour)
	}
	b.blank()
}

func (p *Page) header(ctx context.Context, b *builder, person *hub.Person, v view) {
	b.line("#!c=0")
	b.line("`c")
	hasBanner := len(p.cfg.Banner) > 0
	if v.flag && hasBanner {
		for _, row := range p.cfg.Banner {
			b.line("%s", row)
		}
		b.blank()
	}
	title := "`!`F2c8" + esc(p.cfg.Title) + "`f`!"
	sub := "#" + p.cfg.GroupRoom
	if ms, err := p.hub.Members(ctx, p.cfg.GroupRoom); err == nil {
		sub += fmt.Sprintf(" · %d here now", len(ms))
	}
	if r, ok, err := p.hub.Room(ctx, p.cfg.GroupRoom); err == nil && ok && r.Topic != "" {
		sub += " · " + r.Topic
	}
	if v.flag && hasBanner {
		b.line("%s", title)
		b.line("`F9ab%s`f", esc(sub))
	} else {
		b.line("%s  `F9ab%s`f", title, esc(sub)) // one compact line without the banner
	}
	b.line("`a")
	b.line("-")
	if person != nil {
		b.line("You are `F5d8`!%s`!`f  ·  %s", esc(person.Name), link("Change name or settings", PathMe, ""))
		b.blank()
	}
}

func (p *Page) postForm(b *builder, person *hub.Person) {
	if person == nil {
		b.line("`F9abTo post, identify yourself to this node: the fingerprint button in MeshChatX, or \"Identify when connecting\" in NomadNet's node settings.`f")
		return
	}
	b.line("Say: `B334`<52|message`>`b  %s", fieldLink("Send", PathIndex, "message", "action=send|tok="+p.actionToken(person.Identity)))
	b.line("`F9abCommands work here too, starting with / or !: try /help.`f")
	if !person.Claimed {
		b.line("`F9abYou're shown as %s until you %s.`f", esc(person.Name), link("choose a name", PathMe, ""))
	}
}
