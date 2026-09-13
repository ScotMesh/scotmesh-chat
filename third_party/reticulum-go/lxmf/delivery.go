package lxmf

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

// ErrRecipientUnknown is returned by Send when the recipient hasn't
// announced yet, so we have no public key to encrypt to. Callers wrapping
// Send in a retry loop should treat this as a recoverable error: issue a
// path request to the recipient and try again later, since an inbound
// announce can populate the public key out-of-band.
var ErrRecipientUnknown = errors.New("recipient has not announced; cannot encrypt")

// ErrPropagationNodeUnknown is returned by SendPropagated when the
// propagation node hasn't announced yet — without its announce we have
// neither its public key (link handshake) nor its app_data (stamp cost,
// transfer limits). Recoverable the same way as ErrRecipientUnknown:
// request a path and retry.
var ErrPropagationNodeUnknown = errors.New("propagation node has not announced; cannot open link")

// ErrPropagationNodeDisabled is returned by SendPropagated when the
// node's most recent announce says it is not currently accepting
// messages (§5.8.5 element [2] false).
var ErrPropagationNodeDisabled = errors.New("propagation node is not accepting messages")

// ErrPropagationTransferTooLarge is returned when the packed bundle
// exceeds the per-transfer limit the node announced (§5.8.5 element [3]).
var ErrPropagationTransferTooLarge = errors.New("message exceeds propagation node per-transfer limit")

// ErrDeliveryProofTimeout is returned by Send when an opportunistic DATA
// packet went out but the recipient's SPEC §6.5 delivery proof never
// arrived — the LXMF-level signal for "recipient is offline or
// unreachable". Callers with a retry queue treat it like any transient
// failure; with propagation fallback configured, exhausting the retry
// budget on this error re-routes the message via a propagation node.
var ErrDeliveryProofTimeout = errors.New("no delivery proof from recipient")

// DefaultDeliveryProofTimeout is how long Send waits for the recipient's
// §6.5 proof of an opportunistic DATA packet before reporting the
// attempt failed. Long enough for a multi-hop testnet round trip; short
// enough that five retries + backoff resolve to the propagation
// fallback within ~2 minutes.
const DefaultDeliveryProofTimeout = 15 * time.Second

// MaxConcurrentInboundHandlers bounds the OnMessage goroutines a
// Delivery will have in flight. The spawn is deliberate (see
// handleInbound) but was unbounded: under a flood each goroutine holds
// a parsed message — fields up to ~1 MiB — while contending on locks
// that are held across disk writes, so they accumulate at exactly the
// rate lock contention slows them. Past the ceiling, inbound messages
// are dropped with a log rather than growing the pile; the peer's
// retry (or its propagation fallback) covers the loss.
const MaxConcurrentInboundHandlers = 256

// DefaultPropagationSendTimeout bounds one upload to a propagation
// node. Kept well under the Resource path's size-scaled ceiling (up to
// 20 minutes) because all propagated traffic funnels through a single
// selected node — see SendPropagated.
const DefaultPropagationSendTimeout = 45 * time.Second

// Delivery is an LXMF "delivery destination" registered on a Transport.
// Inbound opportunistic LXMF messages addressed to its destination hash are
// Token-decrypted, parsed, signature-verified, and handed to OnMessage.
// Outbound messages are built, signed, Token-encrypted, and broadcast via
// the same Transport.
//
// One Delivery wraps one identity. Multi-tenant deployments would
// instantiate multiple Deliveries on the same Transport.
type Delivery struct {
	transport *rns.Transport
	identity  *rns.Identity
	destHash  []byte

	// OnMessage is called from the Transport's dispatcher goroutine for
	// each verified inbound message. If processing is non-trivial, the
	// callback should hand off to its own goroutine.
	OnMessage func(*Message)

	// inflight bounds concurrent OnMessage goroutines; see
	// MaxConcurrentInboundHandlers.
	inflight chan struct{}

	// OnError is called when an inbound packet can't be decrypted, parsed,
	// or verified, and when an outbound send hits a peer-caused problem it
	// can work around — today, a recipient whose announce app_data carries
	// an unreadable stamp_cost (§4.3). Implementations typically log.
	//
	// It is therefore NOT confined to the Transport dispatcher goroutine:
	// the outbound calls run on whatever goroutine invoked Send, so a
	// handler that touches shared state needs its own synchronization.
	OnError func(error)

	// LinkSendTimeout caps the total elapsed time SendLink (or Send when
	// it falls through to link delivery) will spend on a single message:
	// LINKREQUEST/LRPROOF round-trip + DATA broadcast + DATA proof wait.
	// Defaults to rns.DefaultLinkSendTimeout (30s) when zero.
	LinkSendTimeout time.Duration

	// PropagationSendTimeout caps a single SendPropagated upload
	// (handshake + transfer + proof). 0 = DefaultPropagationSendTimeout.
	PropagationSendTimeout time.Duration

	// DeliveryProofTimeout caps how long an opportunistic Send waits for
	// the recipient's SPEC §6.5 delivery proof before returning
	// ErrDeliveryProofTimeout. 0 = DefaultDeliveryProofTimeout (15s).
	// Negative = fire-and-forget (return as soon as Broadcast does) —
	// the pre-v1.13 behavior, kept for tests and for callers that do
	// their own confirmation.
	DeliveryProofTimeout time.Duration

	// DisableOutboundStamps turns off §5.7 delivery-stamp generation.
	//
	// By default an outbound message carries a stamp whenever the
	// recipient's announce asks for one (§5.7.4) — that is what makes us
	// deliverable to a recipient who enforces stamps. The cost is that
	// Send BLOCKS on proof-of-work: a 768 KiB workblock plus ~2^cost
	// hashes, per message per recipient. Set this when the caller would
	// rather be fast and possibly filtered than slow and accepted (a
	// latency-critical path, a constrained device, a test).
	DisableOutboundStamps bool

	// MaxStampCost overrides MaxDeliveryStampCost for this Delivery —
	// the ceiling past which an announced stamp_cost is refused with
	// ErrStampCostTooHigh instead of ground. 0 = the package default.
	// Lower it on constrained hardware; raise it only if you have a
	// concrete peer demanding more and CPU to burn.
	MaxStampCost int

	// InboundStampCost is the §5.7.4 cost we require of senders — the
	// same value we announce in app_data element [1]. Zero means we ask
	// for nothing and inbound stamps are neither checked nor scored.
	//
	// Setting it makes every stamped inbound message cost a 768 KiB
	// workblock to verify, which is attacker-triggered work; see
	// MaxConcurrentStampValidations.
	InboundStampCost int

	// EnforceStamps drops inbound messages that do not clear
	// InboundStampCost, instead of delivering them with StampValid
	// false. Off by default, matching upstream's _enforce_stamps —
	// §5.7.4's third row is the default behaviour, not the second.
	EnforceStamps bool

	// Tickets holds the §5.7.3 relationships in both directions:
	// tickets peers granted us (used to stamp outbound for free) and
	// tickets we granted them (used to validate their inbound stamps).
	// Nil disables tickets entirely — every stamp is then proof-of-work.
	Tickets *TicketStore

	// stampFailures is the per-sender allowance for FAILED stamp
	// validations — the bound on how much workblock time one sender can
	// cost us. See lxmf/stamp_budget.go. A pointer so Delivery stays
	// copyable; nil (a Delivery not built by NewDelivery) means no
	// budget. Allocated in NewDelivery.
	stampFailures *stampBudget

	// Ratchets, when set, publishes an X25519 ratchet in our announces
	// and is tried on inbound decrypt before the long-term key (§7.3,
	// §7.4). Nil means no ratchet: everything still works, and every
	// message we receive stays decryptable by our long-term key
	// forever, which is exactly the forward secrecy a ratchet buys.
	Ratchets *rns.RatchetKeeper
}

// decryptInbound opens an inbound token, trying our ratchet ring
// before the long-term key (§7.4).
func (d *Delivery) decryptInbound(ciphertext []byte) ([]byte, error) {
	if d.Ratchets == nil {
		return rns.TokenDecrypt(d.identity, ciphertext)
	}
	return rns.TokenDecryptWithRatchets(d.identity, ciphertext, d.Ratchets.PrivateKeys())
}

// NewDelivery registers the LXMF delivery destination for `identity` on
// `transport` and returns the wrapper. OnMessage and OnError can be set
// before or after this call (we check at dispatch time).
//
// `buildAnnounce`, if non-nil, lets the Transport answer SPEC §7.2
// path? requests targeting this destination. Pass a closure that
// produces a fresh announce with the given context byte (typically
// rns.ContextPathResponse). When nil, path? requests for our delivery
// destination go unanswered and clients have to wait for our periodic
// announce instead.
func NewDelivery(transport *rns.Transport, identity *rns.Identity, buildAnnounce func(context byte) (*rns.Packet, error)) (*Delivery, error) {
	if transport == nil || identity == nil {
		return nil, errors.New("nil transport or identity")
	}
	d := &Delivery{
		transport:     transport,
		identity:      identity,
		destHash:      identity.DestinationHashFor(FullName()),
		inflight:      make(chan struct{}, MaxConcurrentInboundHandlers),
		stampFailures: &stampBudget{},
	}
	if err := transport.RegisterLocal(&rns.LocalDestination{
		DestHash:        d.destHash,
		Identity:        d.identity, // enables SPEC §6.5 PROOF emission on inbound DATA
		OnPacket:        d.handleInbound,
		OnLinkPlaintext: d.handleInboundLinkPlaintext,
		BuildAnnounce:   buildAnnounce,
	}); err != nil {
		return nil, err
	}
	return d, nil
}

// Hash returns this delivery destination's 16-byte hash.
func (d *Delivery) Hash() []byte {
	out := make([]byte, len(d.destHash))
	copy(out, d.destHash)
	return out
}

// Identity returns the underlying identity.
func (d *Delivery) Identity() *rns.Identity { return d.identity }

// Send delivers an LXMF message to `recipientDestHash`. Routes
// automatically:
//
//   - If the message fits the opportunistic single-packet cap
//     (MaxOpportunisticPayload, 295 bytes msgpack), it is sent in one
//     Token-encrypted Reticulum DATA packet, then BLOCKS until the
//     recipient's SPEC §6.5 delivery proof arrives or
//     DeliveryProofTimeout (15s default) elapses — an unreachable
//     recipient surfaces as ErrDeliveryProofTimeout instead of a
//     silent success.
//   - If it would overflow that cap, falls through to link delivery:
//     opens a Reticulum Link to the recipient (handshake) if no Active
//     one exists, then sends the LXMF body in direct form on the link
//     and BLOCKS until the responder's link DATA proof arrives or the
//     LinkSendTimeout elapses.
//
// The recipient MUST have announced previously, since we need their
// X25519 public key to encrypt opportunistically and their long-term
// Ed25519 public key to verify the LRPROOF + link DATA proofs. An
// unknown recipient yields an error before any wire activity.
//
// If that announce declares a stamp_cost (SPEC §4.3 / §5.7.4), Send also
// grinds a §5.7.2 delivery stamp before transmitting — proof-of-work
// that BLOCKS the call for roughly 2^cost hashes over a 768 KiB
// workblock. Set DisableOutboundStamps to skip it (at the cost of being
// dropped or flagged by recipients that enforce stamps), or MaxStampCost
// to lower the ceiling past which an announced cost is refused with
// ErrStampCostTooHigh.
func (d *Delivery) Send(recipientDestHash []byte, title, content []byte, fields map[any]any) error {
	_, err := d.SendWithID(recipientDestHash, title, content, fields)
	return err
}

// stampOptionsFor resolves the §5.7 stamp policy for one recipient from
// the app_data of their most recent announce (§4.3 element [1]).
//
// A malformed app_data is NOT fatal: the peer garbled its own announce,
// and refusing to message them over it would be a peer's bug taking down
// our send path. We fall back to "no stamp" and report the decode error
// through OnError so it is visible rather than silent — the message may
// well be dropped on arrival if that peer actually enforces stamps.
//
// A cost we understand but refuse to grind is a different matter and
// surfaces as an error from the pack call (ErrStampCostTooHigh): there
// the recipient stated a demand and we are declining it, which the
// caller needs to know about.
func (d *Delivery) stampOptionsFor(appData []byte) StampOptions {
	if d.DisableOutboundStamps {
		return StampOptions{}
	}
	cost, err := rns.DecodeLXMFAppDataStampCost(appData)
	if err != nil {
		d.errorf("stamp_cost from announce app_data: %w", err)
		return StampOptions{}
	}
	return StampOptions{Cost: cost, MaxCost: d.MaxStampCost}
}

// stampOptionsForPeer is stampOptionsFor plus the §5.7.3 shortcut: if
// the recipient has granted us a live ticket, it replaces the grind.
func (d *Delivery) stampOptionsForPeer(appData, peerHash []byte) StampOptions {
	opts := d.stampOptionsFor(appData)
	if d.Tickets == nil || d.DisableOutboundStamps {
		return opts
	}
	if t := d.Tickets.Held(peerHash, time.Now()); t != nil {
		opts.Ticket = t
	}
	return opts
}

// SendWithID is Send but also returns the 32-byte LXMF message_id the
// recipient will compute on parse (SPEC §5.4: H(dest||source||payload)).
// Returned on success; on error the returned msgID is nil. Used by the
// forwarding service to register the per-recipient view of a relayed
// bubble so reactions / reply-to fields can be rewritten to bind on the
// receiving client.
func (d *Delivery) SendWithID(recipientDestHash []byte, title, content []byte, fields map[any]any) (msgID []byte, err error) {
	if len(recipientDestHash) != rns.IdentityHashLen {
		return nil, fmt.Errorf("recipient dest_hash must be %d bytes", rns.IdentityHashLen)
	}
	known := d.transport.Recall(recipientDestHash)
	if known == nil {
		return nil, fmt.Errorf("%w: %x", ErrRecipientUnknown, recipientDestHash[:4])
	}

	// Stamp policy comes from the recipient's own announce (§5.7.4), so
	// it is resolved once here and reused by whichever route we take —
	// re-deriving it inside sendOverLink would grind the proof-of-work a
	// second time for every message that overflows the packet cap.
	opts := d.stampOptionsForPeer(known.AppData, recipientDestHash)

	// Try opportunistic first. The size check runs on the STAMPED payload
	// (a stamp costs 34 bytes of the 295-byte budget), so a stamped
	// message near the cap correctly routes to link delivery instead of
	// being emitted as a packet upstream would have sent as a Resource.
	body, packedID, err := SignAndPackOpportunisticStamped(d.identity, d.destHash, recipientDestHash, title, content, fields, opts)
	if err != nil {
		if errors.Is(err, ErrPayloadTooLarge) {
			return d.sendOverLink(recipientDestHash, title, content, fields, opts)
		}
		return nil, fmt.Errorf("pack: %w", err)
	}

	// Recipient's identity hash drives the Token HKDF salt (SPEC §3.2).
	recipientIdentityHash := identityHashFromPublic(known.PublicKey)
	// §3 step 2 / §7.3: encrypt to the peer's announced ratchet when
	// they publish one, so a later compromise of their long-term key
	// cannot decrypt this message.
	ciphertext, err := rns.TokenEncrypt(body, known.EncryptionPublic(), recipientIdentityHash)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	pkt := buildOutboundPacket(recipientDestHash, ciphertext, known.TransportID)

	// Delivery confirmation (SPEC §6.5): register for the recipient's
	// proof BEFORE broadcasting — a fast/local recipient can ack before
	// Broadcast returns. Without this wait, opportunistic Send is
	// fire-and-forget: it reports success the moment the packet leaves
	// our interface, an offline recipient never surfaces as a failure,
	// and retry/propagation-fallback logic upstream of us never engages.
	// HashablePart is invariant under the HEADER_1↔HEADER_2 rewriting
	// relays do in flight, so our hash matches what the recipient signs.
	hashable, err := pkt.HashablePart()
	if err != nil {
		return nil, fmt.Errorf("hashable: %w", err)
	}
	packetHash := sha256Sum(hashable)
	proofCh, cancelWaiter, err := d.transport.RegisterPacketProofWaiter(packetHash, known.Ed25519Public())
	if err != nil {
		return nil, fmt.Errorf("register proof waiter: %w", err)
	}
	defer cancelWaiter()

	if err := d.transport.Broadcast(pkt); err != nil {
		return nil, err
	}

	wait := d.DeliveryProofTimeout
	if wait < 0 {
		return packedID, nil // explicit fire-and-forget
	}
	if wait == 0 {
		wait = DefaultDeliveryProofTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case proofErr := <-proofCh:
		if proofErr != nil {
			return nil, proofErr
		}
		return packedID, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w: %x after %s", ErrDeliveryProofTimeout, recipientDestHash[:4], wait)
	}
}

// sendOverLink is the link-delivery fallback path used by Send when the
// opportunistic msgpack payload would exceed MaxOpportunisticPayload.
// Builds the LXMF body in direct form (SPEC §5.2 — destination_hash is
// in the body because the outer Reticulum packet is addressed to a
// link_id, not the recipient destination), then hands the bytes to
// rns.Transport.SendOverLink which manages the link state machine and
// blocks for the responder's proof. Returns the LXMF message_id alongside
// the error so SendWithID can register the recipient view for cross-
// client reaction / reply rewriting.
func (d *Delivery) sendOverLink(recipientDestHash, title, content []byte, fields map[any]any, opts StampOptions) (msgID []byte, err error) {
	directBody, packedID, err := SignAndPackDirectStamped(d.identity, d.destHash, recipientDestHash, title, content, fields, opts)
	if err != nil {
		return nil, fmt.Errorf("pack direct: %w", err)
	}
	timeout := d.LinkSendTimeout
	if timeout <= 0 {
		timeout = rns.DefaultLinkSendTimeout
	}
	if err := d.transport.SendOverLink(recipientDestHash, directBody, timeout); err != nil {
		return nil, fmt.Errorf("link send: %w", err)
	}
	return packedID, nil
}

// SendPropagated submits an LXMF message for `recipientDestHash` to the
// propagation node at `nodeDestHash` for store-and-forward delivery
// (SPEC §5.8, flows/send-propagated-lxmf.md). The recipient fetches it
// later via /get; this call succeeds when the NODE has the message, not
// when the recipient does.
//
// Pipeline: pack the propagated form (encrypted to the RECIPIENT — the
// node never decrypts), grind a §5.7 propagation stamp if the node's
// announce demands one, wrap in the msgpack upload envelope, and ship it
// over a Link to the node's lxmf.propagation destination —
// Transport.SendOverLink picks single link DATA or a Resource transfer
// by size and blocks for the node's proof.
//
// Both the recipient AND the node must have announced: the recipient for
// the encryption keys, the node for the link handshake + its §5.8.5
// app_data (enabled state, stamp cost, transfer limit — all enforced
// here). Returns the recipient-view message_id, same as SendWithID.
func (d *Delivery) SendPropagated(nodeDestHash, recipientDestHash []byte, title, content []byte, fields map[any]any) (msgID []byte, err error) {
	if len(nodeDestHash) != rns.IdentityHashLen {
		return nil, fmt.Errorf("propagation node dest_hash must be %d bytes", rns.IdentityHashLen)
	}
	if len(recipientDestHash) != rns.IdentityHashLen {
		return nil, fmt.Errorf("recipient dest_hash must be %d bytes", rns.IdentityHashLen)
	}
	recipient := d.transport.Recall(recipientDestHash)
	if recipient == nil {
		return nil, fmt.Errorf("%w: %x", ErrRecipientUnknown, recipientDestHash[:4])
	}
	node := d.transport.Recall(nodeDestHash)
	if node == nil {
		return nil, fmt.Errorf("%w: %x", ErrPropagationNodeUnknown, nodeDestHash[:4])
	}
	info, err := ParsePropagationNodeAppData(node.AppData)
	if err != nil {
		return nil, fmt.Errorf("node %x: %w", nodeDestHash[:4], err)
	}
	if !info.Enabled {
		return nil, fmt.Errorf("%w: %x", ErrPropagationNodeDisabled, nodeDestHash[:4])
	}

	// Two independent stamps, two different audiences: the recipient's
	// delivery stamp (§5.7.2, ground over message_id, sealed inside the
	// payload) travels with the message to its destination, while the
	// node's propagation stamp below only buys storage. Skipping the
	// first would hand a stamp-enforcing recipient a message they drop
	// after it survived the whole store-and-forward trip.
	lxmfData, transientID, packedID, err := SignAndPackPropagatedStamped(
		d.identity, d.destHash, recipientDestHash,
		recipient.EncryptionPublic(), identityHashFromPublic(recipient.PublicKey),
		title, content, fields, d.stampOptionsForPeer(recipient.AppData, recipientDestHash))
	if err != nil {
		return nil, fmt.Errorf("pack propagated: %w", err)
	}

	if info.StampCost > 0 {
		stamp, err := GeneratePropagationStamp(transientID, info.StampCost)
		if err != nil {
			return nil, fmt.Errorf("node %x: %w", nodeDestHash[:4], err)
		}
		// Appended AFTER the transient_id is computed — the stamp is
		// derived from it (flows/send-propagated-lxmf.md step 5).
		lxmfData = append(lxmfData, stamp...)
	}

	bundle, err := PackPropagationBundle(time.Now(), lxmfData)
	if err != nil {
		return nil, err
	}
	if info.PerTransferLimitKB > 0 && int64(len(bundle)) > info.PerTransferLimitKB*1000 {
		return nil, fmt.Errorf("%w: bundle is %d B, node cap is %d KB",
			ErrPropagationTransferTooLarge, len(bundle), info.PerTransferLimitKB)
	}

	// Deliberately SHORTER than the general link timeout, and never
	// extended by size. SendOverLink's Resource path stretches its
	// deadline to a size-proportional budget capped at 20 minutes —
	// sensible for interactive delivery to a peer, dangerous here:
	// every propagated message targets the SAME selected node, so a
	// node that completes the handshake and then stalls would wedge
	// every outbound worker simultaneously and halt the queue,
	// including direct sends to online members. A store-and-forward
	// upload has no interactivity to preserve, and failing fast just
	// re-queues the message.
	timeout := d.PropagationSendTimeout
	if timeout <= 0 {
		timeout = DefaultPropagationSendTimeout
	}
	if err := d.transport.SendOverLink(nodeDestHash, bundle, timeout); err != nil {
		return nil, fmt.Errorf("propagation upload: %w", err)
	}
	return packedID, nil
}

// buildOutboundPacket frames the encrypted LXMF body as a Reticulum DATA
// packet. If a transport_id is known (the recipient announced via a
// HEADER_2 relay), we emit HEADER_2 with TransportType=NetworkTransport
// per SPEC §2.3 so relays can route the packet to the multi-hop recipient.
// Otherwise we emit HEADER_1 broadcast — sufficient for direct neighbors
// and for cases where the receiving rnsd auto-fills transport_id for a
// 1-hop local client.
func buildOutboundPacket(recipientDestHash, ciphertext, transportID []byte) *rns.Packet {
	if len(transportID) == rns.IdentityHashLen {
		return &rns.Packet{
			HeaderType:      rns.HeaderType2,
			ContextFlag:     false,
			TransportType:   rns.NetworkTransport,
			DestinationType: rns.DestinationSingle,
			PacketType:      rns.PacketData,
			Hops:            0,
			TransportID:     transportID,
			DestHash:        recipientDestHash,
			Context:         rns.ContextNone,
			Data:            ciphertext,
		}
	}
	return &rns.Packet{
		HeaderType:      rns.HeaderType1,
		ContextFlag:     false,
		TransportType:   rns.BroadcastTransport,
		DestinationType: rns.DestinationSingle,
		PacketType:      rns.PacketData,
		Hops:            0,
		DestHash:        recipientDestHash,
		Context:         rns.ContextNone,
		Data:            ciphertext,
	}
}

// handleInbound is invoked by the Transport for each DATA packet
// addressed to our destination hash. It does Token decrypt + LXMF parse
// + signature verify, then fires OnMessage on success.
func (d *Delivery) handleInbound(p *rns.Packet) {
	plain, err := d.decryptInbound(p.Data)
	if err != nil {
		d.errorf("decrypt: %w", err)
		return
	}

	msg, err := ParseOpportunisticBody(plain, p.DestHash)
	if err != nil {
		d.errorf("parse: %w", err)
		return
	}

	sender := d.transport.Recall(msg.SourceHash)
	if sender == nil {
		d.errorf("sender %x unknown — must announce first", msg.SourceHash[:4])
		// Ask the network for their announce. Future messages from this
		// sender will succeed once a path-response announce arrives and
		// populates Transport.known. The current message stays dropped —
		// PROOF was already emitted so the mobile client won't retry it.
		if err := d.transport.RequestPath(msg.SourceHash); err != nil {
			d.errorf("path? request: %w", err)
		}
		return
	}
	if err := msg.Verify(sender.Ed25519Public()); err != nil {
		d.errorf("verify: %w", err)
		return
	}
	// Both of these run only after the signature checks out: validating
	// a stamp on an unauthenticated body would spend a 768 KiB workblock
	// on whatever a stranger sent, and remembering a ticket from one
	// would let anybody grant themselves free delivery in our name.
	d.rememberInboundTicket(msg)
	if !d.validateInboundStamp(msg) {
		return
	}

	d.dispatchInbound(msg)
}

// handleInboundLinkPlaintext is invoked by the Transport when a link
// DATA packet addressed to our destination has been decrypted on an
// active Link. The plaintext is the FULL LXMF body in direct form
// (SPEC §5.2): dest_hash || source_hash || sig || msgpack — unlike
// opportunistic, the dest_hash is in the body itself because the outer
// Reticulum packet was addressed to a link_id rather than our
// destination.
//
// The link layer has already done its own authenticated decryption +
// per-packet PROOF; this function just extracts the LXMF semantics and
// fires the application-level OnMessage callback.
func (d *Delivery) handleInboundLinkPlaintext(plaintext []byte) {
	msg, err := ParseDirectBody(plaintext)
	if err != nil {
		d.errorf("link LXMF parse: %w", err)
		return
	}
	// CRITICAL: the direct form carries its destination INSIDE the
	// signed body, and Verify signs over that same field — so a body
	// the sender legitimately addressed to someone else verifies
	// perfectly here. Without this check, anyone who holds a link-form
	// body signed by Alice (any peer she ever sent a >295-byte message
	// to) can open an unauthenticated link to us and replay it: it is
	// attributed to Alice, fanned out to the roster, and executed with
	// her privileges if it parses as a command and she is an admin.
	// The opportunistic path needs no equivalent check — there the
	// dest_hash comes from the outer packet header the Transport
	// already routed to us.
	if !bytes.Equal(msg.DestHash, d.destHash) {
		d.errorf("link LXMF addressed to %x, not us (%x) — refusing replayed body",
			msg.DestHash[:4], d.destHash[:4])
		return
	}
	sender := d.transport.Recall(msg.SourceHash)
	if sender == nil {
		d.errorf("link sender %x unknown — must announce first", msg.SourceHash[:4])
		if err := d.transport.RequestPath(msg.SourceHash); err != nil {
			d.errorf("path? request: %w", err)
		}
		return
	}
	if err := msg.Verify(sender.Ed25519Public()); err != nil {
		d.errorf("link LXMF verify: %w", err)
		return
	}
	// Identical to the opportunistic path, and for the same reasons
	// (SPEC §5.7.3, §5.7.4): both run only after the signature checks
	// out, because validating a stamp on an unauthenticated body spends
	// a 768 KiB workblock on whatever a stranger sent, and remembering
	// a ticket from one lets anybody grant themselves free delivery in
	// our name.
	//
	// These were missing here until v0.6.1, which made InboundStampCost,
	// EnforceStamps and Tickets apply to single-packet messages only —
	// so a sender bypassed stamp enforcement entirely just by opening a
	// Link, which Send does automatically for anything over
	// MaxOpportunisticPayload. Enforcement that a 296-byte message walks
	// straight past is not enforcement.
	d.rememberInboundTicket(msg)
	if !d.validateInboundStamp(msg) {
		return
	}

	d.dispatchInbound(msg)
}

// dispatchInbound hands a verified message to OnMessage on its own
// goroutine, bounded by MaxConcurrentInboundHandlers.
//
// CRITICAL that this is NOT inline: the forwarder calls Delivery.Send
// from within OnMessage, and Send may block on a Reticulum Link
// handshake whose LRPROOF arrives on the SAME dispatcher goroutine.
// Running OnMessage inline deadlocks — dispatch stuck in Send, waiting
// for an LRPROOF queued behind dispatch. Spawning restores Send's
// documented per-call blocking without holding the dispatcher.
func (d *Delivery) dispatchInbound(msg *Message) {
	if d.OnMessage == nil {
		return
	}
	select {
	case d.inflight <- struct{}{}:
	default:
		d.errorf("inbound handler pool full (%d in flight) — dropping message from %x",
			MaxConcurrentInboundHandlers, msg.SourceHash[:4])
		return
	}
	go func() {
		defer func() { <-d.inflight }()
		d.OnMessage(msg)
	}()
}

func (d *Delivery) errorf(format string, args ...any) {
	if d.OnError != nil {
		d.OnError(fmt.Errorf(format, args...))
	}
}

// identityHashFromPublic recomputes the recipient's identity hash from
// their announced public key. Needed because Transport.Recall holds the
// public key + dest_hash, but the Token cipher's HKDF salt is the
// identity hash (SPEC §3.2).
func identityHashFromPublic(publicKey []byte) []byte {
	h := sha256Sum(publicKey)
	return h[:rns.IdentityHashLen]
}

// sha256Sum is a trivial wrapper to avoid an import cycle in the body of
// identityHashFromPublic above.
func sha256Sum(b []byte) []byte {
	hash := newSHA256()
	hash.Write(b)
	return hash.Sum(nil)
}
