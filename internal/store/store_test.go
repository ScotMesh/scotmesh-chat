package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func id(b byte) []byte { return bytes.Repeat([]byte{b}, IdentityLen) }

func update(t *testing.T, s *Store, fn func(*Tx) error) {
	t.Helper()
	if err := s.Update(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "hub.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.SchemaVersion(context.Background())
	if err != nil || v != 4 {
		t.Fatalf("schema version %d, %v; want 4", v, err)
	}
	update(t, s, func(tx *Tx) error { return tx.SetMeta("k", "v") })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Dir(path))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("data directory mode %v, %v; want 0700", st.Mode().Perm(), err)
	}

	s, err = Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	err = s.View(context.Background(), func(tx *Tx) error {
		v, ok, err := tx.Meta("k")
		if err != nil || !ok || v != "v" {
			return fmt.Errorf("meta after reopen = %q, %v: %w", v, ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestViewIsReadOnly(t *testing.T) {
	s := openTest(t)
	err := s.View(context.Background(), func(tx *Tx) error { return tx.SetMeta("k", "v") })
	if err == nil {
		t.Fatal("a write inside View succeeded")
	}
}

func TestUpdateRollsBackOnError(t *testing.T) {
	s := openTest(t)
	boom := errors.New("boom")
	err := s.Update(context.Background(), func(tx *Tx) error {
		if err := tx.SetMeta("k", "v"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	_ = s.View(context.Background(), func(tx *Tx) error {
		if _, ok, _ := tx.Meta("k"); ok {
			t.Error("rolled-back write is visible")
		}
		return nil
	})
}

func TestUpdateRollsBackOnPanic(t *testing.T) {
	s := openTest(t)
	func() {
		defer func() { _ = recover() }()
		_ = s.Update(context.Background(), func(tx *Tx) error {
			_ = tx.SetMeta("k", "v")
			panic("boom")
		})
	}()
	_ = s.View(context.Background(), func(tx *Tx) error {
		if _, ok, _ := tx.Meta("k"); ok {
			t.Error("write before a panic was committed")
		}
		return nil
	})
	// The writer connection is usable again.
	update(t, s, func(tx *Tx) error { return tx.SetMeta("after", "panic") })
}

func TestIdentitiesAndSettings(t *testing.T) {
	s := openTest(t)
	pk := bytes.Repeat([]byte{9}, 64)
	update(t, s, func(tx *Tx) error {
		if err := tx.TouchIdentity(id(1), nil, 100); err != nil {
			return err
		}
		if err := tx.TouchIdentity(id(1), pk, 200); err != nil {
			return err
		}
		if err := tx.TouchIdentity(id(1), nil, 150); err != nil { // older sighting, no key
			return err
		}
		i, ok, err := tx.Identity(id(1))
		if err != nil || !ok {
			return fmt.Errorf("identity found=%v: %w", ok, err)
		}
		if i.FirstSeen != 100 || i.LastSeen != 200 || !bytes.Equal(i.PublicKey, pk) || i.LXMFMode != LXMFOn {
			return fmt.Errorf("identity %+v", i)
		}
		if err := tx.SetLXMFMode(id(1), LXMFAuto); err != nil {
			return err
		}
		if i, _, _ = tx.Identity(id(1)); i.LXMFMode != LXMFAuto {
			return fmt.Errorf("mode %q", i.LXMFMode)
		}
		if err := tx.SetLXMFMode(id(2), LXMFOff); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("unknown identity: %w", err)
		}
		if err := tx.SetLXMFMine(id(1), true); err != nil {
			return err
		}
		if i, _, _ = tx.Identity(id(1)); !i.LXMFMine {
			return errors.New("lxmf_mine not set")
		}
		if err := tx.SetLXMFMine(id(2), true); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("mine on unknown identity: %w", err)
		}
		if err := tx.SetLXMFMode(id(1), "sometimes"); err == nil {
			return errors.New("invalid mode accepted")
		}
		if _, ok, _ := tx.Identity(id(3)); ok {
			return errors.New("unknown identity found")
		}
		if err := tx.TouchIdentity([]byte{1, 2}, nil, 1); err == nil {
			return errors.New("short identity accepted")
		}
		return nil
	})
	for _, c := range []struct {
		in   string
		want LXMFMode
	}{{"ON", LXMFOn}, {"  off\t", LXMFOff}, {"Auto", LXMFAuto}} {
		if got, ok := ParseLXMFMode(c.in); !ok || got != c.want {
			t.Errorf("ParseLXMFMode(%q) = %q, %v", c.in, got, ok)
		}
	}
	if _, ok := ParseLXMFMode("maybe"); ok {
		t.Error("ParseLXMFMode accepted maybe")
	}
}

func TestNames(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for _, b := range []byte{1, 2} {
			if err := tx.TouchIdentity(id(b), nil, 1); err != nil {
				return err
			}
		}
		if err := tx.ClaimName(id(1), "Alex", "alex", 10); err != nil {
			return err
		}
		if err := tx.ClaimName(id(2), "ALEX", "alex", 11); !errors.Is(err, ErrNameTaken) {
			return fmt.Errorf("second claim err = %v", err)
		}
		// Re-casing your own name keeps the claim time.
		if err := tx.ClaimName(id(1), "ALEX", "alex", 12); err != nil {
			return err
		}
		n, ok, err := tx.NameOf(id(1))
		if err != nil || !ok || n.Name != "ALEX" || n.ClaimedAt != 10 {
			return fmt.Errorf("after re-case: %+v %v %v", n, ok, err)
		}
		// Changing name frees the old skeleton in the same step.
		if err := tx.ClaimName(id(1), "Rab", "rab", 13); err != nil {
			return err
		}
		if err := tx.ClaimName(id(2), "Alex", "alex", 14); err != nil {
			return fmt.Errorf("old name not freed: %v", err)
		}
		if h, _, _ := tx.NameBySkeleton("rab"); !bytes.Equal(h.Identity, id(1)) || h.ClaimedAt != 13 {
			return fmt.Errorf("rab holder %+v", h)
		}
		old, ok, err := tx.ReleaseName(id(2))
		if err != nil || !ok || old.Name != "Alex" {
			return fmt.Errorf("release: %+v %v %v", old, ok, err)
		}
		if _, ok, _ := tx.ReleaseName(id(2)); ok {
			return errors.New("released twice")
		}
		if _, ok, _ := tx.NameBySkeleton("alex"); ok {
			return errors.New("released name still held")
		}
		if err := tx.ClaimName(id(9), "Ghost", "ghost", 1); err == nil {
			return errors.New("claim by an unknown identity succeeded (foreign key)")
		}
		if err := tx.ClaimName(id(1), "", "", 1); err == nil {
			return errors.New("empty name accepted")
		}
		return nil
	})
}

func addMsg(t *testing.T, tx *Tx, room string, author byte, body string, origin []byte, hubAt int64) int64 {
	t.Helper()
	msgID, dup, err := tx.AddMessage(&Message{Room: room, Kind: KindMsg, Author: id(author), AuthorName: "n", Body: body,
		Via: ViaRRC, OriginID: origin, SaidAt: hubAt, HubAt: hubAt})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatalf("unexpected duplicate for %q", body)
	}
	return msgID
}

func TestMessagesAndCursors(t *testing.T) {
	s := openTest(t)
	var ids []int64
	update(t, s, func(tx *Tx) error {
		for i := range 5 {
			ids = append(ids, addMsg(t, tx, "scotmesh", 1, fmt.Sprintf("m%d", i), []byte{byte(i)}, int64(100+i)))
		}
		addMsg(t, tx, "other", 1, "elsewhere", nil, 200)

		// Same origin via the same way in is a duplicate; via another way is not.
		dupID, dup, err := tx.AddMessage(&Message{Room: "scotmesh", Kind: KindMsg, Author: id(1), AuthorName: "n", Body: "again", Via: ViaRRC, OriginID: []byte{2}, HubAt: 1})
		if err != nil || !dup || dupID != ids[2] {
			return fmt.Errorf("duplicate: id %d dup %v err %v", dupID, dup, err)
		}
		if _, dup, _ := tx.AddMessage(&Message{Room: "scotmesh", Kind: KindAction, Author: id(1), AuthorName: "n", Body: "lx", Via: ViaLXMF, OriginID: []byte{2}, HubAt: 106, Ext: []byte{0xa0}}); dup {
			return errors.New("same origin via another way in counted as duplicate")
		}
		return nil
	})
	_ = s.View(context.Background(), func(tx *Tx) error {
		after, err := tx.MessagesAfter("scotmesh", ids[1], 2)
		if err != nil || len(after) != 2 || after[0].Body != "m2" || after[1].Body != "m3" {
			t.Errorf("MessagesAfter = %+v, %v", after, err)
		}
		latest, err := tx.LatestMessages("scotmesh", 0, 3)
		if err != nil || len(latest) != 3 || latest[0].Body != "m3" || latest[2].Body != "lx" || latest[2].Kind != KindAction || latest[2].Ext == nil {
			t.Errorf("LatestMessages = %+v, %v", latest, err)
		}
		older, err := tx.LatestMessages("scotmesh", ids[3], 2)
		if err != nil || len(older) != 2 || older[0].Body != "m1" || older[1].Body != "m2" {
			t.Errorf("LatestMessages before = %+v, %v", older, err)
		}
		if n, _ := tx.CountAfter("scotmesh", ids[0]); n != 5 {
			t.Errorf("CountAfter = %d", n)
		}
		if last, _ := tx.LastMessageID("nowhere"); last != 0 {
			t.Errorf("LastMessageID(empty room) = %d", last)
		}
		m, ok, err := tx.Message(ids[4])
		if err != nil || !ok || m.Body != "m4" || !bytes.Equal(m.OriginID, []byte{4}) || m.Via != ViaRRC {
			t.Errorf("Message = %+v %v %v", m, ok, err)
		}
		if _, ok, _ := tx.Message(9999); ok {
			t.Error("Message(9999) found")
		}
		return nil
	})

	update(t, s, func(tx *Tx) error {
		if _, ok, _ := tx.Cursor(id(2), "scotmesh", ViaRRC); ok {
			return errors.New("cursor before any was set")
		}
		if err := tx.SetCursor(id(2), "scotmesh", ViaRRC, ids[3], 1); err != nil {
			return err
		}
		if err := tx.SetCursor(id(2), "scotmesh", ViaRRC, ids[1], 2); err != nil { // never moves back
			return err
		}
		if c, ok, _ := tx.Cursor(id(2), "scotmesh", ViaRRC); !ok || c.MessageID != ids[3] || c.UpdatedAt != 2 {
			return fmt.Errorf("cursor %+v %v, want message %d at 2", c, ok, ids[3])
		}
		if _, ok, _ := tx.Cursor(id(2), "scotmesh", ViaPage); ok {
			return errors.New("cursors are not separate per way in")
		}
		return nil
	})

	update(t, s, func(tx *Tx) error {
		if err := tx.TouchIdentity(id(3), nil, 1); err != nil {
			return err
		}
		if _, err := tx.AddMember(id(3), 1); err != nil {
			return err
		}
		if err := tx.PutDelivery(&Delivery{MessageID: ids[0], Identity: id(3), State: DeliveryQueued, NextAt: 1, UpdatedAt: 1}); err != nil {
			return err
		}
		n, err := tx.PruneMessages(103)
		if err != nil || n != 3 {
			return fmt.Errorf("pruned %d, %v; want 4", n, err)
		}
		if due, _ := tx.DueDeliveries(10, 10); len(due) != 0 {
			return fmt.Errorf("delivery of a pruned message survived: %+v", due)
		}
		return nil
	})
}

func TestRoomsRolesInvites(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		r := &Room{Name: "scotmesh", Topic: "hi", Founder: id(1), Registered: true, CreatedAt: 1, LastUsed: 2}
		r.SetMode('t', true)
		r.SetMode('n', true)
		r.SetMode('n', true)
		if r.Modes != "nt" || !r.HasMode('t') || r.HasMode('m') {
			return fmt.Errorf("modes %q", r.Modes)
		}
		if err := tx.PutRoom(r); err != nil {
			return err
		}
		r.SetMode('t', false)
		r.Topic = "changed"
		if err := tx.PutRoom(r); err != nil {
			return err
		}
		got, ok, err := tx.Room("scotmesh")
		if err != nil || !ok || got.Topic != "changed" || got.Modes != "n" || !got.Registered || !bytes.Equal(got.Founder, id(1)) || got.CreatedAt != 1 {
			return fmt.Errorf("room %+v %v %v", got, ok, err)
		}
		if err := tx.PutRoom(&Room{Name: "bad", Modes: "z"}); err == nil {
			return errors.New("unknown mode accepted")
		}
		if err := tx.PutRoom(&Room{Name: "anon", CreatedAt: 1, LastUsed: 1}); err != nil {
			return err
		}
		all, err := tx.Rooms()
		if err != nil || len(all) != 2 || all[0].Name != "anon" || all[0].Founder != nil {
			return fmt.Errorf("rooms %+v %v", all, err)
		}

		if err := tx.SetRole("scotmesh", id(2), RoleOp, true, id(1), 5); err != nil {
			return err
		}
		if err := tx.SetRole("scotmesh", id(2), RoleOp, true, id(1), 6); err != nil {
			return err
		}
		if err := tx.SetRole("scotmesh", id(3), RoleBan, true, nil, 5); err != nil {
			return err
		}
		if ok, _ := tx.HasRole("scotmesh", id(2), RoleOp); !ok {
			return errors.New("op not granted")
		}
		if ok, _ := tx.HasRole("scotmesh", id(2), RoleVoice); ok {
			return errors.New("voice granted by op")
		}
		if ops, _ := tx.RoleHolders("scotmesh", RoleOp); len(ops) != 1 {
			return fmt.Errorf("ops %x", ops)
		}
		if err := tx.SetRole("scotmesh", id(2), RoleOp, false, nil, 7); err != nil {
			return err
		}
		if ok, _ := tx.HasRole("scotmesh", id(2), RoleOp); ok {
			return errors.New("op not removed")
		}

		if err := tx.SetInvite("scotmesh", id(4), 100); err != nil {
			return err
		}
		if ok, _ := tx.InviteValid("scotmesh", id(4), 50); !ok {
			return errors.New("invite not valid before expiry")
		}
		if ok, _ := tx.InviteValid("scotmesh", id(4), 100); ok {
			return errors.New("invite valid at expiry")
		}
		if inv, _ := tx.Invites("scotmesh", 50); len(inv) != 1 {
			return fmt.Errorf("invites %+v", inv)
		}
		if n, _ := tx.PruneInvites(100); n != 1 {
			return fmt.Errorf("pruned %d invites", n)
		}
		if err := tx.SetInvite("scotmesh", id(5), 500); err != nil {
			return err
		}
		if err := tx.DeleteInvite("scotmesh", id(5)); err != nil {
			return err
		}
		if ok, _ := tx.InviteValid("scotmesh", id(5), 1); ok {
			return errors.New("deleted invite valid")
		}

		if err := tx.SetInvite("scotmesh", id(6), 500); err != nil {
			return err
		}
		if err := tx.DeleteRoom("scotmesh"); err != nil {
			return err
		}
		if b, _ := tx.RoleHolders("scotmesh", RoleBan); len(b) != 0 {
			return errors.New("roles survived room deletion")
		}
		if inv, _ := tx.Invites("scotmesh", 0); len(inv) != 0 {
			return errors.New("invites survived room deletion")
		}
		return nil
	})
}

func TestGroupAndDeliveries(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for _, b := range []byte{1, 2} {
			if err := tx.TouchIdentity(id(b), nil, 1); err != nil {
				return err
			}
		}
		if err := tx.SetLXMFMode(id(2), LXMFOff); err != nil {
			return err
		}
		if added, err := tx.AddMember(id(1), 10); err != nil || !added {
			return fmt.Errorf("add: %v %v", added, err)
		}
		if added, _ := tx.AddMember(id(1), 20); added {
			return errors.New("re-join counted as a new member")
		}
		if _, err := tx.AddMember(id(2), 15); err != nil {
			return err
		}
		if _, err := tx.AddMember(id(7), 1); err == nil {
			return errors.New("unknown identity joined (foreign key)")
		}
		ms, err := tx.Members()
		if err != nil || len(ms) != 2 || ms[0].JoinedAt != 10 || ms[1].LXMFMode != LXMFOff {
			return fmt.Errorf("members %+v %v", ms, err)
		}
		if err := tx.SetAway(id(1), 999); err != nil {
			return err
		}
		if ms, _ = tx.Members(); ms[0].AwayUntil != 999 {
			return fmt.Errorf("away %+v", ms[0])
		}
		if ok, _ := tx.IsMember(id(2)); !ok {
			return errors.New("IsMember false")
		}

		for i, st := range []DeliveryState{DeliveryQueued, DeliveryQueued, DeliveryProven} {
			if err := tx.PutDelivery(&Delivery{MessageID: int64(i + 1), Identity: id(1), State: st, NextAt: int64(i * 10), UpdatedAt: 5}); err != nil {
				return err
			}
		}
		due, err := tx.DueDeliveries(5, 10)
		if err != nil || len(due) != 1 || due[0].MessageID != 1 {
			return fmt.Errorf("due %+v %v", due, err)
		}
		due[0].State, due[0].Attempts = DeliverySent, 1
		if err := tx.PutDelivery(&due[0]); err != nil {
			return err
		}
		if due, _ = tx.DueDeliveries(100, 10); len(due) != 1 || due[0].MessageID != 2 {
			return fmt.Errorf("due after update %+v", due)
		}
		// A split message keeps count of the parts that arrived.
		due[0].PartsDone, due[0].NextAt = 2, 7
		if err := tx.PutDelivery(&due[0]); err != nil {
			return err
		}
		if due, _ = tx.DueDeliveries(100, 10); len(due) != 1 || due[0].PartsDone != 2 {
			return fmt.Errorf("parts done not kept: %+v", due)
		}
		if err := tx.PutDelivery(&Delivery{MessageID: 9, Identity: id(1), State: DeliveryQueued, PartsDone: -1, UpdatedAt: 5}); err == nil {
			return errors.New("negative parts_done accepted")
		}
		if n, _ := tx.FinishDeliveries(6); n != 1 {
			return fmt.Errorf("finished %d, want the one proven", n)
		}
		if removed, _ := tx.RemoveMember(id(1)); !removed {
			return errors.New("remove failed")
		}
		if due, _ = tx.DueDeliveries(100, 10); len(due) != 0 {
			return errors.New("deliveries survived leaving")
		}
		if removed, _ := tx.RemoveMember(id(1)); removed {
			return errors.New("removed twice")
		}
		return nil
	})
}

func TestBansAuditMeta(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		if err := tx.AddBan(&Ban{Identity: id(1), Reason: "spam", BannedBy: id(9), BannedAt: 10}); err != nil {
			return err
		}
		if err := tx.AddBan(&Ban{Identity: id(2), BannedAt: 20}); err != nil {
			return err
		}
		if ok, _ := tx.IsBanned(id(1)); !ok {
			return errors.New("not banned")
		}
		bans, err := tx.Bans()
		if err != nil || len(bans) != 2 || !bytes.Equal(bans[0].Identity, id(2)) || bans[1].Reason != "spam" || bans[0].BannedBy != nil {
			return fmt.Errorf("bans %+v %v", bans, err)
		}
		if ok, _ := tx.RemoveBan(id(1)); !ok {
			return errors.New("unban failed")
		}
		if ok, _ := tx.RemoveBan(id(1)); ok {
			return errors.New("unbanned twice")
		}
		if ok, _ := tx.IsBanned(id(1)); ok {
			return errors.New("still banned")
		}

		if err := tx.Audit(1, id(9), "release", "Alex", "squatting"); err != nil {
			return err
		}
		if err := tx.Audit(2, nil, "prune", "", ""); err != nil {
			return err
		}
		log, err := tx.AuditLog(10)
		if err != nil || len(log) != 2 || log[0].Action != "prune" || log[1].Target != "Alex" || log[0].Actor != nil {
			return fmt.Errorf("audit %+v %v", log, err)
		}
		if err := tx.SetMeta("a", "1"); err != nil {
			return err
		}
		if err := tx.SetMeta("a", "2"); err != nil {
			return err
		}
		if v, ok, _ := tx.Meta("a"); !ok || v != "2" {
			return fmt.Errorf("meta %q", v)
		}
		return nil
	})
}

func TestBackup(t *testing.T) {
	s := openTest(t)
	update(t, s, func(tx *Tx) error { return tx.SetMeta("k", "backed up") })
	dst := filepath.Join(t.TempDir(), "backups", "hub-copy.db")
	if err := s.Backup(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	b, err := Open(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	_ = b.View(context.Background(), func(tx *Tx) error {
		if v, _, _ := tx.Meta("k"); v != "backed up" {
			t.Errorf("backup meta %q", v)
		}
		return nil
	})
	if err := s.Backup(context.Background(), dst); err == nil {
		t.Error("backup over an existing file succeeded")
	}
}

// TestKillDuringWrites kills a writer process with SIGKILL in the middle of a
// stream of name claims. Every claim it reported committed must be there, and
// the database must pass an integrity check.
func TestKillDuringWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a process")
	}
	path := filepath.Join(t.TempDir(), "hub.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWriter$")
	cmd.Env = append(os.Environ(), "STORE_HELPER_DB="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	committed := 0
	for sc.Scan() {
		n, err := strconv.Atoi(strings.TrimPrefix(sc.Text(), "committed "))
		if err != nil {
			continue
		}
		committed = n
		if committed >= 300 {
			break
		}
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if committed < 300 {
		t.Fatalf("writer committed only %d claims before exiting", committed)
	}

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	defer func() { _ = s.Close() }()
	var integrity string
	if err := s.r.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
	_ = s.View(context.Background(), func(tx *Tx) error {
		for i := 1; i <= committed; i++ {
			n, ok, err := tx.NameOf(helperID(i))
			if err != nil || !ok || n.Name != fmt.Sprintf("name%d", i) {
				t.Fatalf("claim %d reported committed is missing: %+v %v %v", i, n, ok, err)
			}
		}
		return nil
	})
}

func helperID(i int) []byte {
	b := make([]byte, IdentityLen)
	copy(b, fmt.Sprintf("%016d", i))
	return b
}

// TestHelperWriter is the child process for TestKillDuringWrites.
func TestHelperWriter(t *testing.T) {
	path := os.Getenv("STORE_HELPER_DB")
	if path == "" {
		t.Skip("helper process")
	}
	s, err := Open(context.Background(), path)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	for i := 1; ; i++ {
		err := s.Update(context.Background(), func(tx *Tx) error {
			if err := tx.TouchIdentity(helperID(i), nil, time.Now().UnixMilli()); err != nil {
				return err
			}
			return tx.ClaimName(helperID(i), fmt.Sprintf("name%d", i), fmt.Sprintf("name%d", i), 1)
		})
		if err != nil {
			fmt.Println("update:", err)
			os.Exit(1)
		}
		fmt.Printf("committed %d\n", i)
	}
}
