package rns

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

// ifaceReader consumes captureIface's recorded packets in order. The
// shared helper is snapshot-based; §11 tests care about "the next packet
// this side emitted", so they need a cursor.
type ifaceReader struct {
	iface *captureIface
	n     int
}

func reader(i *captureIface) *ifaceReader { return &ifaceReader{iface: i} }

func (r *ifaceReader) take(t *testing.T) []byte {
	t.Helper()
	if !r.iface.WaitForN(r.n+1, time.Now().Add(2*time.Second)) {
		t.Fatalf("expected a packet at index %d, none was emitted", r.n)
	}
	p := r.iface.Snapshot()[r.n]
	r.n++
	return p
}

func (r *ifaceReader) tryTake() []byte {
	pkts := r.iface.Snapshot()
	if len(pkts) <= r.n {
		return nil
	}
	p := pkts[r.n]
	r.n++
	return p
}

// deliverToSelf feeds a packet the capture interface recorded back into
// the same Transport's handler, which is enough to exercise the §11
// server and initiator halves against each other over one link: both
// ends share the session keys, and handleLinkData routes purely on the
// context byte.
func deliverToSelf(t *testing.T, tp *Transport, raw []byte) {
	t.Helper()
	p, err := ParsePacket(raw)
	if err != nil {
		t.Fatalf("parse captured packet: %v", err)
	}
	tp.handleLinkData(p)
}

func TestRequestResponseRoundTrip(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)

	var seen *RequestContext
	err := tp.RegisterRequestHandler("/page/index.mu", AllowAll, nil, func(rc *RequestContext) (any, error) {
		seen = rc
		return map[string]any{"body": "hello"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	receipt, err := tp.SendRequest(link.ID, "/page/index.mu", nil)
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	reqRaw := rd.take(t)

	// Serve it. The handler runs and the response goes back out.
	deliverToSelf(t, tp, reqRaw)
	if seen == nil {
		t.Fatal("handler never ran")
	}
	if seen.Path != "/page/index.mu" {
		t.Errorf("handler saw path %q", seen.Path)
	}
	if seen.Data != nil {
		t.Errorf("plain GET data = %v, want nil", seen.Data)
	}

	respRaw := rd.take(t)
	deliverToSelf(t, tp, respRaw)

	resp, err := receipt.Response(2 * time.Second)
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	m, ok := resp.(map[any]any)
	if !ok {
		t.Fatalf("response is %T, want map[any]any", resp)
	}
	if m["body"] != "hello" {
		t.Errorf("body = %v", m["body"])
	}
}

// The request_id a server labels its response with is the hash of the
// REQUEST PACKET's hashable part, not of the inner plaintext (§11.1,
// §11.2). If the initiator computes it the other way every response is
// dropped as unsolicited and the fetch times out in silence — so pin
// that both sides derive the same value from the same bytes.
func TestRequestIDIsThePacketHashNotThePlaintextHash(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)
	if _, err := tp.SendRequest(link.ID, "/x", nil); err != nil {
		t.Fatal(err)
	}
	raw := rd.take(t)
	p, err := ParsePacket(raw)
	if err != nil {
		t.Fatal(err)
	}

	fromPacket, err := RequestIDFromPacket(p)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := LinkTokenDecrypt(p.Data, link.Signing, link.Encryption)
	if err != nil {
		t.Fatal(err)
	}
	fromPlaintext := RequestIDFromPacked(plaintext)

	if bytes.Equal(fromPacket, fromPlaintext) {
		t.Fatal("premise broken: packet-hash and plaintext-hash forms coincide")
	}
	// The pending receipt must be keyed on the packet-hash form.
	if r := tp.takePendingRequest(fromPlaintext); r != nil {
		t.Error("receipt was registered under the plaintext hash — responses will never match")
	}
	if r := tp.takePendingRequest(fromPacket); r == nil {
		t.Error("receipt not registered under the packet hash")
	}
}

// TestRequestIDFormulaMatchesSpec computes the §11.2 formula from the
// raw wire bytes independently of HashablePart(), so this pins the
// formula rather than our own implementation of it:
//
//	request_id = SHA-256( (raw[0] & 0x0F) || raw[2:] )[:16]   // HEADER_1
//
// A round-trip test cannot catch a wrong formula here — both ends
// compute it the same way and agree with each other while disagreeing
// with every real peer, which is the failure §11.2 spends a callout on.
func TestRequestIDFormulaMatchesSpec(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)
	if _, err := tp.SendRequest(link.ID, "/x", nil); err != nil {
		t.Fatal(err)
	}
	raw := rd.take(t)
	p, err := ParsePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.HeaderType != HeaderType1 {
		t.Fatalf("expected a HEADER_1 link packet, got header type %d", p.HeaderType)
	}

	hashable := append([]byte{raw[0] & 0x0F}, raw[2:]...)
	sum := sha256.Sum256(hashable)
	want := sum[:RequestIDLen]

	got, err := RequestIDFromPacket(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("request_id = %x, spec formula gives %x", got, want)
	}
	// And it must not be any of the near-misses: the encrypted body, the
	// whole raw packet, or the decrypted plaintext.
	for _, wrong := range [][]byte{p.Data, raw} {
		s := sha256.Sum256(wrong)
		if bytes.Equal(got, s[:RequestIDLen]) {
			t.Error("request_id matches a hash of the wrong bytes")
		}
	}
}

// §11.2: an initiator MUST verify element [0]. A RESPONSE bearing an id
// nobody asked for is dropped, not handed to whatever is in flight —
// otherwise a relay can replay a stale response into a pending request.
func TestUnsolicitedResponseIsDropped(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)
	receipt, err := tp.SendRequest(link.ID, "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	rd.take(t) // discard the request

	stray, err := PackResponse(bytes.Repeat([]byte{0x99}, RequestIDLen), "not yours")
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := BuildLinkDataPacket(link.ID, link.Signing, link.Encryption, stray)
	if err != nil {
		t.Fatal(err)
	}
	pkt.Context = ContextResponse
	raw, err := pkt.Pack()
	if err != nil {
		t.Fatal(err)
	}
	deliverToSelf(t, tp, raw)

	if _, err := receipt.Response(150 * time.Millisecond); !errors.Is(err, ErrRequestTimeout) {
		t.Errorf("err = %v, want the request to still be pending (ErrRequestTimeout)", err)
	}
}

func TestRequestAllowModes(t *testing.T) {
	identity, _ := NewIdentity()
	pub := identity.PublicKey()
	idHash := IdentityHashFromPublicKey(pub)

	for _, c := range []struct {
		name       string
		allow      AllowMode
		allowed    [][]byte
		identified []byte
		wantServed bool
	}{
		{"AllowAll serves an unidentified peer", AllowAll, nil, nil, true},
		{"AllowNone refuses everyone", AllowNone, nil, pub, false},
		{"AllowList refuses an unidentified peer", AllowList, [][]byte{idHash}, nil, false},
		{"AllowList serves a listed identity", AllowList, [][]byte{idHash}, pub, true},
		{"AllowList refuses an unlisted identity", AllowList, [][]byte{bytes.Repeat([]byte{0x01}, IdentityHashLen)}, pub, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			link, tp, iface := makeActiveTestLink(t)
			rd := reader(iface)
			if c.identified != nil {
				link.mu.Lock()
				link.remoteIdentity = append([]byte(nil), c.identified...)
				link.mu.Unlock()
			}
			served := false
			if err := tp.RegisterRequestHandler("/p", c.allow, c.allowed, func(*RequestContext) (any, error) {
				served = true
				return "ok", nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tp.SendRequest(link.ID, "/p", nil); err != nil {
				t.Fatal(err)
			}
			deliverToSelf(t, tp, rd.take(t))
			if served != c.wantServed {
				t.Errorf("handler ran = %t, want %t", served, c.wantServed)
			}
		})
	}
}

func TestRequestUnknownPathIsDropped(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)
	if _, err := tp.SendRequest(link.ID, "/nothing/here", nil); err != nil {
		t.Fatal(err)
	}
	deliverToSelf(t, tp, rd.take(t))
	// No handler, so no response packet may be emitted.
	if raw := rd.tryTake(); raw != nil {
		t.Errorf("a response was sent for an unregistered path (%d bytes)", len(raw))
	}
}

func TestRegisterRequestHandlerRejectsBadInputs(t *testing.T) {
	_, tp, _ := makeActiveTestLink(t)
	if err := tp.RegisterRequestHandler("", AllowAll, nil, func(*RequestContext) (any, error) { return nil, nil }); err == nil {
		t.Error("empty path accepted")
	}
	if err := tp.RegisterRequestHandler("/p", AllowAll, nil, nil); err == nil {
		t.Error("nil handler accepted")
	}
	// An AllowList handler with nobody on the list refuses every caller;
	// that is a configuration mistake, not a policy.
	if err := tp.RegisterRequestHandler("/p", AllowList, nil, func(*RequestContext) (any, error) { return nil, nil }); err == nil {
		t.Error("AllowList with an empty allowed list accepted")
	}
}

func TestUnregisterRequestHandler(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)
	if err := tp.RegisterRequestHandler("/p", AllowAll, nil, func(*RequestContext) (any, error) { return "x", nil }); err != nil {
		t.Fatal(err)
	}
	tp.UnregisterRequestHandler("/p")
	if _, err := tp.SendRequest(link.ID, "/p", nil); err != nil {
		t.Fatal(err)
	}
	deliverToSelf(t, tp, rd.take(t))
	if raw := rd.tryTake(); raw != nil {
		t.Error("an unregistered handler still answered")
	}
}

// A Resource carrying a §11 RESPONSE must be labelled in the
// advertisement with the request_id (`q`) and the is_response flag
// (`p`), §10.4. Without the labels the assembled body is
// indistinguishable from application payload: it would be handed to the
// link's data callback while the initiator's receipt waited out its
// timeout, which is exactly how a large page fetch fails silently.
func TestRPCResourceAdvertisementIsLabelled(t *testing.T) {
	link, tp, _ := makeActiveTestLink(t)
	requestID := bytes.Repeat([]byte{0x5A}, RequestIDLen)
	body := bytes.Repeat([]byte{0x01}, LinkMDU*3)

	rs, err := NewRPCResourceSender(tp, link, body, nil, noopLogger{}, requestID, ResourceFlagIsResponse)
	if err != nil {
		t.Fatalf("NewRPCResourceSender: %v", err)
	}
	adv, err := rs.ParseAdvertisement()
	if err != nil {
		t.Fatalf("ParseAdvertisement: %v", err)
	}
	if !bytes.Equal(adv.RequestID, requestID) {
		t.Errorf("adv q = %x, want %x", adv.RequestID, requestID)
	}
	if byte(adv.Flags)&ResourceFlagIsResponse == 0 {
		t.Errorf("adv flags = %#x, missing the p is_response bit", adv.Flags)
	}
	if byte(adv.Flags)&ResourceFlagIsRequest != 0 {
		t.Error("adv carries the u is_request bit on a response")
	}
	// The plain constructor must stay unlabelled, or every ordinary
	// Resource would be mistaken for an RPC answer.
	plain, err := NewResourceSender(tp, link, body, nil, noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	plainAdv, err := plain.ParseAdvertisement()
	if err != nil {
		t.Fatal(err)
	}
	if len(plainAdv.RequestID) != 0 || byte(plainAdv.Flags)&(ResourceFlagIsResponse|ResourceFlagIsRequest) != 0 {
		t.Errorf("plain Resource is labelled as RPC: q=%x flags=%#x", plainAdv.RequestID, plainAdv.Flags)
	}
}

// TestServingRequestsKeepsTheLinkAlive is the regression for a link that
// was torn down while in active use.
//
// handleLinkData consumes ContextRequest in its own switch and returns
// before HandleLinkData, which was the only inbound path that refreshed
// Link.LastActivity. Serving a request therefore did not count as
// activity, and sweepLinks closed the link 15 minutes after it was
// established no matter how many requests crossed it. A NomadNet page
// server carries nothing but these packets, so its links died
// mid-session, every session.
func TestServingRequestsKeepsTheLinkAlive(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)

	if err := tp.RegisterRequestHandler("/page/play.mu", AllowAll, nil,
		func(*RequestContext) (any, error) { return "board", nil }); err != nil {
		t.Fatal(err)
	}

	// A link on the brink: idle long enough that the next sweep closes
	// it, which is where a busy link kept ending up.
	tp.SetLinkLifetime(30*time.Second, time.Minute, time.Minute)
	stale := time.Now().Add(-59 * time.Second)
	link.mu.Lock()
	link.LastActivity = stale
	link.mu.Unlock()

	if _, err := tp.SendRequest(link.ID, "/page/play.mu", nil); err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	deliverToSelf(t, tp, rd.take(t))

	link.mu.Lock()
	after := link.LastActivity
	link.mu.Unlock()
	if !after.After(stale) {
		t.Fatalf("serving a request left LastActivity at %v — a busy link still looks idle", after)
	}

	// And the sweep must now leave it alone.
	var closed int
	tp.linkManager.SetLinkClosedHandler(func([]byte, byte) { closed++ })
	tp.sweepLinks()
	if closed != 0 {
		t.Errorf("the sweep closed a link that just served a request (%d teardowns)", closed)
	}
}

// The same guarantee for the §10 Resource path: a page body large enough
// to need a Resource is answered with resource-control traffic, which
// also bypasses HandleLinkData.
func TestResourceControlTrafficKeepsTheLinkAlive(t *testing.T) {
	link, _, _ := makeActiveTestLink(t)

	stale := time.Now().Add(-59 * time.Second)
	link.mu.Lock()
	link.LastActivity = stale
	link.mu.Unlock()

	link.NoteInboundTraffic()

	link.mu.Lock()
	after, auth := link.LastActivity, link.authenticated
	link.mu.Unlock()
	if !after.After(stale) {
		t.Errorf("NoteInboundTraffic did not refresh the idle timer")
	}
	if !auth {
		t.Errorf("NoteInboundTraffic did not mark the link authenticated")
	}
}
