package lxmf

import (
	"bytes"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

// A ticket stamp is 16 bytes, NOT StampSize. Upstream derives it with
// RNS.Identity.truncated_hash (128 bits); SPEC §5.7.3 writes `[:32]`.
// A 32-byte emitter and a 16-byte verifier never agree, so this pins the
// length and the derivation together.
func TestTicketStampIsTruncatedTo16Bytes(t *testing.T) {
	ticket := bytes.Repeat([]byte{0x01}, TicketLength)
	msgID := bytes.Repeat([]byte{0x02}, 32)

	stamp, err := TicketStamp(ticket, msgID)
	if err != nil {
		t.Fatalf("TicketStamp: %v", err)
	}
	if len(stamp) != TicketStampSize {
		t.Fatalf("ticket stamp is %d bytes, want %d", len(stamp), TicketStampSize)
	}
	if TicketStampSize == StampSize {
		t.Fatal("premise broken: the ticket form is meant to be half a PoW stamp")
	}
	// Value pinned against upstream RNS.Identity.truncated_hash(ticket+message_id)
	// at rns 1.5.0 for exactly these inputs.
	const want = "ac2bca3db969f4464f7b2759d64430ea"
	if got := hexs(stamp); got != want {
		t.Errorf("ticket stamp = %s, upstream truncated_hash gives %s", got, want)
	}
}

func hexs(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&0xf])
	}
	return string(out)
}

func TestTicketStampRejectsBadInputs(t *testing.T) {
	good := bytes.Repeat([]byte{0x01}, TicketLength)
	if _, err := TicketStamp(good[:8], bytes.Repeat([]byte{0x02}, 32)); err == nil {
		t.Error("accepted a short ticket")
	}
	if _, err := TicketStamp(good, bytes.Repeat([]byte{0x02}, 16)); err == nil {
		t.Error("accepted a short message_id")
	}
}

func TestTicketGrantRoundTrip(t *testing.T) {
	ticket, err := NewTicket()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).Truncate(time.Second)

	field, err := TicketGrantField(ticket, expires)
	if err != nil {
		t.Fatal(err)
	}
	got, gotExp, ok := ParseTicketGrant([]any{field[0], field[1]})
	if !ok {
		t.Fatal("round trip did not parse")
	}
	if !bytes.Equal(got, ticket) {
		t.Error("ticket did not survive the round trip")
	}
	if !gotExp.Equal(expires) {
		t.Errorf("expiry = %v, want %v", gotExp, expires)
	}
}

func TestParseTicketGrantRejectsMalformed(t *testing.T) {
	for _, c := range []struct {
		name string
		in   any
	}{
		{"not a list", []byte("x")},
		{"too short", []any{int64(1)}},
		{"expiry not an int", []any{"soon", bytes.Repeat([]byte{1}, TicketLength)}},
		{"ticket wrong length", []any{int64(1), []byte{1, 2, 3}}},
		{"ticket not bytes", []any{int64(1), "ticket"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, ok := ParseTicketGrant(c.in); ok {
				t.Error("accepted a malformed grant")
			}
		})
	}
}

func TestTicketStoreExpiry(t *testing.T) {
	s := NewTicketStore()
	peer := bytes.Repeat([]byte{0xAA}, rns.IdentityHashLen)
	ticket := bytes.Repeat([]byte{0x07}, TicketLength)
	now := time.Now()

	s.RememberHeld(peer, ticket, now.Add(time.Minute))
	if got := s.Held(peer, now); !bytes.Equal(got, ticket) {
		t.Error("a live held ticket was not returned")
	}
	// An expired ticket must read as absent so the caller falls back to
	// proof-of-work rather than stamping with something the peer rejects.
	if got := s.Held(peer, now.Add(2*time.Minute)); got != nil {
		t.Error("an expired held ticket was returned")
	}
	if got := s.Held(peer, now); got != nil {
		t.Error("the expired ticket was not evicted")
	}

	s.RememberIssued(peer, ticket, now.Add(time.Minute))
	if got := s.Issued(peer, now); len(got) != 1 {
		t.Fatalf("issued returned %d tickets, want 1", len(got))
	}
	if got := s.Issued(peer, now.Add(2*time.Minute)); len(got) != 0 {
		t.Error("expired issued tickets were returned")
	}
}

// A peer may still be using the previous ticket when a new one is
// issued, so more than one must be live — but the list is walked on
// every inbound stamp and fed by our own issuance, so it is capped.
func TestTicketStoreCapsIssuedPerPeer(t *testing.T) {
	s := NewTicketStore()
	peer := bytes.Repeat([]byte{0xBB}, rns.IdentityHashLen)
	now := time.Now()
	for i := 0; i < MaxIssuedTicketsPerPeer*3; i++ {
		s.RememberIssued(peer, bytes.Repeat([]byte{byte(i)}, TicketLength), now.Add(time.Hour))
	}
	if got := len(s.Issued(peer, now)); got != MaxIssuedTicketsPerPeer {
		t.Errorf("kept %d issued tickets, want the cap of %d", got, MaxIssuedTicketsPerPeer)
	}
	// The most recent must survive; the oldest must not.
	live := s.Issued(peer, now)
	newest := bytes.Repeat([]byte{byte(MaxIssuedTicketsPerPeer*3 - 1)}, TicketLength)
	found := false
	for _, l := range live {
		if bytes.Equal(l, newest) {
			found = true
		}
	}
	if !found {
		t.Error("the newest issued ticket was evicted")
	}
}

// End to end: bob grants alice a ticket, alice's next message to bob is
// stamped with it instead of ground, and bob validates it by ticket —
// scoring the COST_TICKET sentinel rather than a leading-zero count.
func TestTicketShortcutEndToEnd(t *testing.T) {
	f := newStampFixture(t, testStampCost)
	f.delA.Tickets = NewTicketStore()
	f.delB.Tickets = NewTicketStore()
	f.delB.InboundStampCost = testStampCost
	f.delB.EnforceStamps = true

	aliceDest := f.alice.DestinationHashFor(FullName())

	// Bob issues alice a ticket and remembers it for validation.
	grant, err := f.delB.IssueTicket(aliceDest, time.Hour)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	ticket, expires, ok := ParseTicketGrant(grant)
	if !ok {
		t.Fatal("issued grant does not parse")
	}
	// Alice receives it the way a real grant arrives: in a verified
	// message's fields.
	f.delA.Tickets.RememberHeld(f.bob.DestinationHashFor(FullName()), ticket, expires)

	// Alice sends. The stamp must be the ticket form, not a grind.
	opts := f.delA.stampOptionsForPeer(nil, f.delB.Hash())
	if !bytes.Equal(opts.Ticket, ticket) {
		t.Fatal("outbound options did not pick up the held ticket")
	}
	body, msgID, err := SignAndPackOpportunisticStamped(f.alice, aliceDest, f.delB.Hash(),
		nil, []byte("free delivery"), nil, opts)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	m, err := ParseOpportunisticBody(body, f.delB.Hash())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Stamp) != TicketStampSize {
		t.Fatalf("stamp is %d bytes, want the %d-byte ticket form", len(m.Stamp), TicketStampSize)
	}
	want, err := TicketStamp(ticket, msgID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.Stamp, want) {
		t.Error("stamp is not SHA256(ticket || message_id)[:16]")
	}

	// Bob validates it by ticket, without grinding a workblock.
	if !f.delB.validateInboundStamp(m) {
		t.Fatal("bob dropped a ticket-stamped message while enforcing")
	}
	if !m.StampValid {
		t.Error("ticket stamp not marked valid")
	}
	if m.StampValue != CostTicket {
		t.Errorf("StampValue = %d, want the CostTicket sentinel %d", m.StampValue, CostTicket)
	}
}

// A ticket we never issued must not validate anything — otherwise the
// shortcut is a bypass anyone can use.
func TestUnknownTicketDoesNotValidate(t *testing.T) {
	f := newStampFixture(t, testStampCost)
	f.delB.Tickets = NewTicketStore()
	f.delB.InboundStampCost = testStampCost
	f.delB.EnforceStamps = true

	forged := bytes.Repeat([]byte{0x09}, TicketLength)
	aliceDest := f.alice.DestinationHashFor(FullName())
	body, _, err := SignAndPackOpportunisticStamped(f.alice, aliceDest, f.delB.Hash(),
		nil, []byte("forged"), nil, StampOptions{Ticket: forged})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseOpportunisticBody(body, f.delB.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if f.delB.validateInboundStamp(m) {
		t.Error("a stamp from a ticket we never issued was accepted while enforcing")
	}
	if m.StampValid {
		t.Error("forged ticket stamp marked valid")
	}
}

// An expired grant is not stored: §15 warns a clockless sender emits
// exactly these, and keeping one means stamping with something the peer
// will reject.
func TestExpiredGrantIsNotStored(t *testing.T) {
	f := newStampFixture(t, 0)
	f.delA.Tickets = NewTicketStore()

	peer := bytes.Repeat([]byte{0xCC}, rns.IdentityHashLen)
	m := &Message{
		SourceHash: peer,
		Fields:     map[any]any{FieldTicket: []any{int64(time.Now().Add(-time.Hour).Unix()), bytes.Repeat([]byte{1}, TicketLength)}},
	}
	f.delA.rememberInboundTicket(m)
	if got := f.delA.Tickets.Held(peer, time.Now()); got != nil {
		t.Error("an already-expired ticket grant was stored")
	}
}

func TestInboundTicketGrantIsRemembered(t *testing.T) {
	f := newStampFixture(t, 0)
	f.delA.Tickets = NewTicketStore()

	peer := bytes.Repeat([]byte{0xDD}, rns.IdentityHashLen)
	ticket := bytes.Repeat([]byte{0x11}, TicketLength)
	expires := time.Now().Add(time.Hour)
	m := &Message{
		SourceHash: peer,
		Fields:     map[any]any{int64(FieldTicket): []any{int64(expires.Unix()), ticket}},
	}
	f.delA.rememberInboundTicket(m)
	if got := f.delA.Tickets.Held(peer, time.Now()); !bytes.Equal(got, ticket) {
		t.Error("a live ticket grant was not remembered (int64-keyed fields map)")
	}
}
