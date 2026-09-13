package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Ban is a hub-wide ban. A ban by name covers every identity of the person,
// one row each.
type Ban struct {
	Identity  []byte
	Name      string // the name they had, for /bans
	Reason    string
	BannedBy  []byte
	BannedAt  int64
	ExpiresAt int64 // 0: never
}

// AddBan bans an identity everywhere. Banning again replaces the ban.
func (t *Tx) AddBan(b *Ban) error {
	if err := checkID(b.Identity); err != nil {
		return err
	}
	var by any
	if b.BannedBy != nil {
		by = b.BannedBy
	}
	_, err := t.exec(`
		INSERT INTO bans (identity, name, reason, banned_by, banned_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (identity) DO UPDATE SET name = excluded.name, reason = excluded.reason, banned_by = excluded.banned_by,
			banned_at = excluded.banned_at, expires_at = excluded.expires_at`,
		b.Identity, b.Name, b.Reason, by, b.BannedAt, b.ExpiresAt)
	if err != nil {
		return fmt.Errorf("ban %x: %w", b.Identity, err)
	}
	return nil
}

// RemoveBan lifts a ban and reports whether there was one.
func (t *Tx) RemoveBan(identity []byte) (bool, error) {
	res, err := t.exec(`DELETE FROM bans WHERE identity = ?`, identity)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// IsBanned reports whether an identity is banned hub-wide.
func (t *Tx) IsBanned(identity []byte) (bool, error) {
	var one int
	err := t.queryRow(`SELECT 1 FROM bans WHERE identity = ?`, identity).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Bans lists hub-wide bans, most recent first.
func (t *Tx) Bans() ([]Ban, error) {
	return t.bans(`ORDER BY banned_at DESC, identity`)
}

// ExpiredBans lists timed bans that have run out by now. The minute
// maintenance tick lifts them.
func (t *Tx) ExpiredBans(now int64) ([]Ban, error) {
	return t.bans(`WHERE expires_at != 0 AND expires_at <= ? ORDER BY expires_at, identity`, now)
}

func (t *Tx) bans(q string, args ...any) ([]Ban, error) {
	rows, err := t.query(`SELECT identity, name, reason, banned_by, banned_at, expires_at FROM bans `+q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Ban
	for rows.Next() {
		var b Ban
		if err := rows.Scan(&b.Identity, &b.Name, &b.Reason, &b.BannedBy, &b.BannedAt, &b.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Audit records an administrative action.
func (t *Tx) Audit(at int64, actor []byte, action, target, detail string) error {
	var a any
	if actor != nil {
		a = actor
	}
	if _, err := t.exec(`INSERT INTO audit (at, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`, at, a, action, target, detail); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	ID     int64
	At     int64
	Actor  []byte
	Action string
	Target string
	Detail string
}

// AuditLog returns the most recent audit entries, newest first.
func (t *Tx) AuditLog(limit int) ([]AuditEntry, error) {
	rows, err := t.query(`SELECT id, at, actor, action, target, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Meta returns a value from the key/value table.
func (t *Tx) Meta(key string) (string, bool, error) {
	var v string
	err := t.queryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetMeta stores a value in the key/value table.
func (t *Tx) SetMeta(key, value string) error {
	_, err := t.exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
