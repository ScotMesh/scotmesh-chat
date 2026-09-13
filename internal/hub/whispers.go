package hub

// Whispers (ADR 0009): a message to one person inside the hub, that only
// they see. It reaches them on RRC as a direct notice, in their LXMF app,
// and on the chat page, now or when they're next on. Whispers are kept 7
// days and never appear in history, catch-up or the conversation.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// WhisperRetention is how long whispers are kept.
const WhisperRetention = 7 * 24 * time.Hour

// WhisperEvent is a new whisper for the RRC links of To, the recipient's
// apps. LXMF copies go through the delivery queue.
type WhisperEvent struct {
	Whisper store.Whisper
	To      [][]byte
}

func (WhisperEvent) event() {}

// WhisperText is how a whisper reads in an LXMF app.
func WhisperText(w *store.Whisper) string {
	return w.FromName + " whispers: " + w.Body
}

func cmdWhisper(c *cmd) error {
	if len(c.args) < 2 {
		return c.usage()
	}
	return c.whisperTo(c.args[0], strings.Join(c.args[1:], " "))
}

func cmdReply(c *cmd) error {
	if len(c.args) == 0 {
		return c.usage()
	}
	last, ok, err := c.tx.LastWhisperTo(c.me.PersonID)
	if err != nil {
		return err
	}
	if !ok {
		return c.fail("Nobody has whispered to you lately, so there's nobody to reply to.")
	}
	return c.whisperTo(hexID(last.From), strings.Join(c.args, " "))
}

func (c *cmd) whisperTo(token, body string) error {
	body = strings.TrimSpace(body)
	if !utf8.ValidString(body) {
		return c.fail("Not whispered: the message is not valid text.")
	}
	if len(body) > c.h.cfg.MaxBodyBytes {
		return c.fail("Not whispered: message too large: %d bytes > %d bytes", len(body), c.h.cfg.MaxBodyBytes)
	}
	id, err := c.resolve(token)
	var ue *UserError
	if errors.As(err, &ue) {
		return c.fail("%s", ue.Text)
	}
	if err != nil {
		return err
	}
	to, err := c.h.person(c.tx, id)
	if err != nil {
		return err
	}
	if to.PersonID != 0 && to.PersonID == c.me.PersonID {
		return c.fail("You can't whisper to yourself.")
	}
	// Banned, ignoring you, or unknown: the same answer, so a whisper never
	// tells you someone has ignored you.
	notFound := func() error { return c.fail("target '%s' not found", token) }
	if to.PersonID == 0 {
		return notFound()
	}
	for _, app := range to.Identities {
		if banned, err := c.tx.IsBanned(app); err != nil {
			return err
		} else if banned {
			return notFound()
		}
	}
	if ignoring, err := c.tx.IsIgnoring(to.PersonID, c.me.PersonID); err != nil {
		return err
	} else if ignoring {
		return notFound()
	}
	if !to.Prefs.Whispers {
		return c.fail("%s isn't taking whispers.", to.Name)
	}
	if !c.h.allowPost(c.req.Identity) {
		return c.fail("Not whispered: rate limited")
	}

	w := store.Whisper{From: c.req.Identity, FromName: c.me.Name, ToPerson: to.PersonID, ToName: to.Name, Body: body, Via: c.req.Via, SaidAt: c.nowMS}
	if err := c.tx.AddWhisper(&w); err != nil {
		return err
	}
	var where []string
	onRRC := false
	for _, app := range to.Identities {
		onRRC = onRRC || len(c.h.roomsOf(app)) > 0
	}
	if onRRC {
		where = append(where, "on RRC now")
	}
	if to.Prefs.WhispersLXMF {
		queued := false
		for _, app := range to.Identities {
			member, err := c.tx.IsMember(app)
			if err != nil {
				return err
			}
			if !member {
				continue
			}
			if err := c.tx.PutDelivery(&store.Delivery{MessageID: w.ID, Whisper: true, Identity: app, State: store.DeliveryQueued, NextAt: c.nowMS, UpdatedAt: c.nowMS}); err != nil {
				return err
			}
			queued = true
		}
		if queued {
			where = append(where, "by LXMF")
		}
	}
	c.out.emit(WhisperEvent{Whisper: w, To: to.Identities})
	if len(where) == 0 {
		c.say("Whispered to %s. They'll see it when they're next on.", to.Name)
	} else {
		c.say("Whispered to %s (%s).", to.Name, strings.Join(where, "; "))
	}
	return nil
}

// MayNotifyDirect reports whether from may send an RRC direct NOTICE to to
// right now, applying the same checks /whisper does (banned, ignoring, the
// whisper preference, the post rate limit), without recording a whisper:
// unlike /whisper, a raw client-sent direct NOTICE isn't delivered by LXMF
// or shown in the recipient's Whispers inbox — it's the lighter, RRC-only
// primitive the hub's own whisper delivery uses. Without this check, a
// direct NOTICE reached its target with none of /whisper's rules applied,
// including to someone who had ignored the sender or turned whispers off.
// A refusal never distinguishes banned, ignoring or unknown, so it can't
// be used to learn any of the three.
func (h *Hub) MayNotifyDirect(ctx context.Context, from, to []byte) error {
	var err error
	if derr := h.do(ctx, func() {
		err = h.st.View(ctx, func(tx *store.Tx) error {
			toP, verr := h.person(tx, to)
			if verr != nil {
				return verr
			}
			notFound := refuse("destination not connected")
			if toP.PersonID == 0 {
				return notFound
			}
			for _, app := range toP.Identities {
				if banned, berr := tx.IsBanned(app); berr != nil {
					return berr
				} else if banned {
					return notFound
				}
			}
			fromP, ferr := h.person(tx, from)
			if ferr != nil {
				return ferr
			}
			if ignoring, ierr := tx.IsIgnoring(toP.PersonID, fromP.PersonID); ierr != nil {
				return ierr
			} else if ignoring {
				return notFound
			}
			if !toP.Prefs.Whispers {
				return notFound
			}
			return nil
		})
		if err == nil && !h.allowPost(from) {
			err = refuse("rate limited")
		}
	}); derr != nil {
		return derr
	}
	return err
}

func cmdWhispers(c *cmd) error {
	prefs := c.me.Prefs
	switch {
	case len(c.args) == 0:
		state := "on"
		if !prefs.Whispers {
			state = "off"
		}
		lxmf := "on"
		if !prefs.WhispersLXMF {
			lxmf = "off"
		}
		unread, err := c.tx.UnreadWhispers(c.me.PersonID)
		if err != nil {
			return err
		}
		c.say("Whispers: %s; to your LXMF app: %s. %s unread on the chat page. Change it with /whispers on|off or /whispers lxmf on|off.", state, lxmf, plural(unread, "whisper"))
		return nil
	case len(c.args) == 1 && isOnOff(c.args[0]):
		prefs.Whispers = strings.EqualFold(c.args[0], "on")
	case len(c.args) == 2 && strings.EqualFold(c.args[0], "lxmf") && isOnOff(c.args[1]):
		prefs.WhispersLXMF = strings.EqualFold(c.args[1], "on")
	default:
		return c.usage()
	}
	if err := c.tx.SetPrefs(c.req.Identity, prefs); err != nil {
		return err
	}
	switch {
	case len(c.args) == 1 && prefs.Whispers:
		c.say("Whispers are on: people can whisper to you.")
	case len(c.args) == 1:
		c.say("Whispers are off: nobody can whisper to you.")
	case prefs.WhispersLXMF:
		c.say("Whispers will come to your LXMF app too, even while group delivery is off.")
	default:
		c.say("Whispers won't come to your LXMF app; you'll see them on RRC and the chat page.")
	}
	return nil
}

func isOnOff(s string) bool { return strings.EqualFold(s, "on") || strings.EqualFold(s, "off") }

func cmdIgnore(c *cmd) error {
	if len(c.args) != 1 {
		return c.usage()
	}
	id, ok, err := c.resolveOrFail(c.args[0])
	if !ok || err != nil {
		return err
	}
	them, err := c.h.person(c.tx, id)
	if err != nil {
		return err
	}
	if them.PersonID == c.me.PersonID || them.PersonID == 0 {
		return c.fail("You can't ignore yourself.")
	}
	if err := c.tx.Ignore(c.me.PersonID, them.PersonID, c.nowMS); err != nil {
		return err
	}
	c.say("Ignoring %s: their whispers won't reach you, and they aren't told. /unignore %s undoes it.", them.Name, them.Name)
	return nil
}

func cmdUnignore(c *cmd) error {
	if len(c.args) != 1 {
		return c.usage()
	}
	id, ok, err := c.resolveOrFail(c.args[0])
	if !ok || err != nil {
		return err
	}
	them, err := c.h.person(c.tx, id)
	if err != nil {
		return err
	}
	removed, err := c.tx.Unignore(c.me.PersonID, them.PersonID)
	if err != nil {
		return err
	}
	if !removed {
		return c.fail("You weren't ignoring %s.", them.Name)
	}
	c.say("No longer ignoring %s.", them.Name)
	return nil
}

func cmdIgnored(c *cmd) error {
	if len(c.args) != 0 {
		return c.usage()
	}
	people, err := c.tx.Ignored(c.me.PersonID)
	if err != nil {
		return err
	}
	if len(people) == 0 {
		c.say("You aren't ignoring anyone.")
		return nil
	}
	names := make([]string, 0, len(people))
	for _, pid := range people {
		name, err := c.h.personName(c.tx, pid)
		if err != nil {
			return err
		}
		names = append(names, name)
	}
	c.say("Ignoring: %s.", strings.Join(names, ", "))
	return nil
}

// personName is a person's name, or the guest name of their first app.
func (h *Hub) personName(tx *store.Tx, person int64) (string, error) {
	ids, err := tx.PersonIdentities(person)
	if err != nil || len(ids) == 0 {
		return fmt.Sprintf("person %d", person), err
	}
	return h.displayName(tx, ids[0].ID)
}

// UnsentRRCWhispers are whispers to an identity's person that haven't gone
// to one of their RRC links yet: the RRC server sends them after WELCOME.
func (h *Hub) UnsentRRCWhispers(ctx context.Context, id []byte) ([]store.Whisper, error) {
	var out []store.Whisper
	err := h.st.View(ctx, func(tx *store.Tx) error {
		p, ok, err := tx.PersonOf(id)
		if err != nil || !ok {
			return err
		}
		out, err = tx.UnsentRRCWhispers(p.ID)
		return err
	})
	return out, err
}

// WhisperSentRRC records that a whisper went to one of its recipient's RRC
// links.
func (h *Hub) WhisperSentRRC(ctx context.Context, whisperID int64) error {
	return h.update(ctx, func(tx *store.Tx, _ *outbox) error {
		return tx.MarkWhisperRRC(whisperID, h.nowMS())
	})
}

// WhisperInbox is what the settings page shows: recent whispers, oldest
// first, and how many of them are new.
type WhisperInbox struct {
	Whispers []store.Whisper
	Unread   int
	Ignored  []Ignored // people whose whispers are blocked
}

// Ignored is someone a person ignores.
type Ignored struct {
	Name     string
	Identity []byte // their first app, to unignore by
}

// Whispers returns an identity's person's recent whispers and marks them
// read, as the settings page shows them.
func (h *Hub) Whispers(ctx context.Context, id []byte, limit int) (WhisperInbox, error) {
	var in WhisperInbox
	err := h.update(ctx, func(tx *store.Tx, _ *outbox) error {
		p, ok, err := tx.PersonOf(id)
		if err != nil || !ok {
			return err
		}
		if in.Unread, err = tx.UnreadWhispers(p.ID); err != nil {
			return err
		}
		ignored, err := tx.Ignored(p.ID)
		if err != nil {
			return err
		}
		for _, pid := range ignored {
			ids, err := tx.PersonIdentities(pid)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				continue
			}
			name, err := h.displayName(tx, ids[0].ID)
			if err != nil {
				return err
			}
			in.Ignored = append(in.Ignored, Ignored{Name: name, Identity: ids[0].ID})
		}
		if in.Whispers, err = tx.WhispersTo(p.ID, limit); err != nil || len(in.Whispers) == 0 {
			return err
		}
		_, err = tx.MarkWhispersRead(p.ID, in.Whispers[len(in.Whispers)-1].ID, h.nowMS())
		return err
	})
	return in, err
}
