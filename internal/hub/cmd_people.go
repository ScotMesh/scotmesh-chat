package hub

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// lxmfJoinHowTo is the answer wherever the LXMF group can't be joined: it
// needs the member's LXMF address, which the hub only learns from a message.
func (c *cmd) lxmfJoinHowTo() {
	c.say("To get the chat by LXMF, send /join to %s from your LXMF app (Sideband, MeshChatX, Columba). Joining needs your LXMF address, which the hub only learns from a message.", c.h.groupAddress())
}

func (h *Hub) groupAddress() string {
	if h.cfg.GroupAddress != "" {
		return h.cfg.GroupAddress
	}
	return "the " + h.cfg.HubName + " group"
}

func cmdJoin(c *cmd) error {
	t, ok, err := c.takeTarget(true)
	if !ok || err != nil {
		return err
	}
	group := c.h.cfg.GroupRoom
	switch c.req.Via {
	case ViaRRC:
		if t.lxmf {
			c.lxmfJoinHowTo()
			return nil
		}
		room := t.room
		if !t.given && len(c.args) == 1 {
			room = c.args[0]
		}
		c.say("Join rooms from your RRC app: in MeshChatX, type %s in the room name box under the hub; in NomadNet, send /join %s.", strings.TrimPrefix(room, "#"), strings.TrimPrefix(room, "#"))
		return nil
	case ViaPage:
		switch {
		case t.lxmf || !t.given:
			c.lxmfJoinHowTo()
		case t.room == group:
			c.say("You're reading #%s here already; post with Say.", group)
		default:
			c.say("The chat page carries #%s only. Other rooms are joined over RRC.", group)
		}
		return nil
	}

	// Over LXMF: join the group, optionally picking a name.
	if t.given && !t.lxmf && t.room != group {
		return c.fail("The LXMF group carries #%s only. Other rooms are joined over RRC.", group)
	}
	if len(c.args) > 1 {
		return c.fail("A name can't contain spaces.")
	}
	name := ""
	if len(c.args) == 1 {
		name = c.args[0]
	}
	was := c.me.Member
	p, note, err := c.h.joinGroup(c.tx, c.out, c.req.Identity, name)
	var ue *UserError
	if errors.As(err, &ue) {
		return c.fail("%s", ue.Text)
	}
	if err != nil {
		return err
	}
	if was {
		c.say("You're already in the group, as %s.", p.Name)
	} else {
		c.say("Welcome to %s, %s. Everyone in #%s on RRC and on the chat page sees what you send here.", c.h.cfg.HubName, p.Name, group)
		c.say("You'll get every message, your own from RRC and the page included. /lxmf auto pauses it while you're on RRC; /lxmf mine off stops your own.")
	}
	if note != "" {
		c.say("%s", note)
	} else if !p.Claimed {
		c.say("Pick a name with /nick YourName so people know who you are.")
	}
	if !was {
		c.say("/help lists the commands.")
	}
	return nil
}

func cmdLeave(c *cmd) error {
	t, ok, err := c.takeTarget(false)
	if !ok || err != nil {
		return err
	}
	if len(c.args) > 0 {
		return c.usage()
	}
	group := c.h.cfg.GroupRoom
	if c.req.Via == ViaRRC && !t.lxmf {
		if !c.h.rrcPresent(t.room, c.req.Identity) {
			return c.fail("You're not in #%s.", t.room)
		}
		c.reply.PartRoom = t.room
		c.say("You've left #%s.", t.room)
		return nil
	}
	if t.given && !t.lxmf && t.room != group {
		return c.fail("The LXMF group carries #%s only. Leave other rooms from your RRC app.", group)
	}
	left, err := c.h.leaveGroup(c.tx, c.out, c.req.Identity)
	if err != nil {
		return err
	}
	if !left {
		return c.fail("You're not in the LXMF group.")
	}
	c.say("You've left the LXMF group; nothing more comes to you by LXMF. Your name stays yours (/forget frees it). To come back, send /join to %s from your LXMF app.", c.h.groupAddress())
	return nil
}

func cmdForget(c *cmd) error {
	if len(c.args) > 0 {
		return c.usage()
	}
	if !c.me.Claimed && !c.me.Member {
		c.say("You have no name and aren't in the group, so there's nothing to forget.")
		return nil
	}
	after, err := c.h.forget(c.tx, c.out, c.req.Identity)
	if err != nil {
		return err
	}
	var parts []string
	if c.me.Claimed {
		parts = append(parts, fmt.Sprintf("%s is free again and you are shown as %s", c.me.Name, after.Name))
	}
	if c.me.Member {
		parts = append(parts, "you have left the LXMF group")
	}
	c.say("Done: %s.", strings.Join(parts, ", and "))
	return nil
}

func cmdNick(c *cmd) error {
	if len(c.args) != 1 {
		if len(c.args) == 0 {
			return c.usage()
		}
		return c.fail("A name can't contain spaces.")
	}
	after, err := c.h.claimName(c.tx, c.out, c.req.Identity, c.args[0])
	var ue *UserError
	if errors.As(err, &ue) {
		return c.fail("%s", ue.Text)
	}
	if err != nil {
		return err
	}
	if c.me.Claimed && c.me.Name != after.Name {
		c.say("You are now %s. %s is free for anyone.", after.Name, c.me.Name)
	} else {
		c.say("You are now %s. The name is yours on RRC, the LXMF group and the page until you /forget it.", after.Name)
	}
	return nil
}

// cmdMe is for a client that sends "/me" as a command rather than an action:
// the adapters normally say actions themselves, with their message IDs.
func cmdMe(c *cmd) error {
	if len(c.args) == 0 {
		return c.usage()
	}
	_, err := c.h.post(c.tx, c.out, PostRequest{Via: c.req.Via, Identity: c.req.Identity, Room: c.here(), Action: true, Body: strings.Join(c.args, " ")})
	var ue *UserError
	if errors.As(err, &ue) {
		return c.fail("Not sent: %s", ue.Text)
	}
	return err
}

func cmdWhoami(c *cmd) error {
	name := c.me.Name
	if c.me.Claimed {
		name += fmt.Sprintf(" (yours since %s)", time.UnixMilli(c.me.ClaimedAt).In(london).Format("2 Jan 2006"))
	} else {
		name += " (no name claimed yet: /nick YourName)"
	}
	c.say("You are %s.", name)
	c.say("Identity %s.", hex.EncodeToString(c.me.Identity))
	var where []string
	if rooms := c.h.roomsOf(c.me.Identity); len(rooms) > 0 {
		where = append(where, "RRC in #"+strings.Join(rooms, ", #"))
	}
	if c.me.Member {
		where = append(where, "the LXMF group")
	}
	if len(where) == 0 {
		c.say("You're not in any room or the group right now.")
	} else {
		c.say("Here via %s.", strings.Join(where, " and "))
	}
	mine := "off"
	if c.me.LXMFMine {
		mine = "on"
	}
	if c.me.Member {
		c.say("LXMF delivery: %s; your own messages: %s.", describeMode(c, c.me), mine)
	}
	if c.me.Linked() {
		c.say("%s shares your name with %s (/devices).", c.me.Name, plural(len(c.me.Identities)-1, "other app"))
	}
	if c.me.Rank > RankMember {
		c.say("You are %s of this hub.", article(c.me.Rank))
	}
	return nil
}

func describeMode(c *cmd, p Person) string {
	switch p.LXMFMode {
	case store.LXMFOff:
		return "off"
	case store.LXMFAuto:
		if on, err := c.h.personOnRRC(c.tx, p.Identity, c.h.cfg.GroupRoom, nil); err == nil && on {
			return "auto, paused while you're on RRC"
		}
		return "auto, on"
	}
	return "on"
}

// notMemberYet is said after an LXMF setting changed by someone not in the
// group: it is kept, but joining sets delivery on with their own messages.
func (c *cmd) notMemberYet(member bool) {
	if !member {
		c.say("Saved, but you're not in the LXMF group. Joining it turns delivery on, your own messages included; change it again after you /join if you like.")
	}
}

func cmdLXMF(c *cmd) error {
	if len(c.args) == 2 && strings.EqualFold(c.args[0], "mine") {
		on := strings.EqualFold(c.args[1], "on")
		if !on && !strings.EqualFold(c.args[1], "off") {
			return c.usage()
		}
		if err := c.tx.SetLXMFMine(c.req.Identity, on); err != nil {
			return err
		}
		if on {
			c.say("Your own messages from RRC and the chat page will now come to you by LXMF too, so your LXMF conversation shows everything you say. (Ones you send from your LXMF app aren't repeated; it already shows them.)")
		} else {
			c.say("Your own messages from RRC and the chat page won't be sent to you by LXMF any more.")
		}
		c.notMemberYet(c.me.Member)
		return nil
	}
	if len(c.args) != 1 {
		if len(c.args) == 0 {
			mine := "off"
			if c.me.LXMFMine {
				mine = "on"
			}
			c.say("LXMF delivery is %s, and your own messages from RRC and the page: %s. Change it with /lxmf on, /lxmf off, /lxmf auto, or /lxmf mine on|off.", describeMode(c, c.me), mine)
			return nil
		}
		return c.usage()
	}
	mode, ok := store.ParseLXMFMode(c.args[0])
	if !ok {
		return c.usage()
	}
	after, err := c.h.setLXMFMode(c.tx, c.out, c.req.Identity, mode)
	if err != nil {
		return err
	}
	switch mode {
	case store.LXMFOff:
		c.say("LXMF delivery is off. You stay in the group; /lxmf on starts it again.")
	case store.LXMFAuto:
		c.say("LXMF delivery is %s: it pauses while you're in #%s over RRC.", describeMode(c, after), c.h.cfg.GroupRoom)
	default:
		c.say("LXMF delivery is on.")
	}
	c.notMemberYet(after.Member)
	return nil
}
