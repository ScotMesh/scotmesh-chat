package rns

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// SPEC §11 dispatch: the server half (inbound REQUEST -> handler ->
// RESPONSE) and the initiator half (outbound REQUEST, inbound RESPONSE
// matched by request_id).

// SendRequest issues a §11.1 REQUEST over an established link and
// returns a receipt to await the answer on.
//
// `data` is the application value itself — nil for a plain GET, a map
// for a form post, a slice for an LXMF propagation /get round. It is
// encoded directly into the envelope; do NOT pre-msgpack it (§11.1).
//
// The request_id is computed from the OUTBOUND PACKET's hashable part,
// because that is what the server will hash to label its response
// (§11.2). The receipt is registered before the packet is broadcast so
// a fast responder cannot answer into a gap.
func (t *Transport) SendRequest(linkID []byte, path string, data any) (*RequestReceipt, error) {
	if path == "" {
		return nil, errors.New("request path must not be empty")
	}
	l := t.linkManager.Get(linkID)
	if l == nil {
		return nil, fmt.Errorf("SendRequest: unknown link_id %x", linkID)
	}
	l.mu.Lock()
	state := l.State
	signing, encryption := l.Signing, l.Encryption
	l.mu.Unlock()
	if state != LinkActive {
		return nil, fmt.Errorf("SendRequest: link is %s, want active", state)
	}

	packed, err := PackRequest(RequestPathHash(path), data, time.Now())
	if err != nil {
		return nil, err
	}
	// §11.1 dispatches on size, and the two forms derive request_id
	// DIFFERENTLY: a single packet is identified by the hash of its own
	// wire bytes, which a Resource has none of, so the Resource form
	// hashes the packed envelope and carries the id explicitly in the
	// advertisement's `q` (RNS/Link.py:504).
	oversize := len(packed) > LinkMDU

	var requestID []byte
	var pkt *Packet
	if oversize {
		requestID = RequestIDFromPacked(packed)
	} else {
		var err error
		pkt, err = BuildLinkDataPacket(linkID, signing, encryption, packed)
		if err != nil {
			return nil, err
		}
		pkt.Context = ContextRequest
		requestID, err = RequestIDFromPacket(pkt)
		if err != nil {
			return nil, err
		}
	}

	receipt := &RequestReceipt{
		ID:     requestID,
		Path:   path,
		LinkID: append([]byte(nil), linkID...),
		ch:     make(chan requestResult, 1),
	}
	t.requestMu.Lock()
	if t.pendingRequests == nil {
		t.pendingRequests = make(map[string]*RequestReceipt)
	}
	t.pendingRequests[hex.EncodeToString(requestID)] = receipt
	t.requestMu.Unlock()

	if oversize {
		// The transfer outlives this call, so it runs in the background
		// and the caller waits on the receipt exactly as for the
		// single-packet form.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceTransferTimeout(len(packed)))
			defer cancel()
			if err := t.sendRPCResourceOverLink(ctx, l, packed, nil, requestID, ResourceFlagIsRequest); err != nil {
				t.logger.Printf("resource request %x on link %x: %v", requestID[:4], linkID[:4], err)
				t.cancelPendingRequest(requestID)
				receipt.deliver(requestResult{err: fmt.Errorf("resource request: %w", err)})
			}
		}()
		t.logger.Printf("request %x sent on link %x path %q by Resource (%d bytes)", requestID[:4], linkID[:4], path, len(packed))
		return receipt, nil
	}

	if err := t.Broadcast(pkt); err != nil {
		t.cancelPendingRequest(requestID)
		return nil, err
	}
	t.logger.Printf("request %x sent on link %x path %q (%d bytes)", requestID[:4], linkID[:4], path, len(packed))
	return receipt, nil
}

// CancelRequest drops a pending receipt without waiting for a response.
func (t *Transport) CancelRequest(r *RequestReceipt) {
	if r != nil {
		t.cancelPendingRequest(r.ID)
	}
}

func (t *Transport) cancelPendingRequest(requestID []byte) {
	t.requestMu.Lock()
	delete(t.pendingRequests, hex.EncodeToString(requestID))
	t.requestMu.Unlock()
}

func (t *Transport) takePendingRequest(requestID []byte) *RequestReceipt {
	t.requestMu.Lock()
	defer t.requestMu.Unlock()
	key := hex.EncodeToString(requestID)
	r := t.pendingRequests[key]
	delete(t.pendingRequests, key)
	return r
}

// handleRequest processes an inbound SINGLE-PACKET §11.1 REQUEST:
// decrypt, derive the request_id from this packet's wire bytes, and hand
// off to the shared server logic.
func (t *Transport) handleRequest(p *Packet) {
	l := t.linkManager.Get(p.DestHash)
	if l == nil {
		t.logger.Printf("request: unknown link_id %x", p.DestHash)
		return
	}
	l.mu.Lock()
	state := l.State
	signing, encryption := l.Signing, l.Encryption
	l.mu.Unlock()
	if state != LinkActive {
		t.logger.Printf("request on link %x in state %s", p.DestHash[:4], state)
		return
	}

	plaintext, err := LinkTokenDecrypt(p.Data, signing, encryption)
	if err != nil {
		t.logger.Printf("request decrypt: %v", err)
		return
	}
	// The peer just proved it holds the session keys, so this counts as
	// activity for the idle sweep. Without it a link serving requests
	// and nothing else is torn down mid-session — see NoteInboundTraffic.
	l.NoteInboundTraffic()
	requestID, err := RequestIDFromPacket(p)
	if err != nil {
		t.logger.Printf("request id: %v", err)
		return
	}
	t.serveRequest(l, plaintext, requestID)
}

// serveRequest is the form-agnostic server half: parse the envelope,
// find the handler, apply the §11.4 allow mode, and emit the §11.2
// RESPONSE labelled with requestID.
//
// The two REQUEST forms reach here having derived requestID differently
// — a single packet from its own wire-byte hash, a Resource from the
// advertisement's `q` — so it is passed in rather than recomputed.
func (t *Transport) serveRequest(l *Link, plaintext, requestID []byte) {
	l.mu.Lock()
	signing, encryption := l.Signing, l.Encryption
	linkID := append([]byte(nil), l.ID...)
	l.mu.Unlock()

	ts, pathHash, data, err := ParseRequest(plaintext)
	if err != nil {
		t.logger.Printf("request parse: %v", err)
		return
	}

	entry := t.lookupRequestHandler(pathHash)
	if entry == nil {
		// Upstream drops silently; log so an operator can see a client
		// asking for a path this node does not serve.
		t.logger.Printf("request: %v (path_hash %x on link %x)", ErrRequestNoHandler, pathHash[:4], linkID[:4])
		return
	}

	remote := l.RemoteIdentity()
	if !entry.permits(remote) {
		t.logger.Printf("request %q refused on link %x: %v (allow=%d, identified=%t)",
			entry.path, linkID[:4], ErrRequestNotAllowed, entry.allow, remote != nil)
		return
	}

	response, err := entry.handler(&RequestContext{
		Path:           entry.path,
		PathHash:       pathHash,
		Data:           data,
		Timestamp:      ts,
		LinkID:         linkID,
		RemoteIdentity: remote,
	})
	if err != nil {
		// A generator that errors sends nothing, matching upstream's
		// None return. The initiator's own timeout covers it.
		t.logger.Printf("request handler %q: %v", entry.path, err)
		return
	}

	packed, err := PackResponse(requestID, response)
	if err != nil {
		t.logger.Printf("pack response for %q: %v", entry.path, err)
		return
	}
	if len(packed) > LinkMDU {
		// §11.2: a response past the link MDU rides a Resource, labelled
		// with this request_id and the `p` is_response flag so the
		// initiator's receipt claims it. This is the ordinary case, not
		// the exception — at a 431-byte MDU most real answers exceed it.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), resourceTransferTimeout(len(packed)))
			defer cancel()
			if err := t.sendRPCResourceOverLink(ctx, l, packed, nil, requestID, ResourceFlagIsResponse); err != nil {
				t.logger.Printf("resource response for %q: %v", entry.path, err)
			}
		}()
		t.logger.Printf("request %q answered on link %x by Resource (id %x, %d bytes)", entry.path, linkID[:4], requestID[:4], len(packed))
		return
	}

	respPkt, err := BuildLinkDataPacket(linkID, signing, encryption, packed)
	if err != nil {
		t.logger.Printf("build response packet: %v", err)
		return
	}
	respPkt.Context = ContextResponse
	if err := t.Broadcast(respPkt); err != nil {
		t.logger.Printf("response broadcast: %v", err)
		return
	}
	t.logger.Printf("request %q answered on link %x (id %x, %d bytes)", entry.path, linkID[:4], requestID[:4], len(packed))
}

// serveResourceRequest handles a §11.1 REQUEST that arrived as a
// Resource. The advertisement carried the request_id in `q`; the
// assembled body is the same envelope a single-packet REQUEST carries.
func (t *Transport) serveResourceRequest(l *Link, requestID, body []byte) {
	t.serveRequest(l, body, requestID)
}

// handleResponse processes an inbound §11.2 RESPONSE and hands it to
// the receipt that asked for it.
func (t *Transport) handleResponse(p *Packet) {
	l := t.linkManager.Get(p.DestHash)
	if l == nil {
		t.logger.Printf("response: unknown link_id %x", p.DestHash)
		return
	}
	l.mu.Lock()
	signing, encryption := l.Signing, l.Encryption
	l.mu.Unlock()

	plaintext, err := LinkTokenDecrypt(p.Data, signing, encryption)
	if err != nil {
		t.logger.Printf("response decrypt: %v", err)
		return
	}
	l.NoteInboundTraffic()
	requestID, response, err := ParseResponse(plaintext)
	if err != nil {
		t.logger.Printf("response parse: %v", err)
		return
	}
	t.deliverResponse(requestID, response)
}

// deliverResponse routes a decoded response to its receipt. Shared by
// the single-packet path and the Resource-carried one.
//
// §11.2 is emphatic that element [0] MUST be checked: without it a
// misbehaving relay can replay a stale RESPONSE and the initiator
// accepts it as the answer to whatever is currently pending. Matching
// on the id is that check — an unmatched response is dropped, never
// handed to whichever request happens to be in flight.
func (t *Transport) deliverResponse(requestID []byte, response any) {
	receipt := t.takePendingRequest(requestID)
	if receipt == nil {
		t.logger.Printf("response: %v (id %x)", ErrResponseUnsolicited, requestID[:4])
		return
	}
	receipt.deliver(requestResult{response: response})
	t.logger.Printf("request %x answered (path %q)", requestID[:4], receipt.Path)
}

// deliverResourceResponse handles a §11.2 RESPONSE that arrived as a
// Resource (§10) rather than a single packet — the ordinary case for
// anything larger than the link MDU, which covers most NomadNet pages
// and every non-trivial propagation /get round. The advertisement
// carried the request_id in `q`; the assembled body is the same
// msgpack([request_id, response]) envelope.
func (t *Transport) deliverResourceResponse(requestID, body []byte) {
	innerID, response, err := ParseResponse(body)
	if err != nil {
		t.logger.Printf("resource response parse (id %x): %v", requestID[:4], err)
		return
	}
	if !bytesEqual(innerID, requestID) {
		// The advertisement and the envelope disagree about which
		// request this answers. Trust neither.
		t.logger.Printf("resource response id mismatch: adv %x, envelope %x", requestID[:4], innerID[:4])
		return
	}
	t.deliverResponse(requestID, response)
}
