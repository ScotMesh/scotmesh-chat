package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNameTaken is returned when a name's skeleton is claimed by another identity.
var ErrNameTaken = errors.New("name is taken")

// LXMFMode is a person's LXMF delivery setting (ADR 0006).
type LXMFMode string

// LXMF delivery settings.
const (
	LXMFOn   LXMFMode = "on"
	LXMFOff  LXMFMode = "off"
	LXMFAuto LXMFMode = "auto" // paused while the identity has an RRC link
)

// ParseLXMFMode accepts "on", "off" or "auto", case-insensitively.
func ParseLXMFMode(s string) (LXMFMode, bool) {
	switch m := LXMFMode(strings.ToLower(strings.TrimSpace(s))); m {
	case LXMFOn, LXMFOff, LXMFAuto:
		return m, true
	}
	return "", false
}

// Identity is a Reticulum identity the hub has seen. LXMF settings belong to
// the identity, since each app has its own LXMF address; the name, role and
// other preferences belong to its Person.
type Identity struct {
	ID          []byte
	PublicKey   []byte // nil until known
	FirstSeen   int64
	LastSeen    int64
	LXMFMode    LXMFMode
	LXMFMine    bool  // also deliver their own RRC and page messages by LXMF
	Person      int64 // the person this identity is part of
	PersonSince int64 // when it became part of that person
}

// Name is a claimed name. It belongs to a person; Identity is that person's
// primary identity, the one that has been part of it longest.
type Name struct {
	Person    int64
	Identity  []byte
	Name      string
	Skeleton  string
	ClaimedAt int64
}

// TouchIdentity records that an identity was seen, creating it, as a person
// of its own, if new. A non-nil publicKey is stored; a nil one leaves a known
// key in place.
func (t *Tx) TouchIdentity(id, publicKey []byte, now int64) error {
	if err := checkID(id); err != nil {
		return err
	}
	var pk any
	if publicKey != nil {
		pk = publicKey
	}
	res, err := t.exec(`UPDATE identities SET last_seen = max(last_seen, ?), public_key = coalesce(?, public_key) WHERE id = ?`, now, pk, id)
	if err != nil {
		return fmt.Errorf("touch identity %x: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	person, err := t.newPerson(now)
	if err != nil {
		return err
	}
	if _, err := t.exec(`INSERT INTO identities (id, public_key, first_seen, last_seen, person, person_since) VALUES (?, ?, ?, ?, ?, ?)`,
		id, pk, now, now, person, now); err != nil {
		return fmt.Errorf("add identity %x: %w", id, err)
	}
	return nil
}

const identityColumns = `id, public_key, first_seen, last_seen, lxmf_mode, lxmf_mine, person, person_since`

func scanIdentity(row interface{ Scan(...any) error }) (Identity, error) {
	var i Identity
	var mode string
	var mine int
	if err := row.Scan(&i.ID, &i.PublicKey, &i.FirstSeen, &i.LastSeen, &mode, &mine, &i.Person, &i.PersonSince); err != nil {
		return Identity{}, err
	}
	i.LXMFMode = LXMFMode(mode)
	i.LXMFMine = mine == 1
	return i, nil
}

// Identity returns an identity, or ok=false if the hub has never seen it.
func (t *Tx) Identity(id []byte) (Identity, bool, error) {
	i, err := scanIdentity(t.queryRow(`SELECT `+identityColumns+` FROM identities WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, fmt.Errorf("identity %x: %w", id, err)
	}
	return i, true, nil
}

// personID returns the person an identity is part of. The identity must exist.
func (t *Tx) personID(id []byte) (int64, error) {
	var p int64
	err := t.queryRow(`SELECT person FROM identities WHERE id = ?`, id).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("identity %x: %w", id, ErrNotFound)
	}
	return p, err
}

// SetLXMFMode changes an identity's LXMF delivery setting. The identity must exist.
func (t *Tx) SetLXMFMode(id []byte, mode LXMFMode) error {
	if _, ok := ParseLXMFMode(string(mode)); !ok {
		return fmt.Errorf("invalid LXMF mode %q", mode)
	}
	res, err := t.exec(`UPDATE identities SET lxmf_mode = ? WHERE id = ?`, string(mode), id)
	if err != nil {
		return fmt.Errorf("set LXMF mode for %x: %w", id, err)
	}
	return mustAffect(res, "identity %x", id)
}

// SetLXMFMine turns delivery of an identity's own RRC and page messages by
// LXMF on or off. The identity must exist.
func (t *Tx) SetLXMFMine(id []byte, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := t.exec(`UPDATE identities SET lxmf_mine = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("set LXMF mine for %x: %w", id, err)
	}
	return mustAffect(res, "identity %x", id)
}

// NameOf returns the name held by the person an identity is part of.
func (t *Tx) NameOf(id []byte) (Name, bool, error) {
	return t.scanName(`SELECT n.person, n.name, n.skeleton, n.claimed_at FROM names n JOIN identities i ON i.person = n.person WHERE i.id = ?`, id)
}

// NameBySkeleton returns the claim on a skeleton, whoever holds it.
func (t *Tx) NameBySkeleton(skeleton string) (Name, bool, error) {
	return t.scanName(`SELECT person, name, skeleton, claimed_at FROM names WHERE skeleton = ?`, skeleton)
}

func (t *Tx) scanName(q string, arg any) (Name, bool, error) {
	var n Name
	err := t.queryRow(q, arg).Scan(&n.Person, &n.Name, &n.Skeleton, &n.ClaimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Name{}, false, nil
	}
	if err != nil {
		return Name{}, false, fmt.Errorf("look up name: %w", err)
	}
	if n.Identity, err = t.primaryOf(n.Person); err != nil {
		return Name{}, false, err
	}
	return n, true, nil
}

// ClaimName gives the person id is part of the name, replacing any name it
// held, in one step. It fails with ErrNameTaken if another person holds the
// skeleton. Claiming the name you already hold (even re-cased) updates its
// spelling and keeps the original claim time. The identity must exist.
func (t *Tx) ClaimName(id []byte, name, skeleton string, now int64) error {
	if err := checkID(id); err != nil {
		return err
	}
	if name == "" || skeleton == "" {
		return errors.New("claim name: empty name or skeleton")
	}
	person, err := t.personID(id)
	if err != nil {
		return err
	}
	holder, ok, err := t.NameBySkeleton(skeleton)
	if err != nil {
		return err
	}
	if ok && holder.Person != person {
		return ErrNameTaken
	}
	claimedAt := now
	if ok {
		claimedAt = holder.ClaimedAt
	}
	_, err = t.exec(`
		INSERT INTO names (person, name, skeleton, claimed_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (person) DO UPDATE SET
			name = excluded.name, skeleton = excluded.skeleton, claimed_at = excluded.claimed_at`,
		person, name, skeleton, claimedAt)
	if err != nil {
		return fmt.Errorf("claim name for %x: %w", id, err)
	}
	return nil
}

// ReleaseName frees the name held by the person id is part of, and returns it.
func (t *Tx) ReleaseName(id []byte) (Name, bool, error) {
	n, ok, err := t.NameOf(id)
	if err != nil || !ok {
		return Name{}, ok, err
	}
	if _, err := t.exec(`DELETE FROM names WHERE person = ?`, n.Person); err != nil {
		return Name{}, false, fmt.Errorf("release name for %x: %w", id, err)
	}
	return n, true, nil
}

func mustAffect(res sql.Result, what string, args ...any) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", fmt.Sprintf(what, args...), ErrNotFound)
	}
	return nil
}

// ErrNotFound is returned by updates to a row that does not exist.
var ErrNotFound = errors.New("not found")
