package lxmf

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

// stampedMessage builds a message carrying a genuine stamp at `cost`,
// parsed back the way a receiver sees it.
func stampedMessage(t *testing.T, cost int) (*Message, *rns.Identity) {
	t.Helper()
	sender, _ := rns.NewIdentity()
	senderDest := sender.DestinationHashFor(FullName())
	destHash := bytes.Repeat([]byte{0x21}, rns.IdentityHashLen)

	body, _, err := SignAndPackOpportunisticStamped(sender, senderDest, destHash,
		nil, []byte("stamped"), nil, StampOptions{Cost: cost})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	m, err := ParseOpportunisticBody(body, destHash)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m, sender
}

// A genuine stamp validates at its own cost, and the reported value is
// the ACTUAL leading-zero count — which may exceed what was required.
// §5.7.2 step 3 exposes that surplus for prioritisation, so reporting
// the required cost instead of the achieved one would erase the signal.
func TestValidateStampAcceptsGenuineStamp(t *testing.T) {
	m, _ := stampedMessage(t, testStampCost)

	value, err := m.ValidateStamp(testStampCost)
	if err != nil {
		t.Fatalf("genuine stamp rejected: %v", err)
	}
	if value < testStampCost {
		t.Errorf("stamp_value = %d, below the %d it cleared", value, testStampCost)
	}
	// Independently: the value must equal the digest's leading zeros.
	wb, err := stampWorkblock(m.MessageID(), workblockExpandRounds)
	if err != nil {
		t.Fatal(err)
	}
	if want := leadingZeroBits(stampDigest(wb, m.Stamp)); value != want {
		t.Errorf("stamp_value = %d, digest has %d leading zero bits", value, want)
	}
}

func TestValidateStampRejects(t *testing.T) {
	m, _ := stampedMessage(t, testStampCost)

	t.Run("cost above what was paid", func(t *testing.T) {
		// Ask for far more than the sender ground for.
		if _, err := m.ValidateStamp(m.StampValue + 40); !errors.Is(err, ErrStampInvalid) {
			t.Errorf("err = %v, want ErrStampInvalid", err)
		}
	})
	t.Run("tampered stamp", func(t *testing.T) {
		// A random 32-byte value clears a 6-bit target about 1 time in
		// 64, so asserting on a single flipped byte would be flaky at
		// that rate. Walk to a tampered value that genuinely misses the
		// bar (expected one step) and assert THAT is rejected — the
		// property under test is that the digest depends on the stamp
		// bytes, not that any particular mutation fails.
		wb, err := stampWorkblock(m.MessageID(), workblockExpandRounds)
		if err != nil {
			t.Fatal(err)
		}
		tampered := *m
		tampered.Stamp = append([]byte(nil), m.Stamp...)
		tampered.Stamp[0] ^= 0xFF
		for i := 0; stampValid(tampered.Stamp, testStampCost, wb); i++ {
			if i > 64 {
				t.Fatal("could not find a tampered stamp below the target")
			}
			tampered.Stamp[1]++
		}
		if _, err := tampered.ValidateStamp(testStampCost); !errors.Is(err, ErrStampInvalid) {
			t.Errorf("err = %v, want ErrStampInvalid", err)
		}
	})
	t.Run("no stamp at all", func(t *testing.T) {
		sender, _ := rns.NewIdentity()
		senderDest := sender.DestinationHashFor(FullName())
		destHash := bytes.Repeat([]byte{0x22}, rns.IdentityHashLen)
		body, _, err := SignAndPackOpportunistic(sender, senderDest, destHash, nil, []byte("bare"), nil)
		if err != nil {
			t.Fatal(err)
		}
		bare, err := ParseOpportunisticBody(body, destHash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bare.ValidateStamp(testStampCost); !errors.Is(err, ErrStampInvalid) {
			t.Errorf("err = %v, want ErrStampInvalid", err)
		}
	})
	t.Run("cost zero checks nothing", func(t *testing.T) {
		if _, err := m.ValidateStamp(0); err != nil {
			t.Errorf("cost 0 should be a no-op, got %v", err)
		}
	})
}

// The workblock material is the message_id over the FOUR-element
// payload (§5.5). Hashing the 5-element wire form derives a workblock
// the sender never used, so every genuine stamp would be rejected —
// and a self-round-trip would not notice, because both sides would be
// wrong together.
//
// The negative half is statistical, for the same reason as the
// tamper case above: SHA256(wbBad || stamp) is uniform, so a stamp
// ground against the stripped id still clears testStampCost bits
// against the wrong workblock once every 2^cost messages — 1 in 64
// here, which flaked CI twice. A single coincidence proves nothing,
// so draw a fresh message and look again. A real regression, where
// validation derives the id from the 5-element payload, collides on
// EVERY attempt rather than one in sixty-four.
func TestValidateStampUsesTheStrippedMessageID(t *testing.T) {
	const attempts = 8 // spurious failure rate 64^-8
	for i := 0; i < attempts; i++ {
		m, _ := stampedMessage(t, testStampCost)

		stripped := m.MessageID()
		wbGood, err := stampWorkblock(stripped, workblockExpandRounds)
		if err != nil {
			t.Fatal(err)
		}
		if !stampValid(m.Stamp, testStampCost, wbGood) {
			t.Fatal("stamp does not validate against the stripped message_id")
		}
		// The 5-element raw payload must NOT produce a working workblock.
		rawID := ComputeMessageID(m.DestHash, m.SourceHash, m.rawPayload)
		if bytes.Equal(rawID, stripped) {
			t.Skip("payload was not stamped; nothing to distinguish")
		}
		wbBad, err := stampWorkblock(rawID, workblockExpandRounds)
		if err != nil {
			t.Fatal(err)
		}
		if !stampValid(m.Stamp, testStampCost, wbBad) {
			return // the two ids are distinguishable, as required
		}
	}
	t.Errorf("stamp validated against the stamp-inclusive id on all %d attempts; "+
		"validation is not using the stripped message_id", attempts)
}

// End to end through the receive path: with a cost set we score every
// inbound stamp, and with EnforceStamps we drop what does not clear it.
func TestInboundStampPolicy(t *testing.T) {
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
			// Drive the sender's grind directly so the case is explicit.
			f.delA.DisableOutboundStamps = c.stampCost == 0
			if c.stampCost > 0 {
				f.delA.MaxStampCost = c.stampCost
			}

			opts := StampOptions{Cost: c.stampCost}
			body, _, err := SignAndPackOpportunisticStamped(f.alice,
				f.alice.DestinationHashFor(FullName()), f.delB.Hash(),
				nil, []byte("policy"), nil, opts)
			if err != nil {
				t.Fatal(err)
			}
			known := f.delB.transport.Recall(f.alice.DestinationHashFor(FullName()))
			if known == nil {
				t.Fatal("bob has not learned alice")
			}
			ct, err := rns.TokenEncrypt(body, f.bob.X25519Public(), identityHashFromPublic(f.bob.PublicKey()))
			if err != nil {
				t.Fatal(err)
			}
			pkt := buildOutboundPacket(f.delB.Hash(), ct, nil)
			raw, err := pkt.Pack()
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := rns.ParsePacket(raw)
			if err != nil {
				t.Fatal(err)
			}
			f.delB.handleInbound(parsed)

			// dispatchInbound hands off to its own goroutine, so the
			// message arrives after handleInbound returns.
			delivered := waitFor(2*time.Second, func() bool { return f.peekMessage() != nil })
			if c.wantDeliver && !delivered {
				t.Fatalf("message was dropped (errCount=%d)", atomic.LoadInt32(&f.errCount))
			}
			if !c.wantDeliver {
				if delivered {
					t.Error("message was delivered despite failing enforcement")
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

// Validation is attacker-triggered 768 KiB work, so it is bounded. Past
// the ceiling a message is delivered UNCHECKED rather than queued —
// blocking would stall the inbound dispatcher.
func TestStampValidationIsBounded(t *testing.T) {
	// Fill every slot, then confirm a validation sheds instead of blocking.
	for i := 0; i < MaxConcurrentStampValidations; i++ {
		stampValidationSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < MaxConcurrentStampValidations; i++ {
			<-stampValidationSlots
		}
	}()

	f := newStampFixture(t, 0)
	f.delB.InboundStampCost = testStampCost
	f.delB.EnforceStamps = true // even enforcing, a shed message is delivered

	m, _ := stampedMessage(t, testStampCost)
	done := make(chan bool, 1)
	go func() { done <- f.delB.validateInboundStamp(m) }()
	select {
	case ok := <-done:
		if !ok {
			t.Error("a shed validation dropped the message; it must be delivered unchecked")
		}
		if m.StampChecked {
			t.Error("StampChecked set on a message whose validation was shed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("validateInboundStamp blocked instead of shedding")
	}
}
