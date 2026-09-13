package rns

import (
	"bytes"
	"errors"
	"testing"
)

// makeBoundedReceiver builds a receiver whose hashmap actually matches
// the supplied parts, so placePart finds a slot for each, and whose ADV
// promised expectedSize bytes.
func makeBoundedReceiver(t *testing.T, parts [][]byte, expectedSize int) *ResourceReceiver {
	return makeBoundedReceiverWithSDU(t, parts, expectedSize, ResourceSDU)
}

// makeBoundedReceiverWithSDU is makeBoundedReceiver with control over
// the per-link SDU the receiver measures parts against (SPEC §10.2 step
// 6 sizes it from the link MTU, not the base one).
func makeBoundedReceiverWithSDU(t *testing.T, parts [][]byte, expectedSize, partSDU int) *ResourceReceiver {
	t.Helper()
	link, tp, _ := makeActiveTestLink(t)

	// ResourceMapHash returns a zero hash for a wrong-length random_hash,
	// which would make every part collide — the map_hash r is 4 bytes
	// (SPEC §10.2 step 5).
	randomR := bytes.Repeat([]byte{0xEE}, ResourceRandomHashSize)
	hashmap := make([]byte, 0, len(parts)*ResourceMapHashLen)
	for _, p := range parts {
		hashmap = append(hashmap, ResourceMapHash(p, randomR)...)
	}
	rr := &ResourceReceiver{
		transport:          tp,
		link:               link,
		logger:             noopLogger{},
		resourceHash:       bytes.Repeat([]byte{0xCD}, 32),
		randomR:            randomR,
		expectedSize:       expectedSize,
		partSDU:            partSDU,
		hashmap:            hashmap,
		hashmapKnownPrefix: len(parts),
		consecutiveHeight:  -1,
		parts:              make([][]byte, len(parts)),
		receivedFlags:      make([]bool, len(parts)),
		partCh:             make(chan []byte, 32),
		cancelCh:           make(chan struct{}, 1),
		hmuCh:              make(chan *ResourceHmu, 4),
		done:               make(chan struct{}),
		linkSigning:        append([]byte(nil), link.Signing...),
		linkEncryption:     append([]byte(nil), link.Encryption...),
	}
	rr.state.Store(int32(ResourceStateTransferring))
	return rr
}

func buffered(rr *ResourceReceiver) int {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	n := 0
	for _, p := range rr.parts {
		n += len(p)
	}
	return n
}

// A legitimate part is one SDU-sized slice of the encrypted body (SPEC
// §10.2 step 6). A larger one is malformed or hostile, and must be
// refused before it is buffered — ParseResourceAdv bounds how MANY
// parts a transfer may have, so without this the per-part size is the
// one term in the memory budget nothing checks.
func TestPlacePartRejectsAnOversizePart(t *testing.T) {
	good := bytes.Repeat([]byte{0x01}, ResourceSDU)
	rr := makeBoundedReceiver(t, [][]byte{good}, ResourceSDU)

	oversize := bytes.Repeat([]byte{0x02}, ResourceSDU+1)
	err := rr.placePart(oversize)
	if !errors.Is(err, errResourcePartOversize) {
		t.Fatalf("placePart(%d bytes) = %v, want errResourcePartOversize", len(oversize), err)
	}
	if got := buffered(rr); got != 0 {
		t.Errorf("%d bytes buffered after rejecting an oversize part", got)
	}
}

// assemble() already rejects a transfer whose total does not match the
// advertisement, but only once every part is buffered. A sender that
// keeps feeding parts is otherwise paid for in memory first and
// rejected second.
func TestPlacePartRejectsPartsPastTheAdvertisedSize(t *testing.T) {
	partA := bytes.Repeat([]byte{0x0A}, 100)
	partB := bytes.Repeat([]byte{0x0B}, 100)
	// The ADV promised room for one of them, not both.
	rr := makeBoundedReceiver(t, [][]byte{partA, partB}, 100)

	if err := rr.placePart(partA); err != nil {
		t.Fatalf("first part rejected: %v", err)
	}
	err := rr.placePart(partB)
	if !errors.Is(err, errResourceOverAdvertisedSize) {
		t.Fatalf("second part = %v, want errResourceOverAdvertisedSize", err)
	}
	if got, want := buffered(rr), 100; got != want {
		t.Errorf("%d bytes buffered, want %d — the over-cap part was kept", got, want)
	}
}

// The bound must not reject a transfer that exactly fills what it
// advertised, which is the ordinary case.
func TestPlacePartAcceptsATransferThatExactlyFits(t *testing.T) {
	partA := bytes.Repeat([]byte{0x0A}, 100)
	partB := bytes.Repeat([]byte{0x0B}, 60)
	rr := makeBoundedReceiver(t, [][]byte{partA, partB}, 160)

	if err := rr.placePart(partA); err != nil {
		t.Fatalf("first part: %v", err)
	}
	if err := rr.placePart(partB); err != nil {
		t.Fatalf("second part: %v", err)
	}
	if got, want := buffered(rr), 160; got != want {
		t.Errorf("buffered %d bytes, want %d", got, want)
	}
}

// A retransmit must not be counted twice — otherwise a lossy link,
// where retransmits are routine, would trip the cumulative bound on a
// perfectly good transfer. Both dispositions a retransmit can get are
// covered here, because SPEC §10.5 changed which one applies where:
// since RNS 1.5.0 the search window starts one PAST the completed
// frontier, so a part at or below it is no longer in the window to be
// recognised as a duplicate. Either way it must not consume budget.
func TestADuplicatePartDoesNotConsumeTheBudget(t *testing.T) {
	t.Run("still inside the search window", func(t *testing.T) {
		partA := bytes.Repeat([]byte{0x0A}, 100)
		partB := bytes.Repeat([]byte{0x0B}, 60)
		rr := makeBoundedReceiver(t, [][]byte{partA, partB}, 160)

		// B arrives first, so the frontier stays at -1 (A is missing)
		// and B's slot is still within the window.
		if err := rr.placePart(partB); err != nil {
			t.Fatalf("first part: %v", err)
		}
		if err := rr.placePart(partB); !errors.Is(err, errResourceDuplicatePart) {
			t.Fatalf("retransmit = %v, want errResourceDuplicatePart", err)
		}
		if err := rr.placePart(partA); err != nil {
			t.Errorf("a retransmit consumed budget the real part needed: %v", err)
		}
		if got, want := buffered(rr), 160; got != want {
			t.Errorf("buffered %d bytes, want %d", got, want)
		}
	})

	t.Run("below the completed frontier", func(t *testing.T) {
		partA := bytes.Repeat([]byte{0x0A}, 100)
		partB := bytes.Repeat([]byte{0x0B}, 60)
		rr := makeBoundedReceiver(t, [][]byte{partA, partB}, 160)

		// A completes slot 0, advancing the frontier past it, so a
		// retransmit of A now falls outside the search window.
		if err := rr.placePart(partA); err != nil {
			t.Fatalf("first part: %v", err)
		}
		if err := rr.placePart(partA); err == nil {
			t.Fatal("retransmit below the frontier was placed")
		}
		if err := rr.placePart(partB); err != nil {
			t.Errorf("a retransmit consumed budget the real part needed: %v", err)
		}
		if got, want := buffered(rr), 160; got != want {
			t.Errorf("buffered %d bytes, want %d", got, want)
		}
	})
}

// TestSearchWindowStartsPastTheFrontier pins SPEC §10.5 as changed in
// RNS 1.5.0 (`search_start = self.consecutive_completed_height+1`,
// Resource.py:868-870). Starting AT the frontier re-examined a slot
// already known to be full on every inbound part and left the window one
// outstanding slot short. A receiver on the old bounds still
// interoperates — the window is a receiver-private search hint, not a
// negotiated value — so this guards the improvement, not the wire.
func TestSearchWindowStartsPastTheFrontier(t *testing.T) {
	partA := bytes.Repeat([]byte{0x0A}, 50)
	partB := bytes.Repeat([]byte{0x0B}, 50)
	rr := makeBoundedReceiver(t, [][]byte{partA, partB}, 100)

	if rr.consecutiveHeight != -1 {
		t.Fatalf("fresh receiver frontier = %d, want -1", rr.consecutiveHeight)
	}
	// At -1 the window must still start at slot 0, or the first part of
	// every transfer would be unplaceable.
	if err := rr.placePart(partA); err != nil {
		t.Fatalf("first part rejected from a fresh receiver: %v", err)
	}
	if rr.consecutiveHeight != 0 {
		t.Fatalf("frontier = %d after slot 0 filled, want 0", rr.consecutiveHeight)
	}
	// Now that slot 0 is complete the scan must begin at slot 1: the
	// next part still places, and the completed slot is no longer
	// re-examined (covered by the retransmit case above).
	if err := rr.placePart(partB); err != nil {
		t.Errorf("second part rejected: %v", err)
	}
}

// TestPlacePartAcceptsPartsFromALargerMTULink is the regression for a
// silent interop break: the per-part bound was measured against the base
// ResourceSDU, but SPEC §10.2 step 6 sizes a part from the LINK's MTU
// (`link.mtu - HEADER_MAXSIZE - IFAC_MIN_SIZE`, upstream
// Resource.py:335). A peer whose §6.6 handshake negotiated a larger MTU
// — the upstream default on any interface whose HW_MTU exceeds 500, e.g.
// TCP — slices its transfer into correspondingly larger parts.
//
// Rejecting those does not surface as an error: the receive loop drops a
// part it cannot place, so the transfer makes no progress and times out
// looking like an unresponsive peer.
func TestPlacePartAcceptsPartsFromALargerMTULink(t *testing.T) {
	const negotiatedMTU = 1064 // a typical TCP-interface HW_MTU
	peerSDU := negotiatedMTU - ReticulumHeaderMaxSize - ReticulumIFACMinSize
	if peerSDU <= ResourceSDU {
		t.Fatalf("premise broken: peer SDU %d is not larger than base %d", peerSDU, ResourceSDU)
	}

	partA := bytes.Repeat([]byte{0x0A}, peerSDU)
	partB := bytes.Repeat([]byte{0x0B}, 200)
	rr := makeBoundedReceiverWithSDU(t, [][]byte{partA, partB}, peerSDU+200, peerSDU)

	if err := rr.placePart(partA); err != nil {
		t.Fatalf("rejected a legitimate %d-byte part from a link negotiated at MTU %d: %v",
			len(partA), negotiatedMTU, err)
	}
	if err := rr.placePart(partB); err != nil {
		t.Fatalf("second part: %v", err)
	}
	// The bound still bites above the link's own SDU.
	over := bytes.Repeat([]byte{0x0C}, peerSDU+1)
	if err := rr.placePart(over); !errors.Is(err, errResourcePartOversize) {
		t.Errorf("part larger than the link SDU = %v, want errResourcePartOversize", err)
	}
}

// TestLinkSDUTracksNegotiatedMTU covers the plumbing the bound depends
// on: a link records what the §6.6 handshake settled (upstream
// Link.validate_request / validate_proof), and never reports an SDU
// below the base one — a peer that signalled an implausibly small MTU
// still sends at least base-sized parts.
func TestLinkSDUTracksNegotiatedMTU(t *testing.T) {
	for _, c := range []struct {
		name string
		mtu  uint32
		want int
	}{
		{"no signalling falls back to base", 0, ResourceSDU},
		{"base MTU", ReticulumMTU, ResourceSDU},
		{"negotiated larger", 1064, 1064 - ReticulumHeaderMaxSize - ReticulumIFACMinSize},
		{"implausibly small never goes below base", 60, ResourceSDU},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := &Link{MTU: c.mtu}
			if got := l.SDU(); got != c.want {
				t.Errorf("SDU() = %d, want %d", got, c.want)
			}
		})
	}
}

// TestResourceConstantsMatchUpstream pins the four derived constants
// against the values upstream computes at the versions the spec repo's
// tools/requirements.txt pins (rns 1.4.2):
//
//	RNS.Link.MDU                             = 431
//	RNS.Resource.SDU                         = 464
//	ResourceAdvertisement.HASHMAP_MAX_LEN    = 74
//	ResourceAdvertisement.COLLISION_GUARD_SIZE = 224
//
// These are GLOBAL on both sides — upstream derives them from the
// class-level RNS.Link.MDU, not from any link's negotiated MTU — so both
// peers compute the same values regardless of what §6.6 settled, and
// hashmap segment arithmetic stays in step. The one quantity upstream
// makes per-link is Resource.sdu (Resource.py:335), which is why
// ResourceReceiver.partSDU exists and why nothing else here follows the
// link MTU. Drift in any of these is a silent interop break: segment
// indices stop lining up and multi-segment transfers stall.
func TestResourceConstantsMatchUpstream(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"Link.MDU", LinkMDU, 431},
		{"Resource.SDU", ResourceSDU, 464},
		{"HASHMAP_MAX_LEN", HashmapMaxLen, 74},
		{"COLLISION_GUARD_SIZE", CollisionGuardSize, 224},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, upstream computes %d", c.name, c.got, c.want)
		}
	}
}

// TestSplitPartsFollowsTheLinkSDU is the send-side half of the per-link
// SDU rule (SPEC §10.2 step 6). It matters because a receiver does NOT
// trust the part count we advertise: upstream computes
// `total_parts = ceil(size/sdu)` from its OWN sdu
// (RNS/Resource.py:187) and sizes its parts array from that. Split at
// the base 464 on a link that negotiated 1064 and the peer allocates
// roughly half the slots we are about to fill.
func TestSplitPartsFollowsTheLinkSDU(t *testing.T) {
	const negotiatedMTU = 1064
	linkSDU := negotiatedMTU - ReticulumHeaderMaxSize - ReticulumIFACMinSize
	body := bytes.Repeat([]byte{0x5A}, 4000)

	parts := SplitPartsWithSDU(body, linkSDU)
	want := (len(body) + linkSDU - 1) / linkSDU
	if len(parts) != want {
		t.Errorf("split into %d parts at sdu %d, want %d", len(parts), linkSDU, want)
	}
	// This is the count a conformant peer on that link derives.
	if got := (len(body) + linkSDU - 1) / linkSDU; len(parts) != got {
		t.Errorf("part count %d disagrees with the peer's ceil(size/sdu) = %d", len(parts), got)
	}
	for i, p := range parts[:len(parts)-1] {
		if len(p) != linkSDU {
			t.Fatalf("part %d is %d bytes, want a full %d-byte slice", i, len(p), linkSDU)
		}
	}
	// Reassembly must be lossless regardless of where the cuts fall.
	var joined []byte
	for _, p := range parts {
		joined = append(joined, p...)
	}
	if !bytes.Equal(joined, body) {
		t.Error("parts do not reassemble to the original body")
	}
	// Splitting the same body at the base SDU yields a different count —
	// which is precisely the disagreement this guards against.
	if base := SplitParts(body); len(base) == len(parts) {
		t.Errorf("premise broken: base and link SDU produced the same count %d", len(base))
	}
	// A non-positive sdu must not divide by zero.
	if got := len(SplitPartsWithSDU(body, 0)); got != len(SplitParts(body)) {
		t.Errorf("sdu=0 produced %d parts, want the base-SDU count %d", got, len(SplitParts(body)))
	}
}
