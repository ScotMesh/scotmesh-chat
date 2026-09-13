package lxmf

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SPEC §5.7.3 — tickets, the pre-shared shortcut around proof-of-work.
//
// A recipient hands a known correspondent a 16-byte secret; messages
// carrying a stamp derived from it skip the grind entirely. For anything
// that sends per message PER RECIPIENT — a relay, a group-chat forwarder
// — that is the difference between a cost that scales with the roster
// and one hash.
const (
	// FieldTicket is the LXMF fields key a ticket grant travels under
	// (LXMF/LXMF.py FIELD_TICKET).
	FieldTicket = 0x0C

	// TicketLength is TICKET_LENGTH = TRUNCATED_HASHLENGTH//8.
	TicketLength = 16

	// TicketStampSize is the length of a ticket-derived stamp.
	//
	// It is 16 bytes, NOT StampSize. Upstream builds it with
	// RNS.Identity.truncated_hash (LXMessage.py:299), which truncates to
	// TRUNCATED_HASHLENGTH = 128 bits — half a proof-of-work stamp.
	// SPEC §5.7.3 writes the formula as `SHA256(ticket || message_id)[:32]
	// # truncated to STAMP_SIZE`, which is wrong; verified against
	// rns 1.5.0 / lxmf 1.1.1. A receiver expecting 32 bytes rejects every
	// ticket-stamped message, and a sender emitting 32 is rejected by
	// upstream, so the two wrong implementations do not even agree.
	TicketStampSize = 16

	// CostTicket is the sentinel stamp_value meaning "valid by ticket"
	// rather than a real leading-zero count (LXMessage.COST_TICKET).
	CostTicket = 256
)

// TicketStamp derives the §5.7.3 stamp for a ticket:
// SHA-256(ticket || message_id) truncated to TicketStampSize.
func TicketStamp(ticket, messageID []byte) ([]byte, error) {
	if len(ticket) != TicketLength {
		return nil, fmt.Errorf("ticket must be %d bytes, got %d", TicketLength, len(ticket))
	}
	if len(messageID) != 32 {
		return nil, fmt.Errorf("message_id must be 32 bytes, got %d", len(messageID))
	}
	sum := sha256.Sum256(append(append([]byte(nil), ticket...), messageID...))
	return sum[:TicketStampSize], nil
}

// NewTicket generates a fresh 16-byte ticket.
func NewTicket() ([]byte, error) {
	t := make([]byte, TicketLength)
	if _, err := rand.Read(t); err != nil {
		return nil, fmt.Errorf("generate ticket: %w", err)
	}
	return t, nil
}

type ticketEntry struct {
	ticket  []byte
	expires time.Time
}

// TicketStore holds both directions of the §5.7.3 relationship, which
// are NOT interchangeable:
//
//   - issued: tickets WE handed to a peer. They stamp with these; we
//     validate inbound stamps against them.
//   - held: tickets a peer handed US. We stamp outbound to them with
//     these instead of grinding.
//
// Both are keyed by the peer's LXMF destination hash and expire at an
// absolute unix time carried on the wire.
type TicketStore struct {
	mu     sync.Mutex
	issued map[string][]ticketEntry
	held   map[string]ticketEntry
}

// NewTicketStore returns an empty in-memory store.
func NewTicketStore() *TicketStore {
	return &TicketStore{
		issued: map[string][]ticketEntry{},
		held:   map[string]ticketEntry{},
	}
}

// MaxIssuedTicketsPerPeer bounds how many live tickets we keep per peer
// for inbound validation. A peer may still be using the previous ticket
// when a new one is issued, so more than one must be accepted — but the
// list is walked on every inbound stamp, and it is fed by our own
// issuance, so it needs a ceiling regardless.
const MaxIssuedTicketsPerPeer = 4

// RememberIssued records a ticket we granted to peerHash.
func (s *TicketStore) RememberIssued(peerHash, ticket []byte, expires time.Time) {
	if len(ticket) != TicketLength {
		return
	}
	key := hex.EncodeToString(peerHash)
	s.mu.Lock()
	defer s.mu.Unlock()
	list := append(s.issued[key], ticketEntry{ticket: append([]byte(nil), ticket...), expires: expires})
	if len(list) > MaxIssuedTicketsPerPeer {
		list = list[len(list)-MaxIssuedTicketsPerPeer:]
	}
	s.issued[key] = list
}

// RememberHeld records a ticket peerHash granted to US. A later grant
// replaces an earlier one — upstream keeps the newest per peer.
func (s *TicketStore) RememberHeld(peerHash, ticket []byte, expires time.Time) {
	if len(ticket) != TicketLength {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[hex.EncodeToString(peerHash)] = ticketEntry{
		ticket: append([]byte(nil), ticket...), expires: expires,
	}
}

// Held returns a live ticket peerHash gave us, or nil. An expired
// ticket is evicted and reported as absent, so the caller falls back to
// proof-of-work exactly as §5.7.3 requires.
func (s *TicketStore) Held(peerHash []byte, now time.Time) []byte {
	key := hex.EncodeToString(peerHash)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.held[key]
	if !ok {
		return nil
	}
	if !now.Before(e.expires) {
		delete(s.held, key)
		return nil
	}
	return append([]byte(nil), e.ticket...)
}

// Issued returns the live tickets we granted peerHash, evicting expired
// ones.
func (s *TicketStore) Issued(peerHash []byte, now time.Time) [][]byte {
	key := hex.EncodeToString(peerHash)
	s.mu.Lock()
	defer s.mu.Unlock()
	var live []ticketEntry
	var out [][]byte
	for _, e := range s.issued[key] {
		if now.Before(e.expires) {
			live = append(live, e)
			out = append(out, append([]byte(nil), e.ticket...))
		}
	}
	if len(live) == 0 {
		delete(s.issued, key)
	} else {
		s.issued[key] = live
	}
	return out
}

// TicketGrantField builds the fields[0x0C] value that grants a ticket:
// [expires_unix_seconds(int), ticket(bytes,16)].
//
// §15 caveat: the expiry is an ABSOLUTE unix time. A device without a
// wall clock must not issue tickets at all — a boot-relative expiry
// looks already-expired or absurdly distant to the recipient, and there
// is no way for them to tell which.
func TicketGrantField(ticket []byte, expires time.Time) ([]any, error) {
	if len(ticket) != TicketLength {
		return nil, fmt.Errorf("ticket must be %d bytes, got %d", TicketLength, len(ticket))
	}
	return []any{expires.Unix(), ticket}, nil
}

// ParseTicketGrant reads a fields[0x0C] value. Returns ok=false when the
// field is absent or malformed — a peer's broken grant is not our error
// to raise, we simply keep grinding.
func ParseTicketGrant(v any) (ticket []byte, expires time.Time, ok bool) {
	list, isList := v.([]any)
	if !isList || len(list) < 2 {
		return nil, time.Time{}, false
	}
	var secs int64
	switch n := list[0].(type) {
	case int64:
		secs = n
	case int32:
		secs = int64(n)
	case int16:
		secs = int64(n)
	case int8:
		secs = int64(n)
	case int:
		secs = int64(n)
	case uint64:
		secs = int64(n)
	case uint32:
		secs = int64(n)
	case float64:
		secs = int64(n)
	default:
		return nil, time.Time{}, false
	}
	b, isBytes := list[1].([]byte)
	if !isBytes || len(b) != TicketLength {
		return nil, time.Time{}, false
	}
	return append([]byte(nil), b...), time.Unix(secs, 0), true
}

// rememberInboundTicket stores a §5.7.3 grant carried in an inbound
// message's fields.
//
// Upstream gates this on signature_validated AND an unexpired timestamp
// (LXMRouter.py:1741-1752); both matter. Without the signature check any
// passer-by could grant a ticket in a peer's name and then use it to
// bypass our proof-of-work requirement — the ticket IS the bypass, so
// accepting an unauthenticated one hands out exactly what the stamp
// exists to charge for.
func (d *Delivery) rememberInboundTicket(m *Message) {
	if d.Tickets == nil || m.Fields == nil {
		return
	}
	v, ok := m.Fields[FieldTicket]
	if !ok {
		if v, ok = m.Fields[int64(FieldTicket)]; !ok {
			return
		}
	}
	ticket, expires, ok := ParseTicketGrant(v)
	if !ok {
		d.errorf("malformed ticket grant from %x", m.SourceHash[:4])
		return
	}
	if !time.Now().Before(expires) {
		// Already-expired grants are dropped rather than stored: §15
		// warns that a clockless sender emits exactly these, and keeping
		// one would mean stamping with a ticket the peer will reject.
		d.errorf("ignoring already-expired ticket from %x (expired %s)", m.SourceHash[:4], expires.UTC().Format(time.RFC3339))
		return
	}
	d.Tickets.RememberHeld(m.SourceHash, ticket, expires)
}

// IssueTicket generates a ticket for peerHash, records it for inbound
// validation, and returns the fields[0x0C] value to attach to a message
// addressed to that peer.
//
// Issuing is explicit rather than automatic: it is a decision to exempt
// somebody from the cost we advertise, and it needs a wall clock (§15).
func (d *Delivery) IssueTicket(peerHash []byte, validFor time.Duration) ([]any, error) {
	if d.Tickets == nil {
		return nil, errors.New("no ticket store configured")
	}
	if validFor <= 0 {
		return nil, errors.New("ticket validity must be positive")
	}
	ticket, err := NewTicket()
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(validFor)
	field, err := TicketGrantField(ticket, expires)
	if err != nil {
		return nil, err
	}
	d.Tickets.RememberIssued(peerHash, ticket, expires)
	return field, nil
}
