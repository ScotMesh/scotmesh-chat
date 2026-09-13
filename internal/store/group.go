package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Member is a member of the LXMF group.
type Member struct {
	Identity  []byte
	JoinedAt  int64
	AwayUntil int64 // direct delivery failed; send via propagation until then
	LXMFMode  LXMFMode
	LXMFMine  bool
}

// AddMember adds identity to the LXMF group; joining again keeps the
// original join time. The identity must exist.
func (t *Tx) AddMember(identity []byte, now int64) (added bool, err error) {
	if err := checkID(identity); err != nil {
		return false, err
	}
	res, err := t.exec(`INSERT INTO members (identity, joined_at) VALUES (?, ?) ON CONFLICT (identity) DO NOTHING`, identity, now)
	if err != nil {
		return false, fmt.Errorf("add member %x: %w", identity, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RemoveMember removes identity from the group with its unfinished
// deliveries, whispers included.
func (t *Tx) RemoveMember(identity []byte) (removed bool, err error) {
	for _, table := range []string{"deliveries", "whisper_deliveries"} {
		if _, err := t.exec(`DELETE FROM `+table+` WHERE identity = ?`, identity); err != nil {
			return false, err
		}
	}
	res, err := t.exec(`DELETE FROM members WHERE identity = ?`, identity)
	if err != nil {
		return false, fmt.Errorf("remove member %x: %w", identity, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// IsMember reports whether identity is in the group.
func (t *Tx) IsMember(identity []byte) (bool, error) {
	var one int
	err := t.queryRow(`SELECT 1 FROM members WHERE identity = ?`, identity).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Members lists the group with each member's LXMF setting, by join time.
func (t *Tx) Members() ([]Member, error) {
	rows, err := t.query(`
		SELECT m.identity, m.joined_at, m.away_until, i.lxmf_mode, i.lxmf_mine
		FROM members m JOIN identities i ON i.id = m.identity
		ORDER BY m.joined_at, m.identity`)
	if err != nil {
		return nil, fmt.Errorf("members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Member
	for rows.Next() {
		var m Member
		var mode string
		var mine int
		if err := rows.Scan(&m.Identity, &m.JoinedAt, &m.AwayUntil, &mode, &mine); err != nil {
			return nil, err
		}
		m.LXMFMode = LXMFMode(mode)
		m.LXMFMine = mine == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetAway marks a member as away (direct delivery failing) until the given time.
func (t *Tx) SetAway(identity []byte, until int64) error {
	_, err := t.exec(`UPDATE members SET away_until = ? WHERE identity = ?`, until, identity)
	return err
}

// DeliveryState is where an LXMF delivery stands.
type DeliveryState string

// Delivery states. Queued means "not yet"; only Failed is a final failure.
const (
	DeliveryQueued     DeliveryState = "queued"
	DeliverySent       DeliveryState = "sent"
	DeliveryPropagated DeliveryState = "propagated"
	DeliveryProven     DeliveryState = "proven"
	DeliveryFailed     DeliveryState = "failed"
)

// Delivery is one message, or one whisper, on its way to one member.
type Delivery struct {
	MessageID int64 // the whisper's ID when Whisper is set
	Whisper   bool
	Identity  []byte
	State     DeliveryState
	Attempts  int
	PartsDone int // parts of a split message that arrived
	NextAt    int64
	UpdatedAt int64
}

// PutDelivery creates or updates a delivery.
func (t *Tx) PutDelivery(d *Delivery) error {
	if err := checkID(d.Identity); err != nil {
		return err
	}
	table, key := "deliveries", "message_id"
	if d.Whisper {
		table, key = "whisper_deliveries", "whisper_id"
	}
	_, err := t.exec(`
		INSERT INTO `+table+` (`+key+`, identity, state, attempts, parts_done, next_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (`+key+`, identity) DO UPDATE SET
			state = excluded.state, attempts = excluded.attempts, parts_done = excluded.parts_done,
			next_at = excluded.next_at, updated_at = excluded.updated_at`,
		d.MessageID, d.Identity, string(d.State), d.Attempts, d.PartsDone, d.NextAt, d.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put delivery %d to %x: %w", d.MessageID, d.Identity, err)
	}
	return nil
}

// DueDeliveries returns queued deliveries whose time has come: whispers
// first, then messages, oldest first.
func (t *Tx) DueDeliveries(now int64, limit int) ([]Delivery, error) {
	rows, err := t.query(`
		SELECT 1, whisper_id, identity, state, attempts, parts_done, next_at, updated_at FROM whisper_deliveries
		WHERE state = 'queued' AND next_at <= ?
		UNION ALL
		SELECT 0, message_id, identity, state, attempts, parts_done, next_at, updated_at FROM deliveries
		WHERE state = 'queued' AND next_at <= ?
		ORDER BY 1 DESC, 2, 3 LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("due deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var st string
		if err := rows.Scan(&d.Whisper, &d.MessageID, &d.Identity, &st, &d.Attempts, &d.PartsDone, &d.NextAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.State = DeliveryState(st)
		out = append(out, d)
	}
	return out, rows.Err()
}

// FinishDeliveries deletes deliveries in a final state updated before cutoff.
func (t *Tx) FinishDeliveries(cutoff int64) (int64, error) {
	var total int64
	for _, table := range []string{"deliveries", "whisper_deliveries"} {
		res, err := t.exec(`DELETE FROM `+table+` WHERE state IN ('proven', 'propagated', 'failed') AND updated_at < ?`, cutoff)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
