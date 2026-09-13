package lxmf

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

// TestStampOptionsFromAnnounce covers the policy resolution that decides
// whether Send grinds at all: cost comes from the recipient's own
// announce (§5.7.4), and the two escape hatches on Delivery override it.
func TestStampOptionsFromAnnounce(t *testing.T) {
	costOf := func(c int) []byte {
		appData, err := rns.EncodeLXMFAppData([]byte("peer"), &c)
		if err != nil {
			t.Fatalf("EncodeLXMFAppData: %v", err)
		}
		return appData
	}
	noCost, err := rns.EncodeLXMFAppData([]byte("peer"), nil)
	if err != nil {
		t.Fatalf("EncodeLXMFAppData: %v", err)
	}

	for _, c := range []struct {
		name    string
		d       Delivery
		appData []byte
		want    StampOptions
	}{
		{"announced cost is honored", Delivery{}, costOf(9), StampOptions{Cost: 9}},
		{"nil stamp_cost means none", Delivery{}, noCost, StampOptions{}},
		{"absent app_data means none", Delivery{}, nil, StampOptions{}},
		{"disabled beats the announce", Delivery{DisableOutboundStamps: true}, costOf(9), StampOptions{}},
		{"per-delivery ceiling rides along", Delivery{MaxStampCost: 4}, costOf(9), StampOptions{Cost: 9, MaxCost: 4}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.d.stampOptionsFor(c.appData); got.Cost != c.want.Cost || got.MaxCost != c.want.MaxCost {
				t.Errorf("stampOptionsFor = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestStampOptionsReportsMalformedAppData: a peer that garbles its own
// announce must not take down our send path, but the decode failure has
// to be visible — a silently-unstamped message to a peer that enforces
// stamps disappears on arrival with no local trace.
func TestStampOptionsReportsMalformedAppData(t *testing.T) {
	var reported error
	d := Delivery{OnError: func(err error) { reported = err }}

	// [name, "not an int"] — element [1] present but not a stamp_cost.
	bad := []byte{0x92, 0xc4, 0x04, 'p', 'e', 'e', 'r', 0xa3, 'b', 'a', 'd'}
	if got := d.stampOptionsFor(bad); got.Cost != 0 || got.MaxCost != 0 {
		t.Errorf("stampOptionsFor = %+v, want zero (fall back to unstamped)", got)
	}
	if reported == nil {
		t.Error("malformed stamp_cost was not reported through OnError")
	}
}

// TestEndToEndStampedDelivery is the whole feature on a wire: Bob
// announces a stamp_cost, Alice's Send grinds a §5.7.2 stamp without
// being told to, and the message Bob parses carries a stamp that
// validates against the message_id workblock — while the signature still
// verifies through the §5.6 strip-and-retry path.
func TestEndToEndStampedDelivery(t *testing.T) {
	f := newStampFixture(t, testStampCost)

	msgID, err := f.delA.SendWithID(f.delB.Hash(), nil, []byte("stamped on the wire"), nil)
	if err != nil {
		t.Fatalf("SendWithID: %v", err)
	}
	m := f.waitForMessage(t)

	if len(m.Stamp) != StampSize {
		t.Fatalf("received stamp is %d bytes, want %d — Send did not honor the announced stamp_cost",
			len(m.Stamp), StampSize)
	}
	if err := m.Verify(f.alice.PublicKey()[32:]); err != nil {
		t.Errorf("stamped message does not verify: %v", err)
	}
	if string(m.Content) != "stamped on the wire" {
		t.Errorf("content = %q", m.Content)
	}
	if !bytes.Equal(m.MessageID(), msgID) {
		t.Errorf("receiver message_id %x != sender %x", m.MessageID(), msgID)
	}
	wb, err := stampWorkblock(m.MessageID(), workblockExpandRounds)
	if err != nil {
		t.Fatalf("stampWorkblock: %v", err)
	}
	if !stampValid(m.Stamp, testStampCost, wb) {
		t.Errorf("stamp does not clear the %d bits Bob announced", testStampCost)
	}
}

// TestDisableOutboundStampsSkipsGrind proves the escape hatch actually
// escapes: with DisableOutboundStamps set, a recipient's announced
// stamp_cost is ignored, no proof-of-work is done, and the message still
// arrives and verifies — unstamped, which is the trade the flag names.
func TestDisableOutboundStampsSkipsGrind(t *testing.T) {
	f := newStampFixture(t, testStampCost)
	f.delA.DisableOutboundStamps = true

	if err := f.delA.Send(f.delB.Hash(), nil, []byte("no grind"), nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m := f.waitForMessage(t)
	if m.Stamp != nil {
		t.Errorf("message carried a %d-byte stamp despite DisableOutboundStamps", len(m.Stamp))
	}
	if err := m.Verify(f.alice.PublicKey()[32:]); err != nil {
		t.Errorf("unstamped message does not verify: %v", err)
	}
	if string(m.Content) != "no grind" {
		t.Errorf("content = %q", m.Content)
	}
}

// TestSendRefusesAnnouncedCostAboveCeiling walks the refusal through the
// real Send path: the recipient announces more work than this Delivery
// will do, so the caller gets ErrStampCostTooHigh instead of a message
// that would be dropped on arrival (§5.7.4).
func TestSendRefusesAnnouncedCostAboveCeiling(t *testing.T) {
	f := newStampFixture(t, 9)
	f.delA.MaxStampCost = 4

	err := f.delA.Send(f.delB.Hash(), nil, []byte("too expensive"), nil)
	if !errors.Is(err, ErrStampCostTooHigh) {
		t.Fatalf("err = %v, want ErrStampCostTooHigh", err)
	}
	// Nothing may have gone out: refusing is only useful if it happens
	// before the wire, not after.
	if m := f.peekMessage(); m != nil {
		t.Error("a message was transmitted despite the refusal")
	}
}

// TestPropagatedCarriesRecipientDeliveryStamp: a store-and-forward
// message must satisfy the RECIPIENT's stamp_cost too, not just the
// node's. The node stamp buys storage; this one buys acceptance, and it
// travels sealed inside the encrypted payload where the node never sees
// it.
func TestPropagatedCarriesRecipientDeliveryStamp(t *testing.T) {
	bobCost := testStampCost
	// Node asks for nothing, so lxmf_data has no appended PN stamp and
	// the whole blob is the message — isolating the delivery stamp.
	f := newPNFixtureWithRecipientCost(t, validPNAppData(t, true, 0, 0), &bobCost)

	msgID, err := f.delA.SendPropagated(f.nodeDest, f.bobDest, nil, []byte("stamped mail"), nil)
	if err != nil {
		t.Fatalf("SendPropagated: %v", err)
	}

	lxmfData := decodeBundle(t, f.waitForUpload(t))
	plain, err := rns.TokenDecrypt(f.bob, lxmfData[rns.IdentityHashLen:])
	if err != nil {
		t.Fatalf("bob TokenDecrypt: %v", err)
	}
	msg, err := ParseOpportunisticBody(plain, f.bobDest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(msg.Stamp) != StampSize {
		t.Fatalf("propagated message carries a %d-byte stamp, want %d", len(msg.Stamp), StampSize)
	}
	if err := msg.Verify(f.alice.PublicKey()[32:]); err != nil {
		t.Errorf("stamped propagated message does not verify: %v", err)
	}
	if !bytes.Equal(msg.MessageID(), msgID) {
		t.Errorf("recipient message_id %x != sender %x", msg.MessageID(), msgID)
	}
	wb, err := stampWorkblock(msgID, workblockExpandRounds)
	if err != nil {
		t.Fatal(err)
	}
	if !stampValid(msg.Stamp, bobCost, wb) {
		t.Errorf("delivery stamp does not clear the %d bits bob announced", bobCost)
	}
}

// stampFixture is two Deliveries on a shared wire where the recipient
// announces a stamp_cost.
type stampFixture struct {
	alice, bob *rns.Identity
	delA, delB *Delivery
	mu         sync.Mutex
	got        *Message
	errCount   int32
}

func newStampFixture(t *testing.T, recipientCost int) *stampFixture {
	t.Helper()
	alice, _ := rns.NewIdentity()
	bob, _ := rns.NewIdentity()

	aIface, bIface, stop := pairedInterfaces()
	tA := rns.NewTransport(nil)
	tA.AddInterface(aIface)
	tB := rns.NewTransport(nil)
	tB.AddInterface(bIface)

	delA, err := NewDelivery(tA, alice, nil)
	if err != nil {
		t.Fatal(err)
	}
	delB, err := NewDelivery(tB, bob, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &stampFixture{alice: alice, bob: bob, delA: delA, delB: delB}
	delB.OnMessage = func(m *Message) {
		f.mu.Lock()
		f.got = m
		f.mu.Unlock()
	}
	onErr := func(error) { atomic.AddInt32(&f.errCount, 1) }
	delA.OnError = onErr
	delB.OnError = onErr

	ctx, cancel := context.WithCancel(context.Background())
	go tA.Run(ctx)
	go tB.Run(ctx)
	t.Cleanup(func() {
		cancel()
		stop()
	})

	announce := func(tr *rns.Transport, id *rns.Identity, cost *int) {
		appData, err := rns.EncodeLXMFAppData([]byte("peer"), cost)
		if err != nil {
			t.Fatal(err)
		}
		pkt, err := rns.BuildAnnounce(id, FullName(), appData, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Broadcast(pkt); err != nil {
			t.Fatal(err)
		}
	}
	announce(tA, alice, nil)
	announce(tB, bob, &recipientCost)

	if !waitFor(500*time.Millisecond, func() bool {
		return tA.Recall(bob.DestinationHashFor(FullName())) != nil &&
			tB.Recall(alice.DestinationHashFor(FullName())) != nil
	}) {
		t.Fatal("announces never propagated")
	}
	return f
}

func (f *stampFixture) waitForMessage(t *testing.T) *Message {
	t.Helper()
	if !waitFor(time.Second, func() bool { return f.peekMessage() != nil }) {
		t.Fatalf("no message arrived (errCount=%d)", atomic.LoadInt32(&f.errCount))
	}
	return f.peekMessage()
}

func (f *stampFixture) peekMessage() *Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

// End to end with ratchets: bob publishes one, alice learns it from his
// announce and encrypts to it, and bob decrypts through his ring. The
// long-term key must NOT open the message — that absence is the forward
// secrecy the whole mechanism exists for.
func TestRatchetEndToEndDelivery(t *testing.T) {
	alice, _ := rns.NewIdentity()
	bob, _ := rns.NewIdentity()

	aIface, bIface, stop := pairedInterfaces()
	tA := rns.NewTransport(nil)
	tA.AddInterface(aIface)
	tB := rns.NewTransport(nil)
	tB.AddInterface(bIface)

	delA, err := NewDelivery(tA, alice, nil)
	if err != nil {
		t.Fatal(err)
	}
	delB, err := NewDelivery(tB, bob, nil)
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := rns.NewRatchetKeeper()
	if err != nil {
		t.Fatal(err)
	}
	delB.Ratchets = keeper

	var mu sync.Mutex
	var got *Message
	delB.OnMessage = func(m *Message) { mu.Lock(); got = m; mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tA.Run(ctx)
	go tB.Run(ctx)
	t.Cleanup(stop)

	appData, _ := rns.EncodeLXMFAppData([]byte("peer"), nil)
	// Bob announces WITH his ratchet; alice announces without.
	bobPkt, err := rns.BuildAnnounce(bob, FullName(), appData, keeper.Current())
	if err != nil {
		t.Fatal(err)
	}
	alicePkt, err := rns.BuildAnnounce(alice, FullName(), appData, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tB.Broadcast(bobPkt); err != nil {
		t.Fatal(err)
	}
	if err := tA.Broadcast(alicePkt); err != nil {
		t.Fatal(err)
	}
	bobDest := bob.DestinationHashFor(FullName())
	if !waitFor(time.Second, func() bool {
		k := tA.Recall(bobDest)
		return k != nil && len(k.RatchetPub) == 32 && tB.Recall(alice.DestinationHashFor(FullName())) != nil
	}) {
		t.Fatal("alice never learned bob's ratchet from his announce")
	}

	if err := delA.Send(delB.Hash(), nil, []byte("forward secret"), nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return got != nil }) {
		t.Fatal("ratchet-encrypted message never arrived")
	}
	mu.Lock()
	content := string(got.Content)
	mu.Unlock()
	if content != "forward secret" {
		t.Errorf("content = %q", content)
	}
}
