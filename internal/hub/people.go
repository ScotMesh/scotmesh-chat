package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// Person is what the hub knows about an identity and the person it is part
// of: the identities linked with /link share a name, a rank and preferences
// (ADR 0007). LXMF settings are the identity's own.
type Person struct {
	Identity   []byte
	PersonID   int64    // 0 for an identity the hub has never seen
	Identities [][]byte // every identity of the person, longest-linked first
	Name       string   // the claimed name, or the guest name
	Claimed    bool
	ClaimedAt  int64
	Rank       Rank   // what this identity can do
	PersonRank Rank   // the person's rank, for the rank rule
	RankBy     []byte // who granted a stored rank
	RankAt     int64
	Member     bool // of the LXMF group
	LXMFMode   store.LXMFMode
	LXMFMine   bool // their own RRC and page messages come to them by LXMF too
	Prefs      store.Prefs
}

// Linked reports whether the person has more than one identity.
func (p Person) Linked() bool { return len(p.Identities) > 1 }

// Owns reports whether an identity is one of the person's apps.
func (p Person) Owns(id []byte) bool {
	for _, app := range p.Identities {
		if bytes.Equal(app, id) {
			return true
		}
	}
	return bytes.Equal(p.Identity, id)
}

// Identify records that an identity has arrived and returns who they are.
// A banned identity is refused. publicKey may be nil.
func (h *Hub) Identify(ctx context.Context, id, publicKey []byte) (Person, error) {
	var p Person
	err := h.update(ctx, func(tx *store.Tx, _ *outbox) error {
		if err := h.checkHubBan(tx, id); err != nil {
			return err
		}
		if err := tx.TouchIdentity(id, publicKey, h.nowMS()); err != nil {
			return err
		}
		var err error
		p, err = h.person(tx, id)
		return err
	})
	return p, err
}

// PersonOf returns what the hub knows about an identity without recording a visit.
func (h *Hub) PersonOf(ctx context.Context, id []byte) (Person, error) {
	var p Person
	err := h.st.View(ctx, func(tx *store.Tx) error {
		var err error
		p, err = h.person(tx, id)
		return err
	})
	return p, err
}

func (h *Hub) person(tx *store.Tx, id []byte) (Person, error) {
	p := Person{Identity: id, Identities: [][]byte{id}, Name: GuestName(id), LXMFMode: store.LXMFOn, Prefs: store.DefaultPrefs()}
	p.Rank = h.identityRank(id, store.RankMember, false)
	p.PersonRank = p.Rank
	ident, known, err := tx.Identity(id)
	if err != nil || !known {
		return p, err
	}
	p.LXMFMode, p.LXMFMine = ident.LXMFMode, ident.LXMFMine
	sp, ok, err := tx.PersonOf(id)
	if err != nil {
		return p, err
	}
	if ok {
		ids, err := tx.PersonIdentities(sp.ID)
		if err != nil {
			return p, err
		}
		p.PersonID, p.Prefs, p.RankBy, p.RankAt = sp.ID, sp.Prefs, sp.RankBy, sp.RankAt
		p.Identities = p.Identities[:0]
		for _, i := range ids {
			p.Identities = append(p.Identities, i.ID)
		}
		p.PersonRank = h.personRank(ids, sp.Rank)
		p.Rank = h.identityRank(id, sp.Rank, p.PersonRank == RankOwner)
	}
	n, ok, err := tx.NameOf(id)
	if err != nil {
		return p, err
	}
	if ok {
		p.Name, p.Claimed, p.ClaimedAt = n.Name, true, n.ClaimedAt
	}
	if p.Member, err = tx.IsMember(id); err != nil {
		return p, err
	}
	return p, nil
}

// rank is what an identity can do, without loading the rest of its person.
func (h *Hub) rank(tx *store.Tx, id []byte) (Rank, error) {
	if h.isOwner(id) {
		return RankOwner, nil
	}
	p, err := h.person(tx, id)
	return p.Rank, err
}

func (h *Hub) displayName(tx *store.Tx, id []byte) (string, error) {
	n, ok, err := tx.NameOf(id)
	if err != nil || !ok {
		return GuestName(id), err
	}
	return n.Name, nil
}

func (h *Hub) checkHubBan(tx *store.Tx, id []byte) error {
	banned, err := tx.IsBanned(id)
	if err != nil {
		return err
	}
	if banned {
		return refuse("you are banned from this hub")
	}
	return nil
}

// ClaimName gives an identity a name (ADR 0005). Asking for the name you
// already have is not an error. A taken name is refused with a message
// saying what they are called meanwhile.
func (h *Hub) ClaimName(ctx context.Context, id []byte, want string) (Person, error) {
	var p Person
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		if err := tx.TouchIdentity(id, nil, h.nowMS()); err != nil {
			return err
		}
		var err error
		p, err = h.claimName(tx, out, id, want)
		return err
	})
	return p, err
}

func (h *Hub) claimName(tx *store.Tx, out *outbox, id []byte, want string) (Person, error) {
	before, err := h.person(tx, id)
	if err != nil {
		return before, err
	}
	clean, sk, err := CleanName(want)
	if err != nil {
		var ne *NameError
		if errors.As(err, &ne) {
			return before, refuse("%s. %s", capitalise(ne.Reason), stillCalled(before))
		}
		return before, err
	}
	if before.Claimed && before.Name == clean {
		return before, nil
	}
	err = tx.ClaimName(id, clean, sk, h.nowMS())
	if errors.Is(err, store.ErrNameTaken) {
		return before, refuse("%q is taken by someone else. %s", clean, stillCalled(before))
	}
	if err != nil {
		return before, err
	}
	after, err := h.person(tx, id)
	if err != nil {
		return after, err
	}
	if after.Name != before.Name {
		out.emit(NameEvent{Identity: id, Old: before.Name, New: after.Name})
		out.then(func() { h.renamePresence(id, after.Name) })
	}
	return after, nil
}

func stillCalled(p Person) string {
	if p.Claimed {
		return fmt.Sprintf("You are still %s.", p.Name)
	}
	return fmt.Sprintf("You are shown as %s until you choose a free name with /nick.", p.Name)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Forget frees an identity's name and takes it out of the LXMF group:
// /forget, which was /leave in ADR 0005 until /leave came to mean leaving what
// you're in. History keeps the name things were said under.
func (h *Hub) Forget(ctx context.Context, id []byte) (Person, error) {
	var p Person
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		var err error
		p, err = h.forget(tx, out, id)
		return err
	})
	return p, err
}

// LeaveGroup takes an identity out of the LXMF group, keeping its name, and
// reports whether it was a member.
func (h *Hub) LeaveGroup(ctx context.Context, id []byte) (bool, error) {
	var left bool
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		var err error
		left, err = h.leaveGroup(tx, out, id)
		return err
	})
	return left, err
}

func (h *Hub) leaveGroup(tx *store.Tx, out *outbox, id []byte) (bool, error) {
	p, err := h.person(tx, id)
	if err != nil || !p.Member {
		return false, err
	}
	if _, err := tx.RemoveMember(id); err != nil {
		return false, err
	}
	out.emit(GroupEvent{Identity: id, Name: p.Name, Joined: false})
	return true, nil
}

func (h *Hub) forget(tx *store.Tx, out *outbox, id []byte) (Person, error) {
	before, err := h.person(tx, id)
	if err != nil {
		return before, err
	}
	if err := h.unlinkAll(tx, out, id); err != nil {
		return before, err
	}
	if before.Member {
		if _, err := tx.RemoveMember(id); err != nil {
			return before, err
		}
		out.emit(GroupEvent{Identity: id, Name: before.Name, Joined: false})
	}
	if before.Claimed {
		if _, _, err := tx.ReleaseName(id); err != nil {
			return before, err
		}
		guest := GuestName(id)
		out.emit(NameEvent{Identity: id, Old: before.Name, New: guest})
		out.then(func() { h.renamePresence(id, guest) })
		if err := tx.Audit(h.nowMS(), id, "forget", before.Name, ""); err != nil {
			return before, err
		}
	}
	return h.person(tx, id)
}

// SetLXMFMode changes how the LXMF group delivers to an identity (ADR 0006).
func (h *Hub) SetLXMFMode(ctx context.Context, id []byte, mode store.LXMFMode) (Person, error) {
	var p Person
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		var err error
		p, err = h.setLXMFMode(tx, out, id, mode)
		return err
	})
	return p, err
}

func (h *Hub) setLXMFMode(tx *store.Tx, out *outbox, id []byte, mode store.LXMFMode) (Person, error) {
	if err := tx.TouchIdentity(id, nil, h.nowMS()); err != nil {
		return Person{}, err
	}
	before, err := h.person(tx, id)
	if err != nil {
		return before, err
	}
	wasPaused, err := h.lxmfPaused(tx, before.LXMFMode, id)
	if err != nil {
		return before, err
	}
	if err := tx.SetLXMFMode(id, mode); err != nil {
		return before, err
	}
	nowPaused, err := h.lxmfPaused(tx, mode, id)
	if err != nil {
		return before, err
	}
	if before.Member {
		if err := h.lxmfPauseChanged(tx, out, id, wasPaused, nowPaused); err != nil {
			return before, err
		}
	}
	return h.person(tx, id)
}

// SetLXMFMine turns on or off delivery of an identity's own messages from RRC
// and the page to them by LXMF, so their LXMF conversation shows everything
// they said. Messages they send from the LXMF app itself are never echoed:
// the app already shows them.
func (h *Hub) SetLXMFMine(ctx context.Context, id []byte, on bool) (Person, error) {
	var p Person
	err := h.update(ctx, func(tx *store.Tx, _ *outbox) error {
		if err := tx.TouchIdentity(id, nil, h.nowMS()); err != nil {
			return err
		}
		if err := tx.SetLXMFMine(id, on); err != nil {
			return err
		}
		var err error
		p, err = h.person(tx, id)
		return err
	})
	return p, err
}

// lxmfPaused reports whether LXMF delivery to id is paused under mode. Auto
// pauses while any app of the person is in the group room over RRC: someone
// reading on their laptop doesn't want the same messages on their phone
// (ADR 0007).
func (h *Hub) lxmfPaused(tx *store.Tx, mode store.LXMFMode, id []byte) (bool, error) {
	switch mode {
	case store.LXMFOff:
		return true, nil
	case store.LXMFAuto:
		return h.personOnRRC(tx, id, h.cfg.GroupRoom, nil)
	}
	return false, nil
}

// personOnRRC reports whether any identity of id's person is in room over
// RRC, leaving out except (an identity on its way out).
func (h *Hub) personOnRRC(tx *store.Tx, id []byte, room string, except []byte) (bool, error) {
	ids, err := h.identitiesOf(tx, id)
	if err != nil {
		return false, err
	}
	for _, i := range ids {
		if !bytes.Equal(i, except) && h.rrcPresent(room, i) {
			return true, nil
		}
	}
	return false, nil
}

// samePerson reports whether two identities are apps of one person.
func (h *Hub) samePerson(tx *store.Tx, a, b []byte) (bool, error) {
	if bytes.Equal(a, b) {
		return true, nil
	}
	pa, okA, err := tx.PersonOf(a)
	if err != nil || !okA {
		return false, err
	}
	pb, okB, err := tx.PersonOf(b)
	return okB && pa.ID == pb.ID, err
}

// identitiesOf lists the identities of id's person, id alone if it has none
// stored yet.
func (h *Hub) identitiesOf(tx *store.Tx, id []byte) ([][]byte, error) {
	p, ok, err := tx.PersonOf(id)
	if err != nil || !ok {
		return [][]byte{id}, err
	}
	ids, err := tx.PersonIdentities(p.ID)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(ids))
	for i := range ids {
		out[i] = ids[i].ID
	}
	return out, nil
}

// autoPauseChanged pauses or resumes LXMF for every app of id's person that
// is a group member on auto, when the person arrives in or leaves the group
// room over RRC.
func (h *Hub) autoPauseChanged(tx *store.Tx, out *outbox, id []byte, paused bool) error {
	ids, err := h.identitiesOf(tx, id)
	if err != nil {
		return err
	}
	for _, i := range ids {
		p, err := h.person(tx, i)
		if err != nil {
			return err
		}
		if p.Member && p.LXMFMode == store.LXMFAuto {
			if err := h.lxmfPauseChanged(tx, out, i, !paused, paused); err != nil {
				return err
			}
		}
	}
	return nil
}

// lxmfPauseChanged records the pause point, or announces the resume with a
// count of what was missed.
func (h *Hub) lxmfPauseChanged(tx *store.Tx, out *outbox, id []byte, was, now bool) error {
	room := h.cfg.GroupRoom
	switch {
	case !was && now:
		last, err := tx.LastMessageID(room)
		if err != nil {
			return err
		}
		return tx.SetCursor(id, room, store.ViaLXMF, last, h.nowMS())
	case was && !now:
		c, ok, err := tx.Cursor(id, room, store.ViaLXMF)
		if err != nil || !ok {
			return err
		}
		missed, err := tx.CountAfter(room, c.MessageID)
		if err != nil {
			return err
		}
		if last, err := tx.LastMessageID(room); err != nil {
			return err
		} else if err := tx.SetCursor(id, room, store.ViaLXMF, last, h.nowMS()); err != nil {
			return err
		}
		if missed > 0 {
			out.emit(LXMFResumeEvent{Identity: id, Missed: missed})
		}
	}
	return nil
}

// JoinGroup adds an identity to the LXMF group, claiming name if one is given.
// A refused name does not stop the join: they join as a guest and the
// refusal comes back as Note.
func (h *Hub) JoinGroup(ctx context.Context, id []byte, name string) (p Person, note string, err error) {
	err = h.update(ctx, func(tx *store.Tx, out *outbox) error {
		p, note, err = h.joinGroup(tx, out, id, name)
		return err
	})
	return p, note, err
}

func (h *Hub) joinGroup(tx *store.Tx, out *outbox, id []byte, name string) (Person, string, error) {
	if err := h.checkHubBan(tx, id); err != nil {
		return Person{}, "", err
	}
	if err := h.checkRoomBan(tx, h.cfg.GroupRoom, id); err != nil {
		return Person{}, "", err
	}
	if err := tx.TouchIdentity(id, nil, h.nowMS()); err != nil {
		return Person{}, "", err
	}
	note := ""
	if name != "" {
		if _, err := h.claimName(tx, out, id, name); err != nil {
			var ue *UserError
			if !errors.As(err, &ue) {
				return Person{}, "", err
			}
			note = ue.Text
		}
	}
	added, err := tx.AddMember(id, h.nowMS())
	if err != nil {
		return Person{}, "", err
	}
	p, err := h.person(tx, id)
	if err != nil {
		return p, "", err
	}
	if added {
		// Joining starts delivery of every message, their own from RRC and
		// the page included, replacing whatever was set before joining.
		if err := tx.SetLXMFMode(id, store.LXMFOn); err != nil {
			return p, "", err
		}
		if err := tx.SetLXMFMine(id, true); err != nil {
			return p, "", err
		}
		if p, err = h.person(tx, id); err != nil {
			return p, "", err
		}
		out.emit(GroupEvent{Identity: id, Name: p.Name, Joined: true})
	}
	return p, note, nil
}

// MarkRead records that an identity has seen a room up to a message by a
// way in: the page's "new since your last visit" marker (ADR 0006).
func (h *Hub) MarkRead(ctx context.Context, id []byte, room string, via Via, messageID int64) error {
	return h.update(ctx, func(tx *store.Tx, _ *outbox) error {
		if err := tx.TouchIdentity(id, nil, h.nowMS()); err != nil {
			return err
		}
		return tx.SetCursor(id, room, via, messageID, h.nowMS())
	})
}

// ReadCursor returns where an identity last read a room by a way in.
func (h *Hub) ReadCursor(ctx context.Context, id []byte, room string, via Via) (store.Cursor, bool, error) {
	var (
		c  store.Cursor
		ok bool
	)
	err := h.st.View(ctx, func(tx *store.Tx) error {
		var err error
		c, ok, err = tx.Cursor(id, room, via)
		return err
	})
	return c, ok, err
}

// Room returns a room's stored state.
func (h *Hub) Room(ctx context.Context, name string) (store.Room, bool, error) {
	var (
		r  store.Room
		ok bool
	)
	err := h.st.View(ctx, func(tx *store.Tx) error {
		var err error
		r, ok, err = tx.Room(name)
		return err
	})
	return r, ok, err
}
