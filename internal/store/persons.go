package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Rank is a person's rank on the hub (ADR 0008). Owners are the config
// file's admins; they are not stored.
type Rank string

// Hub roles, lowest first.
const (
	RankMember Rank = ""
	RankMod    Rank = "mod"
	RankAdmin  Rank = "admin"
)

// Prefs are a person's preferences. Every one can be changed from RRC, LXMF
// and the page.
type Prefs struct {
	ShareLXMF    bool // show their LXMF address on their profile
	Whispers     bool // accept whispers at all
	WhispersLXMF bool // send whispers to their LXMF app when they're a group member
	PageLines    int  // messages on the chat page: 10, 20, 30, 50 or 100
	PageFlag     bool // show the chat page's banner (the Saltire, unless the operator set another one)
	PageRefresh  int  // seconds between chat page refreshes: 0 (off), 10, 30 or 60
}

// Allowed preference values.
var (
	PageLineChoices    = []int{10, 20, 30, 50, 100}
	PageRefreshChoices = []int{0, 10, 30, 60}
)

// DefaultPrefs are what a new person starts with.
func DefaultPrefs() Prefs {
	return Prefs{ShareLXMF: true, Whispers: true, WhispersLXMF: true, PageLines: 30, PageFlag: true, PageRefresh: 30}
}

// Validate reports a preference outside its allowed values.
func (p Prefs) Validate() error {
	if !slices.Contains(PageLineChoices, p.PageLines) {
		return fmt.Errorf("page lines %d: must be one of %v", p.PageLines, PageLineChoices)
	}
	if !slices.Contains(PageRefreshChoices, p.PageRefresh) {
		return fmt.Errorf("page refresh %d: must be one of %v", p.PageRefresh, PageRefreshChoices)
	}
	return nil
}

// Person is one or more identities sharing a name, a role and preferences.
type Person struct {
	ID        int64
	CreatedAt int64
	Rank      Rank
	RankBy    []byte // who granted the rank
	RankAt    int64
	Prefs     Prefs
}

// personTables are the tables whose rows point at people.id, and what
// LinkIdentity does with each when a person is merged away. A test checks
// this list against the schema, so a new table can't be forgotten.
var personTables = map[string]string{
	"identities": "moved",
	"names":      "refused: the name must be released first",
	"link_codes": "dropped with the person",
	"whispers":   "moved",
	"ignores":    "moved",
}

func (t *Tx) newPerson(now int64) (int64, error) {
	res, err := t.exec(`INSERT INTO people (created_at) VALUES (?)`, now)
	if err != nil {
		return 0, fmt.Errorf("add person: %w", err)
	}
	return res.LastInsertId()
}

const personColumns = `id, created_at, role, role_by, role_at, share_lxmf, whispers, whispers_lxmf, page_lines, page_flag, page_refresh`

func scanPerson(row interface{ Scan(...any) error }) (Person, error) {
	var p Person
	var role string
	var share, whispers, whispersLXMF, flag int
	err := row.Scan(&p.ID, &p.CreatedAt, &role, &p.RankBy, &p.RankAt, &share, &whispers, &whispersLXMF,
		&p.Prefs.PageLines, &flag, &p.Prefs.PageRefresh)
	p.Rank = Rank(role)
	p.Prefs.ShareLXMF, p.Prefs.Whispers, p.Prefs.WhispersLXMF, p.Prefs.PageFlag = share == 1, whispers == 1, whispersLXMF == 1, flag == 1
	return p, err
}

// PersonOf returns the person an identity is part of, or ok=false if the hub
// has never seen the identity.
func (t *Tx) PersonOf(id []byte) (Person, bool, error) {
	p, err := scanPerson(t.queryRow(`SELECT `+prefixed("p.", personColumns)+` FROM people p JOIN identities i ON i.person = p.id WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Person{}, false, nil
	}
	if err != nil {
		return Person{}, false, fmt.Errorf("person of %x: %w", id, err)
	}
	return p, true, nil
}

// PersonByID returns a person.
func (t *Tx) PersonByID(person int64) (Person, bool, error) {
	p, err := scanPerson(t.queryRow(`SELECT `+personColumns+` FROM people WHERE id = ?`, person))
	if errors.Is(err, sql.ErrNoRows) {
		return Person{}, false, nil
	}
	if err != nil {
		return Person{}, false, fmt.Errorf("person %d: %w", person, err)
	}
	return p, true, nil
}

// PersonIdentities lists a person's identities, primary (longest part of
// the person) first.
func (t *Tx) PersonIdentities(person int64) ([]Identity, error) {
	rows, err := t.query(`SELECT `+identityColumns+` FROM identities WHERE person = ? ORDER BY person_since, id`, person)
	if err != nil {
		return nil, fmt.Errorf("identities of person %d: %w", person, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Identity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (t *Tx) primaryOf(person int64) ([]byte, error) {
	var id []byte
	err := t.queryRow(`SELECT id FROM identities WHERE person = ? ORDER BY person_since, id LIMIT 1`, person).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("primary identity of person %d: %w", person, err)
	}
	return id, nil
}

// SetPrefs stores the preferences of the person id is part of.
func (t *Tx) SetPrefs(id []byte, p Prefs) error {
	if err := p.Validate(); err != nil {
		return err
	}
	person, err := t.personID(id)
	if err != nil {
		return err
	}
	return t.SetPrefsOfPerson(person, p)
}

// SetRank gives the person id is part of a rank, or takes it away with
// RankMember.
func (t *Tx) SetRank(id []byte, rank Rank, by []byte, now int64) error {
	switch rank {
	case RankMember, RankMod, RankAdmin:
	default:
		return fmt.Errorf("invalid rank %q", rank)
	}
	person, err := t.personID(id)
	if err != nil {
		return err
	}
	var byArg any
	if by != nil && rank != RankMember {
		byArg = by
	}
	at := now
	if rank == RankMember {
		at = 0
	}
	if _, err := t.exec(`UPDATE people SET role = ?, role_by = ?, role_at = ? WHERE id = ?`, string(rank), byArg, at, person); err != nil {
		return fmt.Errorf("set rank for %x: %w", id, err)
	}
	return nil
}

// RankHolders lists the people with a rank, admins first, then by when
// they got it.
func (t *Tx) RankHolders() ([]Person, error) {
	rows, err := t.query(`SELECT ` + personColumns + ` FROM people WHERE role != '' ORDER BY role = 'mod', role_at, id`)
	if err != nil {
		return nil, fmt.Errorf("role holders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Person
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ErrPersonHasName is returned when an identity would be linked away from a
// person that still holds a name.
var ErrPersonHasName = errors.New("the identity's person holds a name")

// LinkIdentity makes id part of person `to` (ADR 0007). If that leaves id's
// old person with no identities, the old person goes, and its whispers and
// ignores move with id. It fails with ErrPersonHasName if the old person
// would be left empty while holding a name: release the name first. Linking
// an identity to the person it is already part of does nothing.
func (t *Tx) LinkIdentity(id []byte, to int64, now int64) error {
	from, err := t.personID(id)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if _, ok, err := t.PersonByID(to); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("person %d: %w", to, ErrNotFound)
	}
	var others int
	if err := t.queryRow(`SELECT count(*) FROM identities WHERE person = ? AND id != ?`, from, id).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		var named int
		if err := t.queryRow(`SELECT count(*) FROM names WHERE person = ?`, from).Scan(&named); err != nil {
			return err
		}
		if named > 0 {
			return ErrPersonHasName
		}
	}
	if _, err := t.exec(`UPDATE identities SET person = ?, person_since = ? WHERE id = ?`, to, now, id); err != nil {
		return fmt.Errorf("link %x: %w", id, err)
	}
	if others > 0 {
		return nil
	}
	// The old person's rows go with it (see personTables). An ignore of
	// each other is dropped: nobody ignores themselves.
	for _, m := range []struct {
		q    string
		args []any
	}{
		{`UPDATE whispers SET to_person = ? WHERE to_person = ?`, []any{to, from}},
		{`INSERT OR IGNORE INTO ignores (person, ignored, at) SELECT ?, ignored, at FROM ignores WHERE person = ? AND ignored != ?`, []any{to, from, to}},
		{`INSERT OR IGNORE INTO ignores (person, ignored, at) SELECT person, ?, at FROM ignores WHERE ignored = ? AND person != ?`, []any{to, from, to}},
	} {
		if _, err := t.exec(m.q, m.args...); err != nil {
			return fmt.Errorf("merge person %d into %d: %w", from, to, err)
		}
	}
	if _, err := t.exec(`DELETE FROM people WHERE id = ?`, from); err != nil {
		return fmt.Errorf("remove merged person %d: %w", from, err)
	}
	return nil
}

// UnlinkIdentity takes id out of its person into a new person of its own,
// with a copy of the preferences but no name and no role, and returns the new
// person. An identity that is its person's only one stays where it is.
func (t *Tx) UnlinkIdentity(id []byte, now int64) (int64, error) {
	p, ok, err := t.PersonOf(id)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("identity %x: %w", id, ErrNotFound)
	}
	var others int
	if err := t.queryRow(`SELECT count(*) FROM identities WHERE person = ? AND id != ?`, p.ID, id).Scan(&others); err != nil {
		return 0, err
	}
	if others == 0 {
		return p.ID, nil
	}
	fresh, err := t.newPerson(now)
	if err != nil {
		return 0, err
	}
	if err := t.SetPrefsOfPerson(fresh, p.Prefs); err != nil {
		return 0, err
	}
	if _, err := t.exec(`UPDATE identities SET person = ?, person_since = ? WHERE id = ?`, fresh, now, id); err != nil {
		return 0, fmt.Errorf("unlink %x: %w", id, err)
	}
	return fresh, nil
}

// SetPrefsOfPerson stores preferences by person rather than identity.
func (t *Tx) SetPrefsOfPerson(person int64, p Prefs) error {
	if err := p.Validate(); err != nil {
		return err
	}
	res, err := t.exec(`UPDATE people SET share_lxmf = ?, whispers = ?, whispers_lxmf = ?, page_lines = ?, page_flag = ?, page_refresh = ? WHERE id = ?`,
		b2i(p.ShareLXMF), b2i(p.Whispers), b2i(p.WhispersLXMF), p.PageLines, b2i(p.PageFlag), p.PageRefresh, person)
	if err != nil {
		return fmt.Errorf("set preferences for person %d: %w", person, err)
	}
	return mustAffect(res, "person %d", person)
}

// LinkCode is a one-time /link code, kept only as its hash.
type LinkCode struct {
	Hash        []byte // SHA-256 of the normalised code
	Person      int64  // the person the code links to
	IssuedBy    []byte
	IssuedAt    int64
	ExpiresAt   int64
	RedeemedBy  []byte // set once redeemed, while it waits for approval
	RedeemedVia Via
}

// PutLinkCode stores a new code for a person, replacing any of their codes
// not yet redeemed: a person has at most one live code.
func (t *Tx) PutLinkCode(c *LinkCode) error {
	if len(c.Hash) != 32 {
		return errors.New("link code hash must be 32 bytes")
	}
	if err := checkID(c.IssuedBy); err != nil {
		return err
	}
	if _, err := t.exec(`DELETE FROM link_codes WHERE person = ? AND redeemed_by IS NULL`, c.Person); err != nil {
		return err
	}
	_, err := t.exec(`INSERT INTO link_codes (code_hash, person, issued_by, issued_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		c.Hash, c.Person, c.IssuedBy, c.IssuedAt, c.ExpiresAt)
	if err != nil {
		return fmt.Errorf("add link code: %w", err)
	}
	return nil
}

const linkCodeColumns = `code_hash, person, issued_by, issued_at, expires_at, redeemed_by, coalesce(redeemed_via, '')`

func scanLinkCode(row interface{ Scan(...any) error }) (LinkCode, error) {
	var c LinkCode
	var via string
	err := row.Scan(&c.Hash, &c.Person, &c.IssuedBy, &c.IssuedAt, &c.ExpiresAt, &c.RedeemedBy, &via)
	c.RedeemedVia = Via(via)
	return c, err
}

// LinkCodeByHash returns a code, expired or not.
func (t *Tx) LinkCodeByHash(hash []byte) (LinkCode, bool, error) {
	c, err := scanLinkCode(t.queryRow(`SELECT `+linkCodeColumns+` FROM link_codes WHERE code_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return LinkCode{}, false, nil
	}
	if err != nil {
		return LinkCode{}, false, fmt.Errorf("link code: %w", err)
	}
	return c, true, nil
}

// RedeemLinkCode records who redeemed a code that needs approval.
func (t *Tx) RedeemLinkCode(hash, by []byte, via Via) error {
	if err := checkID(by); err != nil {
		return err
	}
	res, err := t.exec(`UPDATE link_codes SET redeemed_by = ?, redeemed_via = ? WHERE code_hash = ? AND redeemed_by IS NULL`, by, string(via), hash)
	if err != nil {
		return fmt.Errorf("redeem link code: %w", err)
	}
	return mustAffect(res, "unredeemed link code")
}

// PendingLinks lists a person's redeemed codes waiting for approval that
// haven't expired, oldest first.
func (t *Tx) PendingLinks(person, now int64) ([]LinkCode, error) {
	rows, err := t.query(`SELECT `+linkCodeColumns+` FROM link_codes WHERE person = ? AND redeemed_by IS NOT NULL AND expires_at > ? ORDER BY issued_at, code_hash`, person, now)
	if err != nil {
		return nil, fmt.Errorf("pending links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LinkCode
	for rows.Next() {
		c, err := scanLinkCode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteLinkCode removes a code once used, approved or denied.
func (t *Tx) DeleteLinkCode(hash []byte) error {
	_, err := t.exec(`DELETE FROM link_codes WHERE code_hash = ?`, hash)
	return err
}

// PruneLinkCodes deletes expired codes.
func (t *Tx) PruneLinkCodes(now int64) (int64, error) {
	res, err := t.exec(`DELETE FROM link_codes WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// prefixed qualifies a column list with a table alias: "a, b" -> "p.a, p.b".
func prefixed(prefix, columns string) string {
	parts := strings.Split(columns, ", ")
	for i := range parts {
		parts[i] = prefix + parts[i]
	}
	return strings.Join(parts, ", ")
}
