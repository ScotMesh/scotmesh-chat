package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// TestConfiguredRoomTopicIsOpOnly locks in the fix for a configured room's
// topic being open to anyone, at any length: it's now protected (+t) like
// any room an operator cares enough about to configure, and capped.
func TestConfiguredRoomTopicIsOpOnly(t *testing.T) {
	hs := start(t)
	if r := hs.cmd(ViaLXMF, alex, "", "/topic a new topic"); r.Text() != "not authorized (+t)" {
		t.Errorf("a plain member set the configured room's topic: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "scotmesh", "/topic a new topic"); r.Error || r.Text() != "topic for scotmesh is now: a new topic" {
		t.Errorf("an operator couldn't set it: %q", r.Text())
	}
	long := strings.Repeat("x", hs.h.cfg.MaxTopicBytes+1)
	if r := hs.cmd(ViaRRC, oper, "scotmesh", "/topic "+long); !r.Error || !strings.Contains(r.Text(), "too long") {
		t.Errorf("an over-length topic was accepted: %q", r.Text())
	}
}

// TestConfiguredRoomTakesOverAnExistingRoom: a room created (by chance, or
// ahead of the operator adding it to the config) before it became a
// configured room must not keep its accidental founder or stay wide open,
// once the hub restarts with it configured.
func TestConfiguredRoomTakesOverAnExistingRoom(t *testing.T) {
	hs := start(t)
	hs.join(alex, "newroom") // alex founds it, no modes
	hs.stop()

	hs2 := startAt(t, hs.path, hs.clock, func(c *Config) {
		c.Rooms = append(c.Rooms, RoomConfig{Name: "newroom", Topic: "official"})
	})
	var r store.Room
	if err := hs2.h.st.View(context.Background(), func(tx *store.Tx) error {
		var ok bool
		var err error
		r, ok, err = tx.Room("newroom")
		if !ok || err != nil {
			t.Fatalf("room missing: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if r.Founder != nil {
		t.Errorf("a configured room kept its accidental founder: %x", r.Founder)
	}
	if !r.HasMode('t') {
		t.Error("a configured room's topic isn't protected")
	}
	if r.Topic != "official" {
		t.Errorf("the configured topic wasn't applied: %q", r.Topic)
	}
}

// TestRoomOpCannotActOnHubStaff locks in the fix for a room op — which can
// be a plain member, holding no hub rank at all, made founder or op of
// their own room — reaching hub staff who happen to be in that room.
// Ordinary room moderation between plain members must keep working.
func TestRoomOpCannotActOnHubStaff(t *testing.T) {
	hs := start(t)
	hs.join(alex, "den") // alex founds it, a plain member
	hs.join(oper, "den") // a hub owner joins alex's room
	hs.join(rab, "den")  // a fellow plain member

	if r := hs.cmd(ViaRRC, alex, "den", "/room kick "+hexID(oper)); !r.Error || !strings.Contains(r.Text(), "hub owner") {
		t.Errorf("a room founder kicked a hub owner: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room ban "+hexID(oper)); !r.Error || !strings.Contains(r.Text(), "hub owner") {
		t.Errorf("a room founder banned a hub owner: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/room deop "+hexID(oper)); !r.Error || !strings.Contains(r.Text(), "hub owner") {
		t.Errorf("a room founder deopped a hub owner: %q", r.Text())
	}
	// Granting op is never harmful to the target, so it's untouched.
	if r := hs.cmd(ViaRRC, alex, "den", "/room op "+hexID(oper)); r.Error {
		t.Errorf("granting op to a hub owner should be harmless: %q", r.Text())
	}
	// The owner can still act on the plain-member founder, and an owner
	// may deop a founder (the founder-only exemption is for everyone else).
	if r := hs.cmd(ViaRRC, oper, "den", "/room deop "+hexID(alex)); r.Error {
		t.Errorf("an owner couldn't deop a founder: %q", r.Text())
	}
	// Ordinary room moderation between plain members is unaffected.
	if r := hs.cmd(ViaRRC, alex, "den", "/room kick "+hexID(rab)); r.Error {
		t.Errorf("a founder couldn't kick a fellow member: %q", r.Text())
	}
}

// TestMaintainDoesNotCorruptRateCounters locks in the fix for takeRecent:
// maintain calls it only to decide whether a map entry has gone fully
// stale (to delete the key) and discards the result either way, so
// filtering in place (the old behaviour) silently duplicated whichever
// entry survived into the slot the expired one vacated — a stored
// [old, recent] read back as [recent, recent] after just one maintain
// tick, corrupting the next real rate-limit check even though nothing
// was meant to change it.
func TestMaintainDoesNotCorruptRateCounters(t *testing.T) {
	hs := start(t)
	now := hs.clock.now()
	old := now.Add(-2 * time.Hour)
	// Every access to this hub-loop-owned state runs on the loop goroutine
	// (via do), the same as the real callers that populate and read it.
	if err := hs.h.do(bg, func() {
		hs.h.linkIssued[1] = []time.Time{old, now}
		hs.h.linkFailures[hexID(alex)] = []time.Time{old, now}
		hs.h.maintain(bg)
	}); err != nil {
		t.Fatal(err)
	}

	var issued, failures []time.Time
	if err := hs.h.do(bg, func() {
		issued = hs.h.linkIssued[1]
		failures = hs.h.linkFailures[hexID(alex)]
	}); err != nil {
		t.Fatal(err)
	}
	if len(issued) != 2 || !issued[0].Equal(old) || !issued[1].Equal(now) {
		t.Errorf("linkIssued[1] after maintain = %v, want [%v %v] unchanged", issued, old, now)
	}
	if len(failures) != 2 || !failures[0].Equal(old) || !failures[1].Equal(now) {
		t.Errorf("linkFailures after maintain = %v, want [%v %v] unchanged", failures, old, now)
	}
	// The real counters still see it correctly once genuinely stale.
	hs.clock.advance(3 * time.Hour)
	var issuedGone, failuresGone bool
	if err := hs.h.do(bg, func() {
		hs.h.maintain(bg)
		_, issuedOK := hs.h.linkIssued[1]
		_, failuresOK := hs.h.linkFailures[hexID(alex)]
		issuedGone, failuresGone = !issuedOK, !failuresOK
	}); err != nil {
		t.Fatal(err)
	}
	if !issuedGone {
		t.Error("a fully-expired linkIssued entry should have been evicted")
	}
	if !failuresGone {
		t.Error("a fully-expired linkFailures entry should have been evicted")
	}
}

// TestHistoryAndWhoRespectRoomPrivacy locks in the fix for /history and
// /who leaking +i and +k rooms (previously only +p was checked).
func TestHistoryAndWhoRespectRoomPrivacy(t *testing.T) {
	hs := start(t)
	hs.join(alex, "keyed")
	hs.cmd(ViaRRC, alex, "keyed", "/room mode +k opensesame")
	if _, err := hs.h.Post(context.Background(), PostRequest{Via: ViaRRC, Identity: alex, Room: "keyed", Body: "secret stuff"}); err != nil {
		t.Fatal(err)
	}

	for _, cmdText := range []string{"/history #keyed", "/who #keyed"} {
		if r := hs.cmd(ViaLXMF, rab, "", cmdText); !r.Error || !strings.Contains(r.Text(), "private") {
			t.Errorf("%s from outside a +k room: %q", cmdText, r.Text())
		}
	}
	// A member (rab joins, supplying the key) can see both.
	if _, _, err := hs.h.JoinGroup(context.Background(), rab, "Rab"); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.h.Join(context.Background(), JoinRequest{Identity: rab, Room: "keyed", Key: "opensesame"}); err != nil {
		t.Fatal(err)
	}
	if r := hs.cmd(ViaRRC, rab, "keyed", "/history"); r.Error {
		t.Errorf("a member was refused /history: %q", r.Text())
	}
	// A hub operator can always see in, without joining.
	if r := hs.cmd(ViaLXMF, oper, "", "/history #keyed"); r.Error {
		t.Errorf("an operator was refused /history: %q", r.Text())
	}
}
