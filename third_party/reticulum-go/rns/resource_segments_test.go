package rns

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func segAdv(t *testing.T, original []byte, index, total int) *ResourceAdvertisement {
	t.Helper()
	return &ResourceAdvertisement{
		OriginalHash:  original,
		SegmentIndex:  index,
		TotalSegments: total,
	}
}

// Segments are held until the last one lands. Handing up a segment on
// its own would give the application a truncated body it cannot tell
// apart from a complete one.
func TestSegmentAssemblyWaitsForEverySegment(t *testing.T) {
	s := newSegmentAssembler()
	link := bytes.Repeat([]byte{0x01}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xAA}, 32)
	now := time.Now()

	whole, err := s.add(link, segAdv(t, orig, 1, 3), []byte("aaa"), now)
	if err != nil || whole != nil {
		t.Fatalf("segment 1 = (%q, %v), want (nil, nil)", whole, err)
	}
	whole, err = s.add(link, segAdv(t, orig, 2, 3), []byte("bbb"), now)
	if err != nil || whole != nil {
		t.Fatalf("segment 2 = (%q, %v), want (nil, nil)", whole, err)
	}
	whole, err = s.add(link, segAdv(t, orig, 3, 3), []byte("ccc"), now)
	if err != nil {
		t.Fatalf("segment 3: %v", err)
	}
	if string(whole) != "aaabbbccc" {
		t.Errorf("assembled %q, want aaabbbccc", whole)
	}
	if s.pending() != 0 {
		t.Error("the completed assembly was not released")
	}
}

// Upstream sends segments in order, but trusting that would turn a
// reordering peer into silent corruption. Order is by `i`, not arrival.
func TestSegmentAssemblyOrdersByIndexNotArrival(t *testing.T) {
	s := newSegmentAssembler()
	link := bytes.Repeat([]byte{0x02}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xBB}, 32)
	now := time.Now()

	if _, err := s.add(link, segAdv(t, orig, 3, 3), []byte("ccc"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.add(link, segAdv(t, orig, 1, 3), []byte("aaa"), now); err != nil {
		t.Fatal(err)
	}
	whole, err := s.add(link, segAdv(t, orig, 2, 3), []byte("bbb"), now)
	if err != nil {
		t.Fatal(err)
	}
	if string(whole) != "aaabbbccc" {
		t.Errorf("assembled %q in arrival order, want index order", whole)
	}
}

func TestSegmentAssemblyRejects(t *testing.T) {
	link := bytes.Repeat([]byte{0x03}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xCC}, 32)
	now := time.Now()

	t.Run("index out of range", func(t *testing.T) {
		s := newSegmentAssembler()
		if _, err := s.add(link, segAdv(t, orig, 4, 3), []byte("x"), now); err == nil {
			t.Error("accepted segment 4 of 3")
		}
		if _, err := s.add(link, segAdv(t, orig, 0, 3), []byte("x"), now); err == nil {
			t.Error("accepted segment 0 (indices are 1-based)")
		}
	})
	t.Run("too many segments", func(t *testing.T) {
		s := newSegmentAssembler()
		n := MaxAcceptedResourceSegments + 1
		if _, err := s.add(link, segAdv(t, orig, 1, n), []byte("x"), now); err == nil {
			t.Errorf("accepted a %d-segment transfer, cap is %d", n, MaxAcceptedResourceSegments)
		}
	})
	t.Run("no original_hash", func(t *testing.T) {
		s := newSegmentAssembler()
		if _, err := s.add(link, segAdv(t, nil, 1, 2), []byte("x"), now); err == nil {
			t.Error("accepted a multi-segment ADV with no `o` to correlate on")
		}
	})
	// A sender that changes `l` mid-transfer is broken or trying to grow
	// the allocation past what the first advertisement bought.
	t.Run("total changes mid-transfer", func(t *testing.T) {
		s := newSegmentAssembler()
		if _, err := s.add(link, segAdv(t, orig, 1, 2), []byte("a"), now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.add(link, segAdv(t, orig, 2, 5), []byte("b"), now); err == nil {
			t.Error("accepted a changed total_segments")
		}
		if s.pending() != 0 {
			t.Error("the inconsistent assembly was not discarded")
		}
	})
}

// A retransmitted segment must not be counted twice, or the assembly
// completes early on a lossy link and concatenates a gap.
func TestSegmentAssemblyIgnoresDuplicates(t *testing.T) {
	s := newSegmentAssembler()
	link := bytes.Repeat([]byte{0x04}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xDD}, 32)
	now := time.Now()

	if _, err := s.add(link, segAdv(t, orig, 1, 2), []byte("aaa"), now); err != nil {
		t.Fatal(err)
	}
	whole, err := s.add(link, segAdv(t, orig, 1, 2), []byte("aaa"), now)
	if err != nil || whole != nil {
		t.Fatalf("duplicate segment = (%q, %v), want (nil, nil)", whole, err)
	}
	whole, err = s.add(link, segAdv(t, orig, 2, 2), []byte("bbb"), now)
	if err != nil {
		t.Fatal(err)
	}
	if string(whole) != "aaabbb" {
		t.Errorf("assembled %q", whole)
	}
}

// The sender builds segment N+1 only after N is proved, so a stalled
// sender would otherwise pin every completed segment indefinitely.
func TestSegmentAssemblyExpires(t *testing.T) {
	s := newSegmentAssembler()
	link := bytes.Repeat([]byte{0x05}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xEE}, 32)
	now := time.Now()

	if _, err := s.add(link, segAdv(t, orig, 1, 2), []byte("a"), now); err != nil {
		t.Fatal(err)
	}
	if s.pending() != 1 {
		t.Fatal("segment was not retained")
	}
	// Any later add sweeps expired assemblies.
	other := bytes.Repeat([]byte{0xFF}, 32)
	if _, err := s.add(link, segAdv(t, other, 1, 2), []byte("z"), now.Add(SegmentAssemblyTimeout+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s.pending() != 1 {
		t.Errorf("%d assemblies pending, want only the fresh one", s.pending())
	}
}

// Two transfers on the same link must not blend, and the same
// original_hash on different links must stay separate.
func TestSegmentAssemblyKeysOnLinkAndOriginalHash(t *testing.T) {
	s := newSegmentAssembler()
	linkA := bytes.Repeat([]byte{0x06}, IdentityHashLen)
	linkB := bytes.Repeat([]byte{0x07}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0x11}, 32)
	now := time.Now()

	if _, err := s.add(linkA, segAdv(t, orig, 1, 2), []byte("A1"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.add(linkB, segAdv(t, orig, 1, 2), []byte("B1"), now); err != nil {
		t.Fatal(err)
	}
	whole, err := s.add(linkA, segAdv(t, orig, 2, 2), []byte("A2"), now)
	if err != nil {
		t.Fatal(err)
	}
	if string(whole) != "A1A2" {
		t.Errorf("link A assembled %q; the two links' transfers blended", whole)
	}
	if s.pending() != 1 {
		t.Error("link B's partial transfer was disturbed")
	}
}

// The ADV parser must now accept l>1 — it used to reject outright — but
// still bound the segment count, since the per-advertisement caps bound
// only ONE segment.
func TestResourceAdvAcceptsMultiSegmentWithinCap(t *testing.T) {
	build := func(total int) []byte {
		adv := &ResourceAdvertisement{
			TransferSize: 100, DataSize: 100, NumParts: 1,
			Hash:         bytes.Repeat([]byte{0x01}, 32),
			RandomHash:   bytes.Repeat([]byte{0x02}, ResourceRandomHashSize),
			OriginalHash: bytes.Repeat([]byte{0x01}, 32),
			SegmentIndex: 1, TotalSegments: total,
			Flags:   int(ResourceFlagEncrypted | ResourceFlagSplit),
			Hashmap: bytes.Repeat([]byte{0x03}, ResourceMapHashLen),
		}
		packed, err := PackResourceAdv(adv)
		if err != nil {
			t.Fatal(err)
		}
		return packed
	}
	if _, err := ParseResourceAdv(build(3)); err != nil {
		t.Errorf("rejected a 3-segment advertisement: %v", err)
	}
	if _, err := ParseResourceAdv(build(MaxAcceptedResourceSegments + 1)); err == nil {
		t.Error("accepted a segment count past the cap")
	}
}

// --- bounds (SPEC §10.11 retention limits) ---------------------------

// A Transport limited to one segment must refuse an l>1 advertisement
// at acceptance, BEFORE a receiver is allocated — the point of the
// per-Transport knob is to not pay for a segment transfer at all.
func TestOpenResourceReceiverRefusesOverLimitSegmentCount(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	tp.SetMaxResourceSegments(1)

	adv := &ResourceAdvertisement{
		TransferSize: 100, DataSize: 100, NumParts: 1,
		Hash:         bytes.Repeat([]byte{0x01}, 32),
		RandomHash:   bytes.Repeat([]byte{0x02}, ResourceRandomHashSize),
		OriginalHash: bytes.Repeat([]byte{0x01}, 32),
		SegmentIndex: 1, TotalSegments: 3,
		Flags:   int(ResourceFlagEncrypted | ResourceFlagSplit),
		Hashmap: bytes.Repeat([]byte{0x03}, ResourceMapHashLen),
	}
	err := tp.openResourceReceiver(link, adv)
	if err == nil {
		t.Fatal("accepted a 3-segment ADV on a transport limited to 1")
	}
	if !errors.Is(err, ErrResourceTooLarge) {
		t.Errorf("err = %v, want ErrResourceTooLarge", err)
	}
	if n := tp.linkManager.countReceiversForLink(link.ID); n != 0 {
		t.Errorf("%d receivers allocated; the ADV must be refused before any state is built", n)
	}

	// The same ADV as a single-segment transfer is still accepted, so
	// the limit refuses multi-segment rather than resources generally.
	adv.SegmentIndex, adv.TotalSegments = 1, 1
	if err := tp.openResourceReceiver(link, adv); err != nil {
		t.Fatalf("single-segment ADV refused under limit=1: %v", err)
	}
	tp.linkManager.closeResourcesForLink(link.ID)
}

// The default keeps v0.5.0 behaviour, and the knob can only tighten:
// MaxAcceptedResourceSegments is a memory bound an unauthenticated peer
// can reach, so configuration must not raise it.
func TestSetMaxResourceSegmentsClamps(t *testing.T) {
	tp := NewTransport(noopLogger{})
	if got := tp.maxResourceSegmentsLimit(); got != DefaultMaxResourceSegments {
		t.Errorf("default limit = %d, want %d", got, DefaultMaxResourceSegments)
	}
	tp.SetMaxResourceSegments(MaxAcceptedResourceSegments + 5)
	if got := tp.maxResourceSegmentsLimit(); got != MaxAcceptedResourceSegments {
		t.Errorf("limit = %d, want it clamped to %d", got, MaxAcceptedResourceSegments)
	}
	tp.SetMaxResourceSegments(0)
	if got := tp.maxResourceSegmentsLimit(); got != DefaultMaxResourceSegments {
		t.Errorf("limit = %d, want 0 to restore the default", got)
	}
	tp.SetMaxResourceSegments(2)
	if got := tp.maxResourceSegmentsLimit(); got != 2 {
		t.Errorf("limit = %d, want 2", got)
	}
	// A zero-value Transport (never through NewTransport) must still
	// report a bound rather than 0, which would refuse everything.
	var zero Transport
	if got := zero.maxResourceSegmentsLimit(); got != DefaultMaxResourceSegments {
		t.Errorf("zero-value transport limit = %d, want %d", got, DefaultMaxResourceSegments)
	}
}

// A transport that refuses multi-segment inbound must not emit it
// either — otherwise it sends bodies it could never receive back.
func TestSendSegmentedRefusesWhenLimitIsOne(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	tp.SetMaxResourceSegments(1)

	body := bytes.Repeat([]byte{0x5A}, MaxEfficientSize+1)
	err := tp.SendSegmentedResourceOverLink(context.Background(), link, body, nil)
	if err == nil {
		t.Fatal("emitted a multi-segment transfer on a transport limited to 1")
	}
	if !errors.Is(err, ErrResourceTooLarge) {
		t.Errorf("err = %v, want ErrResourceTooLarge", err)
	}
}

// §10.11 has the sender build segment N+1 only after N is proved, so a
// conformant peer has one multi-segment transfer in flight per link.
// More than that is a peer multiplying its claim on the byte budget.
func TestSegmentAssemblyCapsAssembliesPerLink(t *testing.T) {
	s := newSegmentAssembler()
	link := bytes.Repeat([]byte{0x21}, IdentityHashLen)
	other := bytes.Repeat([]byte{0x22}, IdentityHashLen)
	now := time.Now()

	origA := bytes.Repeat([]byte{0xA1}, 32)
	origB := bytes.Repeat([]byte{0xB1}, 32)
	if _, err := s.add(link, segAdv(t, origA, 1, 2), []byte("a"), now); err != nil {
		t.Fatal(err)
	}
	_, err := s.add(link, segAdv(t, origB, 1, 2), []byte("b"), now)
	if err == nil {
		t.Fatalf("link opened a second assembly, cap is %d", s.maxPerLink)
	}
	if !errors.Is(err, ErrSegmentAssemblyLimit) {
		t.Errorf("err = %v, want ErrSegmentAssemblyLimit", err)
	}
	if s.pending() != 1 {
		t.Errorf("%d assemblies held, want the first one only", s.pending())
	}
	// The cap is per link, not global: another peer is unaffected.
	if _, err := s.add(other, segAdv(t, origB, 1, 2), []byte("b"), now); err != nil {
		t.Errorf("a second link was refused by the per-link cap: %v", err)
	}
	// Completing the first transfer frees the link's slot again.
	if _, err := s.add(link, segAdv(t, origA, 2, 2), []byte("a"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.add(link, segAdv(t, origB, 1, 2), []byte("b"), now); err != nil {
		t.Errorf("the completed assembly did not release the link's slot: %v", err)
	}
}

// On exhaustion the INCOMING segment is refused; a retained assembly is
// never evicted, or a flooding peer could cancel honest transfers.
func TestSegmentAssemblyCapsRetainedBytes(t *testing.T) {
	s := newSegmentAssembler()
	s.maxBytes = 8
	s.maxPerLink = 4 // isolate the byte cap from the per-link cap
	now := time.Now()

	linkA := bytes.Repeat([]byte{0x31}, IdentityHashLen)
	linkB := bytes.Repeat([]byte{0x32}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xC1}, 32)

	if _, err := s.add(linkA, segAdv(t, orig, 1, 2), []byte("123456"), now); err != nil {
		t.Fatal(err)
	}
	if got := s.retainedBytes(); got != 6 {
		t.Fatalf("retained %d bytes, want 6", got)
	}
	// 6 + 4 > 8: refuse the newcomer.
	_, err := s.add(linkB, segAdv(t, orig, 1, 2), []byte("wxyz"), now)
	if err == nil {
		t.Fatal("accepted a segment past the retained-bytes cap")
	}
	if !errors.Is(err, ErrSegmentAssemblyLimit) {
		t.Errorf("err = %v, want ErrSegmentAssemblyLimit", err)
	}
	// Eviction would have dropped link A's honest, in-flight transfer.
	if s.pending() != 1 || s.retainedBytes() != 6 {
		t.Errorf("pending=%d retained=%d; the existing assembly was evicted rather than the newcomer refused",
			s.pending(), s.retainedBytes())
	}
	// Completing link A releases the budget for the next transfer.
	if _, err := s.add(linkA, segAdv(t, orig, 2, 2), []byte("78"), now); err != nil {
		t.Fatal(err)
	}
	if got := s.retainedBytes(); got != 0 {
		t.Fatalf("retained %d bytes after completion, want 0", got)
	}
	if _, err := s.add(linkB, segAdv(t, orig, 1, 2), []byte("wxyz"), now); err != nil {
		t.Errorf("the freed budget was not released: %v", err)
	}
}

// F4: the deadline used to run from the FIRST segment, which no
// legitimate transfer could meet — one full segment alone is allowed
// resourceTransferTimeout(MaxAcceptedResourceSize) ≈ 9m5s, so a
// 16-segment transfer expired mid-flight essentially always. The idle
// deadline tracks the LAST segment instead.
func TestSegmentAssemblyIdleTimeoutSurvivesASlowTransfer(t *testing.T) {
	// Confirm the arithmetic the fix rests on: two segments already
	// outlast a 10-minute deadline measured from the first.
	perSegment := resourceTransferTimeout(MaxAcceptedResourceSize)
	if perSegment <= DefaultSegmentAssemblyIdleTimeout/2 {
		t.Fatalf("one segment may take %s; a %s deadline from the first segment "+
			"would not actually be unmeetable, so this test no longer proves anything",
			perSegment, DefaultSegmentAssemblyIdleTimeout)
	}
	if DefaultSegmentAssemblyIdleTimeout <= perSegment {
		t.Errorf("idle timeout %s does not exceed the worst-case single-segment transfer time %s",
			DefaultSegmentAssemblyIdleTimeout, perSegment)
	}

	s := newSegmentAssembler()
	link := bytes.Repeat([]byte{0x41}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xD1}, 32)

	// Four segments, each arriving just inside the idle window. Total
	// elapsed (~27 min) is far past the old fixed 10-minute deadline.
	step := DefaultSegmentAssemblyIdleTimeout - time.Minute
	now := time.Now()
	for i := 1; i <= 3; i++ {
		if _, err := s.add(link, segAdv(t, orig, i, 4), []byte{byte('0' + i)}, now); err != nil {
			t.Fatalf("segment %d at +%s: %v", i, now.Sub(now), err)
		}
		if s.pending() != 1 {
			t.Fatalf("segment %d: assembly expired mid-transfer", i)
		}
		now = now.Add(step)
	}
	whole, err := s.add(link, segAdv(t, orig, 4, 4), []byte("4"), now)
	if err != nil {
		t.Fatalf("final segment: %v", err)
	}
	if string(whole) != "1234" {
		t.Errorf("assembled %q, want 1234", whole)
	}
}

// The idle window still expires a transfer that has actually gone
// quiet — that is what stops a stalled sender pinning every completed
// segment (the sender builds N+1 only after N is proved, §10.11).
func TestSegmentAssemblyIdleTimeoutExpiresAQuietTransfer(t *testing.T) {
	s := newSegmentAssembler()
	link := bytes.Repeat([]byte{0x51}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xE1}, 32)
	now := time.Now()

	if _, err := s.add(link, segAdv(t, orig, 1, 2), []byte("aa"), now); err != nil {
		t.Fatal(err)
	}
	// Any later add sweeps expired assemblies. Use a different link so
	// the per-link cap doesn't mask the expiry we are testing.
	other := bytes.Repeat([]byte{0x52}, IdentityHashLen)
	if _, err := s.add(other, segAdv(t, orig, 1, 2), []byte("z"), now.Add(DefaultSegmentAssemblyIdleTimeout+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s.pending() != 1 {
		t.Errorf("%d assemblies pending, want only the fresh one", s.pending())
	}
	if got := s.retainedBytes(); got != 1 {
		t.Errorf("retained %d bytes, want 1 — the expired assembly's bytes were not released", got)
	}
}

// Without an absolute ceiling a peer pins memory forever by dripping
// one segment just inside every idle window.
func TestSegmentAssemblyAbsoluteDurationCeiling(t *testing.T) {
	s := newSegmentAssembler()
	s.idleTimeout = time.Minute
	s.maxDuration = 3 * time.Minute
	link := bytes.Repeat([]byte{0x61}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0xF1}, 32)

	start := time.Now()
	now := start
	// Drip a segment every 30s — never idle, but past maxDuration the
	// assembly must go anyway.
	for i := 1; i <= 6; i++ {
		if _, err := s.add(link, segAdv(t, orig, i, 8), []byte{byte(i)}, now); err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		now = now.Add(30 * time.Second)
	}
	if s.pending() != 1 {
		t.Fatalf("assembly gone before the absolute ceiling")
	}
	// One more, now past start+maxDuration: the old assembly is swept
	// and this segment starts a fresh one holding only its own byte.
	if _, err := s.add(link, segAdv(t, orig, 7, 8), []byte("x"), start.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := s.retainedBytes(); got != 1 {
		t.Errorf("retained %d bytes, want 1 — the over-long assembly was not swept", got)
	}
}

// SetSegmentAssemblyTimeouts follows the SetLinkLifetime convention:
// non-positive values keep the current setting.
func TestSetSegmentAssemblyTimeouts(t *testing.T) {
	tp := NewTransport(noopLogger{})
	tp.SetSegmentAssemblyTimeouts(2*time.Minute, 5*time.Minute)
	tp.segments.mu.Lock()
	idle, absolute := tp.segments.idleTimeout, tp.segments.maxDuration
	tp.segments.mu.Unlock()
	if idle != 2*time.Minute || absolute != 5*time.Minute {
		t.Fatalf("idle=%s absolute=%s, want 2m/5m", idle, absolute)
	}
	tp.SetSegmentAssemblyTimeouts(0, -time.Second)
	tp.segments.mu.Lock()
	idle, absolute = tp.segments.idleTimeout, tp.segments.maxDuration
	tp.segments.mu.Unlock()
	if idle != 2*time.Minute || absolute != 5*time.Minute {
		t.Errorf("idle=%s absolute=%s; non-positive values must keep the current setting", idle, absolute)
	}
}

// A §10.11 assembly is keyed on its link and cannot resume across
// links, so tearing the link down must release its retained segments
// immediately. Waiting out the idle deadline is exploitable: exhaustion
// REFUSES rather than evicts, so a peer that ships most of a transfer
// and then closes the link parks bytes that no per-link cap covers
// (each retry is a fresh link_id) and that deny honest transfers.
func TestSegmentAssembliesReleasedOnLinkClose(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	orig := bytes.Repeat([]byte{0x71}, 32)
	now := time.Now()

	if _, err := tp.segments.add(link.ID, segAdv(t, orig, 1, 3), []byte("held"), now); err != nil {
		t.Fatal(err)
	}
	if tp.segments.pending() != 1 || tp.segments.retainedBytes() != 4 {
		t.Fatalf("pending=%d retained=%d, want 1/4", tp.segments.pending(), tp.segments.retainedBytes())
	}

	tp.linkManager.CloseLink(link.ID)

	if tp.segments.pending() != 0 {
		t.Errorf("%d assemblies survived the link that owned them", tp.segments.pending())
	}
	if got := tp.segments.retainedBytes(); got != 0 {
		t.Errorf("retained %d bytes after close, want 0 — the byte budget was not released", got)
	}
	// The link's per-link slot must come back too, or a peer reusing a
	// link_id after a close would be locked out of its own budget.
	tp.segments.mu.Lock()
	n := tp.segments.perLink[bytesHexEncode(link.ID)]
	tp.segments.mu.Unlock()
	if n != 0 {
		t.Errorf("per-link counter = %d after close, want 0", n)
	}
}

// Closing a link that holds nothing must not disturb another link's
// in-flight assembly.
func TestLinkCloseLeavesOtherLinksAssembliesAlone(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	other := bytes.Repeat([]byte{0x81}, IdentityHashLen)
	orig := bytes.Repeat([]byte{0x91}, 32)
	now := time.Now()

	if _, err := tp.segments.add(other, segAdv(t, orig, 1, 2), []byte("keep"), now); err != nil {
		t.Fatal(err)
	}
	tp.linkManager.CloseLink(link.ID)
	if tp.segments.pending() != 1 || tp.segments.retainedBytes() != 4 {
		t.Errorf("pending=%d retained=%d; closing one link disturbed another's transfer",
			tp.segments.pending(), tp.segments.retainedBytes())
	}
}
