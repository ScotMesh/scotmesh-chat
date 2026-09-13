package lxmf

import (
	"sync/atomic"
	"testing"
	"time"
)

// The link path must apply the same §5.7.4 policy as the opportunistic
// one. It did not until v0.6.1: handleInboundLinkPlaintext went straight
// from Verify to dispatch, so InboundStampCost, EnforceStamps and
// Tickets covered single-packet messages only — and Send routes anything
// over MaxOpportunisticPayload to a Link automatically. A sender
// bypassed stamp enforcement entirely by sending 296 bytes.
//
// This mirrors TestInboundStampPolicy case for case, over the link form.
func TestInboundStampPolicyOverLink(t *testing.T) {
	for _, c := range []struct {
		name        string
		cost        int
		enforce     bool
		stampCost   int
		wantDeliver bool
		wantChecked bool
		wantValid   bool
	}{
		{"no cost required, stamp ignored", 0, false, testStampCost, true, false, false},
		{"cost required and paid", testStampCost, false, testStampCost, true, true, true},
		{"cost required, unstamped, tolerated", testStampCost, false, 0, true, true, false},
		{"cost required, unstamped, enforced", testStampCost, true, 0, false, true, false},
		{"cost required and paid, enforced", testStampCost, true, testStampCost, true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStampFixture(t, 0)
			f.delB.InboundStampCost = c.cost
			f.delB.EnforceStamps = c.enforce

			// The link form carries dest_hash INSIDE the signed body, so
			// it must be addressed to Bob or his replay guard refuses it
			// before any stamp logic runs.
			body, _, err := SignAndPackDirectStamped(f.alice,
				f.alice.DestinationHashFor(FullName()), f.delB.Hash(),
				nil, []byte("link policy"), nil, StampOptions{Cost: c.stampCost})
			if err != nil {
				t.Fatal(err)
			}
			if known := f.delB.transport.Recall(f.alice.DestinationHashFor(FullName())); known == nil {
				t.Fatal("bob has not learned alice")
			}

			f.delB.handleInboundLinkPlaintext(body)

			delivered := waitFor(2*time.Second, func() bool { return f.peekMessage() != nil })
			if c.wantDeliver && !delivered {
				t.Fatalf("link message was dropped (errCount=%d)", atomic.LoadInt32(&f.errCount))
			}
			if !c.wantDeliver {
				if delivered {
					t.Error("link message was delivered despite failing enforcement — " +
						"stamp enforcement is bypassable by opening a Link")
				}
				return
			}
			m := f.peekMessage()
			if m.StampChecked != c.wantChecked {
				t.Errorf("StampChecked = %t, want %t", m.StampChecked, c.wantChecked)
			}
			if m.StampValid != c.wantValid {
				t.Errorf("StampValid = %t, want %t", m.StampValid, c.wantValid)
			}
			if c.wantValid && m.StampValue < c.cost {
				t.Errorf("StampValue = %d, below the required %d", m.StampValue, c.cost)
			}
		})
	}
}

// §5.7.3 over the link path: a stamp derived from a ticket we issued is
// accepted regardless of cost, and scores the CostTicket sentinel. This
// is the other half of what handleInboundLinkPlaintext was skipping —
// without rememberInboundTicket the link path never captured an inbound
// ticket either.
func TestLinkPathAcceptsTicketStamp(t *testing.T) {
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
	f.delA.Tickets.RememberHeld(f.bob.DestinationHashFor(FullName()), ticket, expires)

	// Alice holds the ticket and uses it instead of grinding.
	opts := f.delA.stampOptionsForPeer(nil, f.delB.Hash())
	body, _, err := SignAndPackDirectStamped(f.alice, aliceDest, f.delB.Hash(),
		nil, []byte("ticketed over link"), nil, opts)
	if err != nil {
		t.Fatalf("pack with ticket: %v", err)
	}

	f.delB.handleInboundLinkPlaintext(body)

	if !waitFor(2*time.Second, func() bool { return f.peekMessage() != nil }) {
		t.Fatalf("ticket-stamped link message dropped (errCount=%d)", atomic.LoadInt32(&f.errCount))
	}
	m := f.peekMessage()
	if !m.StampChecked || !m.StampValid {
		t.Errorf("StampChecked=%t StampValid=%t, want both true", m.StampChecked, m.StampValid)
	}
	if m.StampValue != CostTicket {
		t.Errorf("StampValue = %d, want the CostTicket sentinel %d", m.StampValue, CostTicket)
	}
}
