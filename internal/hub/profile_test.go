package hub

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

func TestProfilesAndSharing(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab, ellen)
	hs.claim(rab, "Rab")
	addr := func(id []byte) string { return hex.EncodeToString(LXMFAddress(id)) }

	// Rab is only on RRC: the address is the one their identity would have.
	hs.join(rab, "scotmesh")
	want := "Rab\nName held since 12 Sep 2026.\nHere now: RRC in #scotmesh.\nLXMF, if they use it with this identity: " + addr(rab)
	for _, via := range []Via{ViaRRC, ViaLXMF, ViaPage} {
		if r := hs.cmd(via, alex, "", "/profile Rab"); r.Text() != want {
			t.Errorf("/profile over %s\n got %q\nwant %q", via, r.Text(), want)
		}
	}
	if r := hs.cmd(ViaLXMF, alex, "", "!whois rab"); r.Text() != want {
		t.Errorf("/whois %q", r.Text())
	}
	// In the group, the address is the member's, and they can hide it.
	if _, _, err := hs.h.JoinGroup(bg, rab, ""); err != nil {
		t.Fatal(err)
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/profile Rab"); !strings.HasSuffix(r.Text(), "Here now: RRC in #scotmesh and the LXMF group.\nLXMF: "+addr(rab)) {
		t.Errorf("member profile %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "", "/share"); r.Text() != "Your LXMF address is shown on your profile, so people can message you. /share off hides it." {
		t.Errorf("/share status %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "", "/share off"); r.Text() != "Your LXMF address is hidden from your profile now." {
		t.Errorf("/share off %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/profile Rab"); !strings.HasSuffix(r.Text(), "\nTheir LXMF address isn't shared.") {
		t.Errorf("hidden %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "", "/share"); r.Text() != "Your LXMF address isn't shown on your profile. /share on shows it." {
		t.Errorf("/share status off %q", r.Text())
	}
	// A mod's address is shown whatever they chose.
	hs.setRank(rab, store.RankMod)
	if r := hs.cmd(ViaLXMF, alex, "", "/profile Rab"); !strings.HasPrefix(r.Text(), "Rab · mod\n") || !strings.HasSuffix(r.Text(), "LXMF: "+addr(rab)) {
		t.Errorf("mod profile %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "", "/share off"); r.Text() != "Your LXMF address is hidden from your profile now.\nSaved, but as a mod your address is shown anyway, so people can reach you." {
		t.Errorf("mod /share off %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "", "/share"); !strings.HasPrefix(r.Text(), "Your LXMF address is shown on your profile: it always is for mods and up") {
		t.Errorf("mod /share status %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, rab, "", "/share maybe"); r.Text() != "usage: /share on|off" {
		t.Errorf("usage %q", r.Text())
	}

	// Your own profile; someone who isn't here; nobody; too many words.
	hs.claim(alex, "Alex")
	if r := hs.cmd(ViaPage, alex, "", "/profile"); !strings.HasPrefix(r.Text(), "Alex\nName held since") {
		t.Errorf("own profile %q", r.Text())
	}
	if r := hs.cmd(ViaPage, alex, "", "/profile "+hexID(ellen)); r.Text() != "guest-e3e3\nNo name claimed; shown by the start of their identity.\nLast seen Sat 19:00.\nLXMF, if they use it with this identity: "+addr(ellen) {
		t.Errorf("guest profile %q", r.Text())
	}
	if r := hs.cmd(ViaPage, alex, "", "/profile Nobody"); !r.Error || r.Text() != "target 'Nobody' not found" {
		t.Errorf("nobody %q", r.Text())
	}
	if r := hs.cmd(ViaPage, alex, "", "/profile a b"); r.Text() != "usage: /profile [name]" {
		t.Errorf("usage %q", r.Text())
	}
}

func TestProfileOfALinkedPersonForThePage(t *testing.T) {
	hs := start(t)
	phone, laptop := bytes.Repeat([]byte{0xa7}, 16), bytes.Repeat([]byte{0xa8}, 16)
	hs.identify(phone, laptop)
	if _, _, err := hs.h.JoinGroup(bg, phone, "Isla"); err != nil {
		t.Fatal(err)
	}
	hs.cmd(ViaRRC, laptop, "", "/link "+hs.issue(ViaLXMF, phone))
	if _, _, err := hs.h.JoinGroup(bg, laptop, ""); err != nil {
		t.Fatal(err)
	}
	hs.join(laptop, "scotmesh")
	hs.say(ViaLXMF, phone, "from the phone")
	hs.say(ViaRRC, laptop, "from the laptop")
	p := must[Profile](t)(hs.h.ProfileOf(bg, laptop))
	if p.Person.Name != "Isla" || !p.Shared || p.ForcedShare || p.Derived || len(p.Addresses) != 2 {
		t.Errorf("profile %+v", p)
	}
	if strings.Join(p.Here, " and ") != "RRC in #scotmesh and the LXMF group" {
		t.Errorf("here %v", p.Here)
	}
	if got := bodies(p.Recent); strings.Join(got, ",") != "from the phone,from the laptop" {
		t.Errorf("recent %v", got)
	}
	// The page's buttons change preferences with the same checks.
	if _, err := hs.h.SetPrefs(bg, phone, func(pr *store.Prefs) { pr.PageLines = 11 }); err == nil {
		t.Error("a bad preference was saved")
	}
	after, err := hs.h.SetPrefs(bg, phone, func(pr *store.Prefs) { pr.ShareLXMF = false })
	if err != nil || after.Prefs.ShareLXMF {
		t.Errorf("SetPrefs %+v %v", after.Prefs, err)
	}
	if p := must[Profile](t)(hs.h.ProfileOf(bg, laptop)); p.Shared {
		t.Error("sharing is one setting for the whole person")
	}
}

func TestPagePreferencesAndPermissions(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab)
	steps := []struct {
		text, reply string
		err         bool
	}{
		{"/page", "Chat page: 30 messages, flag shown, refresh every 30s. Change it with /page lines 10|20|30|50|100, /page flag on|off or /page refresh off|10|30|60.", false},
		{"/page lines 50", "Saved: the chat page now shows 50 messages, the flag on, and refreshes every 30 seconds.", false},
		{"/page flag off", "Saved: the chat page now shows 50 messages, the flag off, and refreshes every 30 seconds.", false},
		{"/page refresh off", "Saved: the chat page now shows 50 messages, the flag off, and refreshes only when you reload.", false},
		{"/page refresh 10s", "Saved: the chat page now shows 50 messages, the flag off, and refreshes every 10 seconds.", false},
		{"/page", "Chat page: 50 messages, flag hidden, refresh every 10s. Change it with /page lines 10|20|30|50|100, /page flag on|off or /page refresh off|10|30|60.", false},
		{"/page lines 25", "The chat page can show 10, 20, 30, 50 or 100 messages.", true},
		{"/page refresh 5", "The chat page can refresh every 10, 30 or 60 seconds, or not at all (off).", true},
		{"/page flag maybe", "usage: /page [lines|flag|refresh …]", true},
		{"/page colour blue", "usage: /page [lines|flag|refresh …]", true},
		{"/page lines", "usage: /page [lines|flag|refresh …]", true},
	}
	for _, s := range steps {
		if r := hs.cmd(ViaLXMF, alex, "", s.text); r.Text() != s.reply || r.Error != s.err {
			t.Errorf("%s\n got %q (error=%v)\nwant %q", s.text, r.Text(), r.Error, s.reply)
		}
	}
	if p := must[Person](t)(hs.h.PersonOf(bg, alex)); p.Prefs.PageLines != 50 || p.Prefs.PageFlag || p.Prefs.PageRefresh != 10 {
		t.Errorf("saved prefs %+v", p.Prefs)
	}

	hs.setRank(alex, store.RankMod)
	perms := must[map[Action]bool](t)(hs.h.Permissions(bg, alex, rab))
	if !perms[ActKick] || !perms[ActBan] || !perms[ActDelete] || perms[ActMakeMod] || perms[ActStats] {
		t.Errorf("a mod's permissions on a member %v", perms)
	}
	perms = must[map[Action]bool](t)(hs.h.Permissions(bg, rab, alex))
	for a, ok := range perms {
		if ok {
			t.Errorf("a member may %s a mod", a)
		}
	}
}
