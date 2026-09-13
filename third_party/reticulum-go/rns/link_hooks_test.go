package rns

// ScotMesh patch: per-destination LinkHooks, Transport.SendOnLink, and
// the closed callback for links evicted under link-table pressure.

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

// hookEvent is one hook invocation, recorded in order.
type hookEvent struct {
	kind   string
	linkID []byte
	data   []byte
	reason byte
}

type hookRecorder struct {
	mu     sync.Mutex
	events []hookEvent
}

func (r *hookRecorder) add(e hookEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *hookRecorder) of(kind string) []hookEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hookEvent
	for _, e := range r.events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (r *hookRecorder) hooks() *LinkHooks {
	return &LinkHooks{
		OnEstablished: func(id []byte) { r.add(hookEvent{kind: "established", linkID: id}) },
		OnIdentified:  func(id, pub []byte) { r.add(hookEvent{kind: "identified", linkID: id, data: pub}) },
		OnData:        func(id, pt []byte) { r.add(hookEvent{kind: "data", linkID: id, data: append([]byte(nil), pt...)}) },
		OnResource:    func(id, body []byte) { r.add(hookEvent{kind: "resource", linkID: id, data: body}) },
		OnClosed:      func(id []byte, reason byte) { r.add(hookEvent{kind: "closed", linkID: id, reason: reason}) },
	}
}

// hookedServer is a Transport serving one destination with hooks, and an
// initiator link to it that has completed the handshake.
type hookedServer struct {
	tp        *Transport
	iface     *captureIface
	rec       *hookRecorder
	destHash  []byte
	alice     *Identity
	aliceLink *Link
	lrReq     *Packet
}

func newHookedServer(t *testing.T) *hookedServer {
	t.Helper()
	alice, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	s := &hookedServer{
		tp:       NewTransport(noopLogger{}),
		iface:    newCaptureIface(),
		rec:      &hookRecorder{},
		destHash: bob.DestinationHashFor(FullName("rrc", "hub")),
		alice:    alice,
	}
	s.tp.AddInterface(s.iface)
	if err := s.tp.RegisterLocal(&LocalDestination{
		DestHash:  s.destHash,
		Identity:  bob,
		OnPacket:  func(*Packet) {},
		LinkHooks: s.rec.hooks(),
	}); err != nil {
		t.Fatal(err)
	}

	aliceMgr := NewLinkManager()
	link, lrReq, err := aliceMgr.StartLinkAsInitiator(s.destHash, nil)
	if err != nil {
		t.Fatalf("StartLinkAsInitiator: %v", err)
	}
	s.aliceLink, s.lrReq = link, lrReq
	s.tp.handleLinkRequest(lrReq)

	if !s.iface.WaitForN(1, time.Now().Add(2*time.Second)) {
		t.Fatal("no LRPROOF was sent")
	}
	proof, err := ParsePacket(s.iface.Snapshot()[0])
	if err != nil {
		t.Fatalf("parse LRPROOF: %v", err)
	}
	if _, err := aliceMgr.HandleLRProof(proof, bob.PublicKey()[32:]); err != nil {
		t.Fatalf("HandleLRProof: %v", err)
	}
	return s
}

func TestLinkHooksEstablishedFiresOnceWithTheLinkID(t *testing.T) {
	s := newHookedServer(t)
	got := s.rec.of("established")
	if len(got) != 1 {
		t.Fatalf("OnEstablished fired %d times, want 1", len(got))
	}
	if !bytes.Equal(got[0].linkID, s.aliceLink.ID) {
		t.Errorf("OnEstablished link_id %x, want %x", got[0].linkID, s.aliceLink.ID)
	}

	// A retransmitted LINKREQUEST is answered with the saved LRPROOF but
	// is not a new link.
	s.tp.handleLinkRequest(s.lrReq)
	if n := len(s.rec.of("established")); n != 1 {
		t.Errorf("a retransmitted LINKREQUEST fired OnEstablished again (%d calls)", n)
	}
}

func TestLinkRecordsItsLocalDestination(t *testing.T) {
	s := newHookedServer(t)
	l := s.tp.linkManager.Get(s.aliceLink.ID)
	if l == nil {
		t.Fatal("responder link not in the manager")
	}
	if !bytes.Equal(l.LocalDestHash(), s.destHash) {
		t.Errorf("LocalDestHash %x, want %x", l.LocalDestHash(), s.destHash)
	}
	if s.aliceLink.LocalDestHash() != nil {
		t.Error("an initiator link reports a local destination")
	}
}

func TestLinkHooksReceiveDataAndIdentify(t *testing.T) {
	s := newHookedServer(t)
	var fallback int
	s.tp.linkManager.SetDefaultInboundDataHandler(func(_, _ []byte) { fallback++ })
	s.tp.linkManager.SetRemoteIdentifiedHandler(func(_, _ []byte) { fallback++ })

	idPkt, err := BuildLinkIdentify(s.aliceLink.ID, s.aliceLink.Signing, s.aliceLink.Encryption, s.alice)
	if err != nil {
		t.Fatal(err)
	}
	s.tp.handleLinkData(idPkt)
	ids := s.rec.of("identified")
	if len(ids) != 1 || !bytes.Equal(ids[0].data, s.alice.PublicKey()) || !bytes.Equal(ids[0].linkID, s.aliceLink.ID) {
		t.Fatalf("OnIdentified = %+v, want one call with alice's key", ids)
	}

	dataPkt, err := BuildLinkDataPacket(s.aliceLink.ID, s.aliceLink.Signing, s.aliceLink.Encryption, []byte("hello hub"))
	if err != nil {
		t.Fatal(err)
	}
	s.tp.handleLinkData(dataPkt)
	data := s.rec.of("data")
	if len(data) != 1 || string(data[0].data) != "hello hub" || !bytes.Equal(data[0].linkID, s.aliceLink.ID) {
		t.Fatalf("OnData = %+v, want one call with the payload and link_id", data)
	}
	if fallback != 0 {
		t.Errorf("manager-level handlers ran %d times for a destination with hooks", fallback)
	}
}

func TestManagerHandlersStillServeDestinationsWithoutHooks(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	var got []byte
	tp.linkManager.SetDefaultInboundDataHandler(func(_, pt []byte) { got = append([]byte(nil), pt...) })
	pkt, err := BuildLinkDataPacket(link.ID, link.Signing, link.Encryption, []byte("plain"))
	if err != nil {
		t.Fatal(err)
	}
	tp.handleLinkData(pkt)
	if string(got) != "plain" {
		t.Errorf("default handler got %q, want %q", got, "plain")
	}
}

func TestSendOnLinkSendsOneDecryptableDataPacket(t *testing.T) {
	s := newHookedServer(t)
	before := len(s.iface.Snapshot())
	if err := s.tp.SendOnLink(s.aliceLink.ID, []byte("welcome")); err != nil {
		t.Fatalf("SendOnLink: %v", err)
	}
	if !s.iface.WaitForN(before+1, time.Now().Add(2*time.Second)) {
		t.Fatal("nothing was sent")
	}
	var found bool
	for _, raw := range s.iface.Snapshot()[before:] {
		p, err := ParsePacket(raw)
		if err != nil || p.Context != ContextNone || !bytes.Equal(p.DestHash, s.aliceLink.ID) {
			continue
		}
		pt, err := ParseLinkDataPacket(p, s.aliceLink.Signing, s.aliceLink.Encryption)
		if err != nil {
			t.Fatalf("the initiator cannot decrypt the packet: %v", err)
		}
		if string(pt) != "welcome" {
			t.Fatalf("payload %q, want %q", pt, "welcome")
		}
		found = true
	}
	if !found {
		t.Fatal("no link DATA packet for the link was sent")
	}
}

func TestSendOnLinkRefusals(t *testing.T) {
	s := newHookedServer(t)
	err := s.tp.SendOnLink(s.aliceLink.ID, make([]byte, LinkMDU+1))
	if !errors.Is(err, ErrLinkPayloadTooLarge) {
		t.Errorf("oversize payload: err = %v, want ErrLinkPayloadTooLarge", err)
	}
	if err := s.tp.SendOnLink(bytes.Repeat([]byte{0x42}, IdentityHashLen), []byte("x")); err == nil {
		t.Error("unknown link: no error")
	}
	s.tp.TeardownLink(s.aliceLink.ID)
	if err := s.tp.SendOnLink(s.aliceLink.ID, []byte("x")); err == nil {
		t.Error("closed link: no error")
	}
	if err := s.tp.SendOnLink(s.aliceLink.ID, make([]byte, LinkMDU)); errors.Is(err, ErrLinkPayloadTooLarge) {
		t.Error("a payload of exactly LinkMDU was refused as too large")
	}
}

func TestLinkHooksClosedFiresOnceAndReplacesTheManagerHandler(t *testing.T) {
	s := newHookedServer(t)
	var fallback int
	s.tp.linkManager.SetLinkClosedHandler(func([]byte, byte) { fallback++ })

	s.tp.TeardownLink(s.aliceLink.ID)
	s.tp.TeardownLink(s.aliceLink.ID)

	closed := s.rec.of("closed")
	if len(closed) != 1 {
		t.Fatalf("OnClosed fired %d times, want 1", len(closed))
	}
	if closed[0].reason != TeardownLocalClosed || !bytes.Equal(closed[0].linkID, s.aliceLink.ID) {
		t.Errorf("OnClosed = %+v, want the link_id with TeardownLocalClosed", closed[0])
	}
	if fallback != 0 {
		t.Errorf("the manager-level closed handler also ran (%d)", fallback)
	}
}

// Before the patch an evicted link vanished from the table without any
// callback, so a consumer tracking presence kept the peer forever.
func TestEvictedLinkReportsClosed(t *testing.T) {
	s := newHookedServer(t)
	lm := s.tp.linkManager
	victim := lm.Get(s.aliceLink.ID)
	victim.mu.Lock()
	victim.LastActivity = time.Now().Add(-time.Hour) // the oldest, so the one evicted
	victim.mu.Unlock()

	bob, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	lm.mu.Lock()
	for i := 1; i < MaxResponderLinks; i++ {
		id := make([]byte, IdentityHashLen)
		id[0], id[1] = byte(i>>8), byte(i)
		id[15] = 0xEE
		lm.links[bytesHexEncode(id)] = &Link{
			ID: id, State: LinkActive, responderIdentity: bob,
			CreatedAt: time.Now(), LastActivity: time.Now(),
		}
	}
	lm.mu.Unlock()

	// One more inbound link forces an eviction.
	other := NewLinkManager()
	_, lrReq, err := other.StartLinkAsInitiator(s.destHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.tp.handleLinkRequest(lrReq)

	closed := s.rec.of("closed")
	if len(closed) != 1 || !bytes.Equal(closed[0].linkID, s.aliceLink.ID) {
		t.Fatalf("closed events %+v, want exactly the evicted link", closed)
	}
	if closed[0].reason != TeardownTimeout {
		t.Errorf("reason 0x%02x, want TeardownTimeout", closed[0].reason)
	}
	if lm.Get(s.aliceLink.ID) != nil {
		t.Error("evicted link still in the manager")
	}
	if err := s.tp.SendOnLink(s.aliceLink.ID, []byte("x")); err == nil {
		t.Error("SendOnLink succeeded on an evicted link")
	}
}
