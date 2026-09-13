package lxmf

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"

	"golang.org/x/crypto/hkdf"
)

// SPEC §5.7 stamps: a 32-byte proof-of-work value over an HKDF-expanded
// workblock. This file implements outbound generation in both flavors:
//
//   - Delivery stamps (§5.7.2, WORKBLOCK_EXPAND_ROUNDS = 3000) ground
//     over the message_id, carried as payload element [4] (§5.7.1) for
//     recipients whose announce declares a stamp_cost (§4.3 / §5.7.4).
//   - Propagation stamps (WORKBLOCK_EXPAND_ROUNDS_PN = 1000) ground over
//     the transient_id and appended raw to the wire body, for nodes that
//     declare a stamp_cost in §5.8.5 element [5][0] (flows/
//     send-propagated-lxmf.md step 5).
//
// Inbound stamp validation (§5.7.2 step 3), enforcement, and tickets
// (§5.7.3) remain out of scope — see CLAUDE.md §2.3.
const (
	// StampSize is LXMessage.STAMP_SIZE — the stamp length in both
	// placements: payload element [4] for delivery stamps (§5.7.1) and the
	// raw suffix of a propagated body for propagation stamps.
	StampSize = 32

	// workblockExpandRoundsPN is WORKBLOCK_EXPAND_ROUNDS_PN: propagation
	// stamps use a cheaper 1000-round (250 KiB) workblock than the
	// 3000-round regular stamps, because store-and-forward already
	// throttles (§5.7.2).
	workblockExpandRoundsPN = 1000

	// workblockExpandRounds is WORKBLOCK_EXPAND_ROUNDS: regular delivery
	// stamps expand 3000 rounds, producing a 768 KiB workblock. The size
	// is the point — it is deliberately too large to sit in cache, which
	// is what limits GPU/ASIC speedup (§5.7.2).
	workblockExpandRounds = 3000

	// workblockRoundLen is the HKDF output length per expansion round.
	workblockRoundLen = 256

	// MaxPropagationStampCost caps the stamp_cost this implementation
	// will grind for. The cost comes from an announce field that any
	// stranger can set, so without a cap a hostile node announcing
	// cost=200 would pin a CPU forever.
	//
	// Lowered from 24 to 16 after the v1.13.1 audit. The grind is per
	// message PER RECIPIENT and runs inside an outbound worker, so at
	// cost 24 (~16.7M expected iterations) a hostile node could turn one
	// group message into hundreds of CPU-seconds and saturate the whole
	// worker pool. 2^16 is ~256x cheaper and still far above real-world
	// node policies, which are typically 0-8. Nodes demanding more are
	// filtered out at SELECTION time (see service.propagationTracker) so
	// the refusal never costs a message.
	MaxPropagationStampCost = 20

	// MaxDeliveryStampCost is the same guard for §5.7.2 delivery stamps,
	// whose cost comes from element [1] of the recipient's own announce
	// app_data (§4.3) — equally stranger-controlled, and with no
	// equivalent of the propagation path's escape: there is no alternative
	// recipient to select, so refusing costs the message.
	//
	// 20 is where the measured curve still sits comfortably: the 3000-round
	// workblock is built ONCE at ~30ms (each subsequent attempt resumes a
	// SHA-256 midstate over 32 bytes, it does not re-walk 768 KiB), and a
	// full grind at this cap averages ~210ms, worst observed ~410ms. That
	// is ~250x above the 0-8 costs real deployments announce while staying
	// far short of pinning a core. Past the cap Send fails with
	// ErrStampCostTooHigh rather than silently shipping unstamped, since a
	// recipient enforcing stamps would drop that message anyway (§5.7.4).
	// Per-Delivery override: Delivery.MaxStampCost.
	MaxDeliveryStampCost = 20
)

// ErrStampCostTooHigh is returned when a peer demands more proof-of-work
// than the local cap allows — MaxDeliveryStampCost for a recipient's
// announced stamp_cost, MaxPropagationStampCost for a propagation node's.
var ErrStampCostTooHigh = errors.New("stamp cost exceeds local limit")

// stampWorkblock builds the §5.7.2 workblock: `rounds` iterations of
// 256-byte HKDF-SHA256 output where round n uses ikm=material and
// salt=SHA256(material || msgpack(n)).
func stampWorkblock(material []byte, rounds int) ([]byte, error) {
	out := make([]byte, 0, rounds*workblockRoundLen)
	for n := 0; n < rounds; n++ {
		// Hash input: the salt must match upstream's
		// SHA256(material || umsgpack.packb(n)) exactly or every stamp
		// we generate and validate is wrong (SPEC §5.7.2). A Go `int`
		// already encodes compactly, so this is unchanged bytes — made
		// explicit so it stays that way.
		packedN, err := canonicalMarshal(n)
		if err != nil {
			return nil, fmt.Errorf("marshal round counter: %w", err)
		}
		salt := sha256.Sum256(append(append([]byte{}, material...), packedN...))
		r := hkdf.New(sha256.New, material, salt[:], nil)
		block := make([]byte, workblockRoundLen)
		if _, err := r.Read(block); err != nil {
			return nil, fmt.Errorf("HKDF expand round %d: %w", n, err)
		}
		out = append(out, block...)
	}
	return out, nil
}

// leadingZeroBits counts consecutive zero bits from the front of b.
func leadingZeroBits(b []byte) int {
	n := 0
	for _, by := range b {
		if by == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(by)
	}
	return n
}

// stampDigest is SHA256(workblock || stamp), the value both stamp_valid
// and stamp_value are defined over (§5.7.2).
func stampDigest(workblock, stamp []byte) []byte {
	h := sha256.New()
	h.Write(workblock)
	h.Write(stamp)
	return h.Sum(nil)
}

// stampValid reports whether SHA256(workblock || stamp) clears
// `cost` leading zero bits (§5.7.2 stamp_valid).
func stampValid(stamp []byte, cost int, workblock []byte) bool {
	return leadingZeroBits(stampDigest(workblock, stamp)) >= cost
}

// GeneratePropagationStamp grinds a §5.7.2 stamp over the PN workblock
// (1000 expansion rounds) for the given transient_id. Blocks until a
// stamp clearing `cost` leading zero bits is found — expected 2^cost
// attempts, each a cheap mid-state SHA-256 resumption rather than a full
// 250 KiB re-hash. cost <= 0 returns (nil, nil): no stamp required.
func GeneratePropagationStamp(transientID []byte, cost int) ([]byte, error) {
	return generateStamp(transientID, cost, workblockExpandRoundsPN, MaxPropagationStampCost)
}

// GenerateDeliveryStamp grinds a §5.7.2 regular delivery stamp over the
// 3000-round workblock for the given 32-byte message_id — the value a
// recipient who announced a stamp_cost (§5.7.4) expects to find as
// payload element [4].
//
// The material is the message_id computed over the FOUR-element payload
// (SPEC §5.5), i.e. the same value the recipient re-derives after
// stripping the stamp. Grinding over the stamped payload's own id would
// be circular and produce a stamp no recipient can validate.
//
// Blocks until a stamp clearing `cost` leading zero bits is found —
// expected 2^cost attempts. cost <= 0 returns (nil, nil): no stamp
// required.
func GenerateDeliveryStamp(messageID []byte, cost int) ([]byte, error) {
	return generateStamp(messageID, cost, workblockExpandRounds, MaxDeliveryStampCost)
}

// generateStamp is the search shared by both stamp flavors: expand
// `material` into a workblock at `rounds` rounds, then find a 32-byte
// value whose SHA-256 digest over the workblock clears `cost` leading
// zero bits.
func generateStamp(material []byte, cost, rounds, maxCost int) ([]byte, error) {
	if cost <= 0 {
		return nil, nil
	}
	if cost > maxCost {
		return nil, fmt.Errorf("%w: %d bits requested, local limit is %d",
			ErrStampCostTooHigh, cost, maxCost)
	}
	if len(material) == 0 {
		return nil, errors.New("stamp material is empty")
	}
	workblock, err := stampWorkblock(material, rounds)
	if err != nil {
		return nil, err
	}

	// Hash the workblock once and snapshot the SHA-256 midstate, then
	// resume from the snapshot per candidate. Turns each attempt from a
	// 250 KiB hash into a 32-byte one.
	base := sha256.New()
	base.Write(workblock)
	state, err := base.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("snapshot hash state: %w", err)
	}

	// One CSPRNG read for the base, then vary a counter suffix per
	// candidate. Reading 32 fresh random bytes per iteration made the
	// syscall — not the hash — the dominant cost of the search, for no
	// security benefit: the candidates only need to be distinct and
	// unpredictable to the node, which a random base plus a counter
	// already provides.
	stamp := make([]byte, StampSize)
	if _, err := rand.Read(stamp); err != nil {
		return nil, fmt.Errorf("stamp base: %w", err)
	}
	digestBuf := make([]byte, 0, sha256.Size)
	var counter uint64
	for {
		counter++
		binary.BigEndian.PutUint64(stamp[StampSize-8:], counter)
		h := sha256.New()
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
			return nil, fmt.Errorf("restore hash state: %w", err)
		}
		h.Write(stamp)
		digest := h.Sum(digestBuf[:0])
		if leadingZeroBits(digest) >= cost {
			return append([]byte(nil), stamp...), nil
		}
	}
}
