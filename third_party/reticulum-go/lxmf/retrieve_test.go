package lxmf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
)

// fakePropagationNode answers §5.8.3 /get the way upstream does: round 1
// lists transient_ids, round 2 returns a FLAT list of bodies and purges
// anything in `haves`, round 3 purges what round 2 delivered.
type fakePropagationNode struct {
	mu       sync.Mutex
	store    map[string][]byte // hex(transient_id) -> body
	rounds   [][]any           // every request's data element, in order
	identity *rns.Identity
	failNext error
}

func (n *fakePropagationNode) handle(rc *rns.RequestContext) (any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	data, ok := rc.Data.([]any)
	if !ok {
		return nil, errors.New("data is not a list")
	}
	n.rounds = append(n.rounds, data)

	// Upstream refuses to say anything to an unidentified caller.
	if rc.RemoteIdentity == nil {
		return propErrNoIdentity, nil
	}
	if n.failNext != nil {
		err := n.failNext
		n.failNext = nil
		return nil, err
	}

	wants, haves := data[0], data[1]
	if wants == nil && haves == nil {
		ids := make([]any, 0, len(n.store))
		for _, b := range n.sortedBodies() {
			tid := sha256.Sum256(b)
			ids = append(ids, tid[:])
		}
		return ids, nil
	}

	// Purge acknowledged ids.
	if hl, ok := haves.([]any); ok {
		for _, h := range hl {
			if b, ok := h.([]byte); ok {
				delete(n.store, hexOf(b))
			}
		}
	}
	// Serve wanted ones.
	out := []any{}
	if wl, ok := wants.([]any); ok {
		for _, w := range wl {
			if b, ok := w.([]byte); ok {
				if body, found := n.store[hexOf(b)]; found {
					out = append(out, body)
				}
			}
		}
	}
	return out, nil
}

func (n *fakePropagationNode) sortedBodies() [][]byte {
	out := make([][]byte, 0, len(n.store))
	for _, b := range n.store {
		out = append(out, b)
	}
	// Deterministic order for the test; upstream sorts by size.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if len(out[j]) < len(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func (n *fakePropagationNode) remaining() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.store)
}

func hexOf(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0x0f])
	}
	return string(out)
}

// retrieveFixture wires alice (the retrieving client) and a node on a
// shared wire, with `count` messages from bob already stored for alice.
type retrieveFixture struct {
	alice, bob *rns.Identity
	delA       *Delivery
	node       *fakePropagationNode
	nodeDest   []byte
	tA         *rns.Transport
}

func newRetrieveFixture(t *testing.T, count int) *retrieveFixture {
	t.Helper()
	alice, _ := rns.NewIdentity()
	bob, _ := rns.NewIdentity()
	nodeID, _ := rns.NewIdentity()

	aIface, bIface, stop := pairedInterfaces()
	tA := rns.NewTransport(nil)
	tA.AddInterface(aIface)
	tB := rns.NewTransport(nil)
	tB.AddInterface(bIface)

	delA, err := NewDelivery(tA, alice, nil)
	if err != nil {
		t.Fatal(err)
	}

	node := &fakePropagationNode{store: map[string][]byte{}, identity: nodeID}
	f := &retrieveFixture{
		alice: alice, bob: bob, delA: delA, node: node, tA: tA,
		nodeDest: nodeID.DestinationHashFor(PropagationFullName()),
	}

	// Store `count` messages addressed to alice, packed exactly as a
	// sender would have submitted them (minus the propagation stamp,
	// which the node strips before serving).
	aliceDest := alice.DestinationHashFor(FullName())
	bobDest := bob.DestinationHashFor(FullName())
	for i := 0; i < count; i++ {
		body, _, _, err := SignAndPackPropagated(bob, bobDest, aliceDest,
			alice.X25519Public(), identityHashFromPublic(alice.PublicKey()),
			nil, []byte{byte('a' + i)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		tid := sha256.Sum256(body)
		node.store[hexOf(tid[:])] = body
	}

	// The node serves /get on its propagation destination.
	if err := tB.RegisterRequestHandler(MessageGetPath, rns.AllowAll, nil, node.handle); err != nil {
		t.Fatal(err)
	}
	if err := tB.RegisterLocal(&rns.LocalDestination{
		DestHash: f.nodeDest,
		Identity: nodeID,
		OnPacket: func(*rns.Packet) {},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go tA.Run(ctx)
	go tB.Run(ctx)
	t.Cleanup(func() { cancel(); stop() })

	// Both sides need to know each other: alice to reach the node, the
	// node's transport to verify nothing (it just answers).
	announce := func(tr *rns.Transport, id *rns.Identity, name string) {
		appData, _ := rns.EncodeLXMFAppData([]byte("peer"), nil)
		pkt, err := rns.BuildAnnounce(id, name, appData, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Broadcast(pkt); err != nil {
			t.Fatal(err)
		}
	}
	announce(tB, nodeID, PropagationFullName())
	announce(tB, bob, FullName())
	announce(tA, alice, FullName())

	if !waitFor(time.Second, func() bool {
		return tA.Recall(f.nodeDest) != nil && tA.Recall(bobDest) != nil
	}) {
		t.Fatal("announces never reached alice")
	}
	return f
}

// TestRetrievePropagatedRoundTrip walks the whole §5.8.3 exchange:
// list, download, and purge.
func TestRetrievePropagatedRoundTrip(t *testing.T) {
	f := newRetrieveFixture(t, 3)

	res, err := f.delA.RetrievePropagated(f.nodeDest, RetrieveOptions{})
	if err != nil {
		t.Fatalf("RetrievePropagated: %v", err)
	}
	if len(res.Offered) != 3 {
		t.Fatalf("offered %d ids, want 3", len(res.Offered))
	}
	if len(res.Messages) != 3 {
		t.Fatalf("downloaded %d messages, want 3 (failed: %v)", len(res.Messages), res.Failed)
	}
	for _, m := range res.Messages {
		if !m.SenderVerified {
			t.Errorf("message %x not sender-verified", m.TransientID[:4])
		}
		if len(m.Message.Content) != 1 {
			t.Errorf("unexpected content %q", m.Message.Content)
		}
		// The transient_id must be the hash of the body as served, which
		// is what the node keys its store on — acknowledge the wrong
		// value and nothing is ever deleted.
		if len(m.TransientID) != 32 {
			t.Errorf("transient_id is %d bytes, want 32", len(m.TransientID))
		}
	}
	// Round 3 must have purged the store.
	if n := f.node.remaining(); n != 0 {
		t.Errorf("%d messages left on the node after acknowledgement", n)
	}
	if len(res.Acknowledged) != 3 {
		t.Errorf("acknowledged %d, want 3", len(res.Acknowledged))
	}
}

// A message we already hold is acknowledged rather than re-downloaded —
// the node deletes it and we never pay for the transfer.
func TestRetrieveSkipsMessagesWeAlreadyHave(t *testing.T) {
	f := newRetrieveFixture(t, 3)

	// Claim we already have everything.
	res, err := f.delA.RetrievePropagated(f.nodeDest, RetrieveOptions{
		Have: func([]byte) bool { return true },
	})
	if err != nil {
		t.Fatalf("RetrievePropagated: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("downloaded %d messages despite having them all", len(res.Messages))
	}
	if len(res.Acknowledged) != 3 {
		t.Errorf("acknowledged %d, want 3", len(res.Acknowledged))
	}
	if n := f.node.remaining(); n != 0 {
		t.Errorf("%d messages left on the node; already-held ids must still purge", n)
	}
}

func TestRetrieveRespectsMaxMessages(t *testing.T) {
	f := newRetrieveFixture(t, 3)
	res, err := f.delA.RetrievePropagated(f.nodeDest, RetrieveOptions{MaxMessages: 2})
	if err != nil {
		t.Fatalf("RetrievePropagated: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("downloaded %d, want 2", len(res.Messages))
	}
	if n := f.node.remaining(); n != 1 {
		t.Errorf("%d left on the node, want 1 undelivered", n)
	}
}

// RetainOnNode suppresses both purge paths, so a client can sync
// without destroying the node's copy.
func TestRetrieveRetainOnNode(t *testing.T) {
	f := newRetrieveFixture(t, 2)
	res, err := f.delA.RetrievePropagated(f.nodeDest, RetrieveOptions{RetainOnNode: true})
	if err != nil {
		t.Fatalf("RetrievePropagated: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("downloaded %d, want 2", len(res.Messages))
	}
	if len(res.Acknowledged) != 0 {
		t.Errorf("acknowledged %d ids under RetainOnNode", len(res.Acknowledged))
	}
	if n := f.node.remaining(); n != 2 {
		t.Errorf("%d messages left, want both retained", n)
	}
}

func TestRetrieveEmptyNode(t *testing.T) {
	f := newRetrieveFixture(t, 0)
	res, err := f.delA.RetrievePropagated(f.nodeDest, RetrieveOptions{})
	if err != nil {
		t.Fatalf("RetrievePropagated: %v", err)
	}
	if len(res.Offered) != 0 || len(res.Messages) != 0 {
		t.Errorf("empty node offered %d / delivered %d", len(res.Offered), len(res.Messages))
	}
}

// The §5.8.2 error constants are integers where a payload would be a
// list. Reading 0xf0 as "no messages" would make a permission problem
// look like an empty mailbox forever.
func TestRetrieveMapsNodeErrorConstants(t *testing.T) {
	for _, c := range []struct {
		code int
		want error
	}{
		{propErrNoIdentity, ErrPropagationNoIdentity},
		{propErrNoAccess, ErrPropagationNoAccess},
		{propErrThrottled, ErrPropagationThrottled},
	} {
		if got, ok := asErrorCode(int64(c.code)); !ok || got != c.code {
			t.Errorf("asErrorCode(0x%02x) = %d, %t", c.code, got, ok)
		}
	}
	// A list answer must NOT be read as an error code.
	if _, ok := asErrorCode([]any{}); ok {
		t.Error("a list answer was treated as an error constant")
	}
}

// Upstream returns a FLAT list of bodies, not the [timestamp, [bodies]]
// envelope used for uploads. Decoding the upload shape here would treat
// the timestamp as a message body and drop every real one.
func TestRetrieveDecodesFlatBodyList(t *testing.T) {
	bodies, err := asBodyList([]any{[]byte("aaa"), []byte("bb")})
	if err != nil {
		t.Fatalf("asBodyList: %v", err)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], []byte("aaa")) {
		t.Errorf("decoded %d bodies: %q", len(bodies), bodies)
	}
	// The upload envelope shape must be rejected, not silently accepted.
	if _, err := asBodyList([]any{1.5, []any{[]byte("x")}}); err == nil {
		t.Error("the [timestamp, [bodies]] upload shape was accepted as a body list")
	}
}

// A §11 REQUEST too large for one packet rides a Resource (§11.1). The
// id derivation differs by form — a Resource has no packet to hash, so
// it hashes the envelope and carries the id in the advertisement's `q`
// — and getting that wrong means the server labels its answer with an
// id the initiator never registered, so every large request times out.
//
// Driven through the propagation fixture because it already has a real
// link, a real handler, and a real Transport on each side.
func TestLargeRequestRidesAResource(t *testing.T) {
	f := newRetrieveFixture(t, 0)

	// A /get whose `haves` list alone exceeds the 431-byte link MDU.
	var haves []any
	for i := 0; i < 40; i++ {
		haves = append(haves, bytes.Repeat([]byte{byte(i)}, 32))
	}

	link, err := f.tA.AcquireLink(f.nodeDest, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLink: %v", err)
	}
	if err := f.tA.IdentifyOnLink(link.ID, f.alice); err != nil {
		t.Fatalf("IdentifyOnLink: %v", err)
	}

	receipt, err := f.tA.SendRequest(link.ID, MessageGetPath, []any{nil, haves})
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if _, err := receipt.Response(5 * time.Second); err != nil {
		t.Fatalf("large request never answered: %v", err)
	}

	// The node must have seen the full purge list, not a truncated one.
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	if len(f.node.rounds) == 0 {
		t.Fatal("the node saw no request at all")
	}
	last := f.node.rounds[len(f.node.rounds)-1]
	got, ok := last[1].([]any)
	if !ok {
		t.Fatalf("haves element is %T, want a list", last[1])
	}
	if len(got) != len(haves) {
		t.Errorf("node received %d haves, want %d — the envelope was truncated", len(got), len(haves))
	}
}
