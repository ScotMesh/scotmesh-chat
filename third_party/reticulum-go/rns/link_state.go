package rns

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
)

// Link state machine — SPEC §6 (state machine itself isn't formally
// specified; modeled after upstream RNS.Link). Tracks the lifecycle of
// an established or in-progress Link between us and a peer:
//
//	Pending  : we sent a LINKREQUEST, waiting for matching LRPROOF.
//	Active   : handshake complete; can send/receive DATA on the link.
//	Closed   : torn down, never reopens with this link_id.
//
// KEEPALIVE-driven Expired/Stale states are deferred to a follow-up;
// the basic state machine here is enough for opportunistic-vs-link
// fallback in Delivery.Send (PR 3 of the link-delivery work).
//
// All public methods on Link and LinkManager are safe to call from
// multiple goroutines.

// LinkState enumerates the link lifecycle.
type LinkState int

const (
	LinkPending LinkState = iota
	LinkActive
	LinkClosed
)

func (s LinkState) String() string {
	switch s {
	case LinkPending:
		return "pending"
	case LinkActive:
		return "active"
	case LinkClosed:
		return "closed"
	}
	return fmt.Sprintf("LinkState(%d)", int(s))
}

// Link is one in-flight or established Reticulum Link.
type Link struct {
	mu sync.Mutex

	ID    []byte // 16-byte link_id (SPEC §6.3)
	State LinkState

	// MTU is the link MTU confirmed by the §6.6 handshake — the
	// responder adopts what the (relay-clamped) LINKREQUEST signalled,
	// the initiator adopts what the LRPROOF confirmed, exactly as
	// upstream does in Link.validate_request and Link.validate_proof
	// (RNS/Link.py:186-200, 404-424). Zero means the peer sent no
	// signalling, i.e. the base ReticulumMTU.
	//
	// It is recorded because §10.2 step 6 sizes Resource parts from it:
	// a peer whose link negotiated a larger MTU slices its transfer into
	// correspondingly larger parts, and a receiver that assumes the base
	// MTU mis-measures every one of them.
	MTU uint32

	// Session keys derived from the handshake (SPEC §6.4). 32 bytes each;
	// signing is also used as the Ed25519 seed for link-DATA proofs.
	Signing    []byte
	Encryption []byte

	// Initiator-side state used while Pending: ephemeral X25519 priv we
	// sent in the LINKREQUEST, plus the peer destination we addressed.
	// X25519 priv is cleared once the link transitions to Active; the
	// destination hash is retained for the lifetime of the link so we
	// can route outbound DATA without re-walking caller state.
	myEphemeralX25519Priv []byte
	peerDestHash          []byte

	// initiatorEd25519Priv is the ephemeral Ed25519 private key whose
	// public half we put in the LINKREQUEST body. Per upstream RNS this
	// is what an initiator uses to sign link DATA proofs for return-
	// direction traffic (responder → initiator) — the responder verifies
	// against initiatorEd25519Pub from the LINKREQUEST. Retained for the
	// life of the link. PR2 will add the signing path that consumes this.
	initiatorEd25519Priv []byte

	// peerEd25519Pub is the long-term Ed25519 public key of the responder.
	// Captured at HandleLRProof time when we are the initiator (not on
	// the LRPROOF wire — taken from the responder's prior announce). Used
	// by Transport.handleLinkProof to verify inbound link DATA proofs
	// the responder signs to ack our outbound DATA.
	peerEd25519Pub []byte

	// remoteIdentity is the peer's 64-byte announced public key, proved
	// over a §6.7.6 LINKIDENTIFY. Responder-side only, and set at most
	// once for the life of the link: upstream assigns it only when it is
	// still None (RNS/Link.py:970-989), so a peer cannot re-identify as
	// somebody else mid-link.
	remoteIdentity []byte

	// pendingProofs maps hex(packet_hash) → channel that SendOverLink
	// blocks on. handleLinkProof closes the channel (with nil) on
	// successful proof verification, or sends an error on bad-sig.
	// Senders MUST register the channel BEFORE broadcasting the link
	// DATA packet to avoid losing a fast proof.
	pendingProofs map[string]chan error

	// activatedCh is closed exactly once when the initiator-side link
	// transitions Pending → Active. Lets SendOverLink block on the
	// handshake completing without polling. Nil on the responder side.
	activatedCh chan struct{}

	// Responder-side state: the responder's ephemeral X25519 priv we
	// generated to derive session keys. Cleared once Active.
	myResponderEphPriv []byte

	// responderIdentity is the local destination identity that signs
	// outbound link DATA proofs when we are the responder (per upstream
	// RNS/Link.py:279, `self.sig_prv = self.owner.identity.sig_prv` on
	// the responder side). Nil when we are the initiator. The initiator
	// peer can verify our proof signatures because it cached our
	// long-term Ed25519 pubkey when it looked us up to send the
	// LINKREQUEST.
	responderIdentity *Identity

	// lrProof is the LRPROOF we issued as responder, retained so a
	// retransmitted or replayed LINKREQUEST can be answered with the
	// same proof instead of re-keying an established link.
	lrProof *Packet

	// authenticated records that at least one link DATA packet from the
	// peer decrypted and authenticated successfully. KEEPALIVE bodies
	// are unencrypted by design, so only an authenticated link may have
	// its idle timer refreshed by them — otherwise anyone who observes
	// a link_id could keep a link alive forever and defeat the idle
	// sweep that bounds the link table.
	authenticated bool

	CreatedAt    time.Time
	LastActivity time.Time

	// OnInboundData is called from the Transport's dispatcher with each
	// successfully decrypted link DATA payload. Non-nil during Active.
	OnInboundData func(plaintext []byte)

	// hooks are the link-lifecycle callbacks of the local destination
	// this link was accepted for (LocalDestination.LinkHooks). Nil on
	// initiator links and on destinations that set none. Immutable once
	// the link is in the manager, so it is read without the link lock.
	hooks *LinkHooks

	// localDestHash is the local destination a responder link was
	// accepted for. Nil on initiator links.
	localDestHash []byte
}

// LinkHooks are per-destination callbacks for links a peer opens to one
// of our destinations. They exist because the manager-level handlers
// (SetDefaultInboundDataHandler, SetLinkClosedHandler, ...) are single
// process-wide slots: a process serving several destinations — a chat
// hub, an LXMF delivery endpoint and a page node in one binary — cannot
// tell whose link a callback is about. Every hook receives the link_id,
// so a consumer can key per-link state on it.
//
// When a hook is set it replaces the manager-level handler for links of
// its destination; when it is nil the manager-level handler still
// applies. The legacy LocalDestination.OnLinkPlaintext, when set, keeps
// precedence over OnData. Hooks run on the Transport's dispatcher
// goroutine and must not block; a hook may call back into the Transport
// (SendOnLink, TeardownLink) because no lock is held while it runs.
type LinkHooks struct {
	// OnEstablished fires once, after the LRPROOF for a new inbound link
	// has been sent.
	OnEstablished func(linkID []byte)
	// OnIdentified fires when the peer proves its identity over §6.7.6.
	// pubKey is the peer's 64-byte public key, already verified.
	OnIdentified func(linkID, pubKey []byte)
	// OnData receives each decrypted link DATA payload (context NONE).
	OnData func(linkID, plaintext []byte)
	// OnResource receives each fully assembled inbound Resource body that
	// is not a §11 request or response.
	OnResource func(linkID, body []byte)
	// OnClosed fires exactly once when a link that reached Active closes,
	// for whatever reason, including eviction under link-table pressure.
	OnClosed func(linkID []byte, reason byte)
}

// LocalDestHash returns the local destination hash a responder link was
// accepted for, or nil for an initiator link.
func (l *Link) LocalDestHash() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.localDestHash == nil {
		return nil
	}
	return append([]byte(nil), l.localDestHash...)
}

// MarkAuthenticated records that traffic from the peer has decrypted
// and authenticated on this link. Gates whether unencrypted KEEPALIVE
// packets may refresh the idle timer — see Link.authenticated.
func (l *Link) MarkAuthenticated() {
	l.mu.Lock()
	l.authenticated = true
	l.mu.Unlock()
}

// NoteInboundTraffic records authenticated traffic from the peer: it
// marks the link authenticated AND refreshes the idle timer.
//
// A successful link-layer decrypt is the signal for both. It proves the
// peer holds the session keys, which is what authenticates the link,
// and it is by definition the peer doing something, which is what the
// idle sweep is asking about.
//
// WHY THIS EXISTS: handleLinkData consumes the §11 REQUEST/RESPONSE,
// §6.8 CHANNEL and §10 Resource-control contexts in its own switch and
// returns before reaching HandleLinkData — which was the only inbound
// path that refreshed LastActivity. So a node whose entire traffic
// profile is those contexts looked permanently idle no matter how busy
// it was, and Transport.sweepLinks tore its links down 15 minutes after
// establishment, mid-session.
//
// That is not a corner case: it is exactly a NomadNet page server,
// where every request is §11.1 and every page body is a §10 Resource.
// Such a link survived only as long as the peer's KEEPALIVEs happened
// to refresh it, so an idle browser held a link for hours while an
// actively used one was reliably killed at the 15-minute mark — the
// busier the link, the sooner it died.
func (l *Link) NoteInboundTraffic() {
	l.mu.Lock()
	l.authenticated = true
	l.LastActivity = time.Now()
	l.mu.Unlock()
}

// RemoteIdentity returns the peer's 64-byte public key if they have
// identified over §6.7.6, or nil. Mirrors upstream's
// Link.get_remote_identity().
func (l *Link) RemoteIdentity() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.remoteIdentity == nil {
		return nil
	}
	return append([]byte(nil), l.remoteIdentity...)
}

// IsInitiator returns true when this Link was opened locally (we sent
// the LINKREQUEST). Initiator-side links carry initiatorEd25519Priv
// and peerEd25519Pub; responder-side links carry responderIdentity.
func (l *Link) IsInitiator() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.responderIdentity == nil && l.peerDestHash != nil
}

// PeerDestHash returns a copy of the peer's destination hash for an
// initiator-side link, or nil for a responder-side link.
func (l *Link) PeerDestHash() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.peerDestHash == nil {
		return nil
	}
	out := make([]byte, len(l.peerDestHash))
	copy(out, l.peerDestHash)
	return out
}

// AwaitActive blocks until the link transitions to Active (closing
// activatedCh) or until ctx fires. Returns nil on Active, the ctx
// error otherwise. Returns immediately if the link is already Active
// or Closed.
func (l *Link) AwaitActive(ctxDone <-chan struct{}) error {
	l.mu.Lock()
	state := l.State
	ch := l.activatedCh
	l.mu.Unlock()
	switch state {
	case LinkActive:
		return nil
	case LinkClosed:
		return errors.New("link closed before activation")
	}
	if ch == nil {
		// Responder-side link or one constructed without an activatedCh.
		return errors.New("link has no activation channel")
	}
	select {
	case <-ch:
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.State != LinkActive {
			return fmt.Errorf("link not Active after activation signal (state=%s)", l.State)
		}
		return nil
	case <-ctxDone:
		return errors.New("link activation cancelled")
	}
}

// registerProofWaiter atomically inserts a fresh channel keyed by
// hex(packetHash) into pendingProofs and returns the channel. The
// caller MUST call clearProofWaiter when done (typically via defer).
//
// Returning a buffered channel makes handleLinkProof's signal a
// non-blocking send so a slow waiter doesn't deadlock the dispatcher.
func (l *Link) registerProofWaiter(packetHashHex string) chan error {
	ch := make(chan error, 1)
	l.mu.Lock()
	if l.pendingProofs == nil {
		l.pendingProofs = map[string]chan error{}
	}
	l.pendingProofs[packetHashHex] = ch
	l.mu.Unlock()
	return ch
}

func (l *Link) clearProofWaiter(packetHashHex string) {
	l.mu.Lock()
	delete(l.pendingProofs, packetHashHex)
	l.mu.Unlock()
}

// signalProof is used by Transport.handleLinkProof to wake a SendOverLink
// caller. Returns true if a waiter was found (and signalled), false if
// no one was waiting on this packet_hash. err may be nil to indicate
// success or non-nil for a bad-sig / wrong-link diagnostic the caller
// can surface.
func (l *Link) signalProof(packetHashHex string, err error) bool {
	l.mu.Lock()
	ch, ok := l.pendingProofs[packetHashHex]
	if ok {
		delete(l.pendingProofs, packetHashHex)
	}
	l.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- err:
	default:
	}
	return true
}

// IsActive returns true iff State == LinkActive.
func (l *Link) IsActive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.State == LinkActive
}

// LinkManager tracks links keyed by link_id. It does not own a Transport;
// callers (Delivery / Service) wire its outbound packets through their own
// Transport.Broadcast and route inbound LINKREQUEST/LRPROOF/link-addressed
// DATA/PROOF packets through the Handle* methods.
// Link table bounds. MaxResponderLinks caps links created by INBOUND
// (unauthenticated) LINKREQUESTs; the difference up to
// MaxConcurrentLinks is reserved headroom for links WE initiate, so an
// inbound flood can never starve outbound message delivery. See
// makeRoomForResponderLocked.
const (
	MaxConcurrentLinks = 512
	MaxResponderLinks  = 384
)

type LinkManager struct {
	mu    sync.Mutex
	links map[string]*Link // hex link_id -> *Link

	// Optional per-link callback. The application (Delivery) sets this
	// when it wants to receive plaintext payloads from inbound link
	// DATA. Default no-op.
	defaultOnInboundData func(linkID, plaintext []byte)

	// Optional manager-level callback for fully-assembled inbound
	// Resource bodies on links that have no per-link OnInboundData set.
	// Mirrors defaultOnInboundData for small DATA: the receiver prefers
	// the per-link handler and falls back to this one, which carries the
	// link_id so an application can route the body to the right session.
	defaultOnResourceAssembled func(linkID, body []byte)

	// defaultOnRemoteIdentified fires when a peer completes a §6.7.6
	// LINKIDENTIFY on a link we responded to.
	defaultOnRemoteIdentified func(linkID, pubKey []byte)

	// defaultOnLinkClosed fires when a link leaves Active, whether
	// because the peer sent a §6.7.3 LINKCLOSE or because we closed it
	// ourselves. See SetLinkClosedHandler.
	defaultOnLinkClosed func(linkID []byte, reason byte)

	// senders / receivers index in-flight Resource transfers per link
	// keyed by hex(link_id) || hex(resource_hash) so the Transport
	// dispatcher can route inbound RESOURCE_REQ / RESOURCE_PRF /
	// RESOURCE_RCL packets to the right sender, and inbound
	// RESOURCE_ADV / RESOURCE / RESOURCE_HMU packets to the right
	// receiver. Both maps are guarded by the same mu so iteration
	// remains race-free.
	senders   map[string]*ResourceSender
	receivers map[string]*ResourceReceiver

	// onLinkTornDown, when set, fires with the link_id after every
	// teardown that removes a link from the table, so state a link
	// owns OUTSIDE this manager can be released at the same moment.
	// Transport wires it to the §10.11 segment assembler. Set once at
	// construction, before the manager is shared.
	onLinkTornDown func(linkID []byte)
}

// NewLinkManager constructs an empty manager.
func NewLinkManager() *LinkManager {
	return &LinkManager{
		links:     map[string]*Link{},
		senders:   map[string]*ResourceSender{},
		receivers: map[string]*ResourceReceiver{},
	}
}

// resourceKey is the dictionary key used by the sender/receiver maps:
// hex(link_id) || ":" || hex(resource_hash). The colon separator
// prevents an attacker from crafting a pair (link_id', resource_hash')
// that aliases an existing key.
func resourceKey(linkID, resourceHash []byte) string {
	return hex.EncodeToString(linkID) + ":" + hex.EncodeToString(resourceHash)
}

// registerResourceSender adds a sender to the in-flight index. Returns
// an error if a sender with the same (link_id, resource_hash) already
// exists — duplicate registrations are a programming bug. Holds the
// LinkManager mutex briefly.
func (lm *LinkManager) registerResourceSender(linkID, resourceHash []byte, rs *ResourceSender) error {
	if rs == nil {
		return errors.New("link manager: nil ResourceSender")
	}
	key := resourceKey(linkID, resourceHash)
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if _, dup := lm.senders[key]; dup {
		return fmt.Errorf("link manager: duplicate ResourceSender for link=%x resource=%x", linkID[:4], resourceHash[:4])
	}
	lm.senders[key] = rs
	return nil
}

// registerResourceReceiver adds a receiver to the in-flight index.
// Same dedup semantics as registerResourceSender.
func (lm *LinkManager) registerResourceReceiver(linkID, resourceHash []byte, rr *ResourceReceiver) error {
	if rr == nil {
		return errors.New("link manager: nil ResourceReceiver")
	}
	key := resourceKey(linkID, resourceHash)
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if _, dup := lm.receivers[key]; dup {
		return fmt.Errorf("link manager: duplicate ResourceReceiver for link=%x resource=%x", linkID[:4], resourceHash[:4])
	}
	lm.receivers[key] = rr
	return nil
}

// unregisterResource removes BOTH the sender and the receiver entries
// (if any) for (link_id, resource_hash). Idempotent. Called from the
// sender/receiver Run() defer so a transfer that exits via any path
// (success, cancel, link teardown) frees its slot.
func (lm *LinkManager) unregisterResource(linkID, resourceHash []byte) {
	key := resourceKey(linkID, resourceHash)
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.senders, key)
	delete(lm.receivers, key)
}

// lookupResourceSender returns the active ResourceSender for the
// given (link_id, resource_hash) or nil if none. The Transport
// dispatcher calls this to route inbound RESOURCE_REQ / RESOURCE_PRF /
// RESOURCE_RCL packets to the right sender.
func (lm *LinkManager) lookupResourceSender(linkID, resourceHash []byte) *ResourceSender {
	key := resourceKey(linkID, resourceHash)
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.senders[key]
}

// lookupResourceReceiver returns the active ResourceReceiver for the
// given (link_id, resource_hash) or nil if none. The Transport
// dispatcher uses this for inbound RESOURCE / RESOURCE_HMU packets.
func (lm *LinkManager) lookupResourceReceiver(linkID, resourceHash []byte) *ResourceReceiver {
	key := resourceKey(linkID, resourceHash)
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.receivers[key]
}

// countReceiversForLink returns the number of inbound resource
// transfers currently in-flight on link_id. Used by openResourceReceiver
// to enforce MaxConcurrentInboundResourcesPerLink — defense against
// a peer flooding us with distinct-hash ADVs.
func (lm *LinkManager) countReceiversForLink(linkID []byte) int {
	prefix := hex.EncodeToString(linkID) + ":"
	lm.mu.Lock()
	defer lm.mu.Unlock()
	n := 0
	for k := range lm.receivers {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// closeResourcesForLink terminates every in-flight transfer
// associated with link_id. Used by CloseLink to ensure a torn-down
// link can't leave dangling sender/receiver goroutines.
func (lm *LinkManager) closeResourcesForLink(linkID []byte) {
	prefix := hex.EncodeToString(linkID) + ":"
	var senders []*ResourceSender
	var receivers []*ResourceReceiver
	lm.mu.Lock()
	hook := lm.onLinkTornDown
	for k, rs := range lm.senders {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			senders = append(senders, rs)
			delete(lm.senders, k)
		}
	}
	for k, rr := range lm.receivers {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			receivers = append(receivers, rr)
			delete(lm.receivers, k)
		}
	}
	lm.mu.Unlock()
	// Signal cancellation OUTSIDE the lock so the sender/receiver
	// goroutines that are currently blocked on lm.mu (e.g. in a
	// separate lookup) don't deadlock.
	for _, rs := range senders {
		rs.HandleCancel()
	}
	for _, rr := range receivers {
		rr.HandleCancel()
	}
	if hook != nil {
		hook(linkID)
	}
}

// SetDefaultInboundDataHandler sets a fallback callback used by Active
// links that don't have a per-link OnInboundData set.
func (lm *LinkManager) SetDefaultInboundDataHandler(cb func(linkID, plaintext []byte)) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.defaultOnInboundData = cb
}

// SetResourceAssembledHandler sets a fallback callback for fully-assembled
// inbound Resource bodies on links that have no per-link OnInboundData set.
// The per-link handler still takes precedence; this manager-level handler
// carries the link_id so an application can route the reassembled body to
// the right session.
func (lm *LinkManager) SetResourceAssembledHandler(cb func(linkID, body []byte)) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.defaultOnResourceAssembled = cb
}

// resourceAssembledHandler returns the manager-level assembled-resource
// callback under lock, or nil if none is set.
func (lm *LinkManager) resourceAssembledHandler() func(linkID, body []byte) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.defaultOnResourceAssembled
}

// SetLinkClosedHandler registers a callback fired when a link closes.
//
// It exists because a consumer holding per-link state — a session, a
// subscription, a presence entry — otherwise has no way to learn the
// link is gone, and must wait out its own timer. That wait is not
// harmless: a chat hub goes on listing a departed user as present in a
// room, and anything keyed on "is this peer here?" is confidently wrong
// for the whole window.
//
// The callback receives the link_id and a teardown reason — one of the
// three §6.7.4 values, or TeardownLocalClosed when we closed it
// ourselves. It fires only for a link that reached Active, and exactly
// once per closure, but implementations should still be idempotent
// (SPEC §12, pitfall 3): the closure can originate from an inbound
// LINKCLOSE, the idle sweep, or a local teardown. It is invoked without
// the manager lock held, so a handler may call back into the
// LinkManager.
func (lm *LinkManager) SetLinkClosedHandler(cb func(linkID []byte, reason byte)) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.defaultOnLinkClosed = cb
}

// linkClosedHandler returns the manager-level link-closed callback under
// lock, or nil if none is set.
func (lm *LinkManager) linkClosedHandler() func(linkID []byte, reason byte) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.defaultOnLinkClosed
}

// SetRemoteIdentifiedHandler registers a callback fired when a peer
// proves its identity over §6.7.6 on a responder-side link. The callback
// receives the link_id and the peer's 64-byte announced public key.
func (lm *LinkManager) SetRemoteIdentifiedHandler(cb func(linkID, pubKey []byte)) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.defaultOnRemoteIdentified = cb
}

func (lm *LinkManager) remoteIdentifiedHandler() func(linkID, pubKey []byte) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.defaultOnRemoteIdentified
}

// HandleLinkIdentify processes an inbound §6.7.6 LINKIDENTIFY.
//
// Upstream semantics (RNS/Link.py:970-989), mirrored deliberately:
//
//   - responder side only. Upstream guards on `not self.initiator`; an
//     initiator receiving one ignores it.
//   - the body must be exactly public_key(64) || signature(64).
//   - the signature is verified over link_id || public_key.
//   - remote_identity is assigned only if not already set.
//
// A failure at any step is not an error the caller should act on —
// upstream simply does not assign remote_identity — so this returns the
// reason for logging and leaves the link usable.
func (lm *LinkManager) HandleLinkIdentify(p *Packet) (*Link, []byte, error) {
	if p == nil {
		return nil, nil, errors.New("nil packet")
	}
	l := lm.Get(p.DestHash)
	if l == nil {
		return nil, nil, fmt.Errorf("LINKIDENTIFY for unknown link_id %x", p.DestHash)
	}
	if l.IsInitiator() {
		return l, nil, errors.New("LINKIDENTIFY received on an initiator-side link; ignored")
	}
	l.mu.Lock()
	if l.State != LinkActive {
		l.mu.Unlock()
		return l, nil, fmt.Errorf("LINKIDENTIFY on link in state %s", l.State)
	}
	signing, encryption := l.Signing, l.Encryption
	already := l.remoteIdentity != nil
	linkID := append([]byte(nil), l.ID...)
	l.LastActivity = time.Now()
	l.mu.Unlock()

	pubKey, sig, err := ParseLinkIdentifyPacket(p, signing, encryption)
	if err != nil {
		return l, nil, err
	}
	if !VerifyLinkIdentify(linkID, pubKey, sig) {
		return l, nil, errors.New("LINKIDENTIFY signature invalid")
	}
	if already {
		// Upstream assigns only while remote_identity is None. Re-identify
		// attempts are dropped rather than allowed to rebind the link.
		return l, nil, errors.New("LINKIDENTIFY ignored; link already identified")
	}

	// Re-check under the SAME lock hold that assigns. The `already`
	// read above happened before verification and is only a cheap early
	// out; deciding on it would leave a window where two valid
	// LINKIDENTIFYs both observe an unidentified link and the second
	// overwrites the first. That window is closed today only because
	// inbound packets are handled on one dispatcher goroutine — an
	// accident of the Transport, not a property of this function, and
	// the assign-once rule is what stops a peer re-identifying as
	// somebody else mid-link.
	l.mu.Lock()
	if l.remoteIdentity != nil {
		l.mu.Unlock()
		return l, nil, errors.New("LINKIDENTIFY ignored; link already identified")
	}
	l.remoteIdentity = append([]byte(nil), pubKey...)
	l.mu.Unlock()
	return l, pubKey, nil
}

// Get returns the Link with the given link_id, or nil if unknown.
func (lm *LinkManager) Get(linkID []byte) *Link {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.links[hex.EncodeToString(linkID)]
}

// Active returns the active Link (if any) toward responderDestHash.
// Useful for "do I already have a link to this peer or do I need to
// open one?". Returns nil if no active link is known.
func (lm *LinkManager) ActiveTo(responderDestHash []byte) *Link {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	for _, l := range lm.links {
		l.mu.Lock()
		match := l.State == LinkActive && len(l.peerDestHash) == len(responderDestHash) &&
			bytesEqual(l.peerDestHash, responderDestHash)
		l.mu.Unlock()
		if match {
			return l
		}
	}
	return nil
}

// StartLinkAsInitiator generates an ephemeral X25519 + Ed25519 keypair
// and returns:
//
//	link    -- a *Link in the Pending state (registered in the manager)
//	request -- the LINKREQUEST packet to broadcast on the wire.
//
// The caller is responsible for transmitting `request`. When the
// matching LRPROOF arrives, hand it to HandleLRProof to transition the
// link to Active.
func (lm *LinkManager) StartLinkAsInitiator(responderDestHash []byte, sig *LinkSignalling) (*Link, *Packet, error) {
	if len(responderDestHash) != IdentityHashLen {
		return nil, nil, fmt.Errorf("responder dest_hash must be %d bytes", IdentityHashLen)
	}

	ephPriv, ephPub, err := newClampedX25519()
	if err != nil {
		return nil, nil, err
	}
	// Initiator's ephemeral Ed25519 (per SPEC §6.1, both ephemerals are
	// fresh — they are NOT the long-term identity keys). Per upstream
	// RNS the initiator uses this priv to sign link DATA proofs for
	// return-direction traffic; the responder verifies against the pub
	// bytes carried in the LINKREQUEST. We retain the priv on the Link
	// for that purpose (PR2 wires the actual signing path).
	var ephEd25519Seed [32]byte
	if _, err := rand.Read(ephEd25519Seed[:]); err != nil {
		return nil, nil, fmt.Errorf("Ed25519 seed entropy: %w", err)
	}
	ephID, err := identityFromHalves([32]byte(ephPriv), ephEd25519Seed)
	if err != nil {
		return nil, nil, fmt.Errorf("derive ephemeral identity: %w", err)
	}
	ephEd25519Priv := ed25519.NewKeyFromSeed(ephEd25519Seed[:])

	pkt, err := BuildLinkRequest(ephPub, ephID.PublicKey()[32:], responderDestHash, sig)
	if err != nil {
		return nil, nil, fmt.Errorf("BuildLinkRequest: %w", err)
	}
	id, err := LinkID(pkt)
	if err != nil {
		return nil, nil, fmt.Errorf("LinkID: %w", err)
	}

	l := &Link{
		ID:                    id,
		State:                 LinkPending,
		myEphemeralX25519Priv: ephPriv,
		initiatorEd25519Priv:  ephEd25519Priv,
		peerDestHash:          append([]byte(nil), responderDestHash...),
		pendingProofs:         map[string]chan error{},
		activatedCh:           make(chan struct{}),
		CreatedAt:             time.Now(),
		LastActivity:          time.Now(),
	}
	lm.mu.Lock()
	lm.links[hex.EncodeToString(id)] = l
	lm.mu.Unlock()
	return l, pkt, nil
}

// HandleLRProof transitions a Pending initiator-side Link to Active by
// verifying the responder's LRPROOF and deriving session keys. The
// responder's long-term Ed25519 pub is supplied separately because both
// sides know it from the responder's prior announce — it's NOT on the
// LRPROOF wire (SPEC §6.2).
// SDU returns the Resource part size this link's peer slices to:
// mtu - HEADER_MAXSIZE - IFAC_MIN_SIZE (SPEC §10.2 step 6, upstream
// Resource.py:335). Falls back to the base ResourceSDU when the
// handshake carried no MTU signalling, and never returns less than it —
// a peer that signalled an implausibly small MTU still sends at least
// base-sized parts, and under-measuring them would reject a transfer
// that is perfectly legal.
func (l *Link) SDU() int {
	l.mu.Lock()
	mtu := l.MTU
	l.mu.Unlock()
	sdu := int(mtu) - ReticulumHeaderMaxSize - ReticulumIFACMinSize
	if sdu < ResourceSDU {
		return ResourceSDU
	}
	return sdu
}
func (lm *LinkManager) HandleLRProof(p *Packet, responderEd25519Pub []byte) (*Link, error) {
	parsed, err := ParseLRProof(p)
	if err != nil {
		return nil, err
	}
	if err := parsed.Verify(responderEd25519Pub); err != nil {
		return nil, err
	}

	lm.mu.Lock()
	l, ok := lm.links[hex.EncodeToString(parsed.LinkID)]
	lm.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("LRPROOF for unknown link_id %x", parsed.LinkID)
	}

	l.mu.Lock()
	if l.State != LinkPending {
		state := l.State
		l.mu.Unlock()
		return nil, fmt.Errorf("LRPROOF for link in state %s, want pending", state)
	}
	signing, encryption, err := DeriveLinkSessionKeys(l.myEphemeralX25519Priv, parsed.ResponderX25519Pub, parsed.LinkID)
	if err != nil {
		l.mu.Unlock()
		return nil, fmt.Errorf("derive session keys: %w", err)
	}
	l.Signing = signing
	l.Encryption = encryption
	// Upstream: self.mtu = confirmed_mtu or Reticulum.MTU
	// (RNS/Link.py:422). The responder's LRPROOF is the authoritative
	// end of the §6.6 negotiation — its signature commits to these
	// bytes — so the confirmed value, not what we proposed, is the one
	// that sizes this link's Resource parts.
	if parsed.Signalling != nil {
		l.MTU = parsed.Signalling.MTU
	}
	l.State = LinkActive
	l.LastActivity = time.Now()
	l.myEphemeralX25519Priv = nil // no longer needed
	// Cache responder's long-term Ed25519 pubkey so handleLinkProof can
	// validate ack proofs against it without round-tripping through
	// Recall (which could miss if the known table evicted the entry).
	l.peerEd25519Pub = append([]byte(nil), responderEd25519Pub...)
	activated := l.activatedCh
	l.mu.Unlock()
	if activated != nil {
		// Close exactly once. Pending → Active transition is the only
		// place that closes; HandleLRProof guards on State == Pending so
		// we won't reach here twice.
		close(activated)
	}
	return l, nil
}

// AcceptIncomingLinkRequest is the responder-side counterpart to
// StartLinkAsInitiator. Given an inbound LINKREQUEST + the local
// destination's identity (which signs the LRPROOF), it generates an
// ephemeral X25519 keypair, derives session keys, registers an Active
// link in the manager, and returns the LRPROOF the caller should
// broadcast back.
//
// If sig is nil, the responder mirrors whatever signalling (if any)
// the initiator sent. This is the SPEC §6.6 symmetry requirement: the
// LRPROOF's signed_data MUST include the same signalling bytes the
// initiator put in their LINKREQUEST, otherwise the initiator's
// signature verification fails (their reconstructed signed_data has
// signalling, ours doesn't). Callers that want to clamp the MTU pass
// a non-nil sig with the clamped values; the typical caller (a leaf
// forwarder that doesn't negotiate MTU) passes nil and gets correct
// mirror behavior automatically.
func (lm *LinkManager) AcceptIncomingLinkRequest(reqPkt *Packet, localID *Identity, sig *LinkSignalling) (*Link, *Packet, error) {
	l, proof, _, err := lm.acceptIncomingLinkRequest(reqPkt, localID, sig, nil)
	return l, proof, err
}

// acceptIncomingLinkRequest is AcceptIncomingLinkRequest with the hooks
// of the destination the request is for. The hooks are installed before
// the link enters the manager, so no packet can reach the link without
// them. fresh is false when the request was a retransmit or replay of a
// link already established, in which case nothing new was created.
func (lm *LinkManager) acceptIncomingLinkRequest(reqPkt *Packet, localID *Identity, sig *LinkSignalling, hooks *LinkHooks) (*Link, *Packet, bool, error) {
	req, err := ParseLinkRequest(reqPkt)
	if err != nil {
		return nil, nil, false, err
	}
	id, err := LinkID(reqPkt)
	if err != nil {
		return nil, nil, false, err
	}

	respEphPriv, respEphPub, err := newClampedX25519()
	if err != nil {
		return nil, nil, false, err
	}
	signing, encryption, err := DeriveLinkSessionKeys(respEphPriv, req.InitiatorX25519Pub, id)
	if err != nil {
		return nil, nil, false, fmt.Errorf("derive session keys: %w", err)
	}

	// Mirror initiator's signalling presence when caller didn't override.
	// SPEC §6.6 trap: asymmetric signalling breaks the LRPROOF signature
	// because both sides must reconstruct the same signed_data.
	if sig == nil {
		sig = req.Signalling
	}

	proofPkt, err := BuildLRProof(localID, id, respEphPub, sig)
	if err != nil {
		return nil, nil, false, fmt.Errorf("BuildLRProof: %w", err)
	}

	// Upstream: link.mtu = mtu_from_lr_packet(packet) or Reticulum.MTU
	// (RNS/Link.py:192-197). By §6.6 the value reaching us is already
	// clamped by any transit relay to its next-hop view, so an endpoint
	// responder adopts it as-is — and then signals it back in the
	// LRPROOF, which is what the initiator will size its parts to.
	var mtu uint32
	if sig != nil {
		mtu = sig.MTU
	}
	nl := &Link{
		ID:                 id,
		State:              LinkActive,
		MTU:                mtu,
		Signing:            signing,
		Encryption:         encryption,
		myResponderEphPriv: respEphPriv, // kept around in case we want to renegotiate
		responderIdentity:  localID,     // signs link DATA proofs (SPEC §6.5.6)
		CreatedAt:          time.Now(),
		LastActivity:       time.Now(),
		hooks:              hooks,
		localDestHash:      append([]byte(nil), reqPkt.DestHash...),
	}
	key := hex.EncodeToString(id)
	lm.mu.Lock()
	// REPLAY / RETRANSMIT GUARD. link_id is derived from the request, so
	// a retransmitted or replayed LINKREQUEST maps to the SAME id.
	// Overwriting the entry would install fresh session keys, silently
	// breaking an established peer's link until the idle sweep — a
	// targeted teardown available to anyone who observed the original
	// request on the shared hub. Instead, re-send the LRPROOF we already
	// issued: a genuine retransmit gets the answer it expected, and a
	// replay changes nothing.
	if existing, ok := lm.links[key]; ok {
		existing.mu.Lock()
		state, saved := existing.State, existing.lrProof
		existing.mu.Unlock()
		if state == LinkActive && saved != nil {
			lm.mu.Unlock()
			return existing, saved, false, nil
		}
	}
	evicted, err := lm.makeRoomForResponderLocked()
	if err != nil {
		lm.mu.Unlock()
		return nil, nil, false, err
	}
	nl.lrProof = proofPkt
	lm.links[key] = nl
	lm.mu.Unlock()
	if evicted != nil {
		// closeResourcesForLink takes lm.mu itself; call it after we
		// release ours, matching CloseLink's ordering.
		lm.closeResourcesForLink(evicted.id)
		// An evicted peer is gone as surely as one that timed out, and a
		// consumer tracking presence must hear about it: without this a
		// chat hub under link-table pressure goes on listing everyone it
		// silently dropped.
		if evicted.onClosed != nil {
			evicted.onClosed(evicted.id, TeardownTimeout)
		}
	}
	return nl, proofPkt, true, nil
}

// ErrTooManyLinks is returned when an inbound LINKREQUEST cannot be
// accepted because the responder-link budget is full and nothing was
// evictable.
var ErrTooManyLinks = errors.New("link table full; refusing inbound link request")

// makeRoomForResponderLocked enforces MaxResponderLinks before a new
// inbound link is admitted. Callers must hold lm.mu.
//
// WHY: LINKREQUEST is unauthenticated by design (SPEC §6), and every
// distinct initiator public key derives a distinct link_id — so before
// this bound existed, a flood inserted one entry per request, each
// holding session keys, reaped only by the 15-minute idle sweep. It was
// simultaneously a memory leak and a CPU amplifier (each request costs
// two X25519 operations and an Ed25519 sign, inline on the dispatcher).
//
// Policy: evict the least-recently-active responder link to admit the
// new one. Eviction rather than rejection keeps legitimate peers able
// to connect during a flood — an attacker's idle links are the natural
// eviction target, since a real peer's link sees traffic and stays
// recent. The budget deliberately covers responder links ONLY, leaving
// MaxConcurrentLinks-MaxResponderLinks headroom that a flood can never
// consume, so our own outbound delivery links always have room.
// Returns the link it evicted, or nil if none was needed.
func (lm *LinkManager) makeRoomForResponderLocked() (*evictedLink, error) {
	var (
		responders int
		oldestKey  string
		oldestAt   time.Time
	)
	for k, l := range lm.links {
		l.mu.Lock()
		isResponder := l.responderIdentity != nil
		last := l.LastActivity
		l.mu.Unlock()
		if !isResponder {
			continue
		}
		responders++
		if oldestKey == "" || last.Before(oldestAt) {
			oldestKey, oldestAt = k, last
		}
	}
	if responders < MaxResponderLinks {
		return nil, nil
	}
	if oldestKey == "" {
		return nil, ErrTooManyLinks
	}
	old := lm.links[oldestKey]
	ev := &evictedLink{}
	old.mu.Lock()
	ev.id = append([]byte(nil), old.ID...)
	if old.State == LinkActive {
		ev.onClosed = lm.defaultOnLinkClosed
		if old.hooks != nil && old.hooks.OnClosed != nil {
			ev.onClosed = old.hooks.OnClosed
		}
	}
	old.State = LinkClosed
	old.Signing = nil
	old.Encryption = nil
	old.mu.Unlock()
	delete(lm.links, oldestKey)
	// The caller releases the evicted link's resource transfers once it
	// has dropped lm.mu. Evicting the table entry alone would strand
	// every in-flight sender/receiver goroutine on a link that can
	// never carry another packet, and leave its retained §10.11
	// segments held to the idle deadline — exactly the flood this
	// eviction exists to survive.
	return ev, nil
}

// evictedLink is what makeRoomForResponderLocked hands back so the
// caller can finish the eviction once lm.mu is released.
type evictedLink struct {
	id       []byte
	onClosed func(linkID []byte, reason byte) // nil if the link never reached Active
}

// HandleLinkData processes an inbound link DATA packet — verifies the
// outer wire shape, decrypts using the link's session keys, and routes
// the plaintext through OnInboundData (or the manager's default).
// Returns the plaintext and the original packet (so the caller can emit
// a link DATA proof against it).
func (lm *LinkManager) HandleLinkData(p *Packet) ([]byte, *Link, error) {
	if p == nil {
		return nil, nil, errors.New("nil packet")
	}
	l := lm.Get(p.DestHash)
	if l == nil {
		return nil, nil, fmt.Errorf("link DATA for unknown link_id %x", p.DestHash)
	}
	l.mu.Lock()
	if l.State != LinkActive {
		l.mu.Unlock()
		return nil, l, fmt.Errorf("link DATA on link in state %s", l.State)
	}
	signing := l.Signing
	encryption := l.Encryption
	cb := l.OnInboundData
	hooks := l.hooks
	l.LastActivity = time.Now()
	l.mu.Unlock()

	plaintext, err := ParseLinkDataPacket(p, signing, encryption)
	if err != nil {
		return nil, l, err
	}
	// Authenticated: the payload decrypted and its MAC verified, so the
	// peer holds the session keys. Only now may unencrypted KEEPALIVEs
	// refresh this link's idle timer (see handleKeepalive).
	l.MarkAuthenticated()
	switch {
	case cb != nil:
		cb(plaintext)
	case hooks != nil && hooks.OnData != nil:
		hooks.OnData(append([]byte(nil), l.ID...), plaintext)
	default:
		lm.mu.Lock()
		def := lm.defaultOnInboundData
		lm.mu.Unlock()
		if def != nil {
			def(l.ID, plaintext)
		}
	}
	return plaintext, l, nil
}

// CloseLink moves the link to Closed and removes it from the manager.
// Idempotent. Also cancels every in-flight Resource transfer bound to
// this link — without that, sender/receiver goroutines would block
// forever on a dead link's REQ/PRF channels.
//
// "Silent" here means it sends the peer nothing, not that the local
// consumer hears nothing: a link that was Active still reports closed
// through SetLinkClosedHandler. Use Transport.TeardownLink when the
// peer should be told as well.
func (lm *LinkManager) CloseLink(linkID []byte) {
	lm.closeLink(linkID, TeardownTimeout)
}

// closeLink is CloseLink with an explicit §6.7.4 teardown reason.
//
// The callback fires only for a link that was ACTIVE when we closed it,
// which is a stricter test than "was in the map" and deliberately so.
// Half the internal callers close a link that never established — a
// LINKREQUEST whose broadcast failed, a handshake that timed out — and
// a consumer tracking presence must not be told a link closed when no
// link ever opened. Registration happens at StartLinkAsInitiator, long
// before the LRPROOF, so map membership does not imply a peer.
//
// It doubles as the idempotency guard: the state flips to Closed under
// the lock here, so a second close of the same link finds it non-Active
// and reports nothing. One closure, one callback (SPEC §12 pitfall 3).
//
// The key ordering rule: the session keys are read and the map entry
// removed under the lock, then everything with side effects — the
// callback, the resource cancellation — happens after it is released.
// A handler that calls back into the LinkManager would otherwise
// deadlock, and a handler is exactly the kind of code that wants to.
func (lm *LinkManager) closeLink(linkID []byte, reason byte) {
	lm.mu.Lock()
	key := hex.EncodeToString(linkID)
	wasActive := false
	cb := lm.defaultOnLinkClosed
	if l, ok := lm.links[key]; ok {
		l.mu.Lock()
		wasActive = l.State == LinkActive
		l.State = LinkClosed
		l.Signing = nil
		l.Encryption = nil
		if l.hooks != nil && l.hooks.OnClosed != nil {
			cb = l.hooks.OnClosed
		}
		l.mu.Unlock()
		delete(lm.links, key)
	}
	lm.mu.Unlock()

	// closeResourcesForLink takes the lock itself; call after we
	// release ours to keep lock ordering consistent.
	lm.closeResourcesForLink(linkID)

	if wasActive && cb != nil {
		cb(append([]byte(nil), linkID...), reason)
	}
}

// HandleLinkClose processes an inbound §6.7.3 LINKCLOSE: authenticates
// it against the link's session key, closes the link, and reports the
// §6.7.4 reason appropriate to which side we are.
//
// An unauthenticated or unknown LINKCLOSE closes nothing — see
// ParseLinkClosePacket for why that check is the whole point.
func (lm *LinkManager) HandleLinkClose(p *Packet) (*Link, byte, error) {
	if p == nil {
		return nil, 0, errors.New("nil packet")
	}
	l := lm.Get(p.DestHash)
	if l == nil {
		return nil, 0, fmt.Errorf("LINKCLOSE for unknown link_id %x", p.DestHash)
	}
	l.mu.Lock()
	if l.State != LinkActive {
		state := l.State
		l.mu.Unlock()
		return l, 0, fmt.Errorf("LINKCLOSE on link in state %s", state)
	}
	signing, encryption := l.Signing, l.Encryption
	linkID := append([]byte(nil), l.ID...)
	// IsInitiator's own definition, inlined because we already hold the
	// lock it would take.
	initiator := l.responderIdentity == nil && l.peerDestHash != nil
	l.mu.Unlock()

	if err := ParseLinkClosePacket(p, linkID, signing, encryption); err != nil {
		return l, 0, err
	}

	// SPEC §6.7.4: the reason is inferred from which side we are, since
	// the packet carries no reason code.
	reason := byte(TeardownInitiatorClosed)
	if initiator {
		reason = TeardownDestinationClosed
	}
	lm.closeLink(linkID, reason)
	return l, reason, nil
}

// ActiveCount returns the number of links currently in Active state.
// Useful for tests and debug logs.
func (lm *LinkManager) ActiveCount() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	n := 0
	for _, l := range lm.links {
		l.mu.Lock()
		if l.State == LinkActive {
			n++
		}
		l.mu.Unlock()
	}
	return n
}

// newClampedX25519 generates a fresh X25519 keypair, applying RFC 7748
// scalar clamping to the priv before computing the pub. Returns
// (priv32, pub32). Both slices are freshly allocated.
func newClampedX25519() ([]byte, []byte, error) {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return nil, nil, fmt.Errorf("X25519 priv entropy: %w", err)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, nil, fmt.Errorf("derive X25519 pub: %w", err)
	}
	return priv, pub, nil
}
