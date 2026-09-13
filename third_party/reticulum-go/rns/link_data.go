package rns

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
)

// Link DATA wire format (SPEC §6.4): a regular DATA packet whose outer
// header has dest_type=LINK and dest_hash=link_id; the body is the
// link-form Token ciphertext (no eph_pub prefix; session keys are
// pre-derived from the handshake).
//
// Link DATA proofs (SPEC §6.5.6) are always the explicit 96-byte form
// (packet_hash || signature). The signature is computed with the
// link-derived signing key (used as an Ed25519 seed), NOT either side's
// long-term Ed25519 priv. Both sides share the same session keys, so
// either can sign and either can verify.

// BuildLinkDataPacket encrypts plaintext under the link's session keys
// and wraps the ciphertext in a Reticulum DATA packet addressed to the
// link.
func BuildLinkDataPacket(linkID, signing, encryption, plaintext []byte) (*Packet, error) {
	if len(linkID) != IdentityHashLen {
		return nil, fmt.Errorf("link_id must be %d bytes, got %d", IdentityHashLen, len(linkID))
	}
	ciphertext, err := LinkTokenEncrypt(plaintext, signing, encryption)
	if err != nil {
		return nil, fmt.Errorf("link encrypt: %w", err)
	}
	return &Packet{
		HeaderType:      HeaderType1,
		ContextFlag:     false,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationLink,
		PacketType:      PacketData,
		Hops:            0,
		DestHash:        linkID,
		Context:         ContextNone,
		Data:            ciphertext,
	}, nil
}

// ParseLinkDataPacket decrypts the payload of a DATA packet that
// arrived addressed to this link's link_id. Verifies the wire form is
// link DATA (dest_type=LINK), then runs the link-form Token decryptor.
func ParseLinkDataPacket(p *Packet, signing, encryption []byte) ([]byte, error) {
	if p == nil {
		return nil, errors.New("nil packet")
	}
	if p.PacketType != PacketData {
		return nil, fmt.Errorf("packet_type %d is not DATA", p.PacketType)
	}
	if p.DestinationType != DestinationLink {
		return nil, fmt.Errorf("dest_type %d is not LINK", p.DestinationType)
	}
	if p.Context != ContextNone {
		return nil, fmt.Errorf("link DATA context = 0x%02x, want 0x00", p.Context)
	}
	return LinkTokenDecrypt(p.Data, signing, encryption)
}

// LINKIDENTIFY (SPEC §6.7.6, context = 0xFB).
//
// The initiator proves which long-term identity is driving an already
// established Link, without re-running the handshake. The body is
//
//	public_key(64) || signature(64)
//
// where public_key is the full announced key (X25519 pub || Ed25519 pub,
// §1.1) and signature is over link_id(16) || public_key(64) — NOT over
// link_id alone, which §6.7.6 calls out explicitly as the mistake that
// makes every allow-listed request fail.
const (
	// LinkIdentifySigLen is the Ed25519 signature half of the body.
	LinkIdentifySigLen = 64
	// LinkIdentifyBodyLen is the whole decrypted §6.7.6 body.
	LinkIdentifyBodyLen = PublicKeyLen + LinkIdentifySigLen
)

// ParseLinkIdentifyPacket decrypts a §6.7.6 LINKIDENTIFY packet and
// splits its body.
//
// The packet is ordinary link DATA as far as the wire is concerned —
// §6.7.6 notes that context 0xFB is NOT in upstream Packet.pack()'s
// not-encrypted set, unlike KEEPALIVE (§6.7.1) or link PROOF (§6.5) —
// so it decrypts with the same link-derived Token form as any other
// link DATA.
func ParseLinkIdentifyPacket(p *Packet, signing, encryption []byte) (pubKey, signature []byte, err error) {
	if p == nil {
		return nil, nil, errors.New("nil packet")
	}
	if p.PacketType != PacketData {
		return nil, nil, fmt.Errorf("packet_type %d is not DATA", p.PacketType)
	}
	if p.DestinationType != DestinationLink {
		return nil, nil, fmt.Errorf("dest_type %d is not LINK", p.DestinationType)
	}
	if p.Context != ContextLinkIdentify {
		return nil, nil, fmt.Errorf("context = 0x%02x, want 0x%02x", p.Context, ContextLinkIdentify)
	}
	plaintext, err := LinkTokenDecrypt(p.Data, signing, encryption)
	if err != nil {
		return nil, nil, err
	}
	if len(plaintext) != LinkIdentifyBodyLen {
		return nil, nil, fmt.Errorf("LINKIDENTIFY body is %d bytes, want %d", len(plaintext), LinkIdentifyBodyLen)
	}
	return plaintext[:PublicKeyLen], plaintext[PublicKeyLen:], nil
}

// VerifyLinkIdentify checks a §6.7.6 signature: Ed25519 over
// link_id || public_key, against the Ed25519 half of public_key.
func VerifyLinkIdentify(linkID, pubKey, signature []byte) bool {
	if len(linkID) != IdentityHashLen || len(pubKey) != PublicKeyLen || len(signature) != LinkIdentifySigLen {
		return false
	}
	signed := make([]byte, 0, len(linkID)+len(pubKey))
	signed = append(signed, linkID...)
	signed = append(signed, pubKey...)
	// X25519 pub occupies the first 32 bytes; the Ed25519 verification
	// key is the second half (§1.1).
	return Validate(pubKey[PublicKeyLen/2:], signed, signature)
}

// BuildLinkProof builds the explicit-form (96-byte) PROOF packet that
// acknowledges receipt of an inbound link DATA packet (SPEC §6.5.6).
// Per upstream RNS 1.2.0 link DATA proofs are ALWAYS explicit
// regardless of the global use_implicit_proof setting.
//
// The signature is over SHA-256(original.HashablePart()), signed by the
// LOCAL endpoint's Ed25519 key — NOT a link-derived shared key. Per
// upstream RNS/Link.py:279 the responder uses its destination identity's
// long-term sig_prv; the initiator uses an ephemeral sig_prv generated
// at link-creation time and advertised in the LINKREQUEST. Either way
// the local side knows the priv; the remote side has the corresponding
// pub from the handshake (responder pub from LRPROOF body, initiator
// pub from LINKREQUEST body) and uses it to verify.
//
// `sign` is the local sign function — pass id.Sign as a method value
// when signing as the responder (id is the destination identity), or a
// closure over the ephemeral priv when signing as the initiator.
func BuildLinkProof(linkID []byte, sign func([]byte) []byte, original *Packet) (*Packet, error) {
	if len(linkID) != IdentityHashLen {
		return nil, fmt.Errorf("link_id must be %d bytes", IdentityHashLen)
	}
	if sign == nil {
		return nil, errors.New("sign function is nil")
	}
	if original == nil {
		return nil, errors.New("nil original packet")
	}

	hashable, err := original.HashablePart()
	if err != nil {
		return nil, fmt.Errorf("hashable part: %w", err)
	}
	digest := sha256.Sum256(hashable)
	sig := sign(digest[:])
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("sign returned %d bytes, want %d (Ed25519 signature size)", len(sig), ed25519.SignatureSize)
	}

	// Explicit form body: packet_hash || signature
	body := make([]byte, 0, ProofBodyExplicitLen)
	body = append(body, digest[:]...)
	body = append(body, sig...)

	return &Packet{
		HeaderType:      HeaderType1,
		ContextFlag:     false,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationLink,
		PacketType:      PacketProof,
		Hops:            0,
		DestHash:        linkID,
		Context:         ContextNone, // proof-ness is in packet_type, not context
		Data:            body,
	}, nil
}

// ValidateLinkProof verifies an inbound explicit-form link DATA proof
// against the remote endpoint's Ed25519 pubkey (responder pub for
// initiator-side validation, initiator pub for responder-side
// validation). Returns the 32-byte packet_hash on success.
func ValidateLinkProof(p *Packet, peerEd25519Pub []byte) ([]byte, error) {
	if p == nil {
		return nil, errors.New("nil packet")
	}
	if p.PacketType != PacketProof {
		return nil, fmt.Errorf("packet_type %d is not PROOF", p.PacketType)
	}
	if p.DestinationType != DestinationLink {
		return nil, fmt.Errorf("dest_type %d is not LINK", p.DestinationType)
	}
	if p.Context != ContextNone {
		return nil, fmt.Errorf("link DATA proof context = 0x%02x, want 0x00", p.Context)
	}
	if len(p.Data) != ProofBodyExplicitLen {
		return nil, fmt.Errorf("link proof must be explicit form (%d bytes), got %d", ProofBodyExplicitLen, len(p.Data))
	}
	if len(peerEd25519Pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("peer pubkey must be %d bytes, got %d", ed25519.PublicKeySize, len(peerEd25519Pub))
	}

	packetHash := p.Data[:32]
	sig := p.Data[32:]
	if !ed25519.Verify(ed25519.PublicKey(peerEd25519Pub), packetHash, sig) {
		return nil, errors.New("link proof signature invalid")
	}
	return append([]byte(nil), packetHash...), nil
}

// Context byte for KEEPALIVE on a link (SPEC §6 / RNS source).
const ContextKeepalive = 0xFA

// Context byte for LRRTT (link-request round-trip time measurement).
// Sent by the initiator to the responder immediately after validating the
// LRPROOF; carries the measured round-trip time as a msgpack-packed
// float. The responder uses receipt of this packet as the trigger to
// transition its link from HANDSHAKE to ACTIVE — which is what fires the
// destination's link_established_callback (e.g. LXMRouter sets
// resource_strategy=ACCEPT_APP only at that moment). Without LRRTT, the
// responder silently drops Resource ADV / link DATA because its
// resource_strategy is still default ACCEPT_NONE. Upstream:
// RNS/Link.py:440-442 (initiator send) + 534-553 (responder activate).
const ContextLRRTT = 0xFE

// Keepalive sentinel bytes. The link initiator periodically sends a
// ping (0xFF); the responder answers every ping with a pong (0xFE) so
// the initiator's activity timer is refreshed on an otherwise-idle
// link. Verbatim from upstream RNS: send_keepalive emits bytes([0xFF])
// (RNS/Link.py:849) and the responder replies bytes([0xFE]) on an
// inbound 0xFF (RNS/Link.py:1150-1151). KEEPALIVE packets are NOT
// encrypted — the body is the single sentinel byte on the wire
// (RNS/Packet.py:206-209).
const (
	keepalivePing byte = 0xFF
	keepalivePong byte = 0xFE
)

// BuildLinkKeepalive builds the DATA/KEEPALIVE ping packet the link
// initiator sends to refresh the activity timer on an idle link.
func BuildLinkKeepalive(linkID []byte) (*Packet, error) {
	return buildKeepalivePacket(linkID, keepalivePing)
}

// BuildLinkKeepalivePong builds the DATA/KEEPALIVE pong (0xFE) a link
// responder sends in reply to an inbound ping, so the initiator's
// keepalive timer is satisfied and it doesn't mark the link stale.
func BuildLinkKeepalivePong(linkID []byte) (*Packet, error) {
	return buildKeepalivePacket(linkID, keepalivePong)
}

func buildKeepalivePacket(linkID []byte, sentinel byte) (*Packet, error) {
	if len(linkID) != IdentityHashLen {
		return nil, fmt.Errorf("link_id must be %d bytes", IdentityHashLen)
	}
	return &Packet{
		HeaderType:      HeaderType1,
		ContextFlag:     false,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationLink,
		PacketType:      PacketData,
		Hops:            0,
		DestHash:        linkID,
		Context:         ContextKeepalive,
		Data:            []byte{sentinel},
	}, nil
}

// BuildLinkRTT builds the LRRTT packet the initiator sends to the
// responder right after the link transitions to Active on the
// initiator's side. Body is a msgpack-packed float64 carrying the
// measured RTT in seconds. The responder takes max(its own measurement,
// our reported value) — so a small estimate here is fine; the value
// matters less than the act of sending it (which triggers the
// responder's link_established_callback).
func BuildLinkRTT(linkID, signing, encryption []byte, rttSeconds float64) (*Packet, error) {
	if len(linkID) != IdentityHashLen {
		return nil, fmt.Errorf("link_id must be %d bytes", IdentityHashLen)
	}
	body, err := msgpackMarshalFloat64(rttSeconds)
	if err != nil {
		return nil, fmt.Errorf("rtt msgpack: %w", err)
	}
	ciphertext, err := LinkTokenEncrypt(body, signing, encryption)
	if err != nil {
		return nil, fmt.Errorf("rtt encrypt: %w", err)
	}
	return &Packet{
		HeaderType:      HeaderType1,
		ContextFlag:     false,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationLink,
		PacketType:      PacketData,
		Hops:            0,
		DestHash:        linkID,
		Context:         ContextLRRTT,
		Data:            ciphertext,
	}, nil
}

// BuildLinkIdentify constructs the SPEC §6.6 LINKIDENTIFY packet an
// initiator emits on an Active link to prove which identity (and
// therefore which destination) it's driving the link from. Plaintext
// before link-DATA encryption is:
//
//	public_key(64) || ed25519_signature(64)
//
// matching upstream RNS `Link.identify()` (RNS/Link.py: proof_data =
// identity.get_public_key() + signature). The signature covers
// (link_id(16) || public_key(64)). The responder verifies it against
// the public key carried in the payload itself — no announce lookup
// needed — and caches the identity (and the matching destination)
// on the session, so asynchronous follow-up traffic — most importantly
// tap-back reactions on a relayed group bubble — gets routed back
// through the initiator's destination rather than to some peer hash
// the receiving app inferred from the LXMF body.
//
// `id` is the LOCAL endpoint's long-term identity; it signs. The link's
// session signing/encryption keys protect the wire bytes.
func BuildLinkIdentify(linkID []byte, signing, encryption []byte, id *Identity) (*Packet, error) {
	if len(linkID) != IdentityHashLen {
		return nil, fmt.Errorf("link_id must be %d bytes", IdentityHashLen)
	}
	if id == nil {
		return nil, errors.New("nil identity")
	}
	pubkey := id.PublicKey()
	signedData := make([]byte, 0, len(linkID)+len(pubkey))
	signedData = append(signedData, linkID...)
	signedData = append(signedData, pubkey...)
	sig := id.Sign(signedData)
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("identity sign returned %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}

	plaintext := make([]byte, 0, len(pubkey)+len(sig))
	plaintext = append(plaintext, pubkey...)
	plaintext = append(plaintext, sig...)

	ciphertext, err := LinkTokenEncrypt(plaintext, signing, encryption)
	if err != nil {
		return nil, fmt.Errorf("link encrypt: %w", err)
	}
	return &Packet{
		HeaderType:      HeaderType1,
		ContextFlag:     false,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationLink,
		PacketType:      PacketData,
		Hops:            0,
		DestHash:        linkID,
		Context:         ContextLinkIdentify,
		Data:            ciphertext,
	}, nil
}

// msgpackMarshalFloat64 packs a single float64 in msgpack format —
// emits one float64 marker byte (0xCB) followed by big-endian IEEE 754
// double. Matches upstream `umsgpack.packb(rtt)` output for a Python
// float, which is what the responder unpacks via umsgpack.unpackb at
// RNS/Link.py:539.
func msgpackMarshalFloat64(v float64) ([]byte, error) {
	bits := math.Float64bits(v)
	out := make([]byte, 9)
	out[0] = 0xCB
	out[1] = byte(bits >> 56)
	out[2] = byte(bits >> 48)
	out[3] = byte(bits >> 40)
	out[4] = byte(bits >> 32)
	out[5] = byte(bits >> 24)
	out[6] = byte(bits >> 16)
	out[7] = byte(bits >> 8)
	out[8] = byte(bits)
	return out, nil
}

// --- LINKCLOSE (SPEC §6.7.3) ------------------------------------------

// Teardown reasons (SPEC §6.7.4). These are LOCAL state values; the
// LINKCLOSE packet carries no reason code, and a receiver infers which
// applies from whether it is the initiator or the responder.
const (
	// TeardownLocalClosed is NOT a §6.7.4 value — that table has no
	// entry for "we closed it ourselves", because upstream's teardown()
	// leaves teardown_reason unset on a local close and only the three
	// values below are ever assigned. This makes that absence explicit
	// rather than borrowing a reason that describes something else.
	//
	// It matters because §6.7.2 gives the reason exactly one job: let
	// the application "distinguish 'the peer went dark' from 'the peer
	// cleanly closed'". Reporting TIMEOUT for a deliberate local
	// teardown destroys the distinction the field exists to carry.
	TeardownLocalClosed = 0x00

	TeardownTimeout           = 0x01 // watchdog STALE → CLOSED; no LINKCLOSE seen
	TeardownInitiatorClosed   = 0x02 // we are the responder; the initiator closed
	TeardownDestinationClosed = 0x03 // we are the initiator; the responder closed
)

// BuildLinkClose builds the §6.7.3 teardown packet for a link: DATA,
// context 0xFC, dest_hash = link_id, body = the link_id encrypted under
// the link's session key.
//
// The body is not redundant with the dest_hash it duplicates. dest_hash
// is plaintext on the wire and therefore forgeable by anyone who has
// seen a packet on this link; the encrypted copy is what proves the
// sender holds the session key. See ParseLinkClosePacket.
func BuildLinkClose(linkID, signing, encryption []byte) (*Packet, error) {
	if len(linkID) != IdentityHashLen {
		return nil, fmt.Errorf("link_id must be %d bytes", IdentityHashLen)
	}
	ciphertext, err := LinkTokenEncrypt(linkID, signing, encryption)
	if err != nil {
		return nil, fmt.Errorf("linkclose encrypt: %w", err)
	}
	return &Packet{
		HeaderType:      HeaderType1,
		ContextFlag:     false,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationLink,
		PacketType:      PacketData,
		Hops:            0,
		DestHash:        linkID,
		Context:         ContextLinkClose,
		Data:            ciphertext,
	}, nil
}

// ParseLinkClosePacket decrypts a §6.7.3 LINKCLOSE and checks its body
// against the link it claims to close.
//
// That check is the whole security of this packet type, and upstream
// performs it too (RNS/Link.py:674-683, `if plaintext == self.link_id`).
// A LINKCLOSE is addressed to the link_id in cleartext, so without the
// encrypted-body check any observer who has seen a single packet on a
// link could tear it down at will — a trivial denial of service against
// every link they can see.
func ParseLinkClosePacket(p *Packet, linkID, signing, encryption []byte) error {
	if p == nil {
		return errors.New("nil packet")
	}
	if p.PacketType != PacketData {
		return fmt.Errorf("packet_type %d is not DATA", p.PacketType)
	}
	if p.DestinationType != DestinationLink {
		return fmt.Errorf("dest_type %d is not LINK", p.DestinationType)
	}
	if p.Context != ContextLinkClose {
		return fmt.Errorf("context = 0x%02x, want 0x%02x", p.Context, ContextLinkClose)
	}
	plaintext, err := LinkTokenDecrypt(p.Data, signing, encryption)
	if err != nil {
		return err
	}
	if !hmac.Equal(plaintext, linkID) {
		return errors.New("LINKCLOSE body does not match the link it was sent on")
	}
	return nil
}
