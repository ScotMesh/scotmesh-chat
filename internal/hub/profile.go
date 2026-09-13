package hub

// Profiles and sharing (ADR 0009): what anyone can see about a person, and
// whether their LXMF address is shown so others can message them.

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// LXMFAddress is an identity's lxmf.delivery address.
func LXMFAddress(identity []byte) []byte {
	return rns.DestinationHash(rns.NameHash("lxmf.delivery"), identity)
}

// Profile is what someone sees about a person.
type Profile struct {
	Person      Person
	Here        []string // "RRC in #scotmesh", "the LXMF group"
	LastSeen    int64    // across all their apps
	Shared      bool     // their LXMF addresses are shown
	ForcedShare bool     // shown because of their rank, whatever they chose
	// Addresses are their apps' LXMF addresses: of group members if any,
	// otherwise the address the longest-linked app would have (Derived).
	Addresses [][]byte
	Derived   bool
	Recent    []store.Message // their last messages in the group room
}

// ProfileOf returns the profile of the person an identity is part of, as a
// viewer sees it.
func (h *Hub) ProfileOf(ctx context.Context, id []byte) (Profile, error) {
	var p Profile
	var viewErr error
	err := h.do(ctx, func() {
		viewErr = h.st.View(ctx, func(tx *store.Tx) error {
			var err error
			p, err = h.profile(tx, id, 5)
			return err
		})
	})
	if err == nil {
		err = viewErr
	}
	return p, err
}

func (h *Hub) profile(tx *store.Tx, id []byte, recent int) (Profile, error) {
	person, err := h.person(tx, id)
	if err != nil {
		return Profile{}, err
	}
	p := Profile{Person: person}
	p.ForcedShare = person.PersonRank >= RankMod
	p.Shared = person.Prefs.ShareLXMF || p.ForcedShare
	rooms := map[string]bool{}
	member := false
	for _, app := range person.Identities {
		for _, r := range h.roomsOf(app) {
			rooms[r] = true
		}
		ident, ok, err := tx.Identity(app)
		if err != nil {
			return p, err
		}
		if ok && ident.LastSeen > p.LastSeen {
			p.LastSeen = ident.LastSeen
		}
		isMember, err := tx.IsMember(app)
		if err != nil {
			return p, err
		}
		if isMember {
			member = true
			p.Addresses = append(p.Addresses, LXMFAddress(app))
		}
	}
	if len(rooms) > 0 {
		names := make([]string, 0, len(rooms))
		for r := range rooms {
			names = append(names, r)
		}
		sort.Strings(names)
		p.Here = append(p.Here, "RRC in #"+strings.Join(names, ", #"))
	}
	if member {
		p.Here = append(p.Here, "the LXMF group")
	}
	if len(p.Addresses) == 0 {
		p.Addresses, p.Derived = [][]byte{LXMFAddress(person.Identities[0])}, true
	}
	if recent > 0 && person.PersonID != 0 {
		if p.Recent, err = tx.LatestMessagesByPerson(person.PersonID, h.cfg.GroupRoom, recent); err != nil {
			return p, err
		}
	}
	return p, nil
}

func cmdProfile(c *cmd) error {
	if len(c.args) > 1 {
		return c.usage()
	}
	id := c.req.Identity
	if len(c.args) == 1 {
		found, ok, err := c.resolveOrFail(c.args[0])
		if !ok || err != nil {
			return err
		}
		id = found
	}
	p, err := c.h.profile(c.tx, id, 0)
	if err != nil {
		return err
	}
	who := p.Person
	title := who.Name
	if t := who.PersonRank.Title(); t != "" {
		title += " · " + t
	}
	c.say("%s", title)
	if who.Claimed {
		c.say("Name held since %s.", time.UnixMilli(who.ClaimedAt).In(london).Format("2 Jan 2006"))
	} else {
		c.say("No name claimed; shown by the start of their identity.")
	}
	switch {
	case len(p.Here) > 0:
		c.say("Here now: %s.", strings.Join(p.Here, " and "))
	case p.LastSeen > 0:
		c.say("Last seen %s.", c.h.clock(p.LastSeen))
	}
	if p.Shared {
		addrs := make([]string, len(p.Addresses))
		for i, a := range p.Addresses {
			addrs[i] = hex.EncodeToString(a)
		}
		label := "LXMF: %s"
		if p.Derived {
			label = "LXMF, if they use it with this identity: %s"
		}
		c.say(label, strings.Join(addrs, ", "))
	} else {
		c.say("Their LXMF address isn't shared.")
	}
	return nil
}

func cmdShare(c *cmd) error {
	if len(c.args) == 0 {
		switch {
		case c.me.PersonRank >= RankMod:
			c.say("Your LXMF address is shown on your profile: it always is for mods and up, so people can reach you.")
		case c.me.Prefs.ShareLXMF:
			c.say("Your LXMF address is shown on your profile, so people can message you. /share off hides it.")
		default:
			c.say("Your LXMF address isn't shown on your profile. /share on shows it.")
		}
		return nil
	}
	if len(c.args) != 1 || !strings.EqualFold(c.args[0], "on") && !strings.EqualFold(c.args[0], "off") {
		return c.usage()
	}
	on := strings.EqualFold(c.args[0], "on")
	prefs := c.me.Prefs
	prefs.ShareLXMF = on
	if err := c.tx.SetPrefs(c.req.Identity, prefs); err != nil {
		return err
	}
	if on {
		c.say("Your LXMF address is shown on your profile now, so people can message you.")
	} else {
		c.say("Your LXMF address is hidden from your profile now.")
	}
	if !on && c.me.PersonRank >= RankMod {
		c.say("Saved, but as %s your address is shown anyway, so people can reach you.", article(c.me.PersonRank))
	}
	return nil
}

// SetPrefs changes the preferences of the person an identity is part of: the
// page's buttons use it, with the same rules as the commands.
func (h *Hub) SetPrefs(ctx context.Context, id []byte, change func(*store.Prefs)) (Person, error) {
	var p Person
	err := h.update(ctx, func(tx *store.Tx, _ *outbox) error {
		if err := tx.TouchIdentity(id, nil, h.nowMS()); err != nil {
			return err
		}
		before, err := h.person(tx, id)
		if err != nil {
			return err
		}
		prefs := before.Prefs
		change(&prefs)
		if err := prefs.Validate(); err != nil {
			return refuse("%s", err.Error())
		}
		if err := tx.SetPrefs(id, prefs); err != nil {
			return err
		}
		p, err = h.person(tx, id)
		return err
	})
	return p, err
}

func cmdPage(c *cmd) error {
	prefs := c.me.Prefs
	if len(c.args) == 0 {
		flag := "shown"
		if !prefs.PageFlag {
			flag = "hidden"
		}
		refresh := "off"
		if prefs.PageRefresh > 0 {
			refresh = fmt.Sprintf("every %ds", prefs.PageRefresh)
		}
		c.say("Chat page: %d messages, flag %s, refresh %s. Change it with /page lines 10|20|30|50|100, /page flag on|off or /page refresh off|10|30|60.", prefs.PageLines, flag, refresh)
		return nil
	}
	if len(c.args) != 2 {
		return c.usage()
	}
	value := strings.ToLower(c.args[1])
	switch strings.ToLower(c.args[0]) {
	case "lines":
		n, err := strconv.Atoi(value)
		if err != nil || !slices.Contains(store.PageLineChoices, n) {
			return c.fail("The chat page can show 10, 20, 30, 50 or 100 messages.")
		}
		prefs.PageLines = n
	case "flag":
		if !isOnOff(value) {
			return c.usage()
		}
		prefs.PageFlag = value == "on"
	case "refresh":
		n := 0
		if value != "off" && value != "0" {
			var err error
			if n, err = strconv.Atoi(strings.TrimSuffix(value, "s")); err != nil || n == 0 || !slices.Contains(store.PageRefreshChoices, n) {
				return c.fail("The chat page can refresh every 10, 30 or 60 seconds, or not at all (off).")
			}
		}
		prefs.PageRefresh = n
	default:
		return c.usage()
	}
	if err := c.tx.SetPrefs(c.req.Identity, prefs); err != nil {
		return err
	}
	c.say("Saved: the chat page now shows %d messages, the flag %s, and refreshes %s.", prefs.PageLines, map[bool]string{true: "on", false: "off"}[prefs.PageFlag], refreshText(prefs.PageRefresh))
	return nil
}

func refreshText(seconds int) string {
	if seconds == 0 {
		return "only when you reload"
	}
	return fmt.Sprintf("every %d seconds", seconds)
}

// Permissions says which actions a viewer may take on someone, for the
// buttons on a profile page.
func (h *Hub) Permissions(ctx context.Context, viewer, target []byte) (map[Action]bool, error) {
	allowed := map[Action]bool{}
	err := h.st.View(ctx, func(tx *store.Tx) error {
		me, err := h.person(tx, viewer)
		if err != nil {
			return err
		}
		them, err := h.person(tx, target)
		if err != nil {
			return err
		}
		for a := range actionNames {
			allowed[a] = may(me, a, &them) == nil
		}
		return nil
	})
	return allowed, err
}
