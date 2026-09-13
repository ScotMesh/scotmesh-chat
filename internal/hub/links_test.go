package hub

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

var codeInReply = regexp.MustCompile(`send /link ([A-Z2-7]{4}-[A-Z2-7]{4})`)

// issue asks for a link code and returns it.
func (hs *harness) issue(via Via, id []byte) string {
	hs.t.Helper()
	r := hs.cmd(via, id, "", "/link")
	m := codeInReply.FindStringSubmatch(r.Text())
	if r.Error || m == nil {
		hs.t.Fatalf("/link: %q", r.Text())
	}
	return m[1]
}

func (hs *harness) setRank(id []byte, r store.Rank) {
	hs.t.Helper()
	if err := hs.st.Update(bg, func(tx *store.Tx) error { return tx.SetRank(id, r, oper, 1) }); err != nil {
		hs.t.Fatal(err)
	}
}

func TestLinkAMembersAppsShareOneName(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	hs.claim(alex, "Alex")
	code := hs.issue(ViaLXMF, alex)
	hs.events()

	r := hs.cmd(ViaRRC, rab, "scotmesh", "/link "+strings.ToLower(strings.ReplaceAll(code, "-", "")))
	if r.Error || r.Text() != "This app is now Alex. /devices lists your apps; /unlink takes this one out again." {
		t.Fatalf("redeem: %q", r.Text())
	}
	evs := hs.events()
	if n := eventsOf[NameEvent](evs); len(n) != 1 || !bytes.Equal(n[0].Identity, rab) || n[0].Old != "guest-b2b2" || n[0].New != "Alex" {
		t.Errorf("name events %+v", n)
	}
	if n := eventsOf[NoticeEvent](evs); len(n) != 1 || !bytes.Equal(n[0].Identity, alex) || !strings.Contains(n[0].Text, "A new app was linked to your name: b2b2 (via rrc). Not you? Send /unlink b2b2.") {
		t.Errorf("notices %+v", n)
	}
	if p := must[Person](t)(hs.h.PersonOf(bg, rab)); p.Name != "Alex" || !p.Claimed || !p.Linked() {
		t.Errorf("rab after linking %+v", p)
	}
	// The code worked once.
	hs.identify(ellen)
	if r := hs.cmd(ViaRRC, ellen, "", "/link "+code); !r.Error || !strings.HasPrefix(r.Text(), "That code doesn't work.") {
		t.Errorf("reuse: %q", r.Text())
	}
	// A rename from either app renames both; /whoami and /devices know.
	if r := hs.cmd(ViaRRC, rab, "", "/nick Alexander"); r.Error {
		t.Fatalf("rename: %q", r.Text())
	}
	if p := must[Person](t)(hs.h.PersonOf(bg, alex)); p.Name != "Alexander" {
		t.Errorf("rename from a linked app: %+v", p)
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/whoami"); !strings.Contains(r.Text(), "Alexander shares your name with 1 other app (/devices).") {
		t.Errorf("/whoami: %q", r.Text())
	}
	hs.join(rab, "scotmesh")
	r = hs.cmd(ViaLXMF, alex, "", "/devices")
	if !strings.HasPrefix(r.Text(), "Alexander has 2 apps:\n  a1a1a1a1 — this app · seen ") || !strings.Contains(r.Text(), "  b2b2b2b2 — RRC in #scotmesh · seen ") || !strings.HasSuffix(r.Text(), "/unlink b2b2 takes one out.") {
		t.Errorf("/devices:\n%s", r.Text())
	}
	// /who lists the person once, on each list they're on.
	if _, _, err := hs.h.JoinGroup(bg, alex, ""); err != nil {
		t.Fatal(err)
	}
	if r := hs.cmd(ViaRRC, ellen, "scotmesh", "/who"); r.Text() != "members in scotmesh: Alexander (a1a1a1a1a1a1)\non RRC: Alexander · in the LXMF group: Alexander" {
		t.Errorf("/who: %q", r.Text())
	}
	// /forget from one app frees the name and unlinks the rest.
	hs.events()
	if r := hs.cmd(ViaRRC, rab, "", "/forget"); r.Error {
		t.Fatalf("/forget: %q", r.Text())
	}
	for _, id := range [][]byte{alex, rab} {
		if p := must[Person](t)(hs.h.PersonOf(bg, id)); p.Claimed || p.Linked() {
			t.Errorf("after /forget %x: %+v", id[:2], p)
		}
	}
	if n := eventsOf[NoticeEvent](hs.events()); len(n) != 1 || !bytes.Equal(n[0].Identity, alex) || !strings.Contains(n[0].Text, "Alexander was forgotten from another app") {
		t.Errorf("forget notices %+v", n)
	}
}

func TestLinkCodesAreLimited(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab, ellen)
	hs.claim(alex, "Alex")
	hs.claim(rab, "Rab")

	// Redeeming from an app that holds its own name needs a confirm.
	code := hs.issue(ViaRRC, alex)
	if r := hs.cmd(ViaLXMF, rab, "", "!link "+code); !r.Error || r.Text() != "This app is Rab. Linking frees Rab and makes this app Alex. To go ahead, send /link "+code+" confirm." {
		t.Errorf("confirm needed: %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, rab, "", "/link "+code+" confirm"); r.Error {
		t.Errorf("confirmed: %q", r.Text())
	}
	if err := hs.st.View(bg, func(tx *store.Tx) error {
		if _, ok, _ := tx.NameBySkeleton("rab"); ok {
			t.Error("Rab wasn't freed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Three codes an hour.
	hs.issue(ViaRRC, alex)
	hs.issue(ViaRRC, alex)
	if r := hs.cmd(ViaRRC, alex, "", "/link"); !r.Error || r.Text() != "That's 3 codes this hour; try again later." {
		t.Errorf("fourth code: %q", r.Text())
	}
	hs.clock.advance(time.Hour)
	last := hs.issue(ViaRRC, alex)

	// Five wrong codes lock the guesser out for 10 minutes, even with a good one.
	for i := range 5 {
		if r := hs.cmd(ViaRRC, ellen, "", "/link AAAA-AAAA"); !strings.HasPrefix(r.Text(), "That code doesn't work.") {
			t.Fatalf("wrong code %d: %q", i, r.Text())
		}
	}
	if r := hs.cmd(ViaRRC, ellen, "", "/link "+last); r.Text() != "Too many codes that didn't work; try again in 10 minutes." {
		t.Errorf("locked out: %q", r.Text())
	}
	hs.clock.advance(9 * time.Minute)
	if err := hs.h.do(bg, func() { hs.h.maintain(bg) }); err != nil {
		t.Fatal(err)
	}
	hs.clock.advance(2 * time.Minute)
	// The lockout is over, but so is the code's life.
	if r := hs.cmd(ViaRRC, ellen, "", "/link "+last); !strings.HasPrefix(r.Text(), "That code doesn't work.") {
		t.Errorf("expired code: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, ellen, "", "/link a b c"); r.Text() != "usage: /link [code [confirm]|approve|deny <app>]" {
		t.Errorf("usage: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, alex, "", "/link "+hs.issue(ViaRRC, alex)); r.Text() != "This app is already part of Alex." {
		t.Errorf("own code: %q", r.Text())
	}

	// At most five apps to a name.
	apps := [][]byte{bytes.Repeat([]byte{0x31}, 16), bytes.Repeat([]byte{0x32}, 16), bytes.Repeat([]byte{0x33}, 16)}
	hs.clock.advance(time.Hour)
	for _, app := range apps {
		hs.identify(app)
		hs.clock.advance(21 * time.Minute)
		if r := hs.cmd(ViaRRC, app, "", "/link "+hs.issue(ViaRRC, alex)); r.Error {
			t.Fatalf("linking an app: %q", r.Text())
		}
	}
	hs.clock.advance(time.Hour)
	if r := hs.cmd(ViaRRC, alex, "", "/link"); !r.Error || r.Text() != "Alex already has 5 apps, the most one name can have. /unlink one first." {
		t.Errorf("sixth app: %q", r.Text())
	}
	// A banned app's code doesn't work, and a banned app can't redeem.
	hs.identify(ellen)
	hs.clock.advance(time.Hour)
	code = hs.issue(ViaRRC, ellen)
	hs.cmd(ViaRRC, oper, "", "/kline add "+hexID(ellen))
	stranger := bytes.Repeat([]byte{0x44}, 16)
	hs.identify(stranger)
	if r := hs.cmd(ViaRRC, stranger, "", "/link "+code); !strings.HasPrefix(r.Text(), "That code doesn't work.") {
		t.Errorf("banned issuer's code: %q", r.Text())
	}
	if _, err := hs.h.Command(bg, CommandRequest{Via: ViaRRC, Identity: ellen, Text: "/link " + code}); err != nil {
		t.Fatal(err)
	}
	if r := hs.cmd(ViaRRC, ellen, "", "/whoami"); r.Text() != "you are banned from this hub" {
		t.Errorf("banned redeemer: %q", r.Text())
	}
}

func TestLinkWithRanks(t *testing.T) {
	hs := start(t)
	modPhone, modLaptop, member := bytes.Repeat([]byte{0x51}, 16), bytes.Repeat([]byte{0x52}, 16), bytes.Repeat([]byte{0x53}, 16)
	hs.identify(modPhone, modLaptop, member, oper)
	hs.claim(modPhone, "Morag")
	hs.claim(member, "Rab")
	hs.setRank(modPhone, store.RankMod)

	// No redeeming upwards: the mod's app can't join a member.
	code := hs.issue(ViaLXMF, member)
	if r := hs.cmd(ViaRRC, modPhone, "", "/link "+code); !r.Error || r.Text() != "This app is a mod and Rab isn't. Ask for the code on this app and redeem it on the other." {
		t.Errorf("redeem upwards: %q", r.Text())
	}

	// A mod's new app waits for approval from an app the mod already has.
	code = hs.issue(ViaLXMF, modPhone)
	hs.events()
	r := hs.cmd(ViaRRC, modLaptop, "", "/link "+code)
	if r.Error || !strings.HasPrefix(r.Text(), "Waiting for approval from one of Morag's apps, as Morag is a mod.") {
		t.Fatalf("pending: %q", r.Text())
	}
	if n := eventsOf[NoticeEvent](hs.events()); len(n) != 1 || !bytes.Equal(n[0].Identity, modPhone) || !strings.HasPrefix(n[0].Text, "Approve linking guest-5252 (5252, via rrc) to Morag, with your mod role? Send /link approve 5252 or /link deny 5252.") {
		t.Errorf("approval notice %+v", n)
	}
	if p := must[Person](t)(hs.h.PersonOf(bg, modLaptop)); p.Linked() {
		t.Fatal("linked before approval")
	}
	if r := hs.cmd(ViaLXMF, modPhone, "", "/link approve 9999"); !r.Error || r.Text() != "Nothing is waiting for approval as 9999." {
		t.Errorf("approve nobody: %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, member, "", "/link approve 5252"); !r.Error {
		t.Errorf("someone else approving: %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, modPhone, "", "/link approve 5252"); r.Error || r.Text() != "Approved: guest-5252 (52525252) is now one of Morag's apps." {
		t.Fatalf("approve: %q", r.Text())
	}
	if p := must[Person](t)(hs.h.PersonOf(bg, modLaptop)); p.Name != "Morag" || p.Rank != RankMod {
		t.Errorf("approved app %+v", p)
	}

	// Denied.
	other := bytes.Repeat([]byte{0x54}, 16)
	hs.identify(other)
	hs.cmd(ViaRRC, other, "", "/link "+hs.issue(ViaLXMF, modLaptop))
	if r := hs.cmd(ViaRRC, modLaptop, "", "/link deny guest-5454"); r.Text() != "Denied: guest-5454 (54545454) is not linked." {
		t.Errorf("deny: %q", r.Text())
	}
	if p := must[Person](t)(hs.h.PersonOf(bg, other)); p.Linked() {
		t.Error("linked after a denial")
	}

	// An owner's own identity never joins anyone; an app joining an owner is
	// an admin, and the owner's identity can't be unlinked by it.
	if r := hs.cmd(ViaRRC, oper, "", "/link "+hs.issue(ViaRRC, member)); !r.Error || !strings.HasPrefix(r.Text(), "This is a hub owner's own identity") {
		t.Errorf("owner redeeming: %q", r.Text())
	}
	ownerApp := bytes.Repeat([]byte{0x55}, 16)
	hs.identify(ownerApp)
	hs.cmd(ViaRRC, ownerApp, "", "/link "+hs.issue(ViaRRC, oper))
	if r := hs.cmd(ViaRRC, oper, "", "/link approve 5555"); r.Error {
		t.Fatalf("owner approves: %q", r.Text())
	}
	if p := must[Person](t)(hs.h.PersonOf(bg, ownerApp)); p.Rank != RankAdmin || p.PersonRank != RankOwner {
		t.Errorf("owner's app %+v", p)
	}
	if r := hs.cmd(ViaRRC, ownerApp, "", "/unlink "+hexID(oper)[:6]); !r.Error || r.Text() != "That's a hub owner's own identity; only it can unlink itself." {
		t.Errorf("unlinking the owner's identity: %q", r.Text())
	}
	_ = hs.st.View(bg, func(tx *store.Tx) error {
		log, _ := tx.AuditLog(10)
		var actions []string
		for _, e := range log {
			actions = append(actions, e.Action)
		}
		if got := strings.Join(actions, ","); got != "link,link-deny,link" {
			t.Errorf("audit %s", got)
		}
		return nil
	})
}

func TestUnlink(t *testing.T) {
	hs := start(t)
	a1, a2, a3 := bytes.Repeat([]byte{0x61}, 16), bytes.Repeat([]byte{0x62}, 16), bytes.Repeat([]byte{0x63}, 16)
	mod, member := bytes.Repeat([]byte{0x71}, 16), bytes.Repeat([]byte{0x72}, 16)
	hs.identify(a1, a2, a3, mod, member)
	hs.claim(a1, "Ann")
	hs.claim(mod, "Moddy")
	hs.setRank(mod, store.RankMod)
	for _, app := range [][]byte{a2, a3} {
		if r := hs.cmd(ViaRRC, app, "", "/link "+hs.issue(ViaRRC, a1)); r.Error {
			t.Fatal(r.Text())
		}
	}
	if r := hs.cmd(ViaRRC, a2, "", "/unlink"); r.Text() != "This app is no longer part of Ann; you're guest-6262 here now." {
		t.Errorf("unlink self: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, a2, "", "/unlink"); !r.Error || r.Text() != "guest-6262 (62626262) isn't linked to another app." {
		t.Errorf("unlink alone: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, member, "", "/unlink "+hexID(a3)); !r.Error || r.Text() != "Only mods and up can unlink an app of someone else." {
		t.Errorf("member unlinking someone's app: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, member, "", "/devices Ann"); !r.Error || r.Text() != "Only mods and up can see the apps of someone else." {
		t.Errorf("member listing someone's apps: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, mod, "", "/devices Ann"); r.Error || !strings.HasPrefix(r.Text(), "Ann has 2 apps:") {
		t.Errorf("mod listing a member's apps: %q", r.Text())
	}
	hs.events()
	if r := hs.cmd(ViaRRC, mod, "", "/unlink "+hexID(a3)); r.Text() != "Unlinked 63636363 from Ann." {
		t.Errorf("mod unlinking: %q", r.Text())
	}
	notices := eventsOf[NoticeEvent](hs.events())
	if len(notices) != 2 {
		t.Errorf("unlink notices %+v", notices)
	}
	if r := hs.cmd(ViaRRC, a1, "", "/unlink 6666"); !r.Error || r.Text() != "target '6666' not found" {
		t.Errorf("unlink unknown: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, a1, "", "/unlink a b"); r.Text() != "usage: /unlink [app]" {
		t.Errorf("usage: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, a1, "", "/devices a b"); r.Text() != "usage: /devices [name]" {
		t.Errorf("usage: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, a1, "", "/devices"); r.Text() != "Ann has 1 app:\n  61616161 — this app · seen "+hs.h.clock(hs.clock.now().UnixMilli())+"\n/link gives a code to link another app." {
		t.Errorf("one app: %q", r.Text())
	}
}

// With two apps, LXMF on auto pauses while either is in the room over RRC,
// and "your own messages" means any of your apps.
func TestLinkedAppsPauseAndOwnMessagesTogether(t *testing.T) {
	hs := start(t)
	phone, laptop := bytes.Repeat([]byte{0x81}, 16), bytes.Repeat([]byte{0x82}, 16)
	hs.identify(phone, laptop, rab)
	if _, _, err := hs.h.JoinGroup(bg, phone, "Isla"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hs.h.JoinGroup(bg, rab, "Rab"); err != nil {
		t.Fatal(err)
	}
	hs.cmd(ViaRRC, laptop, "", "/link "+hs.issue(ViaLXMF, phone))
	hs.cmd(ViaLXMF, phone, "", "/lxmf auto")
	hs.cmd(ViaLXMF, phone, "", "/lxmf mine off")
	recipients := func(via Via, author []byte) []string {
		t.Helper()
		hs.events()
		hs.clock.advance(time.Second)
		if _, err := hs.h.Post(bg, PostRequest{Via: via, Identity: author, Body: "hello"}); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range eventsOf[MessageEvent](hs.events()) {
			for _, id := range e.LXMFTo {
				names = append(names, map[string]string{hexID(phone): "phone", hexID(rab): "Rab"}[hexID(id)])
			}
		}
		return names
	}
	hs.join(laptop, "scotmesh")
	if got := recipients(ViaLXMF, rab); strings.Join(got, ",") != "" {
		t.Errorf("laptop on RRC, phone on auto should be paused: %v", got)
	}
	if r := hs.cmd(ViaLXMF, phone, "", "/lxmf"); !strings.HasPrefix(r.Text(), "LXMF delivery is auto, paused while you're on RRC") {
		t.Errorf("status: %q", r.Text())
	}
	hs.events()
	if err := hs.h.Disconnect(bg, laptop, []string{"scotmesh"}); err != nil {
		t.Fatal(err)
	}
	if res := eventsOf[LXMFResumeEvent](hs.events()); len(res) != 1 || !bytes.Equal(res[0].Identity, phone) || res[0].Missed != 1 {
		t.Errorf("resume %+v", res)
	}
	if got := recipients(ViaRRC, laptop); strings.Join(got, ",") != "Rab" {
		t.Errorf("laptop's message, mine off: %v", got)
	}
	hs.cmd(ViaLXMF, phone, "", "/lxmf mine on")
	if got := recipients(ViaRRC, laptop); strings.Join(got, ",") != "phone,Rab" {
		t.Errorf("laptop's message, mine on: %v", got)
	}
	if got := recipients(ViaLXMF, phone); strings.Join(got, ",") != "Rab" {
		t.Errorf("the phone's own LXMF message is never echoed: %v", got)
	}
	if code := normaliseLinkCode("ab0c-d1e8 "); code != "ABOCDIEB" {
		t.Errorf("normalise %q", code)
	}
}

func TestLinkedAppsForTheSettingsPage(t *testing.T) {
	hs := start(t)
	mod, laptop, stranger := bytes.Repeat([]byte{0x91}, 16), bytes.Repeat([]byte{0x92}, 16), bytes.Repeat([]byte{0x93}, 16)
	hs.identify(mod, laptop)
	hs.claim(mod, "Moira")
	hs.setRank(mod, store.RankMod)
	hs.join(mod, "scotmesh")
	if _, _, err := hs.h.JoinGroup(bg, mod, ""); err != nil {
		t.Fatal(err)
	}
	hs.cmd(ViaPage, laptop, "", "/link "+hs.issue(ViaLXMF, mod))
	apps := must[LinkedApps](t)(hs.h.LinkedApps(bg, mod))
	if len(apps.Apps) != 1 || !apps.Apps[0].RRC || !apps.Apps[0].LXMF || apps.Apps[0].LastSeen == 0 {
		t.Errorf("apps %+v", apps.Apps)
	}
	if len(apps.Pending) != 1 || !bytes.Equal(apps.Pending[0].Identity, laptop) || apps.Pending[0].Via != ViaPage || apps.Pending[0].ExpiresAt == 0 {
		t.Errorf("pending %+v", apps.Pending)
	}
	// An identity the hub has never seen is its own only app.
	if apps := must[LinkedApps](t)(hs.h.LinkedApps(bg, stranger)); len(apps.Apps) != 1 || !bytes.Equal(apps.Apps[0].Identity, stranger) || len(apps.Pending) != 0 {
		t.Errorf("unknown identity %+v", apps)
	}
	// Two apps waiting whose identities start alike need more of it.
	twin := []byte{0x92, 0x92, 0x92, 0x92, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	hs.identify(twin)
	hs.clock.advance(time.Minute)
	hs.cmd(ViaPage, twin, "", "/link "+hs.issue(ViaLXMF, mod))
	if r := hs.cmd(ViaLXMF, mod, "", "/link approve 9292"); !r.Error || r.Text() != "9292 matches more than one app waiting; use more of its identity." {
		t.Errorf("ambiguous approval: %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, mod, "", "/link approve 9292929292"); r.Error {
		t.Errorf("longer prefix: %q", r.Text())
	}
	// An app banned while it waits can't be approved.
	hs.cmd(ViaRRC, oper, "", "/kline add "+hexID(twin))
	if r := hs.cmd(ViaLXMF, mod, "", "/link approve 9292929201"); !r.Error || r.Text() != "guest-9292 is banned and can't be linked." {
		t.Errorf("approving a banned app: %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, mod, "", "/link approve 9292929201"); !r.Error || r.Text() != "Nothing is waiting for approval as 9292929201." {
		t.Errorf("a used code: %q", r.Text())
	}
}
