package lxmf

import (
	"bytes"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

func senderHash(b byte) []byte {
	return bytes.Repeat([]byte{b}, 16)
}

// A sender starts with full allowance and loses it only by failing.
func TestStampBudgetChargesOnlyFailures(t *testing.T) {
	var b stampBudget
	s := senderHash(0x01)
	now := time.Now()

	if !b.allow(s, now) {
		t.Fatal("a sender with no history was refused")
	}
	// Never charged, however many times we ask.
	for i := 0; i < 100; i++ {
		if !b.allow(s, now) {
			t.Fatalf("allow() alone consumed allowance at call %d", i)
		}
	}
	if n := b.trackedSenders(); n != 0 {
		t.Errorf("%d senders tracked without a single failure, want 0", n)
	}
}

// The allowance runs out after StampFailureBurstPerSender failures, and
// the sender is then refused before any workblock is built.
func TestStampBudgetExhaustsAfterBurst(t *testing.T) {
	var b stampBudget
	s := senderHash(0x02)
	now := time.Now()

	for i := 0; i < StampFailureBurstPerSender; i++ {
		if !b.allow(s, now) {
			t.Fatalf("refused at failure %d, burst is %d", i, StampFailureBurstPerSender)
		}
		b.charge(s, now)
	}
	if b.allow(s, now) {
		t.Errorf("still allowed after %d charged failures", StampFailureBurstPerSender)
	}
	if n := b.trackedSenders(); n != 1 {
		t.Errorf("%d senders tracked, want 1", n)
	}
}

// The budget is per sender: a flooder must not spend anybody else's.
func TestStampBudgetIsPerSender(t *testing.T) {
	var b stampBudget
	flooder, honest := senderHash(0x03), senderHash(0x04)
	now := time.Now()

	for i := 0; i < StampFailureBurstPerSender*3; i++ {
		b.charge(flooder, now)
	}
	if b.allow(flooder, now) {
		t.Error("the flooder kept its allowance")
	}
	if !b.allow(honest, now) {
		t.Error("a flood on one sender consumed another sender's allowance — " +
			"this is the starvation the budget exists to prevent")
	}
	// And the debt does not overflow past the burst, so one refill
	// interval is always enough to get a sender moving again.
	b.mu.Lock()
	spent := b.senders[hexOf(flooder)].spent
	b.mu.Unlock()
	if spent != StampFailureBurstPerSender {
		t.Errorf("debt = %d, want it capped at the burst %d", spent, StampFailureBurstPerSender)
	}
}

// Allowance comes back with time, and a sender out of debt is forgotten
// so the table holds only who is currently throttled.
func TestStampBudgetRefillsAndForgets(t *testing.T) {
	var b stampBudget
	s := senderHash(0x05)
	now := time.Now()

	for i := 0; i < StampFailureBurstPerSender; i++ {
		b.charge(s, now)
	}
	if b.allow(s, now) {
		t.Fatal("allowance not exhausted")
	}
	// One interval buys exactly one unit back.
	if !b.allow(s, now.Add(StampFailureRefillInterval)) {
		t.Error("one refill interval did not restore any allowance")
	}
	// Enough time restores everything, and drops the entry.
	full := now.Add(StampFailureRefillInterval * time.Duration(StampFailureBurstPerSender+1))
	if !b.allow(s, full) {
		t.Error("fully refilled sender still refused")
	}
	if n := b.trackedSenders(); n != 0 {
		t.Errorf("%d senders still tracked after full refill, want 0 — the table does not self-clean", n)
	}
}

// Partial elapsed time must carry over rather than being discarded on
// each charge, or a steady drip never refills at all.
func TestStampBudgetCarriesPartialInterval(t *testing.T) {
	var b stampBudget
	s := senderHash(0x06)
	now := time.Now()

	b.charge(s, now)
	// Three charges each just under a full interval apart. If the
	// refill clock reset on every charge, no credit would ever accrue.
	step := StampFailureRefillInterval*3/4 + time.Millisecond
	at := now
	for i := 0; i < 3; i++ {
		at = at.Add(step)
		b.charge(s, at)
	}
	b.mu.Lock()
	bucket := b.senders[hexOf(s)]
	spent := 0
	if bucket != nil {
		spent = bucket.spent
	}
	b.mu.Unlock()
	// 4 charges over ~2.25 intervals: at least two units refunded.
	if spent >= 4 {
		t.Errorf("debt = %d after 4 charges spanning %v; partial intervals were discarded",
			spent, at.Sub(now))
	}
}

// The table is bounded: a many-identity flood must not grow it without
// limit, and must not evict an existing debtor (which would clear the
// debt of whichever sender the flood targeted).
func TestStampBudgetTableIsBounded(t *testing.T) {
	var b stampBudget
	now := time.Now()

	victim := senderHash(0xFF)
	for i := 0; i < StampFailureBurstPerSender; i++ {
		b.charge(victim, now)
	}

	for i := 0; i < MaxTrackedStampFailureSenders+500; i++ {
		id := make([]byte, 16)
		id[0], id[1], id[2] = byte(i), byte(i>>8), byte(i>>16)
		b.charge(id, now)
	}
	if n := b.trackedSenders(); n > MaxTrackedStampFailureSenders {
		t.Errorf("%d senders tracked, cap is %d", n, MaxTrackedStampFailureSenders)
	}
	if b.allow(victim, now) {
		t.Error("the flood cleared an existing debtor's debt — eviction is exploitable")
	}
}

// End to end: a sender who keeps failing stops costing us workblocks,
// and under EnforceStamps their later messages are dropped without one
// being built. This is the bound the global MaxConcurrentStampValidations
// cap cannot provide — that cap limits concurrency, and with one
// dispatcher per interface the cost accrues serially instead.
func TestInboundStampBudgetStopsRepeatedFailures(t *testing.T) {
	f := newStampFixture(t, 0)
	f.delB.InboundStampCost = testStampCost
	f.delB.EnforceStamps = true

	// One sender throughout — the budget is per source_hash, so a fresh
	// identity per message would never accumulate. A stamp of the right
	// length that does not clear the cost is the expensive case, and
	// free for the sender to produce.
	_, sender := stampedMessage(t, 1)
	bogus := func() *Message {
		m := bogusStamped(t, sender)
		return m
	}

	for i := 0; i < StampFailureBurstPerSender; i++ {
		m := bogus()
		if f.delB.validateInboundStamp(m) {
			t.Fatalf("failure %d was accepted under EnforceStamps", i)
		}
		if !m.StampChecked {
			t.Fatalf("failure %d was not actually validated; the budget refused too early", i)
		}
	}

	// Allowance spent: refused now WITHOUT validating.
	m := bogus()
	if f.delB.validateInboundStamp(m) {
		t.Error("message accepted after the allowance was exhausted")
	}
	if m.StampChecked {
		t.Error("a workblock was still built after the allowance was exhausted — " +
			"the budget must refuse before spending, not after")
	}
}

// A peer whose stamps clear is never throttled, however many it sends.
// This is what makes the budget safe to fail closed on: only failures
// are charged, so it cannot cost an honest sender their messages.
func TestInboundStampBudgetNeverThrottlesValidStamps(t *testing.T) {
	f := newStampFixture(t, 0)
	f.delB.InboundStampCost = testStampCost
	f.delB.EnforceStamps = true

	for i := 0; i < StampFailureBurstPerSender*2; i++ {
		m, _ := stampedMessage(t, testStampCost)
		if !f.delB.validateInboundStamp(m) {
			t.Fatalf("valid stamp %d rejected — the budget charged a success", i)
		}
		if !m.StampValid {
			t.Fatalf("valid stamp %d scored invalid", i)
		}
	}
	if n := f.delB.stampFailures.trackedSenders(); n != 0 {
		t.Errorf("%d senders in debt after only valid stamps, want 0", n)
	}
}

// Without EnforceStamps, an exhausted allowance means "deliver, but
// unvalidated" — the same posture as the rest of §5.7.4's third row.
func TestInboundStampBudgetToleratesWhenNotEnforcing(t *testing.T) {
	f := newStampFixture(t, 0)
	f.delB.InboundStampCost = testStampCost
	f.delB.EnforceStamps = false

	_, sender := stampedMessage(t, 1)
	for i := 0; i < StampFailureBurstPerSender+2; i++ {
		if !f.delB.validateInboundStamp(bogusStamped(t, sender)) {
			t.Fatalf("message %d dropped without EnforceStamps", i)
		}
	}
}

// bogusStamped builds a message from `sender` carrying a right-length
// stamp that cannot clear any cost — the shape that costs a receiver a
// full workblock to reject.
func bogusStamped(t *testing.T, sender *rns.Identity) *Message {
	t.Helper()
	senderDest := sender.DestinationHashFor(FullName())
	destHash := bytes.Repeat([]byte{0x21}, rns.IdentityHashLen)
	body, _, err := SignAndPackOpportunisticStamped(sender, senderDest, destHash,
		nil, []byte("bogus"), nil, StampOptions{Cost: 1})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	m, err := ParseOpportunisticBody(body, destHash)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// A fixed stamp is not automatically a FAILING stamp. The workblock
	// derives from message_id, which is fresh for every message built
	// here, so any given 32 bytes clear testStampCost (6 bits) for
	// roughly one message in 64. Drawing four per run made this test
	// fail ~6% of the time with "failure N was accepted under
	// EnforceStamps" — a real flake, not a real bug.
	//
	// Ask the validator that the test will ask, and perturb until the
	// stamp genuinely does not clear the cost, so "bogus" is a property
	// of the message rather than a probability. Using the real oracle
	// rather than re-deriving the workblock here means this cannot
	// silently drift back into flakiness if the derivation changes.
	for i := 0; ; i++ {
		stamp := bytes.Repeat([]byte{0xAB}, StampSize)
		stamp[0] = byte(i)
		m.Stamp = stamp
		if _, err := m.ValidateStamp(testStampCost); err != nil {
			break
		}
		if i == 32 {
			t.Fatalf("could not build a stamp that fails cost %d in %d tries", testStampCost, i)
		}
	}
	return m
}
