package hub

// Room membership, modes and permissions follow rrcd 0.3.2 (rooms.py,
// router.py _handle_join/_handle_part), Copyright (c) 2025 S. Miller, KC1AWV,
// MIT License, with presence keyed by identity rather than by link.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// presenceEntry is one identity's presence in one room.
type presenceEntry struct {
	identity []byte
	name     string
	rrcLinks int // RRC links of this identity joined to the room
}

func (h *Hub) rrcPresent(room string, id []byte) bool {
	e := h.presence[room][hexID(id)]
	return e != nil && e.rrcLinks > 0
}

func (h *Hub) renamePresence(id []byte, name string) {
	k := hexID(id)
	for _, members := range h.presence {
		if e := members[k]; e != nil {
			e.name = name
		}
	}
}

func (h *Hub) roomsOf(id []byte) []string {
	k := hexID(id)
	var rooms []string
	for room, members := range h.presence {
		if e := members[k]; e != nil && e.rrcLinks > 0 {
			rooms = append(rooms, room)
		}
	}
	sort.Strings(rooms)
	return rooms
}

// normRoom normalises a room name, refusing with rrcd's wording.
func (h *Hub) normRoom(name string) (string, error) {
	r, err := wire.NormalizeRoom(name, h.cfg.MaxRoomNameBytes)
	if err != nil {
		return "", refuse("%s", err.Error())
	}
	return r, nil
}

func (h *Hub) checkRoomBan(tx *store.Tx, room string, id []byte) error {
	banned, err := tx.HasRole(room, id, store.RoleBan)
	if err != nil {
		return err
	}
	if banned {
		return refuse("banned from room")
	}
	return nil
}

// isRoomOp: hub mods and up (in every room), the founder and granted ops.
func (h *Hub) isRoomOp(tx *store.Tx, r *store.Room, id []byte) (bool, error) {
	if r.Founder != nil && bytes.Equal(r.Founder, id) {
		return true, nil
	}
	if rank, err := h.rank(tx, id); err != nil || rank >= RankMod {
		return err == nil, err
	}
	return tx.HasRole(r.Name, id, store.RoleOp)
}

// mayReadRoom reports whether id may see a room's history and member list.
// A plain room needs nothing; a private (+p), invite-only (+i) or keyed
// (+k) room needs current RRC presence, an unexpired invite, or being a
// room op (which includes hub mods and up, see isRoomOp).
func (h *Hub) mayReadRoom(tx *store.Tx, r *store.Room, id []byte) (bool, error) {
	if !r.HasMode('p') && !r.HasMode('i') && r.Key == "" {
		return true, nil
	}
	if h.rrcPresent(r.Name, id) {
		return true, nil
	}
	if op, err := h.isRoomOp(tx, r, id); err != nil || op {
		return op, err
	}
	return tx.InviteValid(r.Name, id, h.nowMS())
}

func (h *Hub) isVoiced(tx *store.Tx, r *store.Room, id []byte) (bool, error) {
	if op, err := h.isRoomOp(tx, r, id); err != nil || op {
		return op, err
	}
	return tx.HasRole(r.Name, id, store.RoleVoice)
}

// JoinRequest is an RRC JOIN.
type JoinRequest struct {
	Identity []byte
	Room     string
	Key      string // +k, from the JOIN body
}

// JoinResult is what a joiner needs to be told.
type JoinResult struct {
	Room       store.Room
	FirstLink  bool            // this identity was not in the room before
	Members    []Member        // everyone present, for JOINED
	Replay     []store.Message // catch-up, oldest first (ADR 0006)
	ReplayNote string          // the opening bracket line, "" when Replay is empty
	// SeenUpTo is the newest message the joiner counts as delivered: the
	// replay covers everything up to it, so a live message with this ID or
	// lower must not be sent again.
	SeenUpTo int64
}

// Member is someone present in a room.
type Member struct {
	Identity []byte
	Name     string
	RRC      bool
	LXMF     bool
}

// Join puts one RRC link of an identity into a room. Checks run in rrcd's
// order: room name, room count, invite-only, key, room ban.
func (h *Hub) Join(ctx context.Context, req JoinRequest) (JoinResult, error) {
	var res JoinResult
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		var err error
		res, err = h.join(tx, out, req)
		return err
	})
	return res, err
}

func (h *Hub) join(tx *store.Tx, out *outbox, req JoinRequest) (JoinResult, error) {
	var res JoinResult
	id := req.Identity
	if req.Room == "" {
		return res, refuse("JOIN requires room name")
	}
	room, err := h.normRoom(req.Room)
	if err != nil {
		return res, err
	}
	if err := h.checkHubBan(tx, id); err != nil {
		return res, err
	}
	already := h.rrcPresent(room, id)
	if !already && len(h.roomsOf(id)) >= h.cfg.MaxRoomsPerPerson {
		return res, refuse("too many rooms")
	}
	now := h.nowMS()
	r, exists, err := tx.Room(room)
	if err != nil {
		return res, err
	}
	invited := false
	if exists {
		op, err := h.isRoomOp(tx, &r, id)
		if err != nil {
			return res, err
		}
		if invited, err = tx.InviteValid(room, id, now); err != nil {
			return res, err
		}
		if r.HasMode('i') && !op && !invited {
			return res, refuse("invite-only (+i)")
		}
		if r.Key != "" && !op && !invited && req.Key != r.Key {
			return res, refuse("bad key (+k)")
		}
	}
	if err := h.checkRoomBan(tx, room, id); err != nil {
		return res, err
	}
	if err := tx.TouchIdentity(id, nil, now); err != nil {
		return res, err
	}
	if !exists {
		r = store.Room{Name: room, Founder: id, CreatedAt: now}
	}
	r.LastUsed = now
	if err := tx.PutRoom(&r); err != nil {
		return res, err
	}
	if invited {
		if err := tx.DeleteInvite(room, id); err != nil {
			return res, err
		}
	}
	name, err := h.displayName(tx, id)
	if err != nil {
		return res, err
	}
	res.Room = r
	res.FirstLink = !already

	if !already {
		if res.Replay, res.ReplayNote, err = h.catchUp(tx, id, room); err != nil {
			return res, err
		}
		last, err := tx.LastMessageID(room)
		if err != nil {
			return res, err
		}
		res.SeenUpTo = last
		if err := tx.SetCursor(id, room, store.ViaRRC, last, now); err != nil {
			return res, err
		}
		if room == h.cfg.GroupRoom {
			// Presence isn't updated until commit, so this is "before".
			onAlready, err := h.personOnRRC(tx, id, room, nil)
			if err != nil {
				return res, err
			}
			if !onAlready {
				if err := h.autoPauseChanged(tx, out, id, true); err != nil {
					return res, err
				}
			}
		}
		out.emit(JoinedEvent{Room: room, Identity: id, Name: name, Via: ViaRRC})
	}
	out.then(func() {
		members := h.presence[room]
		if members == nil {
			members = map[string]*presenceEntry{}
			h.presence[room] = members
		}
		e := members[hexID(id)]
		if e == nil {
			e = &presenceEntry{identity: id}
			members[hexID(id)] = e
		}
		e.name = name
		e.rrcLinks++
	})
	// Members as they will be once this join is applied.
	res.Members = h.membersAfterJoin(tx, room, id, name)
	return res, nil
}

func (h *Hub) membersAfterJoin(tx *store.Tx, room string, id []byte, name string) []Member {
	ms := h.members(tx, room)
	for i := range ms {
		if bytes.Equal(ms[i].Identity, id) {
			ms[i].RRC = true
			return ms
		}
	}
	ms = append(ms, Member{Identity: id, Name: name, RRC: true})
	sortMembers(ms)
	return ms
}

// members lists everyone present in a room: RRC links, and for the group
// room the LXMF members whose delivery is not paused.
func (h *Hub) members(tx *store.Tx, room string) []Member {
	byID := map[string]*Member{}
	for k, e := range h.presence[room] {
		if e.rrcLinks > 0 {
			byID[k] = &Member{Identity: e.identity, Name: e.name, RRC: true}
		}
	}
	if room == h.cfg.GroupRoom {
		group, err := tx.Members()
		if err != nil {
			h.log.Error("list group members", "err", err)
		}
		for _, gm := range group {
			if paused, err := h.lxmfPaused(tx, gm.LXMFMode, gm.Identity); err != nil {
				h.log.Error("group member pause", "err", err)
			} else if paused {
				continue
			}
			k := hexID(gm.Identity)
			if m := byID[k]; m != nil {
				m.LXMF = true
				continue
			}
			name, err := h.displayName(tx, gm.Identity)
			if err != nil {
				h.log.Error("member name", "err", err)
			}
			byID[k] = &Member{Identity: gm.Identity, Name: name, LXMF: true}
		}
	}
	// A person with apps on both lists is one member (ADR 0007).
	byPerson := map[int64]*Member{}
	out := make([]Member, 0, len(byID))
	for _, m := range byID {
		sp, ok, err := tx.PersonOf(m.Identity)
		if err != nil {
			h.log.Error("member's person", "err", err)
		}
		if !ok || err != nil {
			out = append(out, *m)
			continue
		}
		if first := byPerson[sp.ID]; first != nil {
			first.RRC, first.LXMF = first.RRC || m.RRC, first.LXMF || m.LXMF
			if string(m.Identity) < string(first.Identity) {
				first.Identity = m.Identity
			}
			continue
		}
		byPerson[sp.ID] = m
	}
	for _, m := range byPerson {
		out = append(out, *m)
	}
	sortMembers(out)
	return out
}

func sortMembers(ms []Member) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Name != ms[j].Name {
			return ms[i].Name < ms[j].Name
		}
		return string(ms[i].Identity) < string(ms[j].Identity)
	})
}

// Members returns everyone present in a room.
func (h *Hub) Members(ctx context.Context, room string) ([]Member, error) {
	var ms []Member
	err := h.do(ctx, func() {
		_ = h.st.View(ctx, func(tx *store.Tx) error {
			ms = h.members(tx, room)
			return nil
		})
	})
	return ms, err
}

// catchUp picks the messages to replay to an identity joining a room over RRC.
func (h *Hub) catchUp(tx *store.Tx, id []byte, room string) ([]store.Message, string, error) {
	c, seen, err := tx.Cursor(id, room, store.ViaRRC)
	if err != nil {
		return nil, "", err
	}
	if !seen {
		msgs, err := tx.LatestMessages(room, 0, h.cfg.ReplayFirstVisit)
		if err != nil || len(msgs) == 0 {
			return nil, "", err
		}
		return msgs, fmt.Sprintf("— the last %s in #%s —", plural(len(msgs), "message"), room), nil
	}
	missed, err := tx.CountAfter(room, c.MessageID)
	if err != nil || missed == 0 {
		return nil, "", err
	}
	since := h.clock(c.UpdatedAt)
	if missed > h.cfg.ReplayMax {
		msgs, err := tx.LatestMessages(room, 0, h.cfg.ReplayMax)
		if err != nil {
			return nil, "", err
		}
		return msgs, fmt.Sprintf("— %d messages since you were here (%s); the latest %d follow, /history %d shows more —",
			missed, since, len(msgs), min(missed, h.cfg.HistoryMax)), nil
	}
	msgs, err := tx.MessagesAfter(room, c.MessageID, missed)
	if err != nil {
		return nil, "", err
	}
	return msgs, fmt.Sprintf("— %s since you were here (%s) —", plural(len(msgs), "message"), since), nil
}

// Part takes one RRC link of an identity out of a room.
func (h *Hub) Part(ctx context.Context, id []byte, room string) (string, error) {
	var normalised string
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		if room == "" {
			return refuse("PART requires room name")
		}
		r, err := h.normRoom(room)
		if err != nil {
			return err
		}
		normalised = r
		return h.part(tx, out, id, r)
	})
	return normalised, err
}

// Disconnect takes an identity's closed RRC link out of the rooms it had joined.
func (h *Hub) Disconnect(ctx context.Context, id []byte, rooms []string) error {
	return h.update(ctx, func(tx *store.Tx, out *outbox) error {
		for _, r := range rooms {
			if err := h.part(tx, out, id, r); err != nil {
				return err
			}
		}
		return nil
	})
}

func (h *Hub) part(tx *store.Tx, out *outbox, id []byte, room string) error {
	e := h.presence[room][hexID(id)]
	if e == nil || e.rrcLinks == 0 {
		return nil // rrcd answers PARTED regardless; the adapter does that
	}
	now := h.nowMS()
	if e.rrcLinks == 1 {
		last, err := tx.LastMessageID(room)
		if err != nil {
			return err
		}
		if err := tx.SetCursor(id, room, store.ViaRRC, last, now); err != nil {
			return err
		}
		out.emit(PartedEvent{Room: room, Identity: id, Name: e.name, Via: ViaRRC})
		if room == h.cfg.GroupRoom {
			stillOn, err := h.personOnRRC(tx, id, room, id)
			if err != nil {
				return err
			}
			if !stillOn {
				if err := h.autoPauseChanged(tx, out, id, false); err != nil {
					return err
				}
			}
		}
		if err := h.forgetEmptyRoom(tx, room, id); err != nil {
			return err
		}
	}
	out.then(func() {
		e.rrcLinks--
		if e.rrcLinks <= 0 {
			delete(h.presence[room], hexID(id))
			if len(h.presence[room]) == 0 {
				delete(h.presence, room)
			}
		}
	})
	return nil
}

// forgetEmptyRoom deletes an unregistered room when its last member leaves,
// as rrcd does. leaving is the identity on its way out.
func (h *Hub) forgetEmptyRoom(tx *store.Tx, room string, leaving []byte) error {
	for k, e := range h.presence[room] {
		if k != hexID(leaving) && e.rrcLinks > 0 {
			return nil
		}
	}
	r, ok, err := tx.Room(room)
	if err != nil || !ok {
		return err
	}
	if r.Registered || h.cfg.isDefaultRoom(room) {
		r.LastUsed = h.nowMS()
		return tx.PutRoom(&r)
	}
	return tx.DeleteRoom(room)
}

// saveAllCursors records, for everyone present over RRC, that they have seen
// the room up to now. It runs every minute and at shutdown, so a crash
// replays at most a minute of messages twice.
func (h *Hub) saveAllCursors(ctx context.Context) {
	now := h.nowMS()
	err := h.st.Update(ctx, func(tx *store.Tx) error {
		for room, members := range h.presence {
			last, err := tx.LastMessageID(room)
			if err != nil {
				return err
			}
			for _, e := range members {
				if e.rrcLinks > 0 {
					if err := tx.SetCursor(e.identity, room, store.ViaRRC, last, now); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		h.log.Error("save cursors", "err", err)
	}
}

// clock formats a store time for people: "Sat 18:02" in Scotland's time zone.
func (h *Hub) clock(ms int64) string {
	t := time.UnixMilli(ms).In(london)
	if h.now().In(london).Sub(t) > 6*24*time.Hour {
		return t.Format("2 Jan 15:04")
	}
	return t.Format("Mon 15:04")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
