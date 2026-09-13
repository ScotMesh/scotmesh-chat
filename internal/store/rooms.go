package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Room is a room's persistent state. Rooms that are not registered are kept
// only while they have members; the hub deletes them when they empty.
type Room struct {
	Name       string
	Topic      string
	Founder    []byte // nil if nobody has founded it
	Registered bool
	Modes      string // letters from "ikmnpt", sorted
	Key        string // +k
	CreatedAt  int64
	LastUsed   int64
}

// HasMode reports whether a mode letter is set.
func (r *Room) HasMode(m byte) bool { return strings.IndexByte(r.Modes, m) >= 0 }

// SetMode sets or clears a mode letter, keeping Modes sorted.
func (r *Room) SetMode(m byte, on bool) {
	set := map[byte]bool{}
	for i := 0; i < len(r.Modes); i++ {
		set[r.Modes[i]] = true
	}
	set[m] = on
	var b []byte
	for k, v := range set {
		if v {
			b = append(b, k)
		}
	}
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	r.Modes = string(b)
}

// Role is a per-room role.
type Role string

// Room roles.
const (
	RoleOp    Role = "op"
	RoleVoice Role = "voice"
	RoleBan   Role = "ban"
)

// Room returns a room by (normalised) name.
func (t *Tx) Room(name string) (Room, bool, error) {
	var r Room
	var reg int
	err := t.queryRow(`SELECT name, topic, founder, registered, modes, room_key, created_at, last_used FROM rooms WHERE name = ?`, name).
		Scan(&r.Name, &r.Topic, &r.Founder, &reg, &r.Modes, &r.Key, &r.CreatedAt, &r.LastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Room{}, false, nil
	}
	if err != nil {
		return Room{}, false, fmt.Errorf("room %q: %w", name, err)
	}
	r.Registered = reg == 1
	return r, true, nil
}

// Rooms returns every stored room, by name.
func (t *Tx) Rooms() ([]Room, error) {
	rows, err := t.query(`SELECT name, topic, founder, registered, modes, room_key, created_at, last_used FROM rooms ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("rooms: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Room
	for rows.Next() {
		var r Room
		var reg int
		if err := rows.Scan(&r.Name, &r.Topic, &r.Founder, &reg, &r.Modes, &r.Key, &r.CreatedAt, &r.LastUsed); err != nil {
			return nil, err
		}
		r.Registered = reg == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// PutRoom creates or replaces a room.
func (t *Tx) PutRoom(r *Room) error {
	var founder any
	if r.Founder != nil {
		if err := checkID(r.Founder); err != nil {
			return err
		}
		founder = r.Founder
	}
	for i := 0; i < len(r.Modes); i++ {
		if strings.IndexByte("ikmnpt", r.Modes[i]) < 0 {
			return fmt.Errorf("room %q: unknown mode %q", r.Name, r.Modes[i])
		}
	}
	reg := 0
	if r.Registered {
		reg = 1
	}
	_, err := t.exec(`
		INSERT INTO rooms (name, topic, founder, registered, modes, room_key, created_at, last_used)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			topic = excluded.topic, founder = excluded.founder, registered = excluded.registered,
			modes = excluded.modes, room_key = excluded.room_key, last_used = excluded.last_used`,
		r.Name, r.Topic, founder, reg, r.Modes, r.Key, r.CreatedAt, r.LastUsed)
	if err != nil {
		return fmt.Errorf("put room %q: %w", r.Name, err)
	}
	return nil
}

// DeleteRoom removes a room with its roles and invites.
func (t *Tx) DeleteRoom(name string) error {
	if _, err := t.exec(`DELETE FROM rooms WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete room %q: %w", name, err)
	}
	return nil
}

// SetRole grants or removes a role in a stored room.
func (t *Tx) SetRole(room string, identity []byte, role Role, on bool, by []byte, now int64) error {
	if err := checkID(identity); err != nil {
		return err
	}
	if !on {
		_, err := t.exec(`DELETE FROM room_roles WHERE room = ? AND identity = ? AND role = ?`, room, identity, string(role))
		return err
	}
	var setBy any
	if by != nil {
		setBy = by
	}
	_, err := t.exec(`
		INSERT INTO room_roles (room, identity, role, set_by, set_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (room, identity, role) DO NOTHING`, room, identity, string(role), setBy, now)
	if err != nil {
		return fmt.Errorf("set %s in %q: %w", role, room, err)
	}
	return nil
}

// HasRole reports whether identity holds role in room.
func (t *Tx) HasRole(room string, identity []byte, role Role) (bool, error) {
	var one int
	err := t.queryRow(`SELECT 1 FROM room_roles WHERE room = ? AND identity = ? AND role = ?`, room, identity, string(role)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RoleHolders lists the identities holding role in room, in hash order.
func (t *Tx) RoleHolders(room string, role Role) ([][]byte, error) {
	rows, err := t.query(`SELECT identity FROM room_roles WHERE room = ? AND role = ? ORDER BY identity`, room, string(role))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out [][]byte
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetInvite invites identity to room until expiresAt.
func (t *Tx) SetInvite(room string, identity []byte, expiresAt int64) error {
	if err := checkID(identity); err != nil {
		return err
	}
	_, err := t.exec(`
		INSERT INTO invites (room, identity, expires_at) VALUES (?, ?, ?)
		ON CONFLICT (room, identity) DO UPDATE SET expires_at = excluded.expires_at`, room, identity, expiresAt)
	return err
}

// InviteValid reports whether identity holds an unexpired invite to room.
func (t *Tx) InviteValid(room string, identity []byte, now int64) (bool, error) {
	var exp int64
	err := t.queryRow(`SELECT expires_at FROM invites WHERE room = ? AND identity = ?`, room, identity).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exp > now, nil
}

// Invite is an outstanding invitation.
type Invite struct {
	Identity  []byte
	ExpiresAt int64
}

// Invites lists unexpired invites to room.
func (t *Tx) Invites(room string, now int64) ([]Invite, error) {
	rows, err := t.query(`SELECT identity, expires_at FROM invites WHERE room = ? AND expires_at > ? ORDER BY identity`, room, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Invite
	for rows.Next() {
		var in Invite
		if err := rows.Scan(&in.Identity, &in.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// DeleteInvite removes an invite, used or not.
func (t *Tx) DeleteInvite(room string, identity []byte) error {
	_, err := t.exec(`DELETE FROM invites WHERE room = ? AND identity = ?`, room, identity)
	return err
}

// PruneInvites deletes expired invites everywhere.
func (t *Tx) PruneInvites(now int64) (int64, error) {
	res, err := t.exec(`DELETE FROM invites WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
