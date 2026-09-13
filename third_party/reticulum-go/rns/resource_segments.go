package rns

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SPEC §10.11 — multi-segment resources.
//
// A payload larger than MaxEfficientSize is split at that boundary, and
// each segment is its OWN Resource with its own advertisement, hashmap,
// random_hash and encryption. Only three advertisement fields tie them
// together: `i` (1-based segment index), `l` (total segments), and `o`
// (the first segment's `h`, carried by every segment as the transfer's
// stable identity — `h` itself differs per segment).
//
// Because each segment is separately encrypted and hashed, reassembly at
// this layer is a plain concatenation of the per-segment plaintexts. The
// interesting part is the bookkeeping: segments arrive as independent
// transfers and must not be handed up until the last one lands, or the
// application sees a truncated body it has no way to recognise as
// partial.
//
// Everything retained here is retained on behalf of an UNAUTHENTICATED
// peer — a link is established before any identity is proven — so the
// assembler is bounded on four independent axes: segments per transfer
// (MaxAcceptedResourceSegments, tightened per-Transport by
// SetMaxResourceSegments), concurrent assemblies per link
// (MaxPendingSegmentAssembliesPerLink), total retained bytes across all
// links (MaxRetainedSegmentBytes), and time (idle + absolute).

// MaxAcceptedResourceSegments is the absolute ceiling on a
// multi-segment transfer, enforced in ParseResourceAdv and here.
//
// The per-advertisement caps do not bound the whole: a peer can offer
// 1 MiB per segment and simply claim a large `l`. This ceiling times
// MaxAcceptedResourceSize is the real memory exposure of one transfer,
// and it is reachable pre-authorisation by anyone with a link.
//
// It is a hard ceiling, not a policy knob: a Transport may set a
// TIGHTER limit with SetMaxResourceSegments (1 refuses multi-segment
// outright), but never a looser one.
const MaxAcceptedResourceSegments = 16

// DefaultMaxResourceSegments is the per-Transport limit applied when
// SetMaxResourceSegments has not been called.
const DefaultMaxResourceSegments = MaxAcceptedResourceSegments

// MaxPendingSegmentAssembliesPerLink caps how many distinct
// original_hash transfers one link may have partially assembled.
//
// §10.11 has the sender build segment N+1 only after N is proved, so a
// conformant peer has exactly ONE multi-segment transfer in flight per
// link at a time. Anything beyond that is a peer trying to multiply its
// share of MaxRetainedSegmentBytes; this cap is what stops one link
// monopolising the global budget.
const MaxPendingSegmentAssembliesPerLink = 1

// MaxRetainedSegmentBytes caps the completed-but-not-yet-deliverable
// segment bytes held across ALL links.
//
// The per-transfer caps bound one transfer; with MaxResponderLinks
// inbound links each holding MaxAcceptedResourceSegments-1 completed
// segments, the product is measured in gigabytes. This is the term that
// makes the total finite, and it is sized for the smallest deployment
// target (Raspberry Pi class), not the largest.
//
// On exhaustion the INCOMING segment is refused; an already-retained
// assembly is never evicted, because eviction would let a peer cancel
// somebody else's honest in-flight transfer by flooding.
const MaxRetainedSegmentBytes = 64 << 20 // 64 MiB

// Retention deadlines for a partial multi-segment transfer.
//
//   - Idle: time since the LAST segment landed. Must exceed the
//     worst-case single-segment resourceTransferTimeout, which for a
//     MaxAcceptedResourceSize segment is 30s + 1056767/2048 s ≈ 9m5s —
//     a fixed deadline measured from the FIRST segment (what this code
//     did before) expires mid-flight on essentially every legitimate
//     multi-segment transfer, since segment 2 alone can outlast it.
//   - Absolute: total transfer duration. Without it, a peer pins memory
//     indefinitely by dripping one segment per idle window.
//
// Both are overridable per-Transport via SetSegmentAssemblyTimeouts.
const (
	DefaultSegmentAssemblyIdleTimeout = 10 * time.Minute
	DefaultSegmentAssemblyMaxDuration = 30 * time.Minute
)

// SegmentAssemblyTimeout is the historical name for the idle deadline.
//
// Deprecated: it used to be measured from the first segment, which no
// legitimate transfer could meet; it is now the idle default. Use
// DefaultSegmentAssemblyIdleTimeout.
const SegmentAssemblyTimeout = DefaultSegmentAssemblyIdleTimeout

// ErrSegmentAssemblyLimit is returned when a segment is refused by one
// of the assembler's resource bounds rather than for being malformed.
var ErrSegmentAssemblyLimit = errors.New("resource: segment assembly limit reached")

type segmentAssembly struct {
	originalHash []byte
	linkHex      string // hex(link_id), the per-link counter key
	total        int
	parts        map[int][]byte
	bytes        int
	started      time.Time // first segment — bounds total duration
	lastSeen     time.Time // most recent segment — bounds idle time
}

// segmentAssembler accumulates the segments of multi-segment transfers,
// keyed by hex(link_id) ":" hex(original_hash).
type segmentAssembler struct {
	mu       sync.Mutex
	all      map[string]*segmentAssembly
	perLink  map[string]int // hex(link_id) → partial assemblies held
	retained int            // sum of a.bytes across all

	// Bounds, defaulted by newSegmentAssembler. Tests and
	// SetSegmentAssemblyTimeouts adjust them.
	idleTimeout time.Duration
	maxDuration time.Duration
	maxPerLink  int
	maxBytes    int
}

func newSegmentAssembler() *segmentAssembler {
	return &segmentAssembler{
		all:         map[string]*segmentAssembly{},
		perLink:     map[string]int{},
		idleTimeout: DefaultSegmentAssemblyIdleTimeout,
		maxDuration: DefaultSegmentAssemblyMaxDuration,
		maxPerLink:  MaxPendingSegmentAssembliesPerLink,
		maxBytes:    MaxRetainedSegmentBytes,
	}
}

func segmentKey(linkID, originalHash []byte) string {
	return hex.EncodeToString(linkID) + ":" + hex.EncodeToString(originalHash)
}

// setTimeouts overrides the idle / absolute retention deadlines.
// Non-positive values leave the current setting alone.
func (s *segmentAssembler) setTimeouts(idle, absolute time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idle > 0 {
		s.idleTimeout = idle
	}
	if absolute > 0 {
		s.maxDuration = absolute
	}
}

// add records one assembled segment and returns the full body once the
// final segment has landed. Returns nil when more are outstanding.
//
// Segments may arrive out of order in principle, so they are indexed
// rather than appended; upstream sends them strictly in order, but
// trusting that would turn a reordering peer into silent corruption
// instead of a detectable error.
func (s *segmentAssembler) add(linkID []byte, adv *ResourceAdvertisement, body []byte, now time.Time) ([]byte, error) {
	if adv.SegmentIndex < 1 || adv.SegmentIndex > adv.TotalSegments {
		return nil, fmt.Errorf("segment_index %d outside 1..%d", adv.SegmentIndex, adv.TotalSegments)
	}
	// The absolute ceiling. The tighter per-Transport limit is applied
	// at ADV acceptance (openResourceReceiver) so an over-limit `l` is
	// refused before a whole segment transfer has been paid for; by the
	// time a body reaches here that cost is already sunk.
	if adv.TotalSegments > MaxAcceptedResourceSegments {
		return nil, fmt.Errorf("%w: %d segments, cap is %d", ErrResourceTooLarge, adv.TotalSegments, MaxAcceptedResourceSegments)
	}
	if len(adv.OriginalHash) == 0 {
		return nil, fmt.Errorf("multi-segment advertisement carries no original_hash (`o`)")
	}

	linkHex := hex.EncodeToString(linkID)
	key := segmentKey(linkID, adv.OriginalHash)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)

	a, ok := s.all[key]
	if !ok {
		if n := s.perLink[linkHex]; n >= s.maxPerLink {
			// §10.11 makes one in-flight multi-segment transfer per
			// link the conformant shape; more than that is a peer
			// multiplying its claim on the global byte budget.
			return nil, fmt.Errorf("%w: link=%x already holds %d partial assemblies (cap %d)",
				ErrSegmentAssemblyLimit, linkID[:4], n, s.maxPerLink)
		}
	}
	if s.retained+len(body) > s.maxBytes {
		// Refuse the newcomer rather than evict a retained assembly:
		// eviction would let a flooding peer cancel honest transfers.
		return nil, fmt.Errorf("%w: %d retained bytes + %d would exceed the %d cap",
			ErrSegmentAssemblyLimit, s.retained, len(body), s.maxBytes)
	}
	if !ok {
		a = &segmentAssembly{
			originalHash: append([]byte(nil), adv.OriginalHash...),
			linkHex:      linkHex,
			total:        adv.TotalSegments,
			parts:        map[int][]byte{},
			started:      now,
			lastSeen:     now,
		}
		s.all[key] = a
		s.perLink[linkHex]++
	}
	if a.total != adv.TotalSegments {
		// A sender that changes `l` mid-transfer is either broken or
		// trying to grow the allocation past what the first
		// advertisement bought.
		total := a.total
		s.removeLocked(key, a)
		return nil, fmt.Errorf("total_segments changed mid-transfer: %d then %d", total, adv.TotalSegments)
	}
	if _, dup := a.parts[adv.SegmentIndex]; dup {
		return nil, nil // retransmitted segment; already held
	}

	a.parts[adv.SegmentIndex] = append([]byte(nil), body...)
	a.bytes += len(body)
	s.retained += len(body)
	// Idle, not fixed: the deadline tracks the last segment to arrive,
	// so a slow-but-progressing transfer is never expired mid-flight.
	// a.started still bounds the total duration in expireLocked.
	a.lastSeen = now

	if len(a.parts) < a.total {
		return nil, nil
	}

	out := make([]byte, 0, a.bytes)
	for i := 1; i <= a.total; i++ {
		seg, ok := a.parts[i]
		if !ok {
			// Cannot happen while len(parts) == total, but concatenating
			// a gap would hand up a silently corrupt body.
			s.removeLocked(key, a)
			return nil, fmt.Errorf("segment %d missing at completion", i)
		}
		out = append(out, seg...)
	}
	s.removeLocked(key, a)
	return out, nil
}

// removeLocked drops an assembly and releases its share of both the
// global byte budget and its link's assembly count.
func (s *segmentAssembler) removeLocked(key string, a *segmentAssembly) {
	if _, ok := s.all[key]; !ok {
		return
	}
	delete(s.all, key)
	s.retained -= a.bytes
	if n := s.perLink[a.linkHex] - 1; n > 0 {
		s.perLink[a.linkHex] = n
	} else {
		delete(s.perLink, a.linkHex)
	}
}

// expireLocked drops assemblies that have gone quiet (no segment within
// idleTimeout) or that have simply run too long overall (maxDuration),
// the latter so a peer cannot pin memory by dripping one segment per
// idle window forever.
func (s *segmentAssembler) expireLocked(now time.Time) {
	for k, a := range s.all {
		if now.Sub(a.lastSeen) > s.idleTimeout || now.Sub(a.started) > s.maxDuration {
			s.removeLocked(k, a)
		}
	}
}

// dropLink releases every partial assembly held for a link.
//
// Retention is bounded by time as well, but a torn-down link's segments
// can never make progress — the transfer is keyed on the link_id and
// §10.11 has no resumption across links — so holding them to the idle
// deadline is pure waste of a budget that other links need. It matters
// more than it looks because exhaustion REFUSES rather than evicts
// (MaxRetainedSegmentBytes): a peer that opens a link, ships most of a
// multi-segment transfer, closes, and repeats would otherwise park
// MaxRetainedSegmentBytes' worth of dead assemblies under distinct
// link_ids — each one immune to the per-link cap, and each one denying
// honest transfers on live links for ten minutes at a time.
func (s *segmentAssembler) dropLink(linkID []byte) {
	linkHex := hex.EncodeToString(linkID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perLink[linkHex] == 0 {
		return
	}
	for k, a := range s.all {
		if a.linkHex == linkHex {
			s.removeLocked(k, a)
		}
	}
}

// pending reports how many partial transfers are held, for tests and
// observability.
func (s *segmentAssembler) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.all)
}

// retainedBytes reports the segment bytes currently held against
// MaxRetainedSegmentBytes, for tests and observability.
func (s *segmentAssembler) retainedBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retained
}

// SendSegmentedResourceOverLink transfers a body larger than
// MaxEfficientSize as a §10.11 multi-segment resource, and falls through
// to a single Resource when it fits.
//
// Segments go out strictly in sequence, each waiting for the previous to
// conclude. Upstream builds segment N+1 only after N is proved
// (Resource.py:770-783) for the same reason: a 100 MiB transfer must not
// materialise 100 segments' worth of parts, hashmaps and ciphertext at
// once. Doing them concurrently would also give the receiver no way to
// bound what it is holding.
func (t *Transport) SendSegmentedResourceOverLink(ctx context.Context, link *Link, body, transportID []byte) error {
	if link == nil {
		return errors.New("SendSegmentedResourceOverLink: nil link")
	}
	if len(body) == 0 {
		return errors.New("SendSegmentedResourceOverLink: empty body")
	}
	if len(body) <= MaxEfficientSize {
		return t.SendResourceOverLink(ctx, link, body, transportID)
	}

	total := (len(body) + MaxEfficientSize - 1) / MaxEfficientSize
	// The same per-Transport policy that governs what we ACCEPT governs
	// what we emit, so a Transport configured to refuse multi-segment
	// (limit 1) does not send what it would not receive. Refuse here
	// rather than emit a transfer a conformant receiver with our own
	// limits would reject partway through, after both sides have paid
	// for the segments already sent.
	if max := t.maxResourceSegmentsLimit(); total > max {
		return fmt.Errorf("%w: body needs %d segments, cap is %d",
			ErrResourceTooLarge, total, max)
	}

	var originalHash []byte
	for i := 0; i < total; i++ {
		start := i * MaxEfficientSize
		end := start + MaxEfficientSize
		if end > len(body) {
			end = len(body)
		}
		rs, err := newSegmentSender(t, link, body[start:end], transportID, i+1, total, originalHash)
		if err != nil {
			return fmt.Errorf("segment %d/%d: %w", i+1, total, err)
		}
		if i == 0 {
			// Every later segment carries the FIRST segment's hash as
			// `o`; that is the only field tying the transfer together,
			// since `h` differs per segment.
			originalHash = rs.ResourceHash()
		}
		if err := t.linkManager.registerResourceSender(link.ID, rs.ResourceHash(), rs); err != nil {
			return fmt.Errorf("segment %d/%d: %w", i+1, total, err)
		}
		if err := rs.Run(ctx); err != nil {
			return fmt.Errorf("segment %d/%d: %w", i+1, total, err)
		}
		t.logger.Printf("resource segment %d/%d delivered on link %x", i+1, total, link.ID[:4])
	}
	return nil
}
