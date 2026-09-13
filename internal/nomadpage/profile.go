package nomadpage

// A person's profile page (ADR 0009): who they are, where they are, how to
// message them, a whisper box, and for mods and up the moderation buttons.
// Every action asks once more before it happens, and goes through the same
// commands as everywhere else.

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
)

func (p *Page) profile(ctx context.Context, r Request, viewer *hub.Person, refusal string) string {
	v := p.viewFor(r, viewer)
	var b builder
	p.header(ctx, &b, viewer, v)
	target, err := hex.DecodeString(r.Fields["id"])
	if err != nil || len(target) != 16 {
		b.line("`Ffb3There's nobody here by that link.`f")
		b.blank()
		b.line("%s", link("Back to the chat", PathIndex, v.vars()))
		return b.String()
	}
	idHex := hex.EncodeToString(target)
	if refusal != "" {
		b.line("`Ffb3%s`f", esc(refusal))
		b.blank()
	}

	// Actions, for an identified visitor. profileCommand only recognises the
	// confirmed, executing action names (not the "..._confirm" step that
	// shows the confirmation form), so this is the one place that needs to
	// check the action token: a forged link can name any action, but it
	// can't carry the token this Page issued the viewer when it last
	// rendered a page for them.
	action := r.Fields["action"]
	if viewer != nil {
		if line := profileCommand(r.Fields, idHex); line != "" {
			var reply hub.Reply
			if !p.validAction(r, viewer.Identity) {
				reply = hub.Reply{Lines: []string{"That link has expired; reload the page and try again."}, Error: true}
			} else if got, err := p.hub.Command(ctx, hub.CommandRequest{Via: hub.ViaPage, Identity: viewer.Identity, Text: line}); err != nil {
				reply = hub.Reply{Lines: []string{p.userText(err)}, Error: true}
			} else {
				reply = got
			}
			p.replyBox(&b, &reply)
			action = ""
		}
	}

	prof, err := p.hub.ProfileOf(ctx, target)
	if err != nil {
		p.log.Error("page: profile", "err", err)
		b.line("`Ffb3This profile couldn't be loaded just now. Please reload.`f")
		return b.String()
	}
	who := prof.Person
	title := esc(who.Name)
	if t := who.PersonRank.Title(); t != "" {
		title += "  `F9ab·`f `Fe93" + esc(t) + "`f"
	}
	b.line(">%s", title)
	b.blank()
	if who.Claimed {
		b.line("Name held since %s.", time.UnixMilli(who.ClaimedAt).In(london).Format("2 January 2006"))
	} else {
		b.line("No name claimed; shown by the start of their identity.")
	}
	switch {
	case len(prof.Here) > 0:
		b.line("Here now: %s.", esc(strings.Join(prof.Here, " and ")))
	case prof.LastSeen > 0:
		b.line("Last seen %s.", p.clock(prof.LastSeen))
	}
	b.blank()

	b.line(">>Message them")
	if prof.Shared {
		for _, a := range prof.Addresses {
			addr := hex.EncodeToString(a)
			note := ""
			if prof.Derived {
				note = "  `F9ab(if they use LXMF with this identity)`f"
			}
			b.line("%s  `F9ab%s`f%s", fullLink("Message on LXMF", "lxmf@"+addr), addr, note)
		}
	} else {
		b.line("`F9abTheir LXMF address isn't shared.`f")
	}
	if viewer != nil && viewer.PersonID != who.PersonID {
		b.line("Whisper: `B334`<44|whisper`>`b  %s", fieldLink("Whisper", PathProfile, "whisper", "action=whisper|id="+idHex+"|tok="+p.actionToken(viewer.Identity)))
		b.line("`F9abOnly they see a whisper: on RRC, in their LXMF app and on this page.`f")
	}
	b.blank()

	if len(prof.Recent) > 0 {
		b.line(">>Lately")
		for i := range prof.Recent {
			b.line("%s", p.messageLine(&prof.Recent[i], viewer, v))
		}
		b.blank()
	}

	if viewer != nil {
		p.moderation(ctx, &b, viewer, who, idHex, action)
	}
	b.line("%s", link("Back to the chat", PathIndex, v.vars()))
	return b.String()
}

// profileCommand is the command a profile form stands for, once confirmed,
// or "".
func profileCommand(f map[string]string, idHex string) string {
	reason := strings.TrimSpace(f["reason"])
	switch f["action"] {
	case "whisper":
		if text := strings.TrimSpace(f["whisper"]); text != "" {
			return "/whisper " + idHex + " " + text
		}
	case "kick":
		return strings.TrimSpace("/kick " + idHex + " " + reason)
	case "ban":
		length := f["len"]
		switch length {
		case "1h", "1d", "7d", "perm":
		default:
			length = "perm"
		}
		return strings.TrimSpace("/ban " + idHex + " " + length + " " + reason)
	case "rename":
		if name := strings.TrimSpace(f["newname"]); name != "" {
			return "/rename " + idHex + " " + name
		}
	case "mod", "admin", "demote":
		return "/" + f["action"] + " " + idHex
	case "deletelast":
		return "/delete " + idHex
	}
	return ""
}

// moderation shows the buttons a viewer may use on this person. Each shows
// a confirmation first, with a reason box where one makes sense.
func (p *Page) moderation(ctx context.Context, b *builder, viewer *hub.Person, who hub.Person, idHex, action string) {
	allowed, err := p.hub.Permissions(ctx, viewer.Identity, who.Identity)
	if err != nil {
		p.log.Error("page: permissions", "err", err)
		return
	}
	self := viewer.PersonID == who.PersonID
	var buttons []string
	add := func(a hub.Action, label, confirm string) {
		if allowed[a] && (!self || a == hub.ActDelete) {
			buttons = append(buttons, link(label, PathProfile, "action="+confirm+"|id="+idHex))
		}
	}
	add(hub.ActKick, "Kick", "kick_confirm")
	add(hub.ActBan, "Ban", "ban_confirm")
	add(hub.ActRename, "Rename", "rename_confirm")
	add(hub.ActDelete, "Remove their last message", "deletelast_confirm")
	if who.PersonRank < hub.RankMod {
		add(hub.ActMakeMod, "Make a mod", "mod_confirm")
	}
	if who.PersonRank < hub.RankAdmin {
		add(hub.ActMakeAdmin, "Make an admin", "admin_confirm")
	}
	if who.PersonRank == hub.RankMod || who.PersonRank == hub.RankAdmin {
		add(hub.ActDemote, "Take their role away", "demote_confirm")
	}
	if len(buttons) == 0 {
		return
	}
	b.line(">>Moderation")
	name := esc(who.Name)
	tok := p.actionToken(viewer.Identity)
	confirm := func(question, yes, vars string, reason bool) {
		b.line("`B223`Ffb3  %s`f`b", question)
		vars += "|tok=" + tok
		if reason {
			b.line("`B223  Reason (optional): `B334`<40|reason`>`b`b")
			b.line("`B223  %s  ·  %s`b", fieldLink(yes, PathProfile, "reason", vars+"|id="+idHex), link("No", PathProfile, "id="+idHex))
		} else {
			b.line("`B223  %s  ·  %s`b", link(yes, PathProfile, vars+"|id="+idHex), link("No", PathProfile, "id="+idHex))
		}
		b.blank()
	}
	switch action {
	case "kick_confirm":
		confirm(fmt.Sprintf("Kick %s? They're put out everywhere and their name is freed.", name), "Yes, kick", "action=kick", true)
	case "ban_confirm":
		b.line("`B223`Ffb3  Ban %s? Every app of theirs is banned and their name is freed.`f`b", name)
		b.line("`B223  Reason (optional): `B334`<40|reason`>`b`b")
		var lengths []string
		for _, l := range []struct{ label, value string }{{"1 hour", "1h"}, {"1 day", "1d"}, {"7 days", "7d"}, {"for good", "perm"}} {
			lengths = append(lengths, fieldLink(l.label, PathProfile, "reason", "action=ban|len="+l.value+"|id="+idHex+"|tok="+tok))
		}
		b.line("`B223  Ban for: %s  ·  %s`b", strings.Join(lengths, "  "), link("No", PathProfile, "id="+idHex))
		b.blank()
	case "rename_confirm":
		b.line("`B223`Ffb3  Rename %s to:`f `B334`<24|newname`>`b  %s  ·  %s`b", name, fieldLink("Rename", PathProfile, "newname", "action=rename|id="+idHex+"|tok="+tok), link("No", PathProfile, "id="+idHex))
		b.blank()
	case "deletelast_confirm":
		confirm(fmt.Sprintf("Remove %s's last message from the chat?", name), "Yes, remove it", "action=deletelast", false)
	case "mod_confirm":
		confirm(fmt.Sprintf("Make %s a mod?", name), "Yes, make them a mod", "action=mod", false)
	case "admin_confirm":
		confirm(fmt.Sprintf("Make %s an admin?", name), "Yes, make them an admin", "action=admin", false)
	case "demote_confirm":
		confirm(fmt.Sprintf("Take %s's %s role away?", name, who.PersonRank), "Yes, take it away", "action=demote", false)
	}
	b.line("%s", strings.Join(buttons, "  ·  "))
	b.blank()
}
