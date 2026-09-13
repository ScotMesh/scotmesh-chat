package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// TestMigration0004KeepsNamesAndMakesPeople upgrades a database as rc.3
// left it: every identity becomes a person of its own, names move to their
// person, and nothing else changes.
func TestMigration0004KeepsNamesAndMakesPeople(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	old := &Store{w: w, path: path}
	if err := old.migrateTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO identities (id, first_seen, last_seen, lxmf_mode, lxmf_mine) VALUES (x'01010101010101010101010101010101', 100, 900, 'auto', 1)`,
		`INSERT INTO identities (id, first_seen, last_seen) VALUES (x'02020202020202020202020202020202', 200, 800)`,
		`INSERT INTO identities (id, first_seen, last_seen) VALUES (x'03030303030303030303030303030303', 300, 300)`,
		`INSERT INTO names (identity, name, skeleton, claimed_at) VALUES (x'01010101010101010101010101010101', 'Alex', 'alex', 150)`,
		`INSERT INTO names (identity, name, skeleton, claimed_at) VALUES (x'02020202020202020202020202020202', 'Rab', 'rab', 250)`,
		`INSERT INTO members (identity, joined_at) VALUES (x'01010101010101010101010101010101', 160)`,
		`INSERT INTO bans (identity, reason, banned_at) VALUES (x'03030303030303030303030303030303', 'spam', 310)`,
	} {
		if _, err := w.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	update(t, s, func(tx *Tx) error {
		persons := map[int64]bool{}
		for b, want := range map[byte]string{1: "Alex", 2: "Rab", 3: ""} {
			i, ok, err := tx.Identity(id(b))
			if err != nil || !ok || i.Person == 0 {
				return fmt.Errorf("identity %d: %+v %v %v", b, i, ok, err)
			}
			if persons[i.Person] {
				return fmt.Errorf("identity %d shares person %d", b, i.Person)
			}
			persons[i.Person] = true
			if i.PersonSince != i.FirstSeen {
				return fmt.Errorf("identity %d person_since %d, want first_seen %d", b, i.PersonSince, i.FirstSeen)
			}
			n, named, err := tx.NameOf(id(b))
			if err != nil || named != (want != "") || n.Name != want {
				return fmt.Errorf("name of %d: %+v %v %v", b, n, named, err)
			}
			if named && (!bytes.Equal(n.Identity, id(b)) || n.Person != i.Person) {
				return fmt.Errorf("name %+v not tied to identity %d", n, b)
			}
			p, ok, err := tx.PersonOf(id(b))
			if err != nil || !ok || p.Prefs != DefaultPrefs() || p.Rank != RankMember || p.CreatedAt != i.FirstSeen {
				return fmt.Errorf("person of %d: %+v %v %v", b, p, ok, err)
			}
		}
		if i, _, _ := tx.Identity(id(1)); i.LXMFMode != LXMFAuto || !i.LXMFMine {
			return fmt.Errorf("LXMF settings lost: %+v", i)
		}
		if n, ok, _ := tx.NameBySkeleton("rab"); !ok || !bytes.Equal(n.Identity, id(2)) || n.ClaimedAt != 250 {
			return fmt.Errorf("rab by skeleton %+v", n)
		}
		if ok, _ := tx.IsMember(id(1)); !ok {
			return errors.New("member lost")
		}
		if bans, _ := tx.Bans(); len(bans) != 1 || bans[0].Reason != "spam" || bans[0].ExpiresAt != 0 || bans[0].Name != "" {
			return fmt.Errorf("bans %+v", bans)
		}
		// A new identity after the migration also gets a person of its own.
		if err := tx.TouchIdentity(id(4), nil, 1000); err != nil {
			return err
		}
		if p, ok, _ := tx.PersonOf(id(4)); !ok || persons[p.ID] {
			return fmt.Errorf("new identity's person %+v", p)
		}
		return nil
	})
	// The schema insists every identity has a person.
	err = s.Update(ctx, func(tx *Tx) error {
		_, err := tx.exec(`INSERT INTO identities (id, first_seen, last_seen) VALUES (?, 1, 1)`, id(9))
		return err
	})
	if err == nil {
		t.Error("an identity without a person was accepted")
	}
	err = s.Update(ctx, func(tx *Tx) error {
		_, err := tx.exec(`UPDATE identities SET person = NULL WHERE id = ?`, id(1))
		return err
	})
	if err == nil {
		t.Error("an identity's person was cleared")
	}
}

// TestPersonTablesMatchSchema: every table pointing at people is in
// personTables, so LinkIdentity's merge can't miss one.
func TestPersonTablesMatchSchema(t *testing.T) {
	s := openTest(t)
	var got []string
	err := s.View(context.Background(), func(tx *Tx) error {
		rows, err := tx.query(`SELECT DISTINCT m.name FROM sqlite_master m, pragma_foreign_key_list(m.name) f WHERE m.type = 'table' AND f."table" = 'people'`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			got = append(got, n)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for n := range personTables {
		want = append(want, n)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("tables referencing people %v; personTables lists %v", got, want)
	}
}

func TestPeopleShareANameAndLinkAndUnlink(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for b := byte(1); b <= 4; b++ {
			if err := tx.TouchIdentity(id(b), nil, int64(b)*100); err != nil {
				return err
			}
		}
		if err := tx.ClaimName(id(1), "Alex", "alex", 110); err != nil {
			return err
		}
		if err := tx.ClaimName(id(2), "Rab", "rab", 210); err != nil {
			return err
		}
		alex, _, _ := tx.PersonOf(id(1))
		rab, _, _ := tx.PersonOf(id(2))
		loner, _, _ := tx.PersonOf(id(3))

		// Rab's only identity can't be linked away while Rab holds a name.
		if err := tx.LinkIdentity(id(2), alex.ID, 300); !errors.Is(err, ErrPersonHasName) {
			return fmt.Errorf("linking a named person away: %w", err)
		}
		// Identity 3 had whispers and ignores; they come with it.
		if err := tx.AddWhisper(&Whisper{From: id(2), FromName: "Rab", ToPerson: loner.ID, ToName: "guest-0303", Body: "hi", Via: ViaRRC, SaidAt: 320}); err != nil {
			return err
		}
		if err := tx.Ignore(loner.ID, rab.ID, 330); err != nil {
			return err
		}
		if err := tx.Ignore(rab.ID, loner.ID, 331); err != nil {
			return err
		}
		if err := tx.Ignore(alex.ID, loner.ID, 332); err != nil { // Alex ignoring their own other app goes away
			return err
		}
		if err := tx.LinkIdentity(id(3), alex.ID, 400); err != nil {
			return err
		}
		if err := tx.LinkIdentity(id(3), alex.ID, 401); err != nil { // again: nothing to do
			return err
		}
		if n, ok, _ := tx.NameOf(id(3)); !ok || n.Name != "Alex" || !bytes.Equal(n.Identity, id(1)) {
			return fmt.Errorf("linked identity's name %+v", n)
		}
		if err := tx.ClaimName(id(3), "Alexander", "alexander", 410); err != nil { // renaming from a linked app renames the person
			return err
		}
		if n, _, _ := tx.NameOf(id(1)); n.Name != "Alexander" {
			return fmt.Errorf("rename from a linked identity: %+v", n)
		}
		if _, ok, _ := tx.PersonByID(loner.ID); ok {
			return errors.New("the emptied person was kept")
		}
		if ws, _ := tx.WhispersTo(alex.ID, 10); len(ws) != 1 || ws[0].Body != "hi" {
			return fmt.Errorf("whispers didn't move: %+v", ws)
		}
		if ok, _ := tx.IsIgnoring(alex.ID, rab.ID); !ok {
			return errors.New("an ignore by the merged person didn't move")
		}
		if ok, _ := tx.IsIgnoring(rab.ID, alex.ID); !ok {
			return errors.New("an ignore of the merged person didn't move")
		}
		if ids, _ := tx.PersonIdentities(alex.ID); len(ids) != 2 || !bytes.Equal(ids[0].ID, id(1)) || ids[1].PersonSince != 400 {
			return fmt.Errorf("identities of Alex %+v", ids)
		}
		// Linking to a person that doesn't exist, or an unknown identity.
		if err := tx.LinkIdentity(id(4), 9999, 500); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("link to nobody: %w", err)
		}
		if err := tx.LinkIdentity(id(8), alex.ID, 500); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("link an unknown identity: %w", err)
		}

		// Unlinking the primary: the other identity keeps the name.
		alexPrefs := DefaultPrefs()
		alexPrefs.PageLines, alexPrefs.ShareLXMF = 50, false
		if err := tx.SetPrefs(id(1), alexPrefs); err != nil {
			return err
		}
		if err := tx.SetRank(id(1), RankMod, id(9), 550); err != nil {
			return err
		}
		fresh, err := tx.UnlinkIdentity(id(1), 600)
		if err != nil {
			return err
		}
		if fresh == alex.ID {
			return errors.New("unlinking kept the person")
		}
		if n, ok, _ := tx.NameOf(id(3)); !ok || n.Name != "Alexander" || !bytes.Equal(n.Identity, id(3)) {
			return fmt.Errorf("after unlinking the primary, the name is %+v", n)
		}
		if _, ok, _ := tx.NameOf(id(1)); ok {
			return errors.New("the unlinked identity kept the name")
		}
		p, _, _ := tx.PersonOf(id(1))
		if p.ID != fresh || p.Rank != RankMember || p.Prefs != alexPrefs {
			return fmt.Errorf("unlinked person %+v: want no rank and a copy of the preferences", p)
		}
		if p, _, _ := tx.PersonOf(id(3)); p.Rank != RankMod {
			return fmt.Errorf("the rank didn't stay with the person: %+v", p)
		}
		// An identity alone in its person has nothing to unlink from.
		if same, err := tx.UnlinkIdentity(id(3), 700); err != nil || same != alex.ID {
			return fmt.Errorf("unlink alone: %d %w", same, err)
		}
		if _, err := tx.UnlinkIdentity(id(8), 700); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("unlink unknown: %w", err)
		}
		// Names still can't be shared between people.
		if err := tx.ClaimName(id(1), "ALEXANDER", "alexander", 800); !errors.Is(err, ErrNameTaken) {
			return fmt.Errorf("claimed another person's name: %w", err)
		}
		if err := tx.ClaimName(id(8), "Nobody", "nobody", 800); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("claim by an unknown identity: %w", err)
		}
		return nil
	})
}

func TestPrefsAndRanks(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for b := byte(1); b <= 3; b++ {
			if err := tx.TouchIdentity(id(b), nil, int64(b)); err != nil {
				return err
			}
		}
		for _, bad := range []Prefs{
			{PageLines: 25, PageRefresh: 30},
			{PageLines: 30, PageRefresh: 5},
		} {
			if err := tx.SetPrefs(id(1), bad); err == nil {
				return fmt.Errorf("accepted %+v", bad)
			}
		}
		want := Prefs{ShareLXMF: false, Whispers: false, WhispersLXMF: false, PageLines: 100, PageFlag: false, PageRefresh: 0}
		if err := tx.SetPrefs(id(1), want); err != nil {
			return err
		}
		if p, _, _ := tx.PersonOf(id(1)); p.Prefs != want {
			return fmt.Errorf("prefs %+v, want %+v", p.Prefs, want)
		}
		if err := tx.SetPrefs(id(7), DefaultPrefs()); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("prefs for unknown: %w", err)
		}
		if err := tx.SetPrefsOfPerson(9999, DefaultPrefs()); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("prefs for unknown person: %w", err)
		}

		if err := tx.SetRank(id(2), RankMod, id(1), 20); err != nil {
			return err
		}
		if err := tx.SetRank(id(3), RankAdmin, id(1), 30); err != nil {
			return err
		}
		if err := tx.SetRank(id(1), "owner", nil, 40); err == nil {
			return errors.New("owner stored as a rank")
		}
		if err := tx.SetRank(id(7), RankMod, nil, 40); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("rank for unknown: %w", err)
		}
		holders, err := tx.RankHolders()
		if err != nil || len(holders) != 2 || holders[0].Rank != RankAdmin || holders[1].Rank != RankMod || !bytes.Equal(holders[1].RankBy, id(1)) || holders[1].RankAt != 20 {
			return fmt.Errorf("rank holders %+v %v", holders, err)
		}
		if err := tx.SetRank(id(2), RankMember, id(1), 50); err != nil {
			return err
		}
		if p, _, _ := tx.PersonOf(id(2)); p.Rank != RankMember || p.RankBy != nil || p.RankAt != 0 {
			return fmt.Errorf("demoted %+v", p)
		}
		if _, ok, _ := tx.PersonOf(id(7)); ok {
			return errors.New("person of an unknown identity")
		}
		return nil
	})
}

func TestLinkCodes(t *testing.T) {
	s := openTest(t)
	hash := func(code string) []byte { h := sha256.Sum256([]byte(code)); return h[:] }
	update(t, s, func(tx *Tx) error {
		for b := byte(1); b <= 2; b++ {
			if err := tx.TouchIdentity(id(b), nil, 1); err != nil {
				return err
			}
		}
		p, _, _ := tx.PersonOf(id(1))
		first := &LinkCode{Hash: hash("AAAA"), Person: p.ID, IssuedBy: id(1), IssuedAt: 100, ExpiresAt: 700}
		if err := tx.PutLinkCode(first); err != nil {
			return err
		}
		// A second code replaces the first: one live code per person.
		if err := tx.PutLinkCode(&LinkCode{Hash: hash("BBBB"), Person: p.ID, IssuedBy: id(1), IssuedAt: 200, ExpiresAt: 800}); err != nil {
			return err
		}
		if _, ok, _ := tx.LinkCodeByHash(hash("AAAA")); ok {
			return errors.New("the replaced code still works")
		}
		c, ok, err := tx.LinkCodeByHash(hash("BBBB"))
		if err != nil || !ok || c.Person != p.ID || c.RedeemedBy != nil || c.RedeemedVia != "" {
			return fmt.Errorf("code %+v %v %v", c, ok, err)
		}
		if err := tx.RedeemLinkCode(hash("BBBB"), id(2), ViaLXMF); err != nil {
			return err
		}
		if err := tx.RedeemLinkCode(hash("BBBB"), id(2), ViaLXMF); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("redeemed twice: %w", err)
		}
		// A redeemed code waiting for approval survives a new code.
		if err := tx.PutLinkCode(&LinkCode{Hash: hash("CCCC"), Person: p.ID, IssuedBy: id(1), IssuedAt: 300, ExpiresAt: 900}); err != nil {
			return err
		}
		pending, err := tx.PendingLinks(p.ID, 500)
		if err != nil || len(pending) != 1 || !bytes.Equal(pending[0].RedeemedBy, id(2)) || pending[0].RedeemedVia != ViaLXMF {
			return fmt.Errorf("pending %+v %v", pending, err)
		}
		if pending, _ := tx.PendingLinks(p.ID, 800); len(pending) != 0 {
			return fmt.Errorf("expired approval still pending: %+v", pending)
		}
		if n, err := tx.PruneLinkCodes(850); err != nil || n != 1 {
			return fmt.Errorf("pruned %d %v", n, err)
		}
		if err := tx.DeleteLinkCode(hash("CCCC")); err != nil {
			return err
		}
		if _, ok, _ := tx.LinkCodeByHash(hash("CCCC")); ok {
			return errors.New("deleted code found")
		}
		if err := tx.PutLinkCode(&LinkCode{Hash: []byte{1}, Person: p.ID, IssuedBy: id(1)}); err == nil {
			return errors.New("short hash accepted")
		}
		if err := tx.PutLinkCode(&LinkCode{Hash: hash("DDDD"), Person: p.ID, IssuedBy: []byte{1}}); err == nil {
			return errors.New("short issuer accepted")
		}
		if err := tx.RedeemLinkCode(hash("DDDD"), []byte{1}, ViaRRC); err == nil {
			return errors.New("short redeemer accepted")
		}
		return nil
	})
}

func TestWhispersIgnoresAndTheirDeliveries(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for b := byte(1); b <= 3; b++ {
			if err := tx.TouchIdentity(id(b), nil, 1); err != nil {
				return err
			}
		}
		rab, _, _ := tx.PersonOf(id(2))
		var ids []int64
		for i, body := range []string{"one", "two", "three"} {
			w := &Whisper{From: id(1), FromName: "Alex", ToPerson: rab.ID, ToName: "Rab", Body: body, Via: ViaPage, SaidAt: int64(100 * (i + 1))}
			if err := tx.AddWhisper(w); err != nil {
				return err
			}
			ids = append(ids, w.ID)
		}
		if err := tx.AddWhisper(&Whisper{From: []byte{1}, ToPerson: rab.ID}); err == nil {
			return errors.New("short sender accepted")
		}
		if ws, _ := tx.WhispersTo(rab.ID, 2); len(ws) != 2 || ws[0].Body != "two" || ws[1].Body != "three" || ws[1].Via != ViaPage {
			return fmt.Errorf("latest two %+v", ws)
		}
		if err := tx.MarkWhisperRRC(ids[0], 150); err != nil {
			return err
		}
		if ws, _ := tx.UnsentRRCWhispers(rab.ID); len(ws) != 2 || ws[0].ID != ids[1] {
			return fmt.Errorf("unsent on RRC %+v", ws)
		}
		if n, err := tx.MarkWhispersRead(rab.ID, ids[1], 400); err != nil || n != 2 {
			return fmt.Errorf("marked read %d %v", n, err)
		}
		if n, _ := tx.UnreadWhispers(rab.ID); n != 1 {
			return fmt.Errorf("unread %d", n)
		}
		if w, ok, _ := tx.LastWhisperTo(rab.ID); !ok || w.Body != "three" || !bytes.Equal(w.From, id(1)) {
			return fmt.Errorf("last whisper %+v", w)
		}
		if w, ok, _ := tx.Whisper(ids[0]); !ok || w.RRCAt != 150 || w.ReadAt != 400 {
			return fmt.Errorf("whisper %+v", w)
		}
		if _, ok, _ := tx.Whisper(9999); ok {
			return errors.New("unknown whisper found")
		}
		if _, ok, _ := tx.LastWhisperTo(9999); ok {
			return errors.New("last whisper to nobody")
		}

		// A whisper delivery by LXMF comes before group messages, and goes
		// when the member leaves the group.
		if _, err := tx.AddMember(id(2), 1); err != nil {
			return err
		}
		m := Message{Room: "scotmesh", Kind: KindMsg, Author: id(1), AuthorName: "Alex", Body: "hi all", Via: ViaRRC, SaidAt: 1, HubAt: 1}
		msgID, _, err := tx.AddMessage(&m)
		if err != nil {
			return err
		}
		for _, d := range []Delivery{
			{MessageID: msgID, Identity: id(2), State: DeliveryQueued, NextAt: 1, UpdatedAt: 1},
			{MessageID: ids[2], Whisper: true, Identity: id(2), State: DeliveryQueued, NextAt: 5, UpdatedAt: 1},
		} {
			if err := tx.PutDelivery(&d); err != nil {
				return err
			}
		}
		due, err := tx.DueDeliveries(10, 10)
		if err != nil || len(due) != 2 || !due[0].Whisper || due[0].MessageID != ids[2] || due[1].Whisper {
			return fmt.Errorf("due %+v %v", due, err)
		}
		due[0].State, due[0].UpdatedAt = DeliveryProven, 2
		if err := tx.PutDelivery(&due[0]); err != nil {
			return err
		}
		if n, err := tx.FinishDeliveries(3); err != nil || n != 1 {
			return fmt.Errorf("finished %d %v", n, err)
		}
		if err := tx.PutDelivery(&Delivery{MessageID: ids[1], Whisper: true, Identity: id(2), State: DeliveryQueued, NextAt: 1, UpdatedAt: 1}); err != nil {
			return err
		}
		if _, err := tx.RemoveMember(id(2)); err != nil {
			return err
		}
		if due, _ := tx.DueDeliveries(10, 10); len(due) != 0 {
			return fmt.Errorf("deliveries outlived membership: %+v", due)
		}
		if n, err := tx.PruneWhispers(250); err != nil || n != 2 {
			return fmt.Errorf("pruned %d whispers %v", n, err)
		}

		// Ignores.
		alex, _, _ := tx.PersonOf(id(1))
		if err := tx.Ignore(rab.ID, rab.ID, 1); err == nil {
			return errors.New("ignored self")
		}
		if err := tx.Ignore(rab.ID, alex.ID, 10); err != nil {
			return err
		}
		if err := tx.Ignore(rab.ID, alex.ID, 20); err != nil {
			return err
		}
		third, _, _ := tx.PersonOf(id(3))
		if err := tx.Ignore(rab.ID, third.ID, 15); err != nil {
			return err
		}
		if list, _ := tx.Ignored(rab.ID); !slices.Equal(list, []int64{alex.ID, third.ID}) {
			return fmt.Errorf("ignored %v", list)
		}
		if ok, _ := tx.Unignore(rab.ID, alex.ID); !ok {
			return errors.New("unignore failed")
		}
		if ok, _ := tx.Unignore(rab.ID, alex.ID); ok {
			return errors.New("unignored twice")
		}
		if ok, _ := tx.IsIgnoring(rab.ID, alex.ID); ok {
			return errors.New("still ignoring")
		}
		if err := tx.Ignore(rab.ID, 9999, 1); err == nil {
			return errors.New("ignored a person who doesn't exist")
		}
		return nil
	})
}

func TestTimedBans(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for _, b := range []Ban{
			{Identity: id(1), Name: "Spammer", Reason: "spam", BannedBy: id(9), BannedAt: 100, ExpiresAt: 3700},
			{Identity: id(2), Name: "Troll", BannedAt: 200},
			{Identity: id(3), BannedAt: 300, ExpiresAt: 90000},
		} {
			if err := tx.AddBan(&b); err != nil {
				return err
			}
		}
		expired, err := tx.ExpiredBans(4000)
		if err != nil || len(expired) != 1 || expired[0].Name != "Spammer" || expired[0].Reason != "spam" || !bytes.Equal(expired[0].BannedBy, id(9)) {
			return fmt.Errorf("expired %+v %v", expired, err)
		}
		// Banning again replaces the ban: now permanent.
		if err := tx.AddBan(&Ban{Identity: id(1), Name: "Spammer", Reason: "again", BannedAt: 4100}); err != nil {
			return err
		}
		if expired, _ := tx.ExpiredBans(100000); len(expired) != 1 || !bytes.Equal(expired[0].Identity, id(3)) {
			return fmt.Errorf("after re-ban, expired %+v", expired)
		}
		if bans, _ := tx.Bans(); len(bans) != 3 || bans[0].Reason != "again" || bans[0].ExpiresAt != 0 {
			return fmt.Errorf("bans %+v", bans)
		}
		return nil
	})
}

func TestMessagesByPersonAndDeleting(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for b := byte(1); b <= 3; b++ {
			if err := tx.TouchIdentity(id(b), nil, 1); err != nil {
				return err
			}
		}
		p1, _, _ := tx.PersonOf(id(1))
		if err := tx.LinkIdentity(id(2), p1.ID, 2); err != nil {
			return err
		}
		var ids []int64
		for i, author := range [][]byte{id(1), id(3), id(2), id(1)} {
			m := Message{Room: "scotmesh", Kind: KindMsg, Author: author, AuthorName: "x", Body: fmt.Sprint(i), Via: ViaRRC, SaidAt: 1, HubAt: 1}
			mid, _, err := tx.AddMessage(&m)
			if err != nil {
				return err
			}
			ids = append(ids, mid)
		}
		other := Message{Room: "den", Kind: KindMsg, Author: id(1), AuthorName: "x", Body: "elsewhere", Via: ViaRRC, SaidAt: 1, HubAt: 1}
		if _, _, err := tx.AddMessage(&other); err != nil {
			return err
		}
		got, err := tx.LatestMessagesByPerson(p1.ID, "scotmesh", 2)
		if err != nil || len(got) != 2 || got[0].Body != "2" || got[1].Body != "3" {
			return fmt.Errorf("latest by person %+v %v", got, err)
		}
		if err := tx.PutDelivery(&Delivery{MessageID: ids[3], Identity: id(3), State: DeliveryQueued, NextAt: 1, UpdatedAt: 1}); err != nil {
			return err
		}
		if err := tx.PutDelivery(&Delivery{MessageID: ids[2], Identity: id(3), State: DeliveryProven, NextAt: 1, UpdatedAt: 1}); err != nil {
			return err
		}
		cancelled, err := tx.DeleteMessages([]int64{ids[2], ids[3]})
		if err != nil || cancelled != 1 {
			return fmt.Errorf("cancelled %d %v", cancelled, err)
		}
		if due, _ := tx.DueDeliveries(10, 10); len(due) != 0 {
			return fmt.Errorf("deliveries of deleted messages %+v", due)
		}
		if got, _ := tx.LatestMessagesByPerson(p1.ID, "scotmesh", 10); len(got) != 1 || got[0].Body != "0" {
			return fmt.Errorf("after deleting %+v", got)
		}
		return nil
	})
}

func TestSummaryForUpgradeChecks(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for b := byte(1); b <= 2; b++ {
			if err := tx.TouchIdentity(id(b), nil, 1); err != nil {
				return err
			}
		}
		return tx.ClaimName(id(1), "Alex", "alex", 1)
	})
	sum, err := s.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum["schema_version"] != 4 || sum["identities"] != 2 || sum["people"] != 2 || sum["names"] != 1 || sum["whispers"] != 0 {
		t.Errorf("summary %v", sum)
	}
}
