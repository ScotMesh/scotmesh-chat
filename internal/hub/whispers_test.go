package hub

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

func TestWhispersReachOnlyTheirPerson(t *testing.T) {
	hs := start(t)
	phone, laptop := bytes.Repeat([]byte{0x3a}, 16), bytes.Repeat([]byte{0x3b}, 16)
	hs.identify(alex, phone, laptop, ellen)
	hs.claim(alex, "Alex")
	hs.claim(ellen, "Ellen")
	if _, _, err := hs.h.JoinGroup(bg, phone, "Rab"); err != nil {
		t.Fatal(err)
	}
	hs.cmd(ViaRRC, laptop, "", "/link "+hs.issue(ViaLXMF, phone))
	hs.join(laptop, "scotmesh")
	hs.cmd(ViaLXMF, phone, "", "/lxmf off") // whispers still come by LXMF
	hs.events()

	r := hs.cmd(ViaPage, alex, "", "/whisper Rab are you on the hill today?")
	if r.Error || r.Text() != "Whispered to Rab (on RRC now; by LXMF)." {
		t.Fatalf("whisper: %q", r.Text())
	}
	evs := hs.events()
	w := eventsOf[WhisperEvent](evs)
	if len(w) != 1 || len(w[0].To) != 2 || w[0].Whisper.Body != "are you on the hill today?" || w[0].Whisper.FromName != "Alex" || w[0].Whisper.ToName != "Rab" || w[0].Whisper.Via != ViaPage {
		t.Fatalf("whisper events %+v", w)
	}
	if m := eventsOf[MessageEvent](evs); len(m) != 0 {
		t.Errorf("a whisper was said in the room: %+v", m)
	}
	if msgs := must[[]store.Message](t)(hs.h.Recent(bg, "scotmesh", 10, 0)); len(msgs) != 0 {
		t.Errorf("a whisper is in history: %+v", msgs)
	}
	due := must[[]PendingDelivery](t)(hs.h.DueDeliveries(bg, 10))
	if len(due) != 1 || due[0].Whisper == nil || due[0].Whisper.ID != w[0].Whisper.ID || !bytes.Equal(due[0].Identity, phone) || due[0].Text() != "Alex whispers: are you on the hill today?" || due[0].SaidAt() != w[0].Whisper.SaidAt {
		t.Fatalf("due %+v", due)
	}
	if err := hs.h.RecordDelivery(bg, due[0].Delivery, Delivered, false, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := hs.h.WhisperSentRRC(bg, w[0].Whisper.ID); err != nil {
		t.Fatal(err)
	}
	if unsent := must[[]store.Whisper](t)(hs.h.UnsentRRCWhispers(bg, laptop)); len(unsent) != 0 {
		t.Errorf("unsent after sending %+v", unsent)
	}

	// /r replies to the last whisperer, from any of the recipient's apps.
	if r := hs.cmd(ViaRRC, laptop, "scotmesh", "/r aye, from 2pm"); r.Text() != "Whispered to Alex. They'll see it when they're next on." {
		t.Errorf("/r: %q", r.Text())
	}
	if unsent := must[[]store.Whisper](t)(hs.h.UnsentRRCWhispers(bg, alex)); len(unsent) != 1 || unsent[0].FromName != "Rab" {
		t.Errorf("waiting for Alex on RRC: %+v", unsent)
	}
	if r := hs.cmd(ViaRRC, ellen, "", "/r hello?"); !r.Error || r.Text() != "Nobody has whispered to you lately, so there's nobody to reply to." {
		t.Errorf("/r with nobody: %q", r.Text())
	}

	// The page's inbox: newest last, the count of new ones, then read.
	in := must[WhisperInbox](t)(hs.h.Whispers(bg, phone, 10))
	if len(in.Whispers) != 1 || in.Unread != 1 {
		t.Errorf("inbox %+v", in)
	}
	if in := must[WhisperInbox](t)(hs.h.Whispers(bg, laptop, 10)); in.Unread != 0 {
		t.Errorf("still unread after being shown: %+v", in)
	}

	// Refusals: yourself, nobody, too long, whispers off, ignored, banned.
	steps := []struct {
		who         []byte
		text, reply string
	}{
		{alex, "/whisper Alex hi", "You can't whisper to yourself."},
		{alex, "/w Nobody hi", "target 'Nobody' not found"},
		{alex, "/msg Rab", "usage: /whisper <name> <text>"},
		{alex, "/tell Rab " + strings.Repeat("x", 2001), "Not whispered: message too large: 2001 bytes > 2000 bytes"},
	}
	for _, s := range steps {
		if r := hs.cmd(ViaLXMF, s.who, "", s.text); !r.Error || r.Text() != s.reply {
			t.Errorf("%.30s: %q", s.text, r.Text())
		}
	}
	if r := hs.cmd(ViaLXMF, phone, "", "/ignore Alex"); r.Text() != "Ignoring Alex: their whispers won't reach you, and they aren't told. /unignore Alex undoes it." {
		t.Errorf("/ignore %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/w Rab still there?"); !r.Error || r.Text() != "target 'Rab' not found" {
		t.Errorf("whisper to someone ignoring you: %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/ignored"); r.Text() != "Ignoring: Alex." {
		t.Errorf("/ignored %q", r.Text())
	}
	if in := must[WhisperInbox](t)(hs.h.Whispers(bg, laptop, 10)); len(in.Ignored) != 1 || in.Ignored[0].Name != "Alex" || !bytes.Equal(in.Ignored[0].Identity, alex) {
		t.Errorf("ignored for the page %+v", in.Ignored)
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/unignore Alex"); r.Text() != "No longer ignoring Alex." {
		t.Errorf("/unignore %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/unignore Alex"); !r.Error || r.Text() != "You weren't ignoring Alex." {
		t.Errorf("/unignore again %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/ignore Rab"); !r.Error || r.Text() != "You can't ignore yourself." {
		t.Errorf("/ignore yourself %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/ignored"); r.Text() != "You aren't ignoring anyone." {
		t.Errorf("/ignored empty %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/whispers"); r.Text() != "Whispers: on; to your LXMF app: on. 0 whispers unread on the chat page. Change it with /whispers on|off or /whispers lxmf on|off." {
		t.Errorf("/whispers %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/whispers lxmf off"); r.Text() != "Whispers won't come to your LXMF app; you'll see them on RRC and the chat page." {
		t.Errorf("/whispers lxmf off %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/w Rab not by lxmf"); r.Text() != "Whispered to Rab (on RRC now)." {
		t.Errorf("whisper with LXMF off %q", r.Text())
	}
	if r := hs.cmd(ViaRRC, laptop, "", "/whispers off"); r.Text() != "Whispers are off: nobody can whisper to you." {
		t.Errorf("/whispers off %q", r.Text())
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/w Rab hello"); !r.Error || r.Text() != "Rab isn't taking whispers." {
		t.Errorf("whispers off %q", r.Text())
	}
	hs.cmd(ViaRRC, laptop, "", "/whispers on")
	hs.cmd(ViaRRC, laptop, "", "/whispers lxmf on")
	if r := hs.cmd(ViaRRC, laptop, "", "/whispers maybe"); r.Text() != "usage: /whispers [on|off|lxmf on|off]" {
		t.Errorf("usage %q", r.Text())
	}
	hs.cmd(ViaRRC, oper, "", "/ban Ellen")
	if r := hs.cmd(ViaLXMF, alex, "", "/w "+hexID(ellen)+" hi"); !r.Error || r.Text() != "target '"+hexID(ellen)+"' not found" {
		t.Errorf("whisper to a banned person %q", r.Text())
	}

	// Kept 7 days.
	hs.clock.advance(WhisperRetention + time.Hour)
	if err := hs.h.do(bg, func() { hs.h.maintain(bg) }); err != nil {
		t.Fatal(err)
	}
	if in := must[WhisperInbox](t)(hs.h.Whispers(bg, laptop, 10)); len(in.Whispers) != 0 {
		t.Errorf("whispers kept past a week: %+v", in.Whispers)
	}
}

func TestWhisperRateLimit(t *testing.T) {
	hs := start(t, func(c *Config) { c.PostsPerMinute = 2 })
	hs.identify(alex, rab)
	hs.claim(rab, "Rab")
	for range 2 {
		if r := hs.cmd(ViaLXMF, alex, "", "/w Rab hi"); r.Error {
			t.Fatal(r.Text())
		}
	}
	if r := hs.cmd(ViaLXMF, alex, "", "/w Rab hi"); !r.Error || r.Text() != "Not whispered: rate limited" {
		t.Errorf("third whisper %q", r.Text())
	}
}
