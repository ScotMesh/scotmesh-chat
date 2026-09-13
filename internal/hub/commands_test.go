package hub

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// TestRoomOperatorCommandReplies runs the /room commands through their
// replies, in order, on one hub. Each step is one command and the exact reply.
func TestRoomOperatorCommandReplies(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab, ellen)
	hs.claim(rab, "Rab")
	hs.claim(ellen, "Ellen")
	hs.join(alex, "den") // alex founds #den
	hs.join(rab, "den")

	steps := []struct {
		who   []byte
		via   Via
		room  string
		text  string
		reply string
		err   bool
	}{
		{alex, ViaRRC, "den", "/room", "usage: /room <verb> [#room] …", true},
		{alex, ViaRRC, "den", "/room frob", "There's no /room frob. The verbs are kick, ban, unban, bans, invite, uninvite, invites, op, deop, voice, devoice, mode, register and unregister.", true},
		{alex, ViaRRC, "den", "/room op", "usage: /room op [#room] <name|hashprefix|hash>", true},
		{alex, ViaRRC, "den", "/room op #nowhere rab", "no such room", true},
		{alex, ViaRRC, "den", "/room op #bad!room rab", "bad room: room name may only use letters, digits, '-', '_' and '.'", true},
		{alex, ViaRRC, "den", "/room op lxmf rab", "usage: /room op [#room] <name|hashprefix|hash>", true},
		{rab, ViaRRC, "den", "/room op rab", "not authorized", true},
		{alex, ViaRRC, "den", "/room op rab", "op granted in den", false},
		{rab, ViaRRC, "den", "/room voice ellen", "voice granted in den", false},
		{rab, ViaLXMF, "", "/room devoice #den ellen", "voice removed in den", false},
		{rab, ViaRRC, "den", "/room deop " + hexID(alex), "cannot deop founder", true},
		{alex, ViaPage, "", "/room deop #den rab", "op removed in den", false},
		{alex, ViaRRC, "den", "/room mode", "usage: /room mode [#room] (+m|-m|+i|-i|+k <key>|-k|+t|-t|+n|-n|+p|-p|+o|-o|+v|-v <name>)", true},
		{alex, ViaRRC, "den", "/room mode +x", "supported modes: +m -m +i -i +k -k +t -t +n -n +p -p +r -r +o -o +v -v", true},
		{alex, ViaRRC, "den", "/room mode +r", "use /room register or /room unregister to change +r", true},
		{alex, ViaRRC, "den", "/room mode +k", "usage: /room mode [#room] +k <key>", true},
		{alex, ViaRRC, "den", "/room mode +k open sesame", "mode for den is now: +k", false},
		{alex, ViaRRC, "den", "/room mode -k", "mode for den is now: (none)", false},
		{alex, ViaRRC, "den", "/room mode +o", "usage: /room mode [#room] (+o|-o|+v|-v) <name|hashprefix|hash>", true},
		{alex, ViaRRC, "den", "/room mode +o rab", "mode for den is now: +o b2b2b2b2b2b2", false},
		{alex, ViaRRC, "den", "/room mode -o " + hexID(alex), "cannot deop founder", true},
		{alex, ViaRRC, "den", "/room mode +p", "mode for den is now: +p", false},
		{ellen, ViaRRC, "", "/who #den", "room den is private", true},
		{oper, ViaLXMF, "", "/who den", "members in den: Rab (b2b2b2b2b2b2), guest-a1a1 (a1a1a1a1a1a1)\non RRC: Rab, guest-a1a1", false},
		{alex, ViaRRC, "den", "/room mode -p", "mode for den is now: (none)", false},
		{alex, ViaRRC, "den", "/room ban", "usage: /room ban [#room] <name|hashprefix|hash>", true},
		{ellen, ViaRRC, "", "/room bans #den", "no bans in den", false},
		{ellen, ViaRRC, "", "/room ban #den rab", "not authorized", true},
		{alex, ViaRRC, "den", "/room ban ellen", "ban added in den", false},
		{ellen, ViaLXMF, "", "/room bans #den", "bans in den: Ellen (" + hexID(ellen) + ")", false},
		{alex, ViaRRC, "den", "/room unban ellen", "ban removed in den", false},
		{alex, ViaRRC, "den", "/room invites", "invites in den: (none)", false},
		{alex, ViaRRC, "den", "/room invites ellen", "usage: /room invites [#room]", true},
		{alex, ViaRRC, "den", "/room invite ellen", "invite sent to ellen for den", false},
		{alex, ViaRRC, "den", "/room mode +i", "mode for den is now: +i", false},
		{alex, ViaRRC, "den", "/room invite ellen", "invite added in den (expires in 900s)", false},
		{alex, ViaRRC, "den", "/room invites", "invites in den: " + hexID(ellen) + " expires_in=900s", false},
		{alex, ViaRRC, "den", "/room uninvite ellen", "invite removed in den", false},
		{alex, ViaRRC, "den", "/room invite", "usage: /room invite [#room] <name|hashprefix|hash>", true},
		{alex, ViaRRC, "den", "/room unregister", "room den is not registered", true},
		{alex, ViaRRC, "den", "/room unregister now", "usage: /room unregister [#room]", true},
		{rab, ViaRRC, "den", "/room unregister", "only the room founder can unregister", true},
		{alex, ViaLXMF, "", "/room register #den", "must be present in the room to register it", true},
		{alex, ViaRRC, "den", "/room register", "registered room den", false},
		{alex, ViaRRC, "den", "!room unregister #den", "unregistered room den", false},
	}
	for _, s := range steps {
		r := hs.cmd(s.via, s.who, s.room, s.text)
		if r.Text() != s.reply || r.Error != s.err {
			t.Errorf("%s (%s)\n got %q (error=%v)\nwant %q (error=%v)", s.text, s.via, r.Text(), r.Error, s.reply, s.err)
		}
	}
}

// TestRRCDFormsStillWorkWithAHint: rrcd's top-level room commands, which
// NomadNet forwards, still work, and say what to type next time.
func TestRRCDFormsStillWorkWithAHint(t *testing.T) {
	hs := start(t)
	hs.identify(alex, rab, ellen)
	hs.claim(rab, "Rab")
	hs.claim(ellen, "Ellen")
	hs.join(alex, "den")
	hs.join(rab, "den")
	steps := []struct {
		room, text, reply string
		err               bool
	}{
		{"den", "/op den rab", "op granted in den\n(next time: /room op #den rab)", false},
		{"den", "/voice ellen", "voice granted in den\n(next time: /room voice #den ellen)", false},
		{"den", "/deop nowhere rab", "no such room", true},
		{"den", "/ban den add ellen", "ban added in den\n(next time: /room ban #den ellen)", false},
		{"den", "/ban list", "bans in den: Ellen (" + hexID(ellen) + ")\n(next time: /room bans #den)", false},
		{"den", "/ban den del ellen", "ban removed in den\n(next time: /room unban #den ellen)", false},
		{"den", "/ban den frob", "not authorized", true}, // not rrcd's form: the hub-wide /ban, for mods
		{"den", "/ban", "not authorized", true},
		{"den", "/invite den add ellen", "invite sent to ellen for den\n(next time: /room invite #den ellen)", false},
		{"den", "/mode den +m", "mode for den is now: +m\n(next time: /room mode #den +m)", false},
		{"den", "/mode -m", "mode for den is now: (none)\n(next time: /room mode #den -m)", false},
		{"den", "/kick den rab", "kicked rab from den\n(next time: /room kick #den rab)", false},
		{"den", "/register den", "registered room den\n(next time: /room register #den)", false},
		{"den", "/unregister", "unregistered room den\n(next time: /room unregister #den)", false},
		{"den", "/kick bad! rab", "not authorized", true}, // no such room: the hub-wide /kick, for mods
	}
	for _, s := range steps {
		r := hs.cmd(ViaRRC, alex, s.room, s.text)
		if r.Text() != s.reply || r.Error != s.err {
			t.Errorf("%s\n got %q (error=%v)\nwant %q (error=%v)", s.text, r.Text(), r.Error, s.reply, s.err)
		}
	}
}

func TestGroupCommandsOverLXMF(t *testing.T) {
	hs := start(t, func(c *Config) { c.GroupAddress = "625ad393ca4df62a1382f9d936322a0a" })
	welcome := "Welcome to ScotMesh, %s. Everyone in #scotmesh on RRC and on the chat page sees what you send here.\n" +
		"You'll get every message, your own from RRC and the page included. /lxmf auto pauses it while you're on RRC; /lxmf mine off stops your own.\n"
	steps := []struct {
		who         []byte
		text, reply string
	}{
		{ellen, "/join Ellen", fmt.Sprintf(welcome, "Ellen") + "/help lists the commands."},
		{ellen, "/join", "You're already in the group, as Ellen."},
		{rab, "/join Ellen", fmt.Sprintf(welcome, "guest-b2b2") + "\"Ellen\" is taken by someone else. You are shown as guest-b2b2 until you choose a free name with /nick.\n/help lists the commands."},
		{alex, "/lxmf off", "LXMF delivery is off. You stay in the group; /lxmf on starts it again.\nSaved, but you're not in the LXMF group. Joining it turns delivery on, your own messages included; change it again after you /join if you like."},
		{alex, "!join lxmf", fmt.Sprintf(welcome, "guest-a1a1") + "Pick a name with /nick YourName so people know who you are.\n/help lists the commands."},
		{alex, "/lxmf", "LXMF delivery is on, and your own messages from RRC and the page: on. Change it with /lxmf on, /lxmf off, /lxmf auto, or /lxmf mine on|off."},
		{alex, "/join #den", "The LXMF group carries #scotmesh only. Other rooms are joined over RRC."},
		{alex, "/join Two Words", "A name can't contain spaces."},
		{alex, "/nick", "usage: /nick <name>"},
		{alex, "/nick A B", "A name can't contain spaces."},
		{alex, "/nick 9lives", "A name has to start with a letter. You are shown as guest-a1a1 until you choose a free name with /nick."},
		{alex, "/nick Alex", "You are now Alex. The name is yours on RRC, the LXMF group and the page until you /forget it."},
		{alex, "/pause", "LXMF delivery is off. You stay in the group; /lxmf on starts it again."},
		{alex, "/lxmf sometimes", "usage: /lxmf on|off|auto|mine on|off"},
		{alex, "/lxmf mine maybe", "usage: /lxmf on|off|auto|mine on|off"},
		{alex, "/lxmf auto", "LXMF delivery is auto, on: it pauses while you're in #scotmesh over RRC."},
		{alex, "/who", "members in scotmesh: Alex (a1a1a1a1a1a1), Ellen (e3e3e3e3e3e3), guest-b2b2 (b2b2b2b2b2b2)\nin the LXMF group: Alex, Ellen, guest-b2b2"},
		{alex, "/list", "Registered public rooms:\n  scotmesh - ScotMesh - Scotland's Reticulum community"},
		{alex, "/topic", "topic for scotmesh: ScotMesh - Scotland's Reticulum community"},
		{alex, "/topic Radios and hills", "not authorized (+t)"}, // a configured room's topic is op-only; alex is a plain member
		{oper, "/topic Radios and hills", "topic for scotmesh is now: Radios and hills"},
		{oper, "/topic #scotmesh scotmesh rocks", "topic for scotmesh is now: scotmesh rocks"},
		{alex, "/history", "No messages in #scotmesh yet."},
		{oper, "/stats", ""}, // checked below
		{alex, "/resume", "LXMF delivery is on."},
		{ellen, "/leave #den", "The LXMF group carries #scotmesh only. Leave other rooms from your RRC app."},
		{ellen, "/leave", "You've left the LXMF group; nothing more comes to you by LXMF. Your name stays yours (/forget frees it). To come back, send /join to 625ad393ca4df62a1382f9d936322a0a from your LXMF app."},
		{ellen, "/leave lxmf", "You're not in the LXMF group."},
		{ellen, "/leave now please", "usage: /leave [#room|lxmf]"},
		{ellen, "/lxmf off", "LXMF delivery is off. You stay in the group; /lxmf on starts it again.\nSaved, but you're not in the LXMF group. Joining it turns delivery on, your own messages included; change it again after you /join if you like."},
		{ellen, "/join", fmt.Sprintf(welcome, "Ellen") + "/help lists the commands."},
		{ellen, "/lxmf", "LXMF delivery is on, and your own messages from RRC and the page: on. Change it with /lxmf on, /lxmf off, /lxmf auto, or /lxmf mine on|off."},
	}
	for _, s := range steps {
		hs.clock.advance(20 * time.Second) // past /history's cooldown
		r := hs.cmd(ViaLXMF, s.who, "", s.text)
		if s.text == "/stats" {
			if !strings.HasPrefix(r.Text(), "posts=0 duplicates=0 refusals=0 commands=") || !strings.Contains(r.Text(), "subscriber test: queued=") {
				t.Errorf("/stats %q", r.Text())
			}
			continue
		}
		if r.Text() != s.reply {
			t.Errorf("%s\n got %q\nwant %q", s.text, r.Text(), s.reply)
		}
	}
	// A repeat /join keeps settings someone chose after joining.
	hs.cmd(ViaLXMF, alex, "", "/lxmf mine off")
	hs.cmd(ViaLXMF, alex, "", "/lxmf auto")
	hs.cmd(ViaLXMF, alex, "", "/join")
	if p := must[Person](t)(hs.h.PersonOf(bg, alex)); p.LXMFMine || p.LXMFMode != store.LXMFAuto {
		t.Errorf("a repeat /join changed settings: %+v", p)
	}
}

// TestJoinAndLeaveExplainThemselvesWhereTheyCantAct: rooms are joined from an
// RRC app and the group from an LXMF app; everywhere else the command says
// so rather than failing.
func TestJoinAndLeaveExplainThemselvesWhereTheyCantAct(t *testing.T) {
	hs := start(t, func(c *Config) { c.GroupAddress = "625ad393ca4df62a1382f9d936322a0a" })
	hs.identify(alex)
	howTo := "To get the chat by LXMF, send /join to 625ad393ca4df62a1382f9d936322a0a from your LXMF app (Sideband, MeshChatX, Columba). Joining needs your LXMF address, which the hub only learns from a message."
	steps := []struct {
		via         Via
		room, text  string
		reply       string
		err, member bool
	}{
		{ViaRRC, "scotmesh", "/join lxmf", howTo, false, false},
		{ViaRRC, "scotmesh", "/join #den", "Join rooms from your RRC app: in MeshChatX, type den in the room name box under the hub; in NomadNet, send /join den.", false, false},
		{ViaRRC, "scotmesh", "/join den", "Join rooms from your RRC app: in MeshChatX, type den in the room name box under the hub; in NomadNet, send /join den.", false, false},
		{ViaPage, "", "/join", howTo, false, false},
		{ViaPage, "", "/join lxmf", howTo, false, false},
		{ViaPage, "", "/join #scotmesh", "You're reading #scotmesh here already; post with Say.", false, false},
		{ViaPage, "", "/join #den", "The chat page carries #scotmesh only. Other rooms are joined over RRC.", false, false},
		{ViaRRC, "scotmesh", "/leave", "You're not in #scotmesh.", true, false},
		{ViaPage, "", "/leave", "You're not in the LXMF group.", true, false},
		{ViaRRC, "", "/leave #bad!", "bad room: room name may only use letters, digits, '-', '_' and '.'", true, false},
	}
	for _, s := range steps {
		r := hs.cmd(s.via, alex, s.room, s.text)
		if r.Text() != s.reply || r.Error != s.err {
			t.Errorf("%s over %s\n got %q (error=%v)\nwant %q (error=%v)", s.text, s.via, r.Text(), r.Error, s.reply, s.err)
		}
	}
	// Over RRC, /leave takes the link out of the room it names.
	hs.join(alex, "den")
	if r := hs.cmd(ViaRRC, alex, "scotmesh", "/leave #den"); r.Error || r.PartRoom != "den" || r.Text() != "You've left #den." {
		t.Errorf("/leave #den over RRC: %+v", r)
	}
	// /leave lxmf leaves the group from RRC too.
	if _, _, err := hs.h.JoinGroup(bg, alex, ""); err != nil {
		t.Fatal(err)
	}
	if r := hs.cmd(ViaRRC, alex, "den", "/leave lxmf"); r.Error || r.PartRoom != "" || !strings.HasPrefix(r.Text(), "You've left the LXMF group") {
		t.Errorf("/leave lxmf over RRC: %+v", r)
	}
	// Joining by the typed API sets the defaults too.
	hs.identify(rab)
	if _, err := hs.h.SetLXMFMode(bg, rab, store.LXMFOff); err != nil {
		t.Fatal(err)
	}
	if p, _, err := hs.h.JoinGroup(bg, rab, ""); err != nil || p.LXMFMode != store.LXMFOn || !p.LXMFMine {
		t.Errorf("JoinGroup defaults: %+v %v", p, err)
	}
	if left, err := hs.h.LeaveGroup(bg, rab); err != nil || !left {
		t.Errorf("LeaveGroup %v %v", left, err)
	}
}

// TestCommandsAreTheSameOnEveryWayIn runs one script of commands on three
// hubs in the same state, over RRC, LXMF and the page, and requires the same
// replies from all three: nobody moving between apps meets a different
// command. Commands whose job depends on the way in
// (/join, /leave, /me) are covered by their own tests.
func TestCommandsAreTheSameOnEveryWayIn(t *testing.T) {
	script := []string{
		"/help", "/help 2", "/help 3", "!help 1", "/help nick", "/help /room", "/help frob", "/help 9",
		"/whoami", "/nick Morag", "/nick", "/whoami",
		"/lxmf", "/lxmf mine off", "/lxmf auto", "/pause", "/resume", "/lxmf nonsense",
		"/who", "/who #scotmesh", "/who lxmf", "/who #nowhere", "/names",
		"/list", "/topic", "/topic #scotmesh", "/history", "/history 2", "/history #scotmesh 1", "/history x",
		"/room bans", "/room kick Rab", "/room frob", "/kline list", "/stats", "/frob", "!frob",
		"/forget", "/whoami", "/forget",
	}
	transcripts := map[Via][]string{}
	for _, via := range []Via{ViaRRC, ViaLXMF, ViaPage} {
		hs := start(t, func(c *Config) { c.GroupAddress = "625ad393ca4df62a1382f9d936322a0a" })
		hs.identify(alex, rab)
		hs.claim(rab, "Rab")
		if _, _, err := hs.h.JoinGroup(bg, alex, "Alex"); err != nil {
			t.Fatal(err)
		}
		hs.say(ViaLXMF, alex, "one")
		hs.say(ViaLXMF, alex, "two")
		for _, line := range script {
			hs.clock.advance(time.Minute)
			// Over RRC the room typed in is the group room, as when typing in
			// #scotmesh; the reply must not depend on it.
			r := hs.cmd(via, alex, "", line)
			text := fmt.Sprintf("%s -> %q error=%v", line, r.Text(), r.Error)
			if len(r.History) > 0 {
				text += fmt.Sprintf(" history=%q %v", r.HistoryNote, bodies(r.History))
			}
			transcripts[via] = append(transcripts[via], text)
		}
	}
	for i := range script {
		rrc, lxmf, page := transcripts[ViaRRC][i], transcripts[ViaLXMF][i], transcripts[ViaPage][i]
		if rrc != lxmf || lxmf != page {
			t.Errorf("replies differ:\n RRC  %s\n LXMF %s\n page %s", rrc, lxmf, page)
		}
	}
}

func TestBangCommandsAndActions(t *testing.T) {
	for text, want := range map[string]bool{
		"/help": true, "/anything": true, "!help": true, "!HELP 2": true, "  !whoami": true, "!names": true,
		"!!!": false, "!important news": false, "!": false, "hello": false, "": false, "! help": false,
	} {
		if got := IsCommand(text); got != want {
			t.Errorf("IsCommand(%q) = %v, want %v", text, got, want)
		}
	}
	for text, want := range map[string]string{
		"/me waves": "waves", "!me waves": "waves", "/ME  waves  ": "waves", "/me": "", "/me   ": "", "!meh": "", "me waves": "",
	} {
		got, ok := ActionText(text)
		if got != want || ok != (want != "") {
			t.Errorf("ActionText(%q) = %q, %v", text, got, ok)
		}
	}
	// A /me that reaches the command table is said as an action.
	hs := start(t)
	hs.identify(alex)
	if _, _, err := hs.h.JoinGroup(bg, alex, "Alex"); err != nil {
		t.Fatal(err)
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/me"); !r.Error || r.Text() != "usage: /me <action>" {
		t.Errorf("/me alone: %q", r.Text())
	}
	if r := hs.cmd(ViaPage, alex, "", "/me waves"); r.Error {
		t.Errorf("/me waves: %q", r.Text())
	}
	if msgs := must[[]store.Message](t)(hs.h.Recent(bg, "scotmesh", 1, 0)); len(msgs) != 1 || msgs[0].Kind != store.KindAction || msgs[0].Body != "waves" {
		t.Errorf("the action: %+v", msgs)
	}
	hs.identify(rab)
	if r := hs.cmd(ViaLXMF, rab, "", "/me waves"); !r.Error || r.Text() != "Not sent: send /join first to take part in the group" {
		t.Errorf("/me from a non-member over LXMF: %q", r.Text())
	}
}

func TestResolveAmbiguousPrefixes(t *testing.T) {
	hs := start(t)
	a := []byte{0xab, 0xcd, 0xef, 0x01, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	b := []byte{0xab, 0xcd, 0xef, 0x02, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}
	hs.identify(a, b, alex)
	hs.join(a, "scotmesh")
	hs.join(b, "scotmesh")
	r := hs.cmd(ViaRRC, oper, "", "/ban abcdef")
	want := "ambiguous: 'abcdef' matches 2 identities:\n  - abcdef0101010101 guest-abcd\n  - abcdef0202020202 guest-abcd\nUse full or longer identity hash to disambiguate."
	if r.Text() != want || !r.Error {
		t.Errorf("ambiguous\n got %q\nwant %q", r.Text(), want)
	}
	if r := hs.cmd(ViaRRC, oper, "", "/ban abcdef02"); r.Text() != "Banned guest-abcd (1 app), for good. Their name is free." {
		t.Errorf("longer prefix %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/ban abc"); r.Text() != "target 'abc' not found" {
		t.Errorf("short prefix %q", r.Text())
	}
	// A banned identity has left every room but can still be named by prefix.
	if r := hs.cmd(ViaRRC, oper, "", "/unban guest-abcd"); !strings.HasPrefix(r.Text(), "ambiguous: 'guest-abcd' matches 2 identities:") {
		t.Errorf("guest prefix shared by a present and a banned identity: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/unban abcdef02"); r.Text() != "Unbanned guest-abcd (1 app)." {
		t.Errorf("prefix of a banned identity: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/unban "+hexID(alex)); r.Text() != hexID(alex)+" isn't banned." {
		t.Errorf("unban someone not banned %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/release"); r.Text() != "usage: /release <name>" {
		t.Errorf("release usage %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/release Morag"); r.Text() != "nobody holds Morag" {
		t.Errorf("release free %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, oper, "", "/release 42"); r.Text() != "42 isn't a name anyone could hold" {
		t.Errorf("release invalid %q", r.Text())
	}
}

func TestPublicMethodsAndMaintenance(t *testing.T) {
	hs := start(t, func(c *Config) { c.Retention = time.Hour; c.PruneRoomsAfter = 2 * time.Hour })
	hs.identify(alex)
	hs.claim(alex, "Alex")
	if p, err := hs.h.PersonOf(bg, alex); err != nil || p.Name != "Alex" || p.Member {
		t.Fatalf("PersonOf %+v %v", p, err)
	}
	if _, _, err := hs.h.JoinGroup(bg, alex, ""); err != nil {
		t.Fatal(err)
	}
	if p, err := hs.h.SetLXMFMode(bg, alex, store.LXMFOff); err != nil || p.LXMFMode != store.LXMFOff {
		t.Fatalf("SetLXMFMode %+v %v", p, err)
	}
	hs.say(ViaLXMF, alex, "old news")
	if msgs, err := hs.h.History(bg, alex, "", 5, 0); err != nil || len(msgs) != 1 {
		t.Fatalf("History %v %v", msgs, err)
	}
	if msgs, err := hs.h.Recent(bg, "scotmesh", 5, 0); err != nil || len(msgs) != 1 {
		t.Fatalf("Recent %v %v", msgs, err)
	}

	// A registered room nobody uses, and an old message.
	hs.join(alex, "attic")
	hs.cmd(ViaRRC, alex, "attic", "/room register")
	if err := hs.h.Disconnect(bg, alex, []string{"attic"}); err != nil {
		t.Fatal(err)
	}
	hs.clock.advance(3 * time.Hour)
	if err := hs.h.do(bg, func() { hs.h.maintain(bg) }); err != nil {
		t.Fatal(err)
	}
	_ = hs.st.View(bg, func(tx *store.Tx) error {
		if _, ok, _ := tx.Room("attic"); ok {
			t.Error("unused registered room not pruned")
		}
		if _, ok, _ := tx.Room("scotmesh"); !ok {
			t.Error("default room pruned")
		}
		if msgs, _ := tx.LatestMessages("scotmesh", 0, 10); len(msgs) != 0 {
			t.Errorf("messages past retention kept: %v", bodies(msgs))
		}
		return nil
	})

	if p, err := hs.h.Forget(bg, alex); err != nil || p.Claimed || p.Member {
		t.Fatalf("Forget %+v %v", p, err)
	}
	if (&UserError{Text: "x"}).Error() != "x" {
		t.Error("UserError.Error")
	}
}

// Every personal setting has a command anyone can use, and every command
// works from every way in: there is no per-way-in table any more. (The page
// also has buttons for each; see nomadpage.)
func TestSettingsReachableFromEveryWayIn(t *testing.T) {
	hs := start(t)
	hs.identify(alex)
	for _, line := range []string{"/whoami", "/nick Alex", "/forget", "/lxmf off", "/lxmf mine on"} {
		def := lookupCommand(strings.Fields(line)[0][1:])
		if def == nil || def.access != anyone {
			t.Fatalf("%s is missing or restricted", line)
		}
		for _, via := range []Via{ViaRRC, ViaLXMF, ViaPage} {
			if r := hs.cmd(via, alex, "", line); r.Error {
				t.Errorf("%s over %s: %q", line, via, r.Text())
			}
		}
	}
}

// TestHelpComesInPagesThatFitOnePacket: /help fills as many pages as a
// person's rank needs, each within one LXMF packet, so no client has to
// accept a Resource to read it, and every command they can use is listed.
func TestHelpComesInPagesThatFitOnePacket(t *testing.T) {
	hs := start(t)
	hs.identify(rab, ellen, alex, oper)
	hs.setRank(ellen, store.RankMod)
	hs.setRank(alex, store.RankAdmin)
	pagesFor := map[string]int{}
	for _, who := range []struct {
		name string
		id   []byte
	}{{"member", rab}, {"mod", ellen}, {"admin", alex}, {"owner", oper}} {
		listed := map[string]bool{}
		first := hs.cmd(ViaLXMF, who.id, "", "/help").Lines[0]
		var pages int
		if _, err := fmt.Sscanf(first, "Help 1/%d", &pages); err != nil {
			t.Fatalf("%s: first line %q", who.name, first)
		}
		pagesFor[who.name] = pages
		for page := 1; page <= pages; page++ {
			r := hs.cmd(ViaLXMF, who.id, "", fmt.Sprintf("/help %d", page))
			text := r.Text()
			if r.Error || !strings.HasPrefix(text, fmt.Sprintf("Help %d/%d · ", page, pages)) || len(r.Lines) < 3 {
				t.Errorf("%s /help %d: %q", who.name, page, text)
			}
			if len(text) > OnePacketText {
				t.Errorf("%s /help %d is %d bytes, more than one packet (%d):\n%s", who.name, page, len(text), OnePacketText, text)
			}
			for _, l := range r.Lines[1:] {
				if f := strings.Fields(l); len(f) > 0 && strings.HasPrefix(f[0], "/") {
					listed[f[0]] = true
				}
			}
		}
		r := hs.cmd(ViaLXMF, who.id, "", fmt.Sprintf("/help %d", pages+1))
		if !r.Error || !strings.Contains(r.Text(), fmt.Sprintf("There are %d help pages", pages)) {
			t.Errorf("%s /help past the end: %q", who.name, r.Text())
		}
		me := must[Person](t)(hs.h.PersonOf(bg, who.id))
		for _, def := range commandTable() {
			if me.Rank >= def.access.rank() && def.page > 0 && !listed["/"+def.name] {
				t.Errorf("%s: /%s is on no help page", who.name, def.name)
			}
			if me.Rank < def.access.rank() && listed["/"+def.name] {
				t.Errorf("%s: /%s is listed but not theirs to use", who.name, def.name)
			}
		}
	}
	// Members have four pages; each rank has at least as many as the last.
	if pagesFor["member"] != 4 || pagesFor["mod"] <= pagesFor["member"] || pagesFor["admin"] < pagesFor["mod"] || pagesFor["owner"] < pagesFor["admin"] {
		t.Errorf("pages by rank %v", pagesFor)
	}
	if hs.cmd(ViaLXMF, rab, "", "/help").Text() != hs.cmd(ViaLXMF, rab, "", "/help 1").Text() {
		t.Error("/help and /help 1 differ")
	}
	if got := hs.cmd(ViaLXMF, rab, "", "/help !pause").Text(); got != "/pause — the same as /lxmf off" {
		t.Errorf("/help !pause = %q", got)
	}
	if r := hs.cmd(ViaLXMF, rab, "", "/help 1 2"); !r.Error {
		t.Errorf("/help with two arguments: %q", r.Text())
	}
	// Every command is in a section, is an alias, or is one of rrcd's forms.
	for _, def := range commandTable() {
		legacy := strings.HasPrefix(def.help, "rrcd's form")
		if def.name != "help" && def.expand == "" && !legacy && (def.page < 1 || def.page >= len(helpSections)) {
			t.Errorf("/%s is in help section %d", def.name, def.page)
		}
		if def.help == "" {
			t.Errorf("/%s has no /help text", def.name)
		}
	}
}

// The wiki's Chat commands page comes from the table: every command is on
// it, and no cell's text breaks the table.
func TestHelpWiki(t *testing.T) {
	page := HelpWiki()
	for _, def := range commandTable() {
		if def.expand != "" || strings.HasPrefix(def.help, "rrcd's form") {
			continue
		}
		if !strings.Contains(page, "<code>/"+def.name) {
			t.Errorf("/%s is not on the wiki page", def.name)
		}
	}
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "| <code>") && strings.Count(line, "||") != 2 {
			t.Errorf("a row with the wrong number of cells: %s", line)
		}
	}
	if !strings.Contains(page, "<code>/pause</code> is <code>/lxmf off</code>") || strings.Count(page, "{| class=\"wikitable\"") != 7 {
		t.Errorf("wiki page:\n%s", page)
	}
}
