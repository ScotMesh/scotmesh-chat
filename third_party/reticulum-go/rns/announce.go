package rns

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// safeUnmarshalAnnounce wraps msgpack.Unmarshal for inbound
// attacker-controlled announce app_data. Centralised here so a future
// stricter cap (e.g. switching msgpack libraries) lands in one place.
// vmihailenco/msgpack/v5's built-in allocation limit already bounds
// memory during a single decode, which is the practical defense
// against decoder bombs.
func safeUnmarshalAnnounce(data []byte, v any) error {
	// Announce app_data is fully attacker-controlled and reaches this
	// decoder pre-authentication (an attacker signs their own announce,
	// so verification proves nothing about trust). The pinned msgpack
	// library's allocation limit is broken — see msgpack_guard.go — so
	// structure is validated before decoding.
	if err := ValidateMsgpackBounds(data); err != nil {
		return err
	}
	return msgpack.Unmarshal(data, v)
}

// Announce wire body (SPEC §4.1):
//
//	public_key(64) || name_hash(10) || random_hash(10) || [ratchet_pub(32)] || signature(64) || app_data
//
// signed_data over which the Ed25519 signature is computed (SPEC §4.2):
//
//	dest_hash(16) || public_key(64) || name_hash(10) || random_hash(10) || [ratchet_pub] || app_data
//
// dest_hash comes from the OUTER packet header, not the announce body.
// ratchet_pub is empty bytes (b"") — not absent — in signed_data when
// context_flag == 0.
const (
	announceMinNoRatchet = PublicKeyLen + NameHashLen + 10 + 64 // 148
	announceMinWithRatch = announceMinNoRatchet + 32            // 180
	ratchetPubLen        = 32
	announceSigLen       = 64
	randomHashLen        = 10
)

// Announce is a parsed and validated (or unvalidated) announce.
type Announce struct {
	DestHash    []byte // from outer packet header (16 bytes)
	PublicKey   []byte // 64 bytes
	NameHash    []byte // 10 bytes
	RandomHash  []byte // 10 bytes
	RatchetPub  []byte // 32 bytes when context_flag == 1, nil otherwise
	Signature   []byte // 64 bytes
	AppData     []byte // may be empty
	ContextFlag bool
	Hops        byte

	// TransportID is the next-hop transport node's identity hash, taken
	// from the outer packet header when the announce arrived as HEADER_2
	// (i.e. the announcer is multiple hops away and the announce was
	// relayed). Nil for HEADER_1 announces from direct neighbors. When
	// non-nil, callers should use HEADER_2 with this transport_id when
	// sending DATA back to the announcer's destination, per SPEC §2.3.
	TransportID []byte
}

// EmittedAt returns the timestamp half of random_hash decoded as a unix-time
// seconds value (SPEC §4.1).
func (a *Announce) EmittedAt() (time.Time, error) {
	if len(a.RandomHash) != randomHashLen {
		return time.Time{}, fmt.Errorf("random_hash must be %d bytes, got %d", randomHashLen, len(a.RandomHash))
	}
	secs, err := DecodeBigEndianUint40(a.RandomHash[5:])
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(secs), 0), nil
}

// BuildAnnounce constructs and signs a new announce for `fullName` (e.g.
// "lxmf.delivery"), wraps it in a HEADER_1 ANNOUNCE packet, and returns
// the packet ready for transmission.
//
// `appData` is the application-defined payload (for an LXMF delivery
// destination, build it via EncodeLXMFAppData). Pass nil for none.
//
// `ratchetPub`, if non-nil, MUST be 32 bytes and turns on context_flag.
// Pass nil to omit (recommended for the first-cut implementation —
// SPEC §7.3 ratchet rotation is deferred).
//
// The packet's Context is ContextNone (regular announce). To produce a
// path-response announce (SPEC §7.2, context = ContextPathResponse),
// call BuildAnnounceWithContext.
func BuildAnnounce(id *Identity, fullName string, appData []byte, ratchetPub []byte) (*Packet, error) {
	return BuildAnnounceWithContext(id, fullName, appData, ratchetPub, ContextNone)
}

// BuildAnnounceWithContext is BuildAnnounce with an explicit context
// byte. Used by the Transport's path-request responder to emit
// path-response announces (context = 0x0B) — same body bytes as a
// regular announce so any signature-verifying client can validate
// either form.
func BuildAnnounceWithContext(id *Identity, fullName string, appData []byte, ratchetPub []byte, context byte) (*Packet, error) {
	return buildAnnounce(id, fullName, appData, ratchetPub, context, time.Now, randReader)
}

// buildAnnounce is the testable form: the clock and randomness source are
// injected so tests can produce deterministic announces.
func buildAnnounce(
	id *Identity,
	fullName string,
	appData []byte,
	ratchetPub []byte,
	context byte,
	now func() time.Time,
	rnd func(p []byte) (int, error),
) (*Packet, error) {
	if id == nil {
		return nil, errors.New("nil identity")
	}
	if ratchetPub != nil && len(ratchetPub) != ratchetPubLen {
		return nil, fmt.Errorf("ratchet_pub must be %d bytes, got %d", ratchetPubLen, len(ratchetPub))
	}

	nameHash := NameHash(fullName)
	destHash := DestinationHash(nameHash, id.Hash())

	// random_hash = 5 random || 5 BE uint40 unix seconds
	rh := make([]byte, randomHashLen)
	if _, err := rnd(rh[:5]); err != nil {
		return nil, fmt.Errorf("random_hash entropy: %w", err)
	}
	ts := BigEndianUint40(uint64(now().Unix()))
	copy(rh[5:], ts[:])

	signedData := buildAnnounceSignedData(destHash, id.PublicKey(), nameHash, rh, ratchetPub, appData)
	sig := id.Sign(signedData)

	// Assemble wire body: pubkey || name_hash || random_hash || [ratchet] || sig || app_data
	bodyLen := announceMinNoRatchet + len(appData)
	if ratchetPub != nil {
		bodyLen += ratchetPubLen
	}
	// SPEC §4.5: an announce must fit the 500-byte Reticulum MTU.
	//
	// Upstream has always refused to emit one that doesn't — Packet.pack
	// raises when len(raw) > self.MTU, and for any non-LINK destination
	// that MTU is Reticulum.MTU = 500 (Packet.py:157, :238), long-standing
	// behaviour rather than new. RNS 1.5.2 added the matching receive-side
	// rule: Transport.preprocess_inbound drops an ANNOUNCE whose raw frame
	// exceeds Reticulum.MTU as a protocol violation, *before*
	// validate_announce runs (Transport.py:1804). That closes a real gap,
	// since an interface's own HW_MTU permits up to 512 KiB, so an
	// oversized announce used to reach full signature validation.
	//
	// Packet.Pack can't carry this bound generally — a link destination's
	// MTU is negotiated in §6 signalling and may exceed 500 — so enforce
	// it here, where the destination type is known.
	if header1MinLen+bodyLen > ReticulumMTU {
		return nil, fmt.Errorf(
			"announce frame would be %d bytes, over the %d-byte MTU (SPEC §4.5): app_data is %d bytes, at most %d fits",
			header1MinLen+bodyLen, ReticulumMTU, len(appData),
			ReticulumMTU-header1MinLen-(bodyLen-len(appData)))
	}

	body := make([]byte, 0, bodyLen)
	body = append(body, id.PublicKey()...)
	body = append(body, nameHash...)
	body = append(body, rh...)
	if ratchetPub != nil {
		body = append(body, ratchetPub...)
	}
	body = append(body, sig...)
	body = append(body, appData...)

	return &Packet{
		HeaderType:      HeaderType1,
		ContextFlag:     ratchetPub != nil,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationSingle,
		PacketType:      PacketAnnounce,
		Hops:            0,
		DestHash:        destHash,
		Context:         context,
		Data:            body,
	}, nil
}

// ParseAnnounce extracts the announce body from a packet (whose
// PacketType MUST be PacketAnnounce). The returned Announce is NOT yet
// signature-verified — call Verify() before trusting any fields.
func ParseAnnounce(p *Packet) (*Announce, error) {
	if p == nil {
		return nil, errors.New("nil packet")
	}
	if p.PacketType != PacketAnnounce {
		return nil, fmt.Errorf("packet_type %d is not ANNOUNCE", p.PacketType)
	}
	body := p.Data

	a := &Announce{
		DestHash:    p.DestHash,
		ContextFlag: p.ContextFlag,
		Hops:        p.Hops,
		TransportID: p.TransportID,
	}

	if p.ContextFlag {
		if len(body) < announceMinWithRatch {
			return nil, fmt.Errorf("announce body too short for ratchet form: %d", len(body))
		}
		a.PublicKey = body[0:PublicKeyLen]
		a.NameHash = body[PublicKeyLen : PublicKeyLen+NameHashLen]
		a.RandomHash = body[PublicKeyLen+NameHashLen : PublicKeyLen+NameHashLen+randomHashLen]
		a.RatchetPub = body[PublicKeyLen+NameHashLen+randomHashLen : PublicKeyLen+NameHashLen+randomHashLen+ratchetPubLen]
		sigStart := PublicKeyLen + NameHashLen + randomHashLen + ratchetPubLen
		a.Signature = body[sigStart : sigStart+announceSigLen]
		a.AppData = body[sigStart+announceSigLen:]
	} else {
		if len(body) < announceMinNoRatchet {
			return nil, fmt.Errorf("announce body too short: %d", len(body))
		}
		a.PublicKey = body[0:PublicKeyLen]
		a.NameHash = body[PublicKeyLen : PublicKeyLen+NameHashLen]
		a.RandomHash = body[PublicKeyLen+NameHashLen : PublicKeyLen+NameHashLen+randomHashLen]
		sigStart := PublicKeyLen + NameHashLen + randomHashLen
		a.Signature = body[sigStart : sigStart+announceSigLen]
		a.AppData = body[sigStart+announceSigLen:]
	}
	return a, nil
}

// Verify performs SPEC §4.5 steps 2 + 3: signature verification and
// destination-hash recomputation. Returns nil iff the announce is valid.
func (a *Announce) Verify() error {
	if len(a.PublicKey) != PublicKeyLen || len(a.Signature) != announceSigLen {
		return errors.New("announce: malformed pubkey or signature length")
	}

	// CHEAP CHECK FIRST. Both checks must pass, so the order is purely
	// about cost: the destination-hash recompute is two SHA-256 ops
	// (~1 µs) while Ed25519 verification is ~50 µs. Announces arrive
	// pre-authentication on a shared hub and are verified inline on the
	// single dispatcher goroutine, so doing the expensive one first
	// handed an attacker a ~50x CPU amplifier for garbage input. A
	// determined attacker can still supply a self-consistent dest_hash,
	// so this is a cost reduction, not a defense.
	idHash := sha256.Sum256(a.PublicKey)
	expected := DestinationHash(a.NameHash, idHash[:IdentityHashLen])
	if !bytesEqual(expected, a.DestHash) {
		return fmt.Errorf("announce: destination_hash mismatch (got %x, derived %x)", a.DestHash, expected)
	}

	signed := buildAnnounceSignedData(a.DestHash, a.PublicKey, a.NameHash, a.RandomHash, a.RatchetPub, a.AppData)
	ed25519Pub := a.PublicKey[32:]
	if !Validate(ed25519Pub, signed, a.Signature) {
		return errors.New("announce: Ed25519 signature invalid")
	}
	return nil
}

func buildAnnounceSignedData(destHash, pubKey, nameHash, randomHash, ratchetPub, appData []byte) []byte {
	out := make([]byte, 0, len(destHash)+len(pubKey)+len(nameHash)+len(randomHash)+len(ratchetPub)+len(appData))
	out = append(out, destHash...)
	out = append(out, pubKey...)
	out = append(out, nameHash...)
	out = append(out, randomHash...)
	out = append(out, ratchetPub...) // empty when context_flag == 0
	out = append(out, appData...)
	return out
}

// EncodeLXMFAppData builds the msgpack app_data blob for an lxmf.delivery
// announce per SPEC §4.3:
//
//	[display_name_bytes (msgpack bin), stamp_cost (int or nil)]
//
// display_name MUST be encoded as msgpack `bin` type — encoders that
// emit msgpack `str` will produce app_data that upstream parsers reject
// (SPEC §9.3).
func EncodeLXMFAppData(displayName []byte, stampCost *int) ([]byte, error) {
	// vmihailenco/msgpack/v5 encodes Go []byte as msgpack bin and Go nil
	// as msgpack nil, which is exactly what we want.
	var stampField any
	if stampCost != nil {
		stampField = *stampCost
	}
	// The slice element type must be `any` so the encoder picks per-element
	// types instead of an array-typed homogeneous encoding.
	// app_data is covered by the §4.2 announce signature we produce,
	// so canonical encoding is not load-bearing here — stampCost is a
	// Go *int and already encodes compactly. Kept uniform with every
	// other emit site; see rns/msgpack_canonical.go.
	return canonicalMarshal([]any{displayName, stampField})
}

// DecodeLXMFAppDataDisplayName extracts the display_name from an LXMF
// announce app_data.
//
// Only call this once you KNOW the announce is on the lxmf.delivery
// aspect — prefer LXMFDisplayNameFromAnnounce, which checks. SPEC §4.6:
// app_data is opaque bytes to RNS and its shape does not tell you which
// protocol produced it, so this function cannot and does not report
// "that was not LXMF". Fed a non-LXMF payload it returns confident
// nonsense, exactly as upstream's display_name_from_app_data does.
//
// Tolerant of:
//   - 1-element msgpack array [bin]
//   - 2-element msgpack array [bin, stamp_cost]
//   - 3-element msgpack array [bin, stamp_cost, capability_flags]
//   - raw UTF-8 string (legacy "original announce format" — SPEC §4.3)
//
// Returns nil display_name + nil error if app_data is empty.
func DecodeLXMFAppDataDisplayName(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	// Try msgpack array first.
	var arr []msgpack.RawMessage
	if err := safeUnmarshalAnnounce(data, &arr); err == nil {
		if len(arr) == 0 {
			// A well-formed but EMPTY array carries no name. Falling
			// through to the legacy branch here would hand back the
			// 0x90 fixarray framing byte as though it were the name;
			// upstream's msgpack branch returns None. No legitimate
			// legacy name can reach this, since 0x90-0x9f and 0xdc are
			// not valid UTF-8 lead bytes. SPEC §4.6.
			return nil, nil
		}
		var name []byte
		// Element 0 may be bin (preferred) or str.
		if uerr := safeUnmarshalAnnounce(arr[0], &name); uerr == nil {
			return name, nil
		}
		var nameStr string
		if uerr := safeUnmarshalAnnounce(arr[0], &nameStr); uerr == nil {
			return []byte(nameStr), nil
		}
		return nil, errors.New("app_data: first element neither bin nor str")
	}
	// Fall back to legacy raw-UTF8 form.
	return data, nil
}

// lxmfDeliveryNameHash is NameHash("lxmf.delivery"), the well-known
// 6ec60bc318e2c0f0d908 (SPEC §1.2).
var lxmfDeliveryNameHash = NameHash(FullName("lxmf", "delivery"))

// LXMFDisplayNameFromAnnounce returns the display_name an announce
// carries, and nil for any announce that is not on the lxmf.delivery
// aspect. This is the safe way to label an announce on a promiscuous
// listener, which sees every aspect on the mesh.
//
// SPEC §4.6: the msgpack `[name, stamp_cost, [flags]]` array of §4.3 is
// LXMF's convention for its own destinations, not an RNS rule — to RNS
// app_data is opaque bytes that Destination.announce appends unexamined,
// and other application protocols encode it however their own specs say.
// Key the parser on name_hash (§4.4), NEVER on the shape of the bytes:
// the shape does not tell you, and asking it does not fail loudly.
// Upstream's display_name_from_app_data dispatches on the first byte
// alone, and two of its three non-LXMF outcomes look like success — a
// CBOR text string comes back as a name with the length header glued on
// as a character (`67 "hubname"` reads as "ghubname"), and a CBOR
// 16-item array leads with 0x90, lands in msgpack's fixarray range and
// returns "no name at all". Verified by the spec repo's
// tools/verify_app_data_dispatch.py.
func LXMFDisplayNameFromAnnounce(a *Announce) []byte {
	if a == nil || !bytes.Equal(a.NameHash, lxmfDeliveryNameHash) {
		return nil
	}
	name, _ := DecodeLXMFAppDataDisplayName(a.AppData)
	return name
}

// DecodeLXMFAppDataStampCost extracts the stamp_cost an LXMF delivery
// destination announces in element [1] of its app_data (SPEC §4.3 /
// §5.7.4) — "you must do this much proof-of-work to message me".
//
// Returns 0 for every shape that carries no demand: empty app_data, the
// legacy raw-UTF8 display-name form, a 1-element array, or an explicit
// msgpack nil. Those are indistinguishable in effect and all mean "no
// stamp required".
//
// An element [1] that is present but not a non-negative integer is an
// error rather than a silent 0: the peer is announcing something about
// its stamp policy that we failed to understand, and treating that as
// "no stamp" is how a sender ends up silently dropped by a recipient
// that enforces stamps (§5.7.4).
func DecodeLXMFAppDataStampCost(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	var arr []msgpack.RawMessage
	if err := safeUnmarshalAnnounce(data, &arr); err != nil {
		// Legacy raw-UTF8 announce (name only, no stamp field).
		return 0, nil
	}
	if len(arr) < 2 {
		return 0, nil
	}
	// A nil element decodes to an empty RawMessage under this msgpack
	// library, so both shapes mean "announced, but no demand".
	if len(arr[1]) == 0 || arr[1][0] == msgpackNilByte {
		return 0, nil
	}
	var cost int64
	if err := safeUnmarshalAnnounce(arr[1], &cost); err != nil {
		return 0, fmt.Errorf("app_data: stamp_cost is not an integer: %w", err)
	}
	// A uint64 envelope holding more than MaxInt64 wraps to a negative
	// int64 — 0xcf ff..ff arrives as -1 — which the "< 1 means no stamp"
	// rule below would read as a peer politely asking for nothing,
	// silently swallowing a malformed announce. The type byte is the only
	// reliable discriminator: decoding into uint64 instead does not help,
	// because this library wraps a negative fixint the other way just as
	// happily. Found by FuzzDecodeLXMFAppDataStampCost.
	if arr[1][0] == msgpackUint64Code && cost < 0 {
		return 0, fmt.Errorf("app_data: stamp_cost %d exceeds the %d upstream accepts",
			uint64(cost), maxAnnouncedStampCost)
	}
	// Range per upstream's own setter, LXMRouter.set_inbound_stamp_cost
	// (LXMF/LXMRouter.py:384-390), which is the only way a conformant peer
	// arrives at the value it announces (SPEC §4.3):
	//
	//	cost < 1    -> stored as None, i.e. no stamp required
	//	1..254      -> accepted
	//	cost >= 255 -> REJECTED; the destination keeps its previous cost
	//
	// So a negative cost is not malformed — upstream reads it as "no
	// stamp" — while anything at or above 255 cannot come from a peer
	// that went through the setter, and is treated as a malformed
	// announce. (An earlier revision allowed up to 256 on the theory that
	// a cost is a SHA-256 leading-zero target; that bound is true but
	// looser than anything upstream will emit.)
	if cost < 1 {
		return 0, nil
	}
	if cost > maxAnnouncedStampCost {
		return 0, fmt.Errorf("app_data: stamp_cost %d exceeds the %d upstream accepts",
			cost, maxAnnouncedStampCost)
	}
	return int(cost), nil
}

// msgpackNilByte is the msgpack format byte for nil.
const msgpackNilByte = 0xc0

// msgpackUint64Code is the msgpack type byte for a uint64 envelope.
const msgpackUint64Code = 0xcf

// maxAnnouncedStampCost is the largest stamp_cost a conformant peer can
// announce: upstream's set_inbound_stamp_cost refuses anything >= 255
// (LXMF/LXMRouter.py:387-389), so the accepted range is 1..254.
const maxAnnouncedStampCost = 254

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// randReader is the production randomness source; tests inject deterministic
// alternatives.
func randReader(p []byte) (int, error) { return rand.Read(p) }

// Bounds for a believable announce emission time. EmittedAt decodes a
// 40-bit seconds value out of random_hash, so its full range reaches
// year 36812 — and a peer is free to put anything there.
//
// An implausible value is not merely useless for the freshness
// comparison it feeds; it is actively dangerous to STORE, because
// time.Time refuses to marshal a year outside [0,9999]. Caching one
// such peer failed the ENTIRE announce-cache save, so the cache silently
// stopped persisting and every peer had to be re-learned after a
// restart. (Introduced in v1.14.0, fixed in v1.14.2 — the failure mode
// was one bad peer poisoning a shared structure, which is exactly the
// class this codebase keeps having to defend against.)
var (
	announceTimeFloor = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	announceTimeSkew  = 24 * time.Hour
)

// plausibleEmittedAt returns the announce's signed emission time when it
// is believable, and ok=false otherwise. Callers must treat !ok as "this
// peer has no usable timestamp" rather than substituting a default —
// the value is attacker-controlled.
func plausibleEmittedAt(a *Announce) (time.Time, bool) {
	emitted, err := a.EmittedAt()
	if err != nil {
		return time.Time{}, false
	}
	if emitted.Before(announceTimeFloor) || emitted.After(time.Now().Add(announceTimeSkew)) {
		return time.Time{}, false
	}
	return emitted, true
}
