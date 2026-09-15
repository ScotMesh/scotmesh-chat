package hub

// The `/room` operator commands (kick, ban, invite, op, voice, mode,
// register) and the `/who` and `/list` reply texts are ported from rrcd
// 0.3.2 commands.py, Copyright (c) 2025 S. Miller, KC1AWV, MIT License.
// NomadNet parses the `/who` and `/list` replies, so those texts are exact.
// legacyRoomCommands and legacyRoom accept rrcd's older room-level command
// forms (`/kick <room> <name>` and similar), which some clients still send.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

func cmdWho(c *cmd) error {
	t, ok, err := c.takeTarget(true)
	if !ok || err != nil {
		return err
	}
	if len(c.args) > 0 {
		return c.usage()
	}
	room := t.room
	r, exists, err := c.tx.Room(room)
	if err != nil {
		return err
	}
	if exists {
		if ok, err := c.h.mayReadRoom(c.tx, &r, c.req.Identity); err != nil {
			return err
		} else if !ok {
			return c.fail("room %s is private", room)
		}
	}
	members := c.h.members(c.tx, room)
	// NomadNet parses the first line: "members in <room>: nick (hex12), ...".
	if len(members) == 0 {
		c.say("members in %s: (none)", room)
		return nil
	}
	parts := make([]string, len(members))
	var rrc, lxmf []string
	for i, m := range members {
		parts[i] = fmt.Sprintf("%s (%s)", m.Name, hex.EncodeToString(m.Identity)[:12])
		if m.RRC {
			rrc = append(rrc, m.Name)
		}
		if m.LXMF {
			lxmf = append(lxmf, m.Name)
		}
	}
	c.say("members in %s: %s", room, strings.Join(parts, ", "))
	var where []string
	if len(rrc) > 0 {
		where = append(where, "on RRC: "+strings.Join(rrc, ", "))
	}
	if len(lxmf) > 0 {
		where = append(where, "in the LXMF group: "+strings.Join(lxmf, ", "))
	}
	c.say("%s", strings.Join(where, " · "))
	return nil
}

func cmdList(c *cmd) error {
	if len(c.args) > 0 {
		return c.usage()
	}
	rooms, err := c.tx.Rooms()
	if err != nil {
		return err
	}
	var lines []string
	for _, r := range rooms {
		if !r.Registered || r.HasMode('p') {
			continue
		}
		if r.Topic != "" {
			lines = append(lines, fmt.Sprintf("  %s - %s", r.Name, r.Topic))
		} else {
			lines = append(lines, "  "+r.Name)
		}
	}
	if len(lines) == 0 {
		c.say("No public rooms registered")
		return nil
	}
	// NomadNet and MeshChatX parse this block: a header line, then
	// "  name - topic", all in one notice.
	c.reply.Whole = true
	c.sayLines("Registered public rooms:\n" + strings.Join(lines, "\n"))
	return nil
}

func cmdHistory(c *cmd) error {
	t, ok, err := c.takeTarget(true)
	if !ok || err != nil {
		return err
	}
	n := 20
	if len(c.args) > 1 {
		return c.usage()
	}
	if len(c.args) == 1 {
		v, err := strconv.Atoi(c.args[0])
		if err != nil || v <= 0 {
			return c.usage()
		}
		n = v
	}
	msgs, err := c.h.history(c.tx, c.req.Identity, t.room, n, 0)
	var ue *UserError
	if errors.As(err, &ue) {
		return c.fail("%s", ue.Text)
	}
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		c.say("No messages in #%s yet.", t.room)
		return nil
	}
	c.reply.History = msgs
	c.reply.HistoryNote = fmt.Sprintf("— the last %s in #%s —", plural(len(msgs), "message"), t.room)
	return nil
}

func cmdTopic(c *cmd) error {
	t, ok, err := c.takeTarget(true)
	if !ok || err != nil {
		return err
	}
	room := t.room
	r, exists, err := c.tx.Room(room)
	if err != nil {
		return err
	}
	if len(c.args) == 0 {
		topic := r.Topic
		if !exists || topic == "" {
			topic = "(none)"
		}
		c.say("topic for %s: %s", room, topic)
		return nil
	}
	if !exists {
		return c.fail("no such room")
	}
	if err := c.h.checkRoomBan(c.tx, room, c.req.Identity); err != nil {
		return err
	}
	if r.HasMode('t') {
		op, err := c.h.isRoomOp(c.tx, &r, c.req.Identity)
		if err != nil {
			return err
		}
		if !op {
			return c.fail("not authorized (+t)")
		}
	}
	topic := strings.Join(c.args, " ")
	if len(topic) > c.h.cfg.MaxTopicBytes {
		return c.fail("topic too long: %d bytes > %d bytes", len(topic), c.h.cfg.MaxTopicBytes)
	}
	r.Topic = topic
	if err := c.tx.PutRoom(&r); err != nil {
		return err
	}
	c.out.emit(RoomNoticeEvent{Room: room, Text: fmt.Sprintf("topic for %s is now: %s", room, r.Topic)})
	c.say("topic for %s is now: %s", room, r.Topic)
	return nil
}

// --- /room: rrcd's room operator commands ----------------------------------

const roomHelp = "room operator commands, for the room you're in unless you name one. " +
	"/room kick [#room] <name>; /room ban|unban [#room] <name>; /room bans [#room]; " +
	"/room invite|uninvite [#room] <name>; /room invites [#room]; /room op|deop|voice|devoice [#room] <name>; " +
	"/room mode [#room] <+m|-m|+i|-i|+k key|-k|+t|-t|+n|-n|+p|-p|+o|-o|+v|-v name>; /room register|unregister [#room]"

var roomUsage = map[string]string{
	"kick":       "<name|hashprefix|hash>",
	"ban":        "<name|hashprefix|hash>",
	"unban":      "<name|hashprefix|hash>",
	"bans":       "",
	"invite":     "<name|hashprefix|hash>",
	"uninvite":   "<name|hashprefix|hash>",
	"invites":    "",
	"op":         "<name|hashprefix|hash>",
	"deop":       "<name|hashprefix|hash>",
	"voice":      "<name|hashprefix|hash>",
	"devoice":    "<name|hashprefix|hash>",
	"mode":       "<flag> [arg]",
	"register":   "",
	"unregister": "",
}

func cmdRoom(c *cmd) error {
	if len(c.args) == 0 {
		return c.usage()
	}
	verb := strings.ToLower(c.args[0])
	c.args = c.args[1:]
	if _, ok := roomUsage[verb]; !ok {
		return c.fail("There's no /room %s. The verbs are kick, ban, unban, bans, invite, uninvite, invites, op, deop, voice, devoice, mode, register and unregister.", verb)
	}
	t, ok, err := c.takeTarget(false)
	if !ok || err != nil {
		return err
	}
	if t.lxmf {
		return c.roomUsageFail(verb)
	}
	return c.roomVerb(verb, t.room)
}

func (c *cmd) roomUsageFail(verb string) error {
	u := "/room " + verb + " [#room]"
	if a := roomUsage[verb]; a != "" {
		u += " " + a
	}
	return c.fail("usage: %s", u)
}

func (c *cmd) roomVerb(verb, room string) error {
	switch verb {
	case "kick":
		return roomKick(c, verb, room)
	case "ban", "unban", "bans":
		return roomBan(c, verb, room)
	case "invite", "uninvite", "invites":
		return roomInvite(c, verb, room)
	case "op", "deop", "voice", "devoice":
		return roomRole(c, verb, room)
	case "mode":
		return roomMode(c, room)
	default: // register, unregister
		return roomRegister(c, verb, room)
	}
}

// legacyRoomCommands are rrcd's top-level forms (/kick <room> <name>,
// /ban <room> add <name>, ...). NomadNet's RRC client forwards exactly these,
// so they keep working; each adds a hint with the /room form. They are on no
// help page.
func legacyRoomCommands() []*command {
	usage := map[string]string{
		"kick": "[room] <name|hashprefix|hash>", "ban": "[room] add|del|list [name|hashprefix|hash]",
		"invite": "[room] add|del|list [name|hashprefix|hash]", "op": "[room] <name|hashprefix|hash>",
		"deop": "[room] <name|hashprefix|hash>", "voice": "[room] <name|hashprefix|hash>",
		"devoice": "[room] <name|hashprefix|hash>", "mode": "[room] <flag> [arg]",
		"register": "[room]", "unregister": "[room]",
	}
	out := make([]*command, 0, len(usage))
	// /kick and /ban are hub-wide now; they fall back to these forms themselves.
	for _, verb := range []string{"invite", "op", "deop", "voice", "devoice", "mode", "register", "unregister"} {
		out = append(out, &command{
			name: verb, args: usage[verb], help: "rrcd's form of /room " + verb + ", kept for now: " + roomHelp,
			run: func(c *cmd) error { return legacyRoom(c, verb) },
		})
	}
	return out
}

func legacyRoom(c *cmd, verb string) error {
	args := c.args
	room := ""
	switch verb {
	case "kick", "op", "deop", "voice", "devoice":
		if len(args) == 2 {
			room, args = args[0], args[1:]
		}
	case "ban", "invite":
		if len(args) > 0 && !isListOp(args[0]) {
			room, args = args[0], args[1:]
		}
		if len(args) == 0 {
			return c.fail("usage: /%s %s", verb, c.def.args)
		}
		op := strings.ToLower(args[0])
		args = args[1:]
		mapped := map[string]map[string]string{
			"ban":    {"add": "ban", "del": "unban", "list": "bans"},
			"invite": {"add": "invite", "del": "uninvite", "list": "invites"},
		}[verb][op]
		if mapped == "" {
			return c.fail("usage: /%s %s", verb, c.def.args)
		}
		verb = mapped
	case "mode":
		if len(args) > 0 && !strings.HasPrefix(args[0], "+") && !strings.HasPrefix(args[0], "-") {
			room, args = args[0], args[1:]
		}
	default: // register, unregister
		if len(args) == 1 {
			room, args = args[0], nil
		}
	}
	if room == "" {
		room = c.here()
	} else {
		r, err := c.h.normRoom(room)
		var ue *UserError
		if errors.As(err, &ue) {
			return c.fail("bad room: %s", ue.Text)
		} else if err != nil {
			return err
		}
		room = r
	}
	c.args = args
	if err := c.roomVerb(verb, room); err != nil || c.reply.Error {
		return err
	}
	c.say("(next time: /room %s #%s%s)", verb, room, strings.TrimRight(" "+strings.Join(args, " "), " "))
	return nil
}

func isListOp(s string) bool {
	switch strings.ToLower(s) {
	case "add", "del", "list":
		return true
	}
	return false
}

// opRoom checks the room exists and the sender is its operator.
func (c *cmd) opRoom(room string) (store.Room, bool, error) {
	r, exists, err := c.tx.Room(room)
	if err != nil {
		return r, false, err
	}
	if !exists {
		return r, false, c.fail("no such room")
	}
	op, err := c.h.isRoomOp(c.tx, &r, c.req.Identity)
	if err != nil {
		return r, false, err
	}
	if !op {
		return r, false, c.fail("not authorized")
	}
	return r, true, nil
}

// resolve finds the identity a command names: a claimed name, a full hash,
// a guest name, or a hash prefix of at least 6 hex digits among people
// present, group members and banned identities.
func (c *cmd) resolve(token string) ([]byte, error) {
	token = strings.TrimSpace(token)
	if b, err := hex.DecodeString(strings.TrimPrefix(token, "0x")); err == nil && len(b) == store.IdentityLen {
		return b, nil
	}
	if _, sk, err := CleanName(token); err == nil {
		n, ok, err := c.tx.NameBySkeleton(sk)
		if err != nil {
			return nil, err
		}
		if ok {
			return n.Identity, nil
		}
	}
	prefix := strings.ToLower(strings.TrimPrefix(token, "0x"))
	prefix = strings.TrimPrefix(prefix, "guest-")
	if len(prefix) < 4 || !isHex(prefix) || (len(prefix) < 6 && !strings.HasPrefix(strings.ToLower(token), "guest-")) {
		return nil, refuse("target '%s' not found", token)
	}
	var matches [][]byte
	seen := map[string]bool{}
	add := func(id []byte) {
		k := hexID(id)
		if strings.HasPrefix(k, prefix) && !seen[k] {
			seen[k] = true
			matches = append(matches, id)
		}
	}
	for _, members := range c.h.presence {
		for _, e := range members {
			add(e.identity)
		}
	}
	group, err := c.tx.Members()
	if err != nil {
		return nil, err
	}
	for _, m := range group {
		add(m.Identity)
	}
	bans, err := c.tx.Bans()
	if err != nil {
		return nil, err
	}
	for _, b := range bans {
		add(b.Identity)
	}
	switch len(matches) {
	case 0:
		return nil, refuse("target '%s' not found", token)
	case 1:
		return matches[0], nil
	}
	sort.Slice(matches, func(i, j int) bool { return hexID(matches[i]) < hexID(matches[j]) })
	lines := []string{fmt.Sprintf("ambiguous: '%s' matches %d identities:", token, len(matches))}
	for _, m := range matches {
		name, _ := c.h.displayName(c.tx, m)
		lines = append(lines, fmt.Sprintf("  - %s %s", hexID(m)[:16], name))
	}
	lines = append(lines, "Use full or longer identity hash to disambiguate.")
	return nil, refuse("%s", strings.Join(lines, "\n"))
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		digit := ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f'
		if !digit {
			return false
		}
	}
	return true
}

func (c *cmd) resolveOrFail(token string) ([]byte, bool, error) {
	id, err := c.resolve(token)
	var ue *UserError
	if errors.As(err, &ue) {
		c.reply.Error = true
		c.sayLines(ue.Text)
		return nil, false, nil
	}
	return id, err == nil, err
}

func roomKick(c *cmd, verb, room string) error {
	if len(c.args) != 1 {
		return c.roomUsageFail(verb)
	}
	r, ok, err := c.opRoom(room)
	if !ok || err != nil {
		return err
	}
	target, ok, err := c.resolveOrFail(c.args[0])
	if !ok || err != nil {
		return err
	}
	if err := c.h.mayActOnRoomTarget(c.tx, c.me, target); err != nil {
		return err
	}
	name, err := c.h.displayName(c.tx, target)
	if err != nil {
		return err
	}
	inRoom := c.h.rrcPresent(r.Name, target)
	member := false
	if r.Name == c.h.cfg.GroupRoom {
		if member, err = c.tx.RemoveMember(target); err != nil {
			return err
		}
		if member {
			c.out.emit(GroupEvent{Identity: target, Name: name, Joined: false})
		}
	}
	if !inRoom && !member {
		return c.fail("target not in room")
	}
	if inRoom {
		c.out.emit(RemovedEvent{Room: r.Name, Identity: target, Reason: "kicked from " + r.Name})
		c.h.forgetPresence(c.out, r.Name, target)
	}
	if err := c.tx.Audit(c.nowMS, c.req.Identity, "kick", name, r.Name); err != nil {
		return err
	}
	c.say("kicked %s from %s", c.args[0], r.Name)
	return nil
}

func roomBan(c *cmd, verb, room string) error {
	r, exists, err := c.tx.Room(room)
	if err != nil {
		return err
	}
	if !exists {
		return c.fail("no such room")
	}
	if verb == "bans" {
		if len(c.args) != 0 {
			return c.roomUsageFail(verb)
		}
		bans, err := c.tx.RoleHolders(room, store.RoleBan)
		if err != nil {
			return err
		}
		if len(bans) == 0 {
			c.say("no bans in %s", room)
			return nil
		}
		parts := make([]string, len(bans))
		for i, b := range bans {
			name, _ := c.h.displayName(c.tx, b)
			parts[i] = fmt.Sprintf("%s (%s)", name, hexID(b))
		}
		c.say("bans in %s: %s", room, strings.Join(parts, ", "))
		return nil
	}
	if len(c.args) != 1 {
		return c.roomUsageFail(verb)
	}
	isOp, err := c.h.isRoomOp(c.tx, &r, c.req.Identity)
	if err != nil {
		return err
	}
	if !isOp {
		return c.fail("not authorized")
	}
	target, ok, err := c.resolveOrFail(c.args[0])
	if !ok || err != nil {
		return err
	}
	name, err := c.h.displayName(c.tx, target)
	if err != nil {
		return err
	}
	if verb == "unban" {
		if err := c.tx.SetRole(room, target, store.RoleBan, false, nil, c.nowMS); err != nil {
			return err
		}
		if err := c.tx.Audit(c.nowMS, c.req.Identity, "unban", name, room); err != nil {
			return err
		}
		c.say("ban removed in %s", room)
		return nil
	}
	if err := c.h.mayActOnRoomTarget(c.tx, c.me, target); err != nil {
		return err
	}
	if err := c.tx.TouchIdentity(target, nil, c.nowMS); err != nil {
		return err
	}
	if err := c.tx.SetRole(room, target, store.RoleBan, true, c.req.Identity, c.nowMS); err != nil {
		return err
	}
	if c.h.rrcPresent(room, target) {
		c.out.emit(RemovedEvent{Room: room, Identity: target, Reason: "banned from " + room})
		c.h.forgetPresence(c.out, room, target)
	}
	if room == c.h.cfg.GroupRoom {
		removed, err := c.tx.RemoveMember(target)
		if err != nil {
			return err
		}
		if removed {
			c.out.emit(GroupEvent{Identity: target, Name: name, Joined: false})
		}
	}
	if err := c.tx.Audit(c.nowMS, c.req.Identity, "ban", name, room); err != nil {
		return err
	}
	c.say("ban added in %s", room)
	return nil
}

func roomInvite(c *cmd, verb, room string) error {
	if (verb == "invites") != (len(c.args) == 0) || len(c.args) > 1 {
		return c.roomUsageFail(verb)
	}
	r, ok, err := c.opRoom(room)
	if !ok || err != nil {
		return err
	}
	if verb == "invites" {
		inv, err := c.tx.Invites(r.Name, c.nowMS)
		if err != nil {
			return err
		}
		if len(inv) == 0 {
			c.say("invites in %s: (none)", r.Name)
			return nil
		}
		parts := make([]string, len(inv))
		for i, in := range inv {
			parts[i] = fmt.Sprintf("%s expires_in=%ds", hexID(in.Identity), (in.ExpiresAt-c.nowMS)/1000)
		}
		c.say("invites in %s: %s", r.Name, strings.Join(parts, ", "))
		return nil
	}
	target, ok, err := c.resolveOrFail(c.args[0])
	if !ok || err != nil {
		return err
	}
	if verb == "uninvite" {
		if err := c.tx.DeleteInvite(r.Name, target); err != nil {
			return err
		}
		c.say("invite removed in %s", r.Name)
		return nil
	}
	if err := c.tx.TouchIdentity(target, nil, c.nowMS); err != nil {
		return err
	}
	text := fmt.Sprintf("You have been invited to join %s.", r.Name)
	if r.Key != "" {
		text += " This invite allows joining without the key (+k)."
	}
	c.out.emit(NoticeEvent{Identity: target, Room: r.Name, Text: text})
	if r.HasMode('i') || r.Key != "" {
		ttl := c.h.cfg.InviteTTL
		if err := c.tx.SetInvite(r.Name, target, c.nowMS+ttl.Milliseconds()); err != nil {
			return err
		}
		c.say("invite added in %s (expires in %ds)", r.Name, int(ttl.Seconds()))
		return nil
	}
	c.say("invite sent to %s for %s", c.args[0], r.Name)
	return nil
}

func roomRole(c *cmd, verb, room string) error {
	if len(c.args) != 1 {
		return c.roomUsageFail(verb)
	}
	r, ok, err := c.opRoom(room)
	if !ok || err != nil {
		return err
	}
	target, ok, err := c.resolveOrFail(c.args[0])
	if !ok || err != nil {
		return err
	}
	return setRole(c, &r, verb, target)
}

func setRole(c *cmd, r *store.Room, verb string, target []byte) error {
	role, on := store.RoleOp, true
	msg := "op granted in %s"
	switch verb {
	case "deop":
		on, msg = false, "op removed in %s"
		if r.Founder != nil && bytes.Equal(r.Founder, target) && c.me.Rank != RankOwner {
			return c.fail("cannot deop founder")
		}
		if err := c.h.mayActOnRoomTarget(c.tx, c.me, target); err != nil {
			return err
		}
	case "voice":
		role, msg = store.RoleVoice, "voice granted in %s"
	case "devoice":
		role, on, msg = store.RoleVoice, false, "voice removed in %s"
	}
	if err := c.tx.TouchIdentity(target, nil, c.nowMS); err != nil {
		return err
	}
	if err := c.tx.SetRole(r.Name, target, role, on, c.req.Identity, c.nowMS); err != nil {
		return err
	}
	c.say(msg, r.Name)
	return nil
}

func roomMode(c *cmd, room string) error {
	if len(c.args) == 0 {
		return c.fail("usage: /room mode [#room] (+m|-m|+i|-i|+k <key>|-k|+t|-t|+n|-n|+p|-p|+o|-o|+v|-v <name>)")
	}
	r, ok, err := c.opRoom(room)
	if !ok || err != nil {
		return err
	}
	flag := strings.ToLower(c.args[0])
	rest := c.args[1:]
	switch flag {
	case "+m", "-m", "+i", "-i", "+t", "-t", "+n", "-n", "+p", "-p":
		r.SetMode(flag[1], flag[0] == '+')
	case "+k":
		if len(rest) == 0 {
			return c.fail("usage: /room mode [#room] +k <key>")
		}
		r.Key = strings.Join(rest, " ")
	case "-k":
		r.Key = ""
	case "+r", "-r":
		return c.fail("use /room register or /room unregister to change +r")
	case "+o", "-o", "+v", "-v":
		if len(rest) != 1 {
			return c.fail("usage: /room mode [#room] (+o|-o|+v|-v) <name|hashprefix|hash>")
		}
		target, ok, err := c.resolveOrFail(rest[0])
		if !ok || err != nil {
			return err
		}
		verb := map[string]string{"+o": "op", "-o": "deop", "+v": "voice", "-v": "devoice"}[flag]
		before := len(c.reply.Lines)
		if err := setRole(c, &r, verb, target); err != nil || c.reply.Error {
			return err
		}
		c.reply.Lines = c.reply.Lines[:before]
		text := fmt.Sprintf("mode for %s is now: %s %s", r.Name, flag, hexID(target)[:12])
		c.out.emit(RoomNoticeEvent{Room: r.Name, Text: text})
		c.say("%s", text)
		return nil
	default:
		return c.fail("supported modes: +m -m +i -i +k -k +t -t +n -n +p -p +r -r +o -o +v -v")
	}
	if err := c.tx.PutRoom(&r); err != nil {
		return err
	}
	text := fmt.Sprintf("mode for %s is now: %s", r.Name, modeString(&r))
	c.out.emit(RoomNoticeEvent{Room: r.Name, Text: text})
	c.say("%s", text)
	return nil
}

// modeString is rrcd's mode summary: letters in "ikmnprt" order, or "(none)".
func modeString(r *store.Room) string {
	const order = "ikmnprt"
	var b strings.Builder
	for i := 0; i < len(order); i++ {
		m := order[i]
		switch {
		case m == 'k' && r.Key != "", m == 'r' && r.Registered, m != 'k' && m != 'r' && r.HasMode(m):
			b.WriteByte(m)
		}
	}
	if b.Len() == 0 {
		return "(none)"
	}
	return "+" + b.String()
}

func roomRegister(c *cmd, verb, room string) error {
	if len(c.args) != 0 {
		return c.roomUsageFail(verb)
	}
	if c.req.Room != room || !c.h.rrcPresent(room, c.req.Identity) {
		return c.fail("must be present in the room to %s it", verb)
	}
	r, ok, err := c.tx.Room(room)
	if err != nil || !ok {
		return err
	}
	if r.Founder == nil || !bytes.Equal(r.Founder, c.req.Identity) {
		return c.fail("only the room founder can %s", verb)
	}
	if verb == "register" {
		r.Registered = true
		r.SetMode('n', true)
		r.SetMode('t', true)
		if err := c.tx.SetRole(room, r.Founder, store.RoleOp, true, r.Founder, c.nowMS); err != nil {
			return err
		}
		if err := c.tx.PutRoom(&r); err != nil {
			return err
		}
		c.say("registered room %s", room)
		return nil
	}
	if c.h.cfg.isDefaultRoom(room) {
		return c.fail("room %s is one of the hub's own rooms and stays registered", room)
	}
	if !r.Registered {
		return c.fail("room %s is not registered", room)
	}
	r.Registered = false
	if err := c.tx.PutRoom(&r); err != nil {
		return err
	}
	c.say("unregistered room %s", room)
	return nil
}
