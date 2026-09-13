package hub

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// TestRankRuleForEveryRankAndAction walks every actor rank, action and
// target rank. The expected table is written out rather than derived, so a
// change to the rule has to change this table too.
func TestRankRuleForEveryRankAndAction(t *testing.T) {
	ranks := []Rank{RankMember, RankMod, RankAdmin, RankOwner}
	// allowed[action][actor] lists the target ranks the actor may act on
	// (nil: an action without a target, allowed or not by the bool).
	type rule struct {
		targets map[Rank][]Rank
		plain   map[Rank]bool
	}
	lower := func(r ...Rank) []Rank { return r }
	onLower := map[Rank][]Rank{
		RankMember: nil,
		RankMod:    lower(RankMember),
		RankAdmin:  lower(RankMember, RankMod),
		RankOwner:  lower(RankMember, RankMod, RankAdmin),
	}
	adminsOnLower := map[Rank][]Rank{
		RankMember: nil, RankMod: nil,
		RankAdmin: lower(RankMember, RankMod),
		RankOwner: lower(RankMember, RankMod, RankAdmin),
	}
	modsUp := map[Rank]bool{RankMember: false, RankMod: true, RankAdmin: true, RankOwner: true}
	ownersOnly := map[Rank]bool{RankOwner: true}
	rules := map[Action]rule{
		ActKick:        {targets: onLower},
		ActBan:         {targets: onLower},
		ActRename:      {targets: onLower},
		ActDevicesOf:   {targets: onLower},
		ActUnlinkOther: {targets: onLower},
		ActDelete:      {targets: onLower},
		ActMakeMod:     {targets: adminsOnLower},
		ActMakeAdmin:   {targets: adminsOnLower},
		ActDemote:      {targets: adminsOnLower},
		ActUnban:       {plain: modsUp},
		ActListBans:    {plain: modsUp},
		ActModlog:      {plain: modsUp},
		ActRelease:     {plain: ownersOnly},
		ActStats:       {plain: ownersOnly},
	}
	if len(rules) != len(actionNames) {
		t.Fatalf("the table covers %d actions; there are %d", len(rules), len(actionNames))
	}
	for action, r := range rules {
		for _, actorRank := range ranks {
			actor := Person{Name: "Actor", PersonID: 1, Rank: actorRank, PersonRank: actorRank}
			if r.plain != nil {
				err := may(actor, action, nil)
				if (err == nil) != r.plain[actorRank] {
					t.Errorf("%s may %s: got %v, want allowed=%v", actorRank, action, err, r.plain[actorRank])
				}
				continue
			}
			for _, targetRank := range ranks {
				target := Person{Name: "Target", PersonID: 2, Rank: targetRank, PersonRank: targetRank}
				want := false
				for _, ok := range r.targets[actorRank] {
					want = want || ok == targetRank
				}
				err := may(actor, action, &target)
				if (err == nil) != want {
					t.Errorf("%s may %s %s: got %v, want allowed=%v", actorRank, action, targetRank, err, want)
				}
				if err != nil {
					if _, ok := err.(*UserError); !ok {
						t.Errorf("%s may %s %s: refusal is %T, not a UserError", actorRank, action, targetRank, err)
					}
				}
			}
		}
	}

	// Yourself: you can see and unlink your own apps whatever your rank,
	// but nobody kicks, bans, renames or re-ranks themselves.
	for _, r := range ranks {
		me := Person{Name: "Me", PersonID: 7, Rank: r, PersonRank: r}
		for _, a := range []Action{ActDevicesOf, ActUnlinkOther, ActDelete} {
			if err := may(me, a, &me); err != nil {
				t.Errorf("%s %s themselves: %v", r, a, err)
			}
		}
		for _, a := range []Action{ActKick, ActBan, ActRename, ActDemote} {
			if r >= a.minRank() {
				if err := may(me, a, &me); err == nil || !strings.Contains(err.Error(), "yourself") {
					t.Errorf("%s %s themselves: %v", r, a, err)
				}
			}
		}
	}

	// The rule compares people: an admin's linked app, acting as a member
	// identity, is still an admin's.
	mod := Person{Name: "Mod", PersonID: 3, Rank: RankMod, PersonRank: RankMod}
	adminApp := Person{Name: "Ad", PersonID: 4, Rank: RankAdmin, PersonRank: RankAdmin}
	if err := may(mod, ActKick, &adminApp); err == nil || !strings.Contains(err.Error(), "Ad is an admin") {
		t.Errorf("mod kicking an admin: %v", err)
	}
	ownerApp := Person{Name: "Boss", PersonID: 5, Rank: RankAdmin, PersonRank: RankOwner}
	admin := Person{Name: "Admin", PersonID: 6, Rank: RankOwner, PersonRank: RankOwner}
	if err := may(admin, ActKick, &ownerApp); err == nil || !strings.Contains(err.Error(), "hub owner") {
		t.Errorf("an owner acting on another owner's app: %v", err)
	}
}

func TestRanksComeFromTheConfigAndThePerson(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab, ellen, oper)
	setRank := func(id []byte, r store.Rank) {
		t.Helper()
		if err := hs.st.Update(bg, func(tx *store.Tx) error { return tx.SetRank(id, r, oper, 1) }); err != nil {
			t.Fatal(err)
		}
	}
	link := func(id, to []byte) {
		t.Helper()
		err := hs.st.Update(bg, func(tx *store.Tx) error {
			p, _, err := tx.PersonOf(to)
			if err != nil {
				return err
			}
			return tx.LinkIdentity(id, p.ID, hs.clock.now().UnixMilli()+1)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	person := func(id []byte) Person {
		t.Helper()
		return must[Person](t)(hs.h.PersonOf(bg, id))
	}

	if p := person(oper); p.Rank != RankOwner || p.PersonRank != RankOwner {
		t.Errorf("config admin: %+v", p)
	}
	if p := person(alex); p.Rank != RankMember || p.PersonID == 0 || len(p.Identities) != 1 || p.Linked() || p.Prefs != store.DefaultPrefs() {
		t.Errorf("new person: %+v", p)
	}
	setRank(rab, store.RankMod)
	if p := person(rab); p.Rank != RankMod || !bytes.Equal(p.RankBy, oper) {
		t.Errorf("stored mod: %+v", p)
	}
	// Rab's second app is a mod too.
	link(ellen, rab)
	if p := person(ellen); p.Rank != RankMod || p.PersonRank != RankMod || !p.Linked() || !bytes.Equal(p.Identities[0], rab) {
		t.Errorf("mod's linked app: %+v", p)
	}
	// An owner's other app is an admin, but the person counts as an owner.
	link(alex, oper)
	if p := person(alex); p.Rank != RankAdmin || p.PersonRank != RankOwner {
		t.Errorf("owner's linked app: %+v", p)
	}
	if p := person(oper); p.Rank != RankOwner {
		t.Errorf("owner after linking: %+v", p)
	}
	// An identity never seen: a member with defaults, or an owner if listed.
	stranger := bytes.Repeat([]byte{0x55}, 16)
	if p := person(stranger); p.Rank != RankMember || p.PersonID != 0 || p.Prefs != store.DefaultPrefs() {
		t.Errorf("unknown identity: %+v", p)
	}

	// Mods are operators in every room, including ones they didn't found.
	hs.join(bytes.Repeat([]byte{0x66}, 16), "den")
	if r := hs.cmd(ViaRRC, rab, "", "/voice den "+hexID(alex)); r.Error {
		t.Errorf("a mod giving voice in someone else's room: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, bytes.Repeat([]byte{0x77}, 16), "", "/voice den "+hexID(alex)); !r.Error || r.Text() != "not authorized" {
		t.Errorf("a member giving voice in someone else's room: %q", r.Text())
	}
	if got := hs.cmd(ViaLXMF, rab, "", "/whoami").Text(); !strings.Contains(got, "You are a mod of this hub.") {
		t.Errorf("whoami for a mod: %q", got)
	}
	if got := hs.cmd(ViaLXMF, oper, "", "/whoami").Text(); !strings.Contains(got, "You are an owner of this hub.") {
		t.Errorf("whoami for an owner: %q", got)
	}
	// Owners are never stored: the config file says who they are.
	for r, want := range map[Rank]struct {
		title  string
		stored store.Rank
	}{RankMember: {"", store.RankMember}, RankMod: {"mod", store.RankMod}, RankAdmin: {"admin", store.RankAdmin}, RankOwner: {"owner", store.RankMember}} {
		if r.Title() != want.title || r.stored() != want.stored {
			t.Errorf("rank %d: title %q stored %q", r, r.Title(), r.stored())
		}
	}
	if s := (Person{Name: "Rab", Rank: RankMod}).String(); s != "Rab (mod)" {
		t.Errorf("Person.String %q", s)
	}
}
