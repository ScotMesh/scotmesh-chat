package hub

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

func TestKickPutsAPersonOutEverywhere(t *testing.T) {
	hs := start(t)
	mod, admin := bytes.Repeat([]byte{0xc1}, 16), bytes.Repeat([]byte{0xc2}, 16)
	phone, laptop := bytes.Repeat([]byte{0xc3}, 16), bytes.Repeat([]byte{0xc4}, 16)
	hs.identify(mod, admin, phone, laptop)
	hs.claim(mod, "Moddy")
	hs.claim(admin, "Addy")
	hs.setRank(mod, store.RankMod)
	hs.setRank(admin, store.RankAdmin)
	if _, _, err := hs.h.JoinGroup(bg, phone, "Tam"); err != nil {
		t.Fatal(err)
	}
	hs.cmd(ViaRRC, laptop, "", "/link "+hs.issue(ViaLXMF, phone))
	hs.join(laptop, "scotmesh")
	hs.join(laptop, "den")
	hs.events()

	if r := hs.cmd(ViaRRC, laptop, "", "/kick Moddy"); !r.Error || r.Text() != "not authorized" {
		t.Errorf("a member kicking: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/kick Addy"); !r.Error || r.Text() != "Addy is an admin, which is not below you (mod)." {
		t.Errorf("a mod kicking an admin: %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, mod, "", "/kick Tam shouting"); r.Error || r.Text() != "Kicked Tam. Their name is free." {
		t.Fatalf("kick: %q", r.Text())
	}
	evs := hs.events()
	why := "You were kicked from ScotMesh by Moddy: shouting. Your name is free again; you can come back as a guest."
	if rm := eventsOf[RemovedEvent](evs); len(rm) != 2 || rm[0].Reason != why || !bytes.Equal(rm[0].Identity, laptop) {
		t.Errorf("removed events %+v", rm)
	}
	notices := eventsOf[NoticeEvent](evs)
	told := map[string]bool{}
	for _, n := range notices {
		if n.Text == why {
			told[hexID(n.Identity)] = n.ToLXMF
		}
	}
	if len(told) != 2 || !told[hexID(phone)] || told[hexID(laptop)] {
		t.Errorf("kick notices %v (the phone was a member, so it's told by LXMF)", told)
	}
	if g := eventsOf[GroupEvent](evs); len(g) != 1 || g[0].Joined || !bytes.Equal(g[0].Identity, phone) {
		t.Errorf("group events %+v", g)
	}
	for _, id := range [][]byte{phone, laptop} {
		p := must[Person](t)(hs.h.PersonOf(bg, id))
		if p.Claimed || p.Linked() || p.Member {
			t.Errorf("after the kick %+v", p)
		}
		if len(hs.h.roomsOfSafe(id)) != 0 {
			t.Errorf("still in rooms: %v", hs.h.roomsOfSafe(id))
		}
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/whoami"); r.Error {
		t.Errorf("a kicked person can come back: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/kick"); r.Text() != "usage: /kick <name> [reason]" {
		t.Errorf("usage %q", r.Text())
	}
}

func TestBansAreTimedAndCoverEveryApp(t *testing.T) {
	hs := start(t)
	mod := bytes.Repeat([]byte{0xd1}, 16)
	phone, laptop := bytes.Repeat([]byte{0xd2}, 16), bytes.Repeat([]byte{0xd3}, 16)
	hs.identify(mod, phone, laptop)
	hs.claim(mod, "Moddy")
	hs.setRank(mod, store.RankMod)
	hs.claim(phone, "Spammy")
	hs.cmd(ViaRRC, laptop, "", "/link "+hs.issue(ViaLXMF, phone))
	hs.join(laptop, "scotmesh")
	hs.events()

	if r := hs.cmd(ViaRRC, mod, "", "/ban Spammy 1h selling radios"); r.Text() != "Banned Spammy (2 apps), until Sat 20:00. Their name is free." {
		t.Fatalf("ban: %q", r.Text())
	}
	evs := hs.events()
	if b := eventsOf[BannedEvent](evs); len(b) != 2 || b[0].Reason != "You were banned from ScotMesh by Moddy, until Sat 20:00: selling radios." {
		t.Errorf("banned events %+v", b)
	}
	if rm := eventsOf[RemovedEvent](evs); len(rm) != 0 {
		t.Errorf("a ban disconnects rather than removing from rooms: %+v", rm)
	}
	for _, id := range [][]byte{phone, laptop} {
		_, err := hs.h.Identify(bg, id, nil)
		userErr(t, err, "you are banned from this hub")
	}
	if r := hs.cmd(ViaRRC, mod, "", "/bans"); r.Text() != "Banned (1):\n  Spammy (2 apps) — selling radios — by Moddy, Sat 19:00, until Sat 20:00" {
		t.Errorf("/bans %q", r.Text())
	}
	// The name is free for someone else meanwhile.
	hs.identify(rab)
	hs.claim(rab, "Spammy")
	// The ban runs out on the minute after its hour.
	hs.clock.advance(61 * time.Minute)
	if err := hs.h.do(bg, func() { hs.h.maintain(bg) }); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.h.Identify(bg, laptop, nil); err != nil {
		t.Errorf("identify after the ban ran out: %v", err)
	}
	if r := hs.cmd(ViaRRC, mod, "", "/bans"); r.Text() != "Nobody is banned." {
		t.Errorf("/bans after %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/modlog 2"); r.Text() != "The last 2 actions, newest first:\n  Sat 20:01 · the hub · unban Spammy — the ban ran out\n  Sat 19:00 · Moddy · ban Spammy — until Sat 20:00: selling radios" {
		t.Errorf("/modlog %q", r.Text())
	}

	// perm by default; unban by the name they had or by identity.
	if r := hs.cmd(ViaRRC, mod, "", "/ban "+hexID(laptop)); r.Text() != "Banned guest-d3d3 (1 app), for good. Their name is free." {
		t.Errorf("perm ban: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/unban guest-d3d3"); r.Text() != "Unbanned guest-d3d3 (1 app)." {
		t.Errorf("unban by name: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/ban "+hexID(laptop)+" perm again"); !strings.HasSuffix(r.Text(), "for good. Their name is free.") {
		t.Errorf("explicit perm: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/unban "+hexID(laptop)); r.Text() != "Unbanned guest-d3d3 (1 app)." {
		t.Errorf("unban by identity: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/unban Nobody"); !r.Error || r.Text() != "target 'Nobody' not found" {
		t.Errorf("unban unknown: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/unban Spammy"); !r.Error || r.Text() != "not authorized" {
		t.Errorf("a member unbanning: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/ban "+hexID(oper)); !r.Error || !strings.Contains(r.Text(), "hub owner") {
		t.Errorf("banning an owner: %q", r.Text())
	}
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "1h": time.Hour, "7d": 7 * 24 * time.Hour, "PERM": 0} {
		if got, ok := parseBanLength(in); !ok || got != want {
			t.Errorf("parseBanLength(%q) = %v %v", in, got, ok)
		}
	}
	for _, in := range []string{"0h", "1w", "forever", "12345d"} {
		if _, ok := parseBanLength(in); ok {
			t.Errorf("parseBanLength(%q) accepted", in)
		}
	}
}

func TestRenameAndRanks(t *testing.T) {
	hs := start(t)
	mod, admin, admin2 := bytes.Repeat([]byte{0xe1}, 16), bytes.Repeat([]byte{0xe2}, 16), bytes.Repeat([]byte{0xe4}, 16)
	hs.identify(mod, admin, admin2, rab, ellen)
	hs.claim(mod, "Moddy")
	hs.claim(admin, "Addy")
	hs.claim(admin2, "Ada")
	hs.claim(rab, "Rab")
	hs.claim(ellen, "Ellen")
	hs.setRank(mod, store.RankMod)
	hs.setRank(admin, store.RankAdmin)
	hs.setRank(admin2, store.RankAdmin)
	hs.events()

	steps := []struct {
		who   []byte
		text  string
		reply string
		err   bool
	}{
		{mod, "/rename Rab Robert", "Renamed Rab to Robert.", false},
		{mod, "/rename Robert E11en", "\"E11en\" is taken by someone else.", true},
		{mod, "/rename Robert", "usage: /rename <name> <new name>", true},
		{mod, "/rename Addy Nobody", "Addy is an admin, which is not below you (mod).", true},
		{mod, "/mod Robert", "not authorized", true},
		{admin, "/mod Robert", "Robert is a mod now.", false},
		{admin, "/mod Robert", "Robert is already a mod.", true},
		{admin, "/admin Robert", "Robert is an admin now.", false},
		{admin, "/demote Robert", "Robert is an admin, which is not below you (admin).", true},
		{admin, "/demote Ada", "Ada is an admin, which is not below you (admin).", true},
		{oper, "/demote Robert", "Robert is a member again.", false},
		{oper, "/demote Robert", "Robert has no role to take away.", true},
		{admin, "/demote Moddy", "Moddy is a member again.", false},
		{admin, "/admin", "usage: /admin <name>", true},
		{admin, "/demote", "usage: /demote <name>", true},
		{admin, "/mod " + hexID(oper), "guest-0f0f is a hub owner; that can only be changed in the hub's config file.", true},
		{rab, "/modlog", "not authorized", true},
		{admin, "/modlog x", "usage: /modlog [n]", true},
	}
	for _, s := range steps {
		r := hs.cmd(ViaLXMF, s.who, "", s.text)
		if r.Text() != s.reply || r.Error != s.err {
			t.Errorf("%s\n got %q (error=%v)\nwant %q (error=%v)", s.text, r.Text(), r.Error, s.reply, s.err)
		}
	}
	notices := eventsOf[NoticeEvent](hs.events())
	var texts []string
	for _, n := range notices {
		if bytes.Equal(n.Identity, rab) {
			texts = append(texts, n.Text)
		}
	}
	want := "Moddy renamed you from Rab to Robert.|Addy made you a mod of ScotMesh. /help shows what you can do now.|Addy made you an admin of ScotMesh. /help shows what you can do now.|guest-0f0f took away your admin role."
	if strings.Join(texts, "|") != want {
		t.Errorf("notices to Rab:\n%s", strings.Join(texts, "\n"))
	}
	if r := hs.cmd(ViaLXMF, admin, "", "/modlog 3"); !strings.HasPrefix(r.Text(), "The last 3 actions, newest first:\n  Sat 19:00 · Addy · demote Moddy — mod\n  Sat 19:00 · guest-0f0f · demote Robert — admin") {
		t.Errorf("/modlog %q", r.Text())
	}
	fresh := start(t)
	fresh.identify(oper)
	if r := fresh.cmd(ViaLXMF, oper, "", "/modlog"); r.Text() != "The mod log is empty." {
		t.Errorf("empty modlog %q", r.Text())
	}
}

func TestDeleteMessages(t *testing.T) {
	hs := start(t)
	mod, admin := bytes.Repeat([]byte{0xf1}, 16), bytes.Repeat([]byte{0xf2}, 16)
	hs.identify(mod, admin, rab, ellen)
	hs.claim(mod, "Moddy")
	hs.claim(admin, "Addy")
	hs.setRank(mod, store.RankMod)
	hs.setRank(admin, store.RankAdmin)
	if _, _, err := hs.h.JoinGroup(bg, rab, "Rab"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hs.h.JoinGroup(bg, ellen, "Ellen"); err != nil {
		t.Fatal(err)
	}
	hs.say(ViaLXMF, rab, "one")
	hs.say(ViaLXMF, rab, "two")
	spam := hs.say(ViaLXMF, rab, "buy my radios")
	hs.say(ViaPage, admin, "admin's words")
	hs.events()

	// Your own last message; queued LXMF copies are cancelled.
	r := hs.cmd(ViaLXMF, rab, "", "/delete Rab")
	if r.Error || r.Text() != "Removed Rab's last message in #scotmesh from history and the chat page. Copies already delivered stay in people's apps.\n1 copy by LXMF not sent yet won't be." {
		t.Errorf("own delete: %q", r.Text())
	}
	if n := eventsOf[RoomNoticeEvent](hs.events()); len(n) != 1 || n[0].Text != "Rab removed their last message." {
		t.Errorf("room notice %+v", n)
	}
	if got := bodies(must[[]store.Message](t)(hs.h.Recent(bg, "scotmesh", 10, 0))); strings.Join(got, ",") != "one,two,admin's words" {
		t.Errorf("after deleting: %v", got)
	}
	// A mod removes a member's last two; not an admin's; a member not another's.
	if r := hs.cmd(ViaRRC, mod, "scotmesh", "/delete Rab 2"); !strings.HasPrefix(r.Text(), "Removed Rab's last 2 messages in #scotmesh") {
		t.Errorf("mod delete: %q", r.Text())
	}
	if n := eventsOf[RoomNoticeEvent](hs.events()); len(n) != 1 || n[0].Text != "Rab's last 2 messages removed by Moddy." {
		t.Errorf("room notice %+v", n)
	}
	if r := hs.cmd(ViaRRC, mod, "scotmesh", "/delete Addy"); !r.Error || !strings.Contains(r.Text(), "not below you") {
		t.Errorf("mod deleting an admin's: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, ellen, "scotmesh", "/delete Addy"); !r.Error || r.Text() != "Only mods and up can remove the messages of someone else." {
		t.Errorf("member deleting another's: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "scotmesh", "/delete Rab"); !r.Error || r.Text() != "Rab has no messages in #scotmesh to remove." {
		t.Errorf("nothing left: %q", r.Text())
	}
	for _, bad := range []string{"/delete", "/delete Rab 0", "/delete Rab 21", "/delete Rab x", "/delete a b c"} {
		if r := hs.cmd(ViaRRC, mod, "scotmesh", bad); !r.Error {
			t.Errorf("%s: %q", bad, r.Text())
		}
	}
	// The page's delete link: one message by ID, same rules.
	words := must[[]store.Message](t)(hs.h.Recent(bg, "scotmesh", 1, 0))[0]
	if r := must[Reply](t)(hs.h.DeleteMessage(bg, mod, words.ID)); !r.Error || !strings.Contains(r.Text(), "not below you") {
		t.Errorf("page delete of an admin's by a mod: %+v", r)
	}
	if r := must[Reply](t)(hs.h.DeleteMessage(bg, admin, words.ID)); r.Error {
		t.Errorf("page delete of your own: %+v", r)
	}
	if r := must[Reply](t)(hs.h.DeleteMessage(bg, admin, spam.ID)); !r.Error || r.Text() != "That message is already gone." {
		t.Errorf("already gone: %+v", r)
	}
	_ = hs.st.View(bg, func(tx *store.Tx) error {
		log, _ := tx.AuditLog(1)
		if len(log) != 1 || log[0].Action != "delete" || log[0].Detail != `message in #scotmesh: "admin's words"` {
			t.Errorf("audit %+v", log)
		}
		return nil
	})
}

// roomsOfSafe is roomsOf from outside the loop goroutine.
func (h *Hub) roomsOfSafe(id []byte) []string {
	var rooms []string
	_ = h.do(bg, func() { rooms = h.roomsOf(id) })
	return rooms
}
