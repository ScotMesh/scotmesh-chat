package hub

// Linking another app to your name (ADR 0007), with the rules for ranks
// (ADR 0007's amendment, alongside ADR 0008): roles come only from the
// person who issued the code, nobody redeems upwards, people with a role
// approve new apps from an app they already have, an owner's config
// identity never joins anyone else, and banned identities can't link.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

const (
	linkCodeTTL        = 10 * time.Minute
	linkCodesPerHour   = 3
	linkGuessesAllowed = 5
	linkLockout        = 10 * time.Minute
	maxAppsPerPerson   = 5
)

// newLinkCode makes a code people can type: 8 base32 characters, shown as
// XXXX-XXXX (40 bits).
func newLinkCode() (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	s := base32.StdEncoding.EncodeToString(b[:])
	return s[:4] + "-" + s[4:], nil
}

// normaliseLinkCode forgives what phone keyboards do to a code: case,
// hyphens, spaces, and the digits that look like base32 letters.
func normaliseLinkCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "0", "O", "1", "I", "8", "B").Replace(s))
	return s
}

func linkCodeHash(code string) []byte {
	sum := sha256.Sum256([]byte("scotmesh-chat/link/" + normaliseLinkCode(code)))
	return sum[:]
}

// takeRecent returns the times within window, dropping the older ones. It
// never writes into times' backing array: some callers (maintain, for its
// eviction check) only want the count and never store the result back, and
// filtering in place would then corrupt what's still in the map — dropping
// an expired entry from [old, recent] left the stored slice reading
// [recent, recent], since compacting in place shifts recent into old's
// slot while the map's own slice header still spans both.
func takeRecent(times []time.Time, now time.Time, window time.Duration) []time.Time {
	var kept []time.Time
	for _, t := range times {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	return kept
}

func cmdLink(c *cmd) error {
	switch {
	case len(c.args) == 0:
		return linkIssue(c)
	case len(c.args) == 2 && (strings.EqualFold(c.args[0], "approve") || strings.EqualFold(c.args[0], "deny")):
		return linkDecide(c, strings.EqualFold(c.args[0], "approve"), c.args[1])
	case len(c.args) == 1 || len(c.args) == 2 && strings.EqualFold(c.args[1], "confirm"):
		return linkRedeem(c, c.args[0], len(c.args) == 2)
	}
	return c.usage()
}

func linkIssue(c *cmd) error {
	if len(c.me.Identities) >= maxAppsPerPerson {
		return c.fail("%s already has %d apps, the most one name can have. /unlink one first.", c.me.Name, maxAppsPerPerson)
	}
	now := c.h.now()
	issued := takeRecent(c.h.linkIssued[c.me.PersonID], now, time.Hour)
	if len(issued) >= linkCodesPerHour {
		c.h.linkIssued[c.me.PersonID] = issued
		return c.fail("That's %d codes this hour; try again later.", linkCodesPerHour)
	}
	code, err := newLinkCode()
	if err != nil {
		return err
	}
	lc := &store.LinkCode{Hash: linkCodeHash(code), Person: c.me.PersonID, IssuedBy: c.req.Identity, IssuedAt: c.nowMS, ExpiresAt: now.Add(linkCodeTTL).UnixMilli()}
	if err := c.tx.PutLinkCode(lc); err != nil {
		return err
	}
	c.out.then(func() { c.h.linkIssued[c.me.PersonID] = append(issued, now) })
	c.say("On your other app, send /link %s (or !link %s) to the group, in the room, or on the chat page. It works once, until %s.", code, code, c.h.clock(lc.ExpiresAt))
	if c.me.PersonRank >= RankMod {
		c.say("As you're %s, you'll be asked to approve the new app from one of your apps before it's linked.", article(c.me.PersonRank))
	}
	return nil
}

// badCode answers a code that can't be used, the same way whatever the
// reason, so a guesser learns nothing, and counts it against the guesser.
func badCode(c *cmd) error {
	key := hexID(c.req.Identity)
	now := c.h.now()
	fails := append(takeRecent(c.h.linkFailures[key], now, linkLockout), now)
	c.out.then(func() { c.h.linkFailures[key] = fails })
	return c.fail("That code doesn't work. Codes last 10 minutes and work once; ask for a new one with /link on your other app.")
}

func linkRedeem(c *cmd, code string, confirmed bool) error {
	now := c.h.now()
	if fails := takeRecent(c.h.linkFailures[hexID(c.req.Identity)], now, linkLockout); len(fails) >= linkGuessesAllowed {
		c.h.linkFailures[hexID(c.req.Identity)] = fails
		return c.fail("Too many codes that didn't work; try again in 10 minutes.")
	}
	lc, ok, err := c.tx.LinkCodeByHash(linkCodeHash(code))
	if err != nil {
		return err
	}
	if !ok || lc.ExpiresAt <= c.nowMS || lc.RedeemedBy != nil {
		return badCode(c)
	}
	if banned, err := c.tx.IsBanned(lc.IssuedBy); err != nil || banned {
		if err != nil {
			return err
		}
		return badCode(c)
	}
	if lc.Person == c.me.PersonID {
		return c.fail("This app is already part of %s.", c.me.Name)
	}
	target, err := c.h.personByID(c.tx, lc.Person, lc.IssuedBy)
	if err != nil {
		return err
	}
	if c.h.isOwner(c.req.Identity) {
		return c.fail("This is a hub owner's own identity, which can't join anyone else. Send /link here instead, and redeem the code on the other app.")
	}
	if c.me.PersonRank > target.PersonRank {
		return c.fail("This app is %s and %s isn't. Ask for the code on this app and redeem it on the other.", article(c.me.PersonRank), target.Name)
	}
	if len(target.Identities) >= maxAppsPerPerson {
		return c.fail("%s already has %d apps, the most one name can have.", target.Name, maxAppsPerPerson)
	}
	// An app that is the only one holding its name gives the name up.
	alone := len(c.me.Identities) == 1
	if c.me.Claimed && alone && !confirmed {
		return c.fail("This app is %s. Linking frees %s and makes this app %s. To go ahead, send /link %s confirm.", c.me.Name, c.me.Name, target.Name, code)
	}
	if target.PersonRank >= RankMod {
		if err := c.tx.RedeemLinkCode(lc.Hash, c.req.Identity, c.req.Via); err != nil {
			return err
		}
		short := hexID(c.req.Identity)[:4]
		for _, id := range target.Identities {
			c.out.emit(NoticeEvent{Identity: id, Text: fmt.Sprintf("Approve linking %s (%s, via %s) to %s, with your %s role? Send /link approve %s or /link deny %s. It expires at %s.",
				c.me.Name, short, c.req.Via, target.Name, target.PersonRank, short, short, c.h.clock(lc.ExpiresAt))})
		}
		c.say("Waiting for approval from one of %s's apps, as %s is %s. It expires at %s.", target.Name, target.Name, article(target.PersonRank), c.h.clock(lc.ExpiresAt))
		return nil
	}
	if err := c.h.link(c.tx, c.out, c.req.Identity, &target, c.req.Via); err != nil {
		return err
	}
	if err := c.tx.DeleteLinkCode(lc.Hash); err != nil {
		return err
	}
	c.say("This app is now %s. /devices lists your apps; /unlink takes this one out again.", target.Name)
	return nil
}

func linkDecide(c *cmd, approve bool, who string) error {
	pending, err := c.tx.PendingLinks(c.me.PersonID, c.nowMS)
	if err != nil {
		return err
	}
	prefix := strings.ToLower(strings.TrimPrefix(who, "guest-"))
	var match []store.LinkCode
	for _, lc := range pending {
		if len(prefix) >= 4 && strings.HasPrefix(hexID(lc.RedeemedBy), prefix) {
			match = append(match, lc)
		}
	}
	switch len(match) {
	case 0:
		return c.fail("Nothing is waiting for approval as %s.", who)
	case 1:
	default:
		return c.fail("%s matches more than one app waiting; use more of its identity.", who)
	}
	lc := match[0]
	redeemer := lc.RedeemedBy
	if err := c.tx.DeleteLinkCode(lc.Hash); err != nil {
		return err
	}
	name, err := c.h.displayName(c.tx, redeemer)
	if err != nil {
		return err
	}
	if !approve {
		c.out.emit(NoticeEvent{Identity: redeemer, Text: fmt.Sprintf("%s's app didn't approve linking this app.", c.me.Name)})
		if err := c.tx.Audit(c.nowMS, c.req.Identity, "link-deny", c.me.Name, hexID(redeemer)); err != nil {
			return err
		}
		c.say("Denied: %s (%s) is not linked.", name, hexID(redeemer)[:8])
		return nil
	}
	if banned, err := c.tx.IsBanned(redeemer); err != nil || banned {
		if err != nil {
			return err
		}
		return c.fail("%s is banned and can't be linked.", name)
	}
	if err := c.h.link(c.tx, c.out, redeemer, &c.me, lc.RedeemedVia); err != nil {
		return err
	}
	c.out.emit(NoticeEvent{Identity: redeemer, Text: fmt.Sprintf("Approved: this app is now %s.", c.me.Name)})
	c.say("Approved: %s (%s) is now one of %s's apps.", name, hexID(redeemer)[:8], c.me.Name)
	return nil
}

// link makes id one of target's apps. If id was its person's only app, that
// person goes, with its name (freed) and its rank; if not, the name and rank
// stay with the apps left behind. id takes target's name and rank.
// target's other apps are told.
func (h *Hub) link(tx *store.Tx, out *outbox, id []byte, target *Person, via Via) error {
	before, err := h.person(tx, id)
	if err != nil {
		return err
	}
	if len(before.Identities) == 1 && before.Claimed {
		if _, _, err := tx.ReleaseName(id); err != nil {
			return err
		}
	}
	if err := tx.LinkIdentity(id, target.PersonID, h.nowMS()); err != nil {
		return err
	}
	after, err := h.person(tx, id)
	if err != nil {
		return err
	}
	if after.Name != before.Name {
		out.emit(NameEvent{Identity: id, Old: before.Name, New: after.Name})
		out.then(func() { h.renamePresence(id, after.Name) })
	}
	short := hexID(id)[:4]
	for _, other := range target.Identities {
		out.emit(NoticeEvent{Identity: other, Text: fmt.Sprintf("A new app was linked to your name: %s (via %s). Not you? Send /unlink %s.", short, via, short)})
	}
	if target.PersonRank >= RankMod {
		return tx.Audit(h.nowMS(), id, "link", target.Name, hexID(id))
	}
	return nil
}

// personByID loads a person through one of its identities.
func (h *Hub) personByID(tx *store.Tx, person int64, known []byte) (Person, error) {
	ids, err := tx.PersonIdentities(person)
	if err != nil {
		return Person{}, err
	}
	for _, i := range ids {
		if bytes.Equal(i.ID, known) {
			return h.person(tx, known)
		}
	}
	if len(ids) == 0 {
		return Person{}, fmt.Errorf("person %d has no identities: %w", person, store.ErrNotFound)
	}
	return h.person(tx, ids[0].ID)
}

func cmdDevices(c *cmd) error {
	if len(c.args) > 1 {
		return c.usage()
	}
	p := c.me
	if len(c.args) == 1 {
		id, ok, err := c.resolveOrFail(c.args[0])
		if !ok || err != nil {
			return err
		}
		if p, err = c.h.person(c.tx, id); err != nil {
			return err
		}
		if err := may(c.me, ActDevicesOf, &p); err != nil {
			return c.fail("%s", err.Error())
		}
	}
	ids, err := c.tx.PersonIdentities(p.PersonID)
	if err != nil {
		return err
	}
	if len(ids) == 0 { // never seen: no person yet
		ids = []store.Identity{{ID: p.Identity}}
	}
	c.say("%s has %s:", p.Name, plural(len(ids), "app"))
	for _, i := range ids {
		var notes []string
		if bytes.Equal(i.ID, c.req.Identity) {
			notes = append(notes, "this app")
		}
		if rooms := c.h.roomsOf(i.ID); len(rooms) > 0 {
			notes = append(notes, "RRC in #"+strings.Join(rooms, ", #"))
		}
		if ok, err := c.tx.IsMember(i.ID); err != nil {
			return err
		} else if ok {
			notes = append(notes, "LXMF group")
		}
		if i.LastSeen > 0 {
			notes = append(notes, "seen "+c.h.clock(i.LastSeen))
		}
		c.say("  %s — %s", hexID(i.ID)[:8], strings.Join(notes, " · "))
	}
	if len(ids) > 1 {
		c.say("/unlink %s takes one out.", hexID(ids[len(ids)-1].ID)[:4])
	} else if p.PersonID == c.me.PersonID {
		c.say("/link gives a code to link another app.")
	}
	return nil
}

func cmdUnlink(c *cmd) error {
	if len(c.args) > 1 {
		return c.usage()
	}
	target := c.req.Identity
	if len(c.args) == 1 {
		prefix := strings.ToLower(strings.TrimPrefix(c.args[0], "guest-"))
		var found []byte
		for _, id := range c.me.Identities {
			if len(prefix) >= 4 && strings.HasPrefix(hexID(id), prefix) {
				found = id
			}
		}
		if found == nil {
			id, ok, err := c.resolveOrFail(c.args[0])
			if !ok || err != nil {
				return err
			}
			found = id
		}
		target = found
	}
	p, err := c.h.person(c.tx, target)
	if err != nil {
		return err
	}
	if err := may(c.me, ActUnlinkOther, &p); err != nil {
		return c.fail("%s", err.Error())
	}
	if !p.Linked() {
		return c.fail("%s (%s) isn't linked to another app.", p.Name, hexID(target)[:8])
	}
	if c.h.isOwner(target) && !bytes.Equal(target, c.req.Identity) {
		return c.fail("That's a hub owner's own identity; only it can unlink itself.")
	}
	if _, err := c.tx.UnlinkIdentity(target, c.nowMS); err != nil {
		return err
	}
	after, err := c.h.person(c.tx, target)
	if err != nil {
		return err
	}
	if after.Name != p.Name {
		c.out.emit(NameEvent{Identity: target, Old: p.Name, New: after.Name})
		c.out.then(func() { c.h.renamePresence(target, after.Name) })
	}
	for _, other := range p.Identities {
		if !bytes.Equal(other, target) && !bytes.Equal(other, c.req.Identity) {
			c.out.emit(NoticeEvent{Identity: other, Text: fmt.Sprintf("App %s was unlinked from %s.", hexID(target)[:4], p.Name)})
		}
	}
	if !bytes.Equal(target, c.req.Identity) {
		c.out.emit(NoticeEvent{Identity: target, Text: fmt.Sprintf("This app was unlinked from %s and is %s now.", p.Name, after.Name)})
	}
	if p.PersonRank >= RankMod || c.me.PersonID != p.PersonID {
		if err := c.tx.Audit(c.nowMS, c.req.Identity, "unlink", p.Name, hexID(target)); err != nil {
			return err
		}
	}
	if bytes.Equal(target, c.req.Identity) {
		c.say("This app is no longer part of %s; you're %s here now.", p.Name, after.Name)
	} else {
		c.say("Unlinked %s from %s.", hexID(target)[:8], p.Name)
	}
	return nil
}

// unlinkAll takes every other app out of id's person: /forget.
func (h *Hub) unlinkAll(tx *store.Tx, out *outbox, id []byte) error {
	ids, err := h.identitiesOf(tx, id)
	if err != nil {
		return err
	}
	for _, other := range ids {
		if bytes.Equal(other, id) {
			continue
		}
		before, err := h.displayName(tx, other)
		if err != nil {
			return err
		}
		if _, err := tx.UnlinkIdentity(other, h.nowMS()); err != nil {
			return err
		}
		guest := GuestName(other)
		out.emit(NameEvent{Identity: other, Old: before, New: guest})
		out.then(func() { h.renamePresence(other, guest) })
		out.emit(NoticeEvent{Identity: other, Text: fmt.Sprintf("%s was forgotten from another app, so this app is %s now.", before, guest)})
	}
	return nil
}

// App is one app (identity) of a person, as the settings page lists it.
type App struct {
	Identity  []byte
	LastSeen  int64
	RRC       bool // in a room over RRC now
	LXMF      bool // a member of the LXMF group
	Via       Via  // for an app waiting for approval: where it redeemed the code
	ExpiresAt int64
}

// LinkedApps is what the settings page shows about a person's apps.
type LinkedApps struct {
	Apps    []App // longest-linked first
	Pending []App // redeemed codes waiting for one of these apps to approve
}

// LinkedApps lists an identity's person's apps and those waiting to join.
func (h *Hub) LinkedApps(ctx context.Context, id []byte) (LinkedApps, error) {
	var out LinkedApps
	var viewErr error
	err := h.do(ctx, func() {
		viewErr = h.st.View(ctx, func(tx *store.Tx) error {
			p, ok, err := tx.PersonOf(id)
			if err != nil || !ok {
				out.Apps = []App{{Identity: id}}
				return err
			}
			ids, err := tx.PersonIdentities(p.ID)
			if err != nil {
				return err
			}
			for _, i := range ids {
				member, err := tx.IsMember(i.ID)
				if err != nil {
					return err
				}
				out.Apps = append(out.Apps, App{Identity: i.ID, LastSeen: i.LastSeen, RRC: len(h.roomsOf(i.ID)) > 0, LXMF: member})
			}
			pending, err := tx.PendingLinks(p.ID, h.nowMS())
			for _, lc := range pending {
				out.Pending = append(out.Pending, App{Identity: lc.RedeemedBy, Via: lc.RedeemedVia, ExpiresAt: lc.ExpiresAt})
			}
			return err
		})
	})
	if err == nil {
		err = viewErr
	}
	return out, err
}
