package hub

// Hub-wide moderation (ADR 0008): kick, ban, unban, rename, ranks and the
// mod log. Every action goes through may(), the rank rule, and into the
// audit log with its reason.
//
// cmdKline (rrcd's `/kline add|del|list`) and the legacy room-kick and
// room-ban detection (isLegacyRoomKick, isLegacyRoomBan) are ported from
// rrcd 0.3.2 commands.py, Copyright (c) 2025 S. Miller, KC1AWV, MIT
// License, kept because NomadNet's RRC client still sends these forms.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// targetPerson resolves a command's target to their person and checks the
// rank rule for action. ok is false when the command has already failed.
func (c *cmd) targetPerson(token string, action Action) (Person, bool, error) {
	id, ok, err := c.resolveOrFail(token)
	if !ok || err != nil {
		return Person{}, false, err
	}
	p, err := c.h.person(c.tx, id)
	if err != nil {
		return Person{}, false, err
	}
	if err := may(c.me, action, &p); err != nil {
		return p, false, c.fail("%s", err.Error())
	}
	return p, true, nil
}

// removeEverywhere takes every app of a person out of every room and the
// LXMF group, tells each why, and frees the person's name, links and rank:
// what a kick or a ban does. banned apps are disconnected by the adapters.
func (h *Hub) removeEverywhere(tx *store.Tx, out *outbox, target Person, why string, banned bool) error {
	for _, id := range target.Identities {
		wasMember, err := tx.RemoveMember(id)
		if err != nil {
			return err
		}
		if wasMember {
			out.emit(GroupEvent{Identity: id, Name: target.Name, Joined: false})
		}
		for _, room := range h.roomsOf(id) {
			if !banned {
				out.emit(RemovedEvent{Room: room, Identity: id, Reason: why})
			}
			h.forgetPresence(out, room, id)
		}
		out.emit(NoticeEvent{Identity: id, Text: why, ToLXMF: wasMember})
		if banned {
			out.emit(BannedEvent{Identity: id, Reason: why})
		}
	}
	if err := h.unlinkAll(tx, out, target.Identity); err != nil {
		return err
	}
	if target.Claimed {
		if _, _, err := tx.ReleaseName(target.Identity); err != nil {
			return err
		}
		guest := GuestName(target.Identity)
		out.emit(NameEvent{Identity: target.Identity, Old: target.Name, New: guest})
		out.then(func() { h.renamePresence(target.Identity, guest) })
	}
	if target.PersonRank == RankMod || target.PersonRank == RankAdmin {
		return tx.SetRank(target.Identity, store.RankMember, nil, h.nowMS())
	}
	return nil
}

// forgetPresence drops every RRC link of an identity from a room after it
// was put out of it.
func (h *Hub) forgetPresence(out *outbox, room string, id []byte) {
	e := h.presence[room][hexID(id)]
	if e == nil {
		return
	}
	out.emit(PartedEvent{Room: room, Identity: id, Name: e.name, Via: ViaRRC})
	out.then(func() {
		delete(h.presence[room], hexID(id))
		if len(h.presence[room]) == 0 {
			delete(h.presence, room)
		}
	})
}

func cmdKick(c *cmd) error {
	if len(c.args) == 0 {
		return c.usage()
	}
	target, ok, err := c.targetPerson(c.args[0], ActKick)
	if !ok || err != nil {
		return err
	}
	reason := strings.Join(c.args[1:], " ")
	why := fmt.Sprintf("You were kicked from %s by %s", c.h.cfg.HubName, c.me.Name)
	if reason != "" {
		why += ": " + reason
	}
	why += ". Your name is free again; you can come back as a guest."
	if err := c.h.removeEverywhere(c.tx, c.out, target, why, false); err != nil {
		return err
	}
	if err := c.tx.Audit(c.nowMS, c.req.Identity, "kick", target.Name, reason); err != nil {
		return err
	}
	c.say("Kicked %s. Their name is free.", target.Name)
	return nil
}

// isLegacyRoomKick spots rrcd's /kick <room> <name>, which NomadNet forwards.
func isLegacyRoomKick(c *cmd) bool {
	if len(c.args) != 2 {
		return false
	}
	if strings.HasPrefix(c.args[0], "#") {
		return true
	}
	room, err := c.h.normRoom(c.args[0])
	if err != nil {
		return false
	}
	if _, exists, err := c.tx.Room(room); err != nil || !exists {
		return false
	}
	_, err = c.resolve(c.args[1])
	return err == nil
}

var banLength = regexp.MustCompile(`^(\d{1,4})([mhd])$`)

// parseBanLength reads 30m, 1h, 1d, 7d or perm; ok is false for anything
// else, which is then part of the reason.
func parseBanLength(s string) (time.Duration, bool) {
	if strings.EqualFold(s, "perm") {
		return 0, true
	}
	m := banLength.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	if n == 0 {
		return 0, false
	}
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, true
}

// isLegacyRoomBan spots rrcd's /ban [room] add|del|list [name].
func isLegacyRoomBan(c *cmd) bool {
	return len(c.args) >= 1 && isListOp(c.args[0]) || len(c.args) >= 2 && isListOp(c.args[1])
}

func cmdBan(c *cmd) error {
	if len(c.args) == 0 {
		return c.usage()
	}
	target, ok, err := c.targetPerson(c.args[0], ActBan)
	if !ok || err != nil {
		return err
	}
	rest := c.args[1:]
	var length time.Duration
	if len(rest) > 0 {
		if d, ok := parseBanLength(rest[0]); ok {
			length, rest = d, rest[1:]
		}
	}
	reason := strings.Join(rest, " ")
	ban := store.Ban{Name: target.Name, Reason: reason, BannedBy: c.req.Identity, BannedAt: c.nowMS}
	until := "for good"
	if length > 0 {
		ban.ExpiresAt = c.nowMS + length.Milliseconds()
		until = "until " + c.h.clock(ban.ExpiresAt)
	}
	why := fmt.Sprintf("You were banned from %s by %s, %s", c.h.cfg.HubName, c.me.Name, until)
	if reason != "" {
		why += ": " + reason
	}
	why += "."
	for _, id := range target.Identities {
		b := ban
		b.Identity = id
		if err := c.tx.AddBan(&b); err != nil {
			return err
		}
	}
	if err := c.h.removeEverywhere(c.tx, c.out, target, why, true); err != nil {
		return err
	}
	detail := until
	if reason != "" {
		detail += ": " + reason
	}
	if err := c.tx.Audit(c.nowMS, c.req.Identity, "ban", target.Name, detail); err != nil {
		return err
	}
	c.say("Banned %s (%s), %s. Their name is free.", target.Name, plural(len(target.Identities), "app"), until)
	return nil
}

// cmdKline is rrcd's /kline add|del|list, kept for NomadNet, which forwards
// it: the hub-wide /ban, /unban and /bans.
func cmdKline(c *cmd) error {
	if len(c.args) == 0 || !isListOp(c.args[0]) {
		return c.fail("usage: /kline add|del|list [name|hashprefix|hash]")
	}
	op := strings.ToLower(c.args[0])
	c.args = c.args[1:]
	var err error
	switch op {
	case "add":
		err = cmdBan(c)
	case "del":
		err = cmdUnban(c)
	default:
		err = cmdBans(c)
	}
	if err != nil || c.reply.Error {
		return err
	}
	c.say("(next time: /%s%s)", map[string]string{"add": "ban", "del": "unban", "list": "bans"}[op], strings.TrimRight(" "+strings.Join(c.args, " "), " "))
	return nil
}

func cmdUnban(c *cmd) error {
	if len(c.args) != 1 {
		return c.usage()
	}
	if err := may(c.me, ActUnban, nil); err != nil {
		return c.fail("%s", err.Error())
	}
	bans, err := c.tx.Bans()
	if err != nil {
		return err
	}
	token := c.args[0]
	var lift []store.Ban
	if _, sk, err := CleanName(token); err == nil {
		for _, b := range bans {
			if _, bsk, err := CleanName(b.Name); err == nil && b.Name != "" && bsk == sk {
				lift = append(lift, b)
			}
		}
	}
	if len(lift) == 0 {
		id, ok, err := c.resolveOrFail(token)
		if !ok || err != nil {
			return err
		}
		for _, b := range bans {
			if bytes.Equal(b.Identity, id) {
				lift = append(lift, b)
			}
		}
	}
	if len(lift) == 0 {
		return c.fail("%s isn't banned.", token)
	}
	for _, b := range lift {
		if _, err := c.tx.RemoveBan(b.Identity); err != nil {
			return err
		}
	}
	name := lift[0].Name
	if name == "" {
		name = hexID(lift[0].Identity)[:8]
	}
	if err := c.tx.Audit(c.nowMS, c.req.Identity, "unban", name, ""); err != nil {
		return err
	}
	c.say("Unbanned %s (%s).", name, plural(len(lift), "app"))
	return nil
}

func cmdBans(c *cmd) error {
	if len(c.args) != 0 {
		return c.usage()
	}
	if err := may(c.me, ActListBans, nil); err != nil {
		return c.fail("%s", err.Error())
	}
	bans, err := c.tx.Bans()
	if err != nil {
		return err
	}
	if len(bans) == 0 {
		c.say("Nobody is banned.")
		return nil
	}
	// One line per ban, however many apps it covers.
	type group struct {
		ban  store.Ban
		apps int
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, b := range bans {
		key := fmt.Sprintf("%s|%d|%x", b.Name, b.BannedAt, b.BannedBy)
		if b.Name == "" {
			key = hexID(b.Identity)
		}
		if g := byKey[key]; g != nil {
			g.apps++
			continue
		}
		g := &group{ban: b, apps: 1}
		byKey[key] = g
		groups = append(groups, g)
	}
	c.say("Banned (%d):", len(groups))
	for _, g := range groups {
		b := g.ban
		who := b.Name
		if who == "" {
			who = hexID(b.Identity)[:8]
		}
		line := fmt.Sprintf("  %s (%s)", who, plural(g.apps, "app"))
		if b.Reason != "" {
			line += " — " + b.Reason
		}
		by := "the hub"
		if b.BannedBy != nil {
			if by, err = c.h.displayName(c.tx, b.BannedBy); err != nil {
				return err
			}
		}
		line += fmt.Sprintf(" — by %s, %s", by, c.h.clock(b.BannedAt))
		if b.ExpiresAt != 0 {
			line += ", until " + c.h.clock(b.ExpiresAt)
		}
		c.say("%s", line)
	}
	return nil
}

func cmdRename(c *cmd) error {
	if len(c.args) != 2 {
		return c.usage()
	}
	target, ok, err := c.targetPerson(c.args[0], ActRename)
	if !ok || err != nil {
		return err
	}
	after, err := c.h.claimName(c.tx, c.out, target.Identity, c.args[1])
	var ue *UserError
	if errors.As(err, &ue) {
		// The refusal's first sentence; the rest speaks to the person renamed.
		return c.fail("%s.", strings.SplitN(ue.Text, ". ", 2)[0])
	}
	if err != nil {
		return err
	}
	for _, id := range target.Identities {
		c.out.emit(NoticeEvent{Identity: id, Text: fmt.Sprintf("%s renamed you from %s to %s.", c.me.Name, target.Name, after.Name)})
	}
	if err := c.tx.Audit(c.nowMS, c.req.Identity, "rename", target.Name, after.Name); err != nil {
		return err
	}
	c.say("Renamed %s to %s.", target.Name, after.Name)
	return nil
}

func cmdMakeRank(c *cmd, rank Rank, action Action) error {
	if len(c.args) != 1 {
		return c.usage()
	}
	target, ok, err := c.targetPerson(c.args[0], action)
	if !ok || err != nil {
		return err
	}
	if target.PersonRank == rank {
		return c.fail("%s is already %s.", target.Name, article(rank))
	}
	if err := c.tx.SetRank(target.Identity, rank.stored(), c.req.Identity, c.nowMS); err != nil {
		return err
	}
	for _, id := range target.Identities {
		c.out.emit(NoticeEvent{Identity: id, Text: fmt.Sprintf("%s made you %s of %s. /help shows what you can do now.", c.me.Name, article(rank), c.h.cfg.HubName)})
	}
	if err := c.tx.Audit(c.nowMS, c.req.Identity, rank.String(), target.Name, ""); err != nil {
		return err
	}
	c.say("%s is %s now.", target.Name, article(rank))
	return nil
}

func cmdMod(c *cmd) error   { return cmdMakeRank(c, RankMod, ActMakeMod) }
func cmdAdmin(c *cmd) error { return cmdMakeRank(c, RankAdmin, ActMakeAdmin) }

func cmdDemote(c *cmd) error {
	if len(c.args) != 1 {
		return c.usage()
	}
	target, ok, err := c.targetPerson(c.args[0], ActDemote)
	if !ok || err != nil {
		return err
	}
	if target.PersonRank == RankMember {
		return c.fail("%s has no role to take away.", target.Name)
	}
	if err := c.tx.SetRank(target.Identity, store.RankMember, nil, c.nowMS); err != nil {
		return err
	}
	for _, id := range target.Identities {
		c.out.emit(NoticeEvent{Identity: id, Text: fmt.Sprintf("%s took away your %s role.", c.me.Name, target.PersonRank)})
	}
	if err := c.tx.Audit(c.nowMS, c.req.Identity, "demote", target.Name, target.PersonRank.String()); err != nil {
		return err
	}
	c.say("%s is a member again.", target.Name)
	return nil
}

func cmdModlog(c *cmd) error {
	if len(c.args) > 1 {
		return c.usage()
	}
	if err := may(c.me, ActModlog, nil); err != nil {
		return c.fail("%s", err.Error())
	}
	n := 10
	if len(c.args) == 1 {
		v, err := strconv.Atoi(c.args[0])
		if err != nil || v <= 0 {
			return c.usage()
		}
		n = min(v, 50)
	}
	entries, err := c.tx.AuditLog(n)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		c.say("The mod log is empty.")
		return nil
	}
	c.say("The last %s, newest first:", plural(len(entries), "action"))
	for _, e := range entries {
		by := "the hub"
		if e.Actor != nil {
			if by, err = c.h.displayName(c.tx, e.Actor); err != nil {
				return err
			}
		}
		line := fmt.Sprintf("  %s · %s · %s", c.h.clock(e.At), by, e.Action)
		if e.Target != "" {
			line += " " + e.Target
		}
		if e.Detail != "" {
			line += " — " + e.Detail
		}
		c.say("%s", line)
	}
	return nil
}

// liftExpiredBans runs from maintenance: timed bans end on the minute.
func (h *Hub) liftExpiredBans(tx *store.Tx, now int64) error {
	expired, err := tx.ExpiredBans(now)
	if err != nil {
		return err
	}
	lifted := map[string]bool{}
	for _, b := range expired {
		if _, err := tx.RemoveBan(b.Identity); err != nil {
			return err
		}
		name := b.Name
		if name == "" {
			name = hexID(b.Identity)[:8]
		}
		if !lifted[name] {
			lifted[name] = true
			if err := tx.Audit(now, nil, "unban", name, "the ban ran out"); err != nil {
				return err
			}
		}
	}
	return nil
}

func cmdDelete(c *cmd) error {
	if len(c.args) == 0 || len(c.args) > 2 {
		return c.usage()
	}
	n := 1
	if len(c.args) == 2 {
		v, err := strconv.Atoi(c.args[1])
		if err != nil || v < 1 || v > 20 {
			return c.fail("usage: /delete <name> [n], with n from 1 to 20")
		}
		n = v
	}
	target, ok, err := c.targetPerson(c.args[0], ActDelete)
	if !ok || err != nil {
		return err
	}
	room := c.here()
	msgs, err := c.tx.LatestMessagesByPerson(target.PersonID, room, n)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return c.fail("%s has no messages in #%s to remove.", target.Name, room)
	}
	return c.h.deleteMessages(c.tx, c.out, &c.me, target, msgs, c.reply)
}

// DeleteMessage removes one message for the page's delete link, with the
// same rules as /delete.
func (h *Hub) DeleteMessage(ctx context.Context, actor []byte, messageID int64) (Reply, error) {
	var reply Reply
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		reply = Reply{}
		if err := h.checkHubBan(tx, actor); err != nil {
			return err
		}
		me, err := h.person(tx, actor)
		if err != nil {
			return err
		}
		m, ok, err := tx.Message(messageID)
		if err != nil {
			return err
		}
		if !ok {
			return refuse("That message is already gone.")
		}
		target, err := h.person(tx, m.Author)
		if err != nil {
			return err
		}
		if err := may(me, ActDelete, &target); err != nil {
			return err
		}
		return h.deleteMessages(tx, out, &me, target, []store.Message{m}, &reply)
	})
	var ue *UserError
	if errors.As(err, &ue) {
		return Reply{Lines: []string{ue.Text}, Error: true}, nil
	}
	return reply, err
}

func (h *Hub) deleteMessages(tx *store.Tx, out *outbox, actor *Person, target Person, msgs []store.Message, reply *Reply) error {
	ids := make([]int64, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].ID
	}
	cancelled, err := tx.DeleteMessages(ids)
	if err != nil {
		return err
	}
	room := msgs[0].Room
	what := "message"
	if len(msgs) > 1 {
		what = fmt.Sprintf("%d messages", len(msgs))
	}
	if actor.PersonID == target.PersonID {
		out.emit(RoomNoticeEvent{Room: room, Text: fmt.Sprintf("%s removed their last %s.", target.Name, what)})
	} else {
		out.emit(RoomNoticeEvent{Room: room, Text: fmt.Sprintf("%s's last %s removed by %s.", target.Name, what, actor.Name)})
	}
	excerpt := msgs[len(msgs)-1].Body
	if len(excerpt) > 60 {
		excerpt = strings.ToValidUTF8(excerpt[:60], "") + "…"
	}
	if err := tx.Audit(h.nowMS(), actor.Identity, "delete", target.Name, fmt.Sprintf("%s in #%s: %q", what, room, excerpt)); err != nil {
		return err
	}
	reply.Lines = append(reply.Lines, fmt.Sprintf("Removed %s's last %s in #%s from history and the chat page. Copies already delivered stay in people's apps.", target.Name, what, room))
	if cancelled > 0 {
		reply.Lines = append(reply.Lines, fmt.Sprintf("%s by LXMF not sent yet won't be.", plural(int(cancelled), "copy")))
	}
	return nil
}
