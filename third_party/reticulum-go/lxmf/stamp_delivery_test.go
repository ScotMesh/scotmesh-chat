package lxmf

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/thatSFguy/reticulum-go/rns"
)

// testStampCost is the cost every stamped test grinds at. 6 bits is ~64
// expected SHA-256 attempts on top of a 768 KiB workblock build — enough
// to exercise the real search loop without adding seconds to the suite.
const testStampCost = 6

func TestStampWorkblockShapeDelivery(t *testing.T) {
	// SPEC §5.7.2: WORKBLOCK_EXPAND_ROUNDS = 3000 rounds x 256 bytes =
	// 768 KiB. The size is load-bearing (it is what limits GPU speedup),
	// so pin it rather than trusting the constant.
	wb, err := stampWorkblock([]byte("material"), workblockExpandRounds)
	if err != nil {
		t.Fatalf("stampWorkblock: %v", err)
	}
	if want := 3000 * 256; len(wb) != want {
		t.Errorf("delivery workblock = %d bytes, want %d (768 KiB)", len(wb), want)
	}

	// And it must differ from the propagation workblock over the same
	// material — a stamp ground at the wrong round count validates
	// against nothing.
	pn, err := stampWorkblock([]byte("material"), workblockExpandRoundsPN)
	if err != nil {
		t.Fatalf("stampWorkblock PN: %v", err)
	}
	if bytes.Equal(wb[:len(pn)], pn) == false {
		// The first 1000 rounds ARE identical — expansion is per-round
		// deterministic from the same material — so this is a sanity
		// check on the shared derivation, not a difference assertion.
		t.Error("first 1000 rounds of the delivery workblock differ from the PN workblock")
	}
	if len(wb) == len(pn) {
		t.Error("delivery and propagation workblocks are the same size")
	}
}

func TestGenerateDeliveryStamp(t *testing.T) {
	msgID := bytes.Repeat([]byte{0xA7}, 32)
	stamp, err := GenerateDeliveryStamp(msgID, testStampCost)
	if err != nil {
		t.Fatalf("GenerateDeliveryStamp: %v", err)
	}
	if len(stamp) != StampSize {
		t.Fatalf("stamp length = %d, want %d", len(stamp), StampSize)
	}
	wb, err := stampWorkblock(msgID, workblockExpandRounds)
	if err != nil {
		t.Fatalf("stampWorkblock: %v", err)
	}
	if !stampValid(stamp, testStampCost, wb) {
		t.Errorf("generated stamp does not clear %d bits over the 3000-round workblock", testStampCost)
	}
	// A stamp is only meaningful against ITS workblock: the same bytes
	// checked against the propagation-round workblock must not be
	// assumed valid, which is what makes the round count part of the
	// contract.
	pnWB, err := stampWorkblock(msgID, workblockExpandRoundsPN)
	if err != nil {
		t.Fatalf("stampWorkblock PN: %v", err)
	}
	if stampValid(stamp, 32, pnWB) {
		t.Error("delivery stamp cleared 32 bits against the PN workblock; workblocks are not distinct")
	}
}

func TestGenerateDeliveryStampNoCost(t *testing.T) {
	stamp, err := GenerateDeliveryStamp(bytes.Repeat([]byte{1}, 32), 0)
	if err != nil || stamp != nil {
		t.Errorf("cost 0 should be (nil, nil), got (%x, %v)", stamp, err)
	}
}

func TestGenerateDeliveryStampCostCap(t *testing.T) {
	_, err := GenerateDeliveryStamp(bytes.Repeat([]byte{1}, 32), MaxDeliveryStampCost+1)
	if !errors.Is(err, ErrStampCostTooHigh) {
		t.Errorf("err = %v, want ErrStampCostTooHigh", err)
	}
}

func TestGenerateStampRejectsEmptyMaterial(t *testing.T) {
	if _, err := GenerateDeliveryStamp(nil, 1); err == nil {
		t.Error("empty material accepted")
	}
}

// TestStampedPayloadPreservesSignedBytes is the byte-level guard on the
// splice in appendStamp. Elements [0..3] of the stamped payload must be
// bit-identical to the 4-element payload the signature covers — if they
// are not, the recipient's §5.6 variant-2 check re-encodes different
// bytes than we signed and drops the message.
//
// Note this packs ONCE and splices, rather than packing the same message
// twice with and without a stamp. Two independent packs of identical
// logical content are not byte-identical: `fields` is a Go map and
// msgpack.Marshal emits its keys in map-iteration order, which is
// randomized per run. That is fine on the wire (the receiver verifies
// against the raw bytes it received, and the splice below preserves
// exactly those bytes) but it makes "pack twice, compare" a coin flip.
func TestStampedPayloadPreservesSignedBytes(t *testing.T) {
	sender, _ := rns.NewIdentity()
	senderDest := sender.DestinationHashFor(FullName())
	destHash := bytes.Repeat([]byte{0x44}, rns.IdentityHashLen)
	ts := time.Unix(1750000000, 0)
	// Multi-key, integer-keyed fields — the shape that broke the old
	// decode-and-re-marshal strip (see reencodeFirstFour's comment).
	fields := map[any]any{0x30: bytes.Repeat([]byte{0x11}, 32), 0x06: []byte("img")}

	plain, sig, msgID, err := packSignedAndStamped(sender, senderDest, destHash,
		[]byte("t"), []byte("c"), fields, ts, StampOptions{})
	if err != nil {
		t.Fatalf("pack unstamped: %v", err)
	}
	stamp, err := GenerateDeliveryStamp(msgID, testStampCost)
	if err != nil {
		t.Fatalf("GenerateDeliveryStamp: %v", err)
	}
	stamped, err := appendStamp(plain, stamp)
	if err != nil {
		t.Fatalf("appendStamp: %v", err)
	}

	if plain[0] != 0x94 {
		t.Errorf("unstamped payload header = %#x, want 0x94 (fixarray 4)", plain[0])
	}
	if stamped[0] != 0x95 {
		t.Errorf("stamped payload header = %#x, want 0x95 (fixarray 5)", stamped[0])
	}
	// A stamp costs exactly 34 bytes on the wire: the fixarray header
	// does not change width, plus bin8 (2 bytes) + 32 bytes of stamp.
	if got := len(stamped) - len(plain); got != 34 {
		t.Errorf("stamp overhead = %d bytes, want 34", got)
	}

	stripped, err := reencodeFirstFour(stamped)
	if err != nil {
		t.Fatalf("reencodeFirstFour: %v", err)
	}
	if !bytes.Equal(stripped, plain) {
		t.Errorf("stripped stamped payload != signed payload\n got %x\nwant %x", stripped, plain)
	}
	// The signature over the stripped bytes must still be the one we
	// made over the unstamped payload — that is what §5.6 guarantees.
	if !rns.Validate(sender.PublicKey()[32:], buildSignedData(destHash, senderDest, stripped), sig) {
		t.Error("signature does not verify against the stripped stamped payload")
	}
	// And message_id is unchanged, since it hashes the same four
	// elements (SPEC §5.5).
	if !bytes.Equal(ComputeMessageID(destHash, senderDest, stripped), msgID) {
		t.Error("message_id over the stripped payload differs from the sender's")
	}

	// Element [4] must be a msgpack bin envelope (§5.7.1), not str.
	var elems []msgpack.RawMessage
	if err := msgpack.Unmarshal(stamped, &elems); err != nil {
		t.Fatalf("unmarshal stamped payload: %v", err)
	}
	if len(elems) != 5 {
		t.Fatalf("stamped payload has %d elements, want 5", len(elems))
	}
	if elems[4][0] != 0xc4 {
		t.Errorf("stamp envelope = %#x, want 0xc4 (bin8)", elems[4][0])
	}
}

// TestAppendStampRejectsNonFourElementPayload guards the splice
// precondition: it rewrites the array header blind, so it must refuse
// anything that is not the 4-element fixarray buildSignedPayload emits.
func TestAppendStampRejectsNonFourElementPayload(t *testing.T) {
	stamp := bytes.Repeat([]byte{0x01}, StampSize)
	for _, c := range []struct {
		name    string
		payload []byte
		stamp   []byte
	}{
		{"already stamped", []byte{0x95, 0x00}, stamp},
		{"not an array", []byte{0xc0}, stamp},
		{"empty", nil, stamp},
		// 16 bytes is the VALID ticket-stamp length (§5.7.3), so the
		// wrong-length case has to be a length that is neither.
		{"stamp of neither valid length", []byte{0x94, 0x00}, stamp[:20]},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := appendStamp(c.payload, c.stamp); err == nil {
				t.Error("accepted an invalid splice input")
			}
		})
	}

	// Both legitimate stamp lengths must splice: StampSize for
	// proof-of-work and TicketStampSize for the §5.7.3 ticket form,
	// which is half as long because upstream derives it with
	// truncated_hash.
	for _, n := range []int{StampSize, TicketStampSize} {
		if _, err := appendStamp([]byte{0x94, 0x00}, bytes.Repeat([]byte{0x02}, n)); err != nil {
			t.Errorf("appendStamp rejected a %d-byte stamp: %v", n, err)
		}
	}
}

func TestSignAndPackOpportunisticStampedRoundTrip(t *testing.T) {
	sender, _ := rns.NewIdentity()
	recipient, _ := rns.NewIdentity()
	senderDest := sender.DestinationHashFor(FullName())
	recipientDest := recipient.DestinationHashFor(FullName())

	body, msgID, err := SignAndPackOpportunisticStamped(sender, senderDest, recipientDest,
		nil, []byte("stamped hello"), nil, StampOptions{Cost: testStampCost})
	if err != nil {
		t.Fatalf("SignAndPackOpportunisticStamped: %v", err)
	}

	m, err := ParseOpportunisticBody(body, recipientDest)
	if err != nil {
		t.Fatalf("ParseOpportunisticBody: %v", err)
	}
	if len(m.Stamp) != StampSize {
		t.Fatalf("parsed stamp is %d bytes, want %d", len(m.Stamp), StampSize)
	}
	// Verify must succeed via the §5.6 variant-2 (stamp-stripped) path.
	if err := m.Verify(sender.PublicKey()[32:]); err != nil {
		t.Errorf("Verify of stamped message: %v", err)
	}
	if string(m.Content) != "stamped hello" {
		t.Errorf("content = %q", m.Content)
	}
	// The recipient must derive the same message_id the sender returned,
	// or every reaction and reply bound to it misses (SPEC §5.5).
	if !bytes.Equal(m.MessageID(), msgID) {
		t.Errorf("recipient message_id %x != sender-returned %x", m.MessageID(), msgID)
	}
	// And that id is the workblock material the stamp was ground over.
	wb, err := stampWorkblock(msgID, workblockExpandRounds)
	if err != nil {
		t.Fatalf("stampWorkblock: %v", err)
	}
	if !stampValid(m.Stamp, testStampCost, wb) {
		t.Error("stamp on the wire does not validate against the message_id workblock")
	}
}

func TestSignAndPackDirectStampedRoundTrip(t *testing.T) {
	sender, _ := rns.NewIdentity()
	recipient, _ := rns.NewIdentity()
	senderDest := sender.DestinationHashFor(FullName())
	recipientDest := recipient.DestinationHashFor(FullName())

	body, msgID, err := SignAndPackDirectStamped(sender, senderDest, recipientDest,
		nil, bytes.Repeat([]byte("link-delivered "), 40), nil, StampOptions{Cost: testStampCost})
	if err != nil {
		t.Fatalf("SignAndPackDirectStamped: %v", err)
	}
	m, err := ParseDirectBody(body)
	if err != nil {
		t.Fatalf("ParseDirectBody: %v", err)
	}
	if len(m.Stamp) != StampSize {
		t.Fatalf("parsed stamp is %d bytes, want %d", len(m.Stamp), StampSize)
	}
	if err := m.Verify(sender.PublicKey()[32:]); err != nil {
		t.Errorf("Verify of stamped direct message: %v", err)
	}
	if !bytes.Equal(m.MessageID(), msgID) {
		t.Errorf("recipient message_id %x != sender-returned %x", m.MessageID(), msgID)
	}
}

// TestStampCountsAgainstOpportunisticBudget pins the routing consequence
// of §5.7.1: the stamp rides inside the payload, so it eats the
// single-packet budget. A message that fits unstamped but not stamped
// must report ErrPayloadTooLarge so Send routes it to a link instead of
// emitting an oversize packet.
func TestStampCountsAgainstOpportunisticBudget(t *testing.T) {
	sender, _ := rns.NewIdentity()
	senderDest := sender.DestinationHashFor(FullName())
	destHash := bytes.Repeat([]byte{0x55}, rns.IdentityHashLen)

	// Sized to land in the 34-byte window that only the stamp closes.
	content := bytes.Repeat([]byte{'x'}, MaxOpportunisticPayload-30)
	if _, _, err := SignAndPackOpportunistic(sender, senderDest, destHash, nil, content, nil); err != nil {
		t.Fatalf("premise broken — unstamped message should fit: %v", err)
	}
	_, _, err := SignAndPackOpportunisticStamped(sender, senderDest, destHash,
		nil, content, nil, StampOptions{Cost: testStampCost})
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("err = %v, want ErrPayloadTooLarge", err)
	}
}

func TestStampOptionsMaxCost(t *testing.T) {
	if got := (StampOptions{}).maxCost(); got != MaxDeliveryStampCost {
		t.Errorf("zero MaxCost = %d, want package default %d", got, MaxDeliveryStampCost)
	}
	if got := (StampOptions{MaxCost: 4}).maxCost(); got != 4 {
		t.Errorf("MaxCost override = %d, want 4", got)
	}
	// The override is what makes a per-Delivery ceiling real: a cost the
	// package default would grind must be refused under a lower one.
	sender, _ := rns.NewIdentity()
	senderDest := sender.DestinationHashFor(FullName())
	destHash := bytes.Repeat([]byte{0x66}, rns.IdentityHashLen)
	_, _, err := SignAndPackOpportunisticStamped(sender, senderDest, destHash,
		nil, []byte("x"), nil, StampOptions{Cost: 8, MaxCost: 4})
	if !errors.Is(err, ErrStampCostTooHigh) {
		t.Errorf("err = %v, want ErrStampCostTooHigh", err)
	}
}

// TestOversizeStampedMessageDoesNotGrind is the regression for a wasted
// proof-of-work: the opportunistic packer used to grind, then discover
// the stamped payload overflowed the single-packet budget, and return
// ErrPayloadTooLarge — whereupon Delivery.SendWithID re-packed through
// sendOverLink and ground a second time over a fresh message_id. Every
// oversize message to a stamp-demanding recipient paid twice.
//
// The size is knowable from the unstamped payload, so the refusal must
// come back far faster than a grind at a cost this high could.
func TestOversizeStampedMessageDoesNotGrind(t *testing.T) {
	sender, _ := rns.NewIdentity()
	senderDest := sender.DestinationHashFor(FullName())
	destHash := bytes.Repeat([]byte{0x77}, rns.IdentityHashLen)
	content := bytes.Repeat([]byte{'x'}, MaxOpportunisticPayload)

	start := time.Now()
	_, _, err := SignAndPackOpportunisticStamped(sender, senderDest, destHash,
		nil, content, nil, StampOptions{Cost: MaxDeliveryStampCost})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}
	// A grind at MaxDeliveryStampCost measures in the hundreds of
	// milliseconds, and building the workblock alone is tens. Refusing
	// without touching either is microseconds; 10ms separates them by
	// more than an order of magnitude either way.
	if elapsed > 10*time.Millisecond {
		t.Errorf("oversize refusal took %v — proof-of-work was done before the size check", elapsed)
	}
}

// TestStampElementStrippedEvenWhenUndecodable: a peer can emit a
// 5-element payload whose element [4] is not readable as bytes (msgpack
// nil, or a str). Message.Stamp is then nil, but the payload is still
// the 5-element form, so both the §5.5 message_id and the §5.6 variant-2
// signature retry must strip it. Keying either on Stamp != nil made such
// a message compute an id no other client agrees with, and fail
// verification outright.
func TestStampElementStrippedEvenWhenUndecodable(t *testing.T) {
	sender, _ := rns.NewIdentity()
	senderDest := sender.DestinationHashFor(FullName())
	destHash := bytes.Repeat([]byte{0x88}, rns.IdentityHashLen)

	body, msgID, err := SignAndPackOpportunistic(sender, senderDest, destHash,
		nil, []byte("null stamp"), nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := ParseOpportunisticBody(body, destHash)
	if err != nil {
		t.Fatal(err)
	}

	// Re-pack the payload as a 5-element array with a msgpack-nil stamp.
	stamped := append([]byte{0x95}, base.rawPayload[1:]...)
	stamped = append(stamped, msgpackNil)
	mutated := append(append([]byte{}, body[:rns.IdentityHashLen+signatureLen]...), stamped...)

	m, err := ParseOpportunisticBody(mutated, destHash)
	if err != nil {
		t.Fatalf("5-element payload with a nil stamp did not parse: %v", err)
	}
	if m.Stamp != nil {
		t.Fatalf("premise broken — a nil stamp should not decode to bytes, got %x", m.Stamp)
	}
	if !m.stampElement {
		t.Fatal("stampElement not set for a 5-element payload")
	}
	if err := m.Verify(sender.PublicKey()[32:]); err != nil {
		t.Errorf("Verify did not strip an undecodable stamp element: %v", err)
	}
	if !bytes.Equal(m.MessageID(), msgID) {
		t.Errorf("message_id %x != unstamped %x — element [4] was hashed", m.MessageID(), msgID)
	}
}
