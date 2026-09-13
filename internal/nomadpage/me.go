package nomadpage

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// --- your name & settings ---------------------------------------------------

func (p *Page) me(ctx context.Context, r Request, person *hub.Person, refusal string) string {
	var b builder
	if person == nil {
		p.header(ctx, &b, person, p.viewFor(r, person))
		b.line(">Your name & settings")
		b.blank()
		if refusal != "" {
			b.line("`Ffb3%s`f", esc(refusal))
			b.blank()
		}
		b.line("Names and settings belong to your Reticulum identity, so this page needs to know who you are.")
		b.line("Identify to this node first: the fingerprint button in MeshChatX, or \"Identify when connecting\" in NomadNet's node settings, then reload.")
		b.blank()
		b.line("%s", link("Back to the chat", PathIndex, ""))
		return b.String()
	}

	var notice string
	var reply *hub.Reply
	tok := p.actionToken(person.Identity)
	action := r.Fields["action"]
	// Every action below (but not a "..._confirm" step, which only shows a
	// confirmation form) changes something, so it needs the token this page
	// issued the visitor: a link built anywhere else can't know it, so it
	// can't drive one of these just because the visitor's client auto-
	// identifies when it connects.
	if action != "" && !strings.HasSuffix(action, "_confirm") && !p.validAction(r, person.Identity) {
		notice = "That link has expired; reload the page and try again."
		action = ""
	}
	// Linking goes through the same commands as everywhere else.
	if line := linkCommand(r.Fields); action != "" && line != "" {
		rep, err := p.hub.Command(ctx, hub.CommandRequest{Via: hub.ViaPage, Identity: person.Identity, Text: line})
		if err != nil {
			notice = p.userText(err)
		} else {
			reply = &rep
			if fresh, err := p.hub.PersonOf(ctx, person.Identity); err == nil {
				person = &fresh
			}
		}
	}
	switch action {
	case "nick":
		updated, err := p.hub.ClaimName(ctx, person.Identity, r.Fields["nick"])
		if err != nil {
			notice = p.userText(err)
		} else {
			if updated.Name != person.Name {
				notice = "You are now " + updated.Name + ". The name is yours on RRC, the LXMF group and this page until you forget it."
			}
			person = &updated
		}
	case "lxmf":
		mode, ok := store.ParseLXMFMode(r.Fields["mode"])
		if ok {
			updated, err := p.hub.SetLXMFMode(ctx, person.Identity, mode)
			if err != nil {
				notice = p.userText(err)
			} else {
				person = &updated
				notice = "LXMF delivery is now " + string(mode) + "."
			}
		}
	case "mine":
		on := r.Fields["on"] == "1"
		updated, err := p.hub.SetLXMFMine(ctx, person.Identity, on)
		if err != nil {
			notice = p.userText(err)
		} else {
			person = &updated
			if on {
				notice = "Your own messages from RRC and this page will come to you by LXMF too."
			} else {
				notice = "Your own messages won't be sent to you by LXMF any more."
			}
		}
	case "share":
		on := r.Fields["on"] == "1"
		updated, err := p.hub.SetPrefs(ctx, person.Identity, func(pr *store.Prefs) { pr.ShareLXMF = on })
		if err != nil {
			notice = p.userText(err)
		} else {
			person = &updated
			if on {
				notice = "Your LXMF address is shown on your profile now, so people can message you."
			} else {
				notice = "Your LXMF address is hidden from your profile now."
			}
		}
	case "leave":
		left, err := p.hub.LeaveGroup(ctx, person.Identity)
		switch {
		case err != nil:
			notice = p.userText(err)
		case left:
			notice = "You have left the LXMF group. Your name stays yours."
			person.Member = false
		default:
			notice = "You weren't in the LXMF group."
		}
	case "forget":
		old := *person
		updated, err := p.hub.Forget(ctx, person.Identity)
		if err != nil {
			notice = p.userText(err)
		} else {
			person = &updated
			switch {
			case old.Claimed && old.Member:
				notice = old.Name + " is free again and you have left the LXMF group."
			case old.Claimed:
				notice = old.Name + " is free again."
			case old.Member:
				notice = "You have left the LXMF group."
			default:
				notice = "There was nothing to forget."
			}
		}
	}
	// The header comes after the actions, so it shows the name as it is now.
	p.header(ctx, &b, person, p.viewFor(r, person))
	b.line(">Your name & settings")
	b.blank()
	if notice != "" {
		b.line("`Ffb3%s`f", esc(notice))
		b.blank()
	}
	if reply != nil {
		p.replyBox(&b, reply)
	}

	b.line(">>Name")
	if person.Claimed {
		b.line("You are `!%s`!, yours since %s.", esc(person.Name), time.UnixMilli(person.ClaimedAt).In(london).Format("2 January 2006"))
	} else {
		b.line("You haven't claimed a name, so people see you as `!%s`!.", esc(person.Name))
	}
	b.line("New name: `B334`<24|nick`>`b  %s", fieldLink("Take this name", PathMe, "nick", "action=nick|tok="+tok))
	b.line("`F9ab2 to 24 letters, digits, - _ or . with no spaces, starting with a letter. A name is held by one identity, on RRC, the LXMF group and this page; names that look alike (A1ex and Alex) count as the same.`f")
	b.blank()

	b.line(">>LXMF delivery")
	if person.Member {
		options := []struct{ mode, label string }{
			{"on", "On: every message"},
			{"auto", "Auto: pause while I'm in the RRC room"},
			{"off", "Off: nothing, but keep my name and membership"},
		}
		for _, o := range options {
			mark := "  "
			if string(person.LXMFMode) == o.mode {
				mark = "`F5d8●`f "
			}
			b.line("%s%s", mark, link(o.label, PathMe, "action=lxmf|mode="+o.mode+"|tok="+tok))
		}
		b.blank()
		if person.LXMFMine {
			b.line("`F5d8●`f Your own messages from RRC and this page also come to you by LXMF.  %s", link("Stop", PathMe, "action=mine|on=0|tok="+tok))
		} else {
			b.line("Your own messages from RRC and this page don't come to you by LXMF.  %s", link("Send them to me too", PathMe, "action=mine|on=1|tok="+tok))
		}
	} else {
		b.line("You're not in the LXMF group. Send `!/join`! from any LXMF messenger to %s to get messages there.", esc(p.cfg.GroupAddress))
	}
	b.blank()

	b.line(">>Sharing")
	switch {
	case person.PersonRank >= hub.RankMod:
		b.line("Your LXMF address is always shown on your profile while you're %s, so people can reach you.", person.PersonRank)
	case person.Prefs.ShareLXMF:
		b.line("`F5d8●`f Your LXMF address is shown on your profile, so people can message you.  %s", link("Hide it", PathMe, "action=share|on=0|tok="+tok))
	default:
		b.line("Your LXMF address is hidden from your profile.  %s", link("Show it", PathMe, "action=share|on=1|tok="+tok))
	}
	b.blank()

	b.line(">>Chat page view")
	v := p.viewFor(r, person)
	refresh := map[bool]string{true: fmt.Sprintf("every %ds", v.refresh), false: "off"}[v.refresh > 0]
	if len(p.cfg.Banner) > 0 {
		b.line("Messages shown: %d  ·  Flag: %s  ·  Refresh: %s", v.lines, map[bool]string{true: "shown", false: "hidden"}[v.flag], refresh)
	} else {
		b.line("Messages shown: %d  ·  Refresh: %s", v.lines, refresh)
	}
	b.line("`F9abChange these with the buttons on the %s, or /page lines|flag|refresh. Refresh off suits a slow link, such as LoRa.`f", link("chat page", PathIndex, ""))
	b.blank()

	p.whisperSection(ctx, &b, person, tok)
	p.linkSection(ctx, &b, person, tok)

	b.line(">>Leave or forget")
	if person.Member {
		if r.Fields["action"] == "leave_confirm" {
			b.line("This takes you out of the LXMF group, so nothing more comes to you by LXMF. Your name stays yours.")
			b.line("%s  ·  %s", link("Yes, leave the group", PathMe, "action=leave|tok="+tok), link("No, stay", PathMe, ""))
		} else {
			b.line("%s", link("Leave the LXMF group…", PathMe, "action=leave_confirm"))
		}
	}
	if r.Fields["action"] == "forget_confirm" {
		b.line("This frees your name for anyone and takes you out of the LXMF group. Messages you've sent keep your name.")
		b.line("%s  ·  %s", link("Yes, forget me", PathMe, "action=forget|tok="+tok), link("No, keep everything", PathMe, ""))
	} else {
		b.line("%s", link("Free my name and leave everything…", PathMe, "action=forget_confirm"))
	}
	b.blank()
	b.line("`F9abYour identity: %s`f", hex.EncodeToString(person.Identity))
	b.blank()
	b.line("%s", link("Back to the chat", PathIndex, ""))
	return b.String()
}

// linkCommand is the command a link, whisper or ignore button or form on the
// settings page stands for, or "".
func linkCommand(f map[string]string) string {
	app := strings.TrimSpace(f["app"])
	switch f["action"] {
	case "unignore":
		if app != "" {
			return "/unignore " + app
		}
	case "whispers":
		switch f["set"] {
		case "on", "off":
			return "/whispers " + f["set"]
		case "lxmf-on", "lxmf-off":
			return "/whispers lxmf " + strings.TrimPrefix(f["set"], "lxmf-")
		}
	case "link":
		return "/link"
	case "redeem":
		code := strings.TrimSpace(f["code"])
		if code == "" {
			return ""
		}
		if f["confirm"] == "1" {
			return "/link " + code + " confirm"
		}
		return "/link " + code
	case "unlink":
		return strings.TrimSpace("/unlink " + app)
	case "approve", "deny":
		if app != "" {
			return "/link " + f["action"] + " " + app
		}
	}
	return ""
}

// whisperSection shows the visitor's recent whispers, marking new ones, and
// their whisper settings. Seeing them here marks them read. tok is the
// action token to embed in every link that changes a setting.
func (p *Page) whisperSection(ctx context.Context, b *builder, person *hub.Person, tok string) {
	b.line(">>Whispers")
	in, err := p.hub.Whispers(ctx, person.Identity, 10)
	if err != nil {
		p.log.Error("page: whispers", "err", err)
		b.line("`Ffb3Your whispers couldn't be shown just now.`f")
		b.blank()
		return
	}
	if len(in.Whispers) == 0 {
		b.line("`F9abNo whispers in the last 7 days.`f")
	}
	newFrom := len(in.Whispers) - in.Unread
	for i := range in.Whispers {
		w := &in.Whispers[i]
		if i == newFrom && in.Unread > 0 {
			b.line("`Fe93── new ──`f")
		}
		b.line("`F9ab%s`f  `!%s`! whispers: %s", p.clock(w.SaidAt), esc(w.FromName), esc(strings.ReplaceAll(w.Body, "\n", " ")))
	}
	on, lxmf := "off", "off"
	toggle, lxmfToggle := link("Turn on", PathMe, "action=whispers|set=on|tok="+tok), link("Turn on", PathMe, "action=whispers|set=lxmf-on|tok="+tok)
	if person.Prefs.Whispers {
		on, toggle = "on", link("Turn off", PathMe, "action=whispers|set=off|tok="+tok)
	}
	if person.Prefs.WhispersLXMF {
		lxmf, lxmfToggle = "on", link("Turn off", PathMe, "action=whispers|set=lxmf-off|tok="+tok)
	}
	b.line("Whispers to you: %s  %s  ·  Also to your LXMF app: %s  %s", on, toggle, lxmf, lxmfToggle)
	for _, ig := range in.Ignored {
		b.line("Ignoring `!%s`!  %s", esc(ig.Name), link("Unignore", PathMe, "action=unignore|app="+hex.EncodeToString(ig.Identity)+"|tok="+tok))
	}
	b.line("`F9abTo whisper to someone, open their profile, or send /whisper Name text. /ignore Name blocks someone's whispers.`f")
	b.blank()
}

// linkSection shows the visitor's linked apps. tok is the action token to
// embed in every link that links, unlinks, approves or denies one.
func (p *Page) linkSection(ctx context.Context, b *builder, person *hub.Person, tok string) {
	b.line(">>Your apps")
	apps, err := p.hub.LinkedApps(ctx, person.Identity)
	if err != nil {
		p.log.Error("page: linked apps", "err", err)
		b.line("`Ffb3Your apps couldn't be listed just now.`f")
		b.blank()
		return
	}
	b.line("One name can be shared by up to 5 apps, such as your phone and your laptop.")
	for _, a := range apps.Apps {
		short := hex.EncodeToString(a.Identity)[:8]
		var notes []string
		if bytes.Equal(a.Identity, person.Identity) {
			notes = append(notes, "this app")
		}
		if a.RRC {
			notes = append(notes, "on RRC now")
		}
		if a.LXMF {
			notes = append(notes, "LXMF group")
		}
		if a.LastSeen > 0 {
			notes = append(notes, "seen "+p.clock(a.LastSeen))
		}
		line := fmt.Sprintf("  `!%s`!  %s", short, esc(strings.Join(notes, " · ")))
		if len(apps.Apps) > 1 {
			line += "  " + link("Unlink", PathMe, "action=unlink|app="+short+"|tok="+tok)
		}
		b.line("%s", line)
	}
	for _, a := range apps.Pending {
		short := hex.EncodeToString(a.Identity)[:8]
		b.line("`Fe93  %s wants to join your name (via %s, until %s):`f  %s  ·  %s", short, a.Via, p.clock(a.ExpiresAt),
			link("Approve", PathMe, "action=approve|app="+short+"|tok="+tok), link("Deny", PathMe, "action=deny|app="+short+"|tok="+tok))
	}
	b.line("%s  on this app, then enter it on the other one.", link("Get a link code", PathMe, "action=link|tok="+tok))
	b.line("Code from your other app: `B334`<12|code`>`b  %s", fieldLink("Link this app", PathMe, "code", "action=redeem|tok="+tok))
	b.blank()
}
