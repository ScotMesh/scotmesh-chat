package rns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

// SPEC §6.8 — Channel: a continuous, bi-directional, message-typed
// stream over an established Link.
//
// Distinct from the other two things that ride a Link: §11
// REQUEST/RESPONSE is single-shot and client-server, §10 Resources are
// large and unidirectional. Channel messages are short, flow either way
// at any time, and carry an application-defined type the receiver
// dispatches on.
const (
	// ContextChannel is the link-DATA context byte for a Channel
	// message (§6.8).
	ContextChannel = 0x0E

	// ChannelHeaderLen is the fixed 6-byte prefix: msgtype, sequence
	// and length, each a big-endian uint16 (§6.8.1).
	ChannelHeaderLen = 6

	// SMTStreamData is the reserved system message type upstream uses
	// for its stream-over-channel implementation (§6.8.2). Application
	// types should stay in 0x0000..0xfeff to avoid colliding with
	// reserved ones.
	SMTStreamData = 0xff00

	// StreamHeaderLen is the extra 2-byte header a stream message
	// spends inside the channel payload (§6.8.2).
	StreamHeaderLen = 2

	// StreamIDMax is the largest stream id: the header spends 2 bits on
	// flags, leaving 14 for the id.
	StreamIDMax = 0x3fff
)

// ChannelMDU is the usable payload of one Channel message:
// link.mdu minus the §6.8.1 envelope.
const ChannelMDU = LinkMDU - ChannelHeaderLen

// StreamMaxDataLen is the stream payload per packet — the channel MDU
// less the stream's own 2-byte header.
//
// It is 423 bytes at the default MTU, not 417. Upstream's
// RawChannelWriter used to subtract the 6-byte channel envelope a
// SECOND time, having already accounted for it in Channel.mdu; RNS
// 1.5.0 fixed that (RNS/Buffer.py:230). The wire form did not change and
// both sizes interoperate — a smaller chunk is simply a smaller chunk —
// so this is a throughput figure, not a compatibility one.
const StreamMaxDataLen = ChannelMDU - StreamHeaderLen

var (
	// ErrChannelMessageTooLarge is returned when a payload exceeds what
	// one Channel message can carry.
	ErrChannelMessageTooLarge = errors.New("channel payload exceeds the channel MDU")
	// ErrChannelMalformed is returned for a body that is not a valid
	// §6.8.1 message.
	ErrChannelMalformed = errors.New("malformed channel message")
)

// ChannelMessage is one §6.8.1 message.
type ChannelMessage struct {
	// Type is the application-defined message type the receiver
	// dispatches on.
	Type uint16
	// Sequence is the per-direction counter, starting at 0 and wrapping
	// at 65536.
	Sequence uint16
	// Data is the payload.
	Data []byte
}

// PackChannelMessage encodes the §6.8.1 wire form:
//
//	msgtype(2) || sequence(2) || length(2) || data
//
// All three header fields are big-endian uint16 — Python's
// struct.pack(">HHH", ...). The result is the link DATA plaintext, which
// the caller Token-encrypts under the link's session key.
func PackChannelMessage(m ChannelMessage) ([]byte, error) {
	if len(m.Data) > ChannelMDU {
		return nil, fmt.Errorf("%w: %d bytes, limit is %d", ErrChannelMessageTooLarge, len(m.Data), ChannelMDU)
	}
	if len(m.Data) > 0xffff {
		return nil, fmt.Errorf("%w: payload does not fit the uint16 length field", ErrChannelMessageTooLarge)
	}
	out := make([]byte, ChannelHeaderLen+len(m.Data))
	binary.BigEndian.PutUint16(out[0:2], m.Type)
	binary.BigEndian.PutUint16(out[2:4], m.Sequence)
	binary.BigEndian.PutUint16(out[4:6], uint16(len(m.Data)))
	copy(out[ChannelHeaderLen:], m.Data)
	return out, nil
}

// ParseChannelMessage decodes a §6.8.1 body.
//
// The declared length must match what actually arrived. Trusting the
// header over the frame would let a peer describe a 65535-byte payload
// in a 10-byte packet and have the receiver read past it, or silently
// truncate a message whose header disagrees with its own body.
func ParseChannelMessage(body []byte) (ChannelMessage, error) {
	if len(body) < ChannelHeaderLen {
		return ChannelMessage{}, fmt.Errorf("%w: %d bytes, need at least %d", ErrChannelMalformed, len(body), ChannelHeaderLen)
	}
	declared := int(binary.BigEndian.Uint16(body[4:6]))
	payload := body[ChannelHeaderLen:]
	if declared != len(payload) {
		return ChannelMessage{}, fmt.Errorf("%w: header declares %d payload bytes, frame carries %d",
			ErrChannelMalformed, declared, len(payload))
	}
	return ChannelMessage{
		Type:     binary.BigEndian.Uint16(body[0:2]),
		Sequence: binary.BigEndian.Uint16(body[2:4]),
		Data:     append([]byte(nil), payload...),
	}, nil
}

// PackStreamHeader builds the 2-byte header a SMT_STREAM_DATA payload
// carries (§6.8.2, RNS/Buffer.py pack):
//
//	(stream_id & 0x3fff) | (0x8000 if eof) | (0x4000 if compressed)
func PackStreamHeader(streamID uint16, eof, compressed bool) ([]byte, error) {
	if streamID > StreamIDMax {
		return nil, fmt.Errorf("stream_id %d exceeds the 14-bit maximum %d", streamID, StreamIDMax)
	}
	v := streamID & StreamIDMax
	if eof {
		v |= 0x8000
	}
	if compressed {
		v |= 0x4000
	}
	out := make([]byte, StreamHeaderLen)
	binary.BigEndian.PutUint16(out, v)
	return out, nil
}

// ParseStreamHeader decodes the 2-byte stream header and returns the
// payload that follows it.
func ParseStreamHeader(payload []byte) (streamID uint16, eof, compressed bool, data []byte, err error) {
	if len(payload) < StreamHeaderLen {
		return 0, false, false, nil, fmt.Errorf("%w: stream payload is %d bytes, need at least %d",
			ErrChannelMalformed, len(payload), StreamHeaderLen)
	}
	v := binary.BigEndian.Uint16(payload[:StreamHeaderLen])
	return v & StreamIDMax, v&0x8000 != 0, v&0x4000 != 0, payload[StreamHeaderLen:], nil
}

// ChannelHandler receives one inbound message on a channel.
type ChannelHandler func(*Channel, ChannelMessage)

// Channel is one link's message-typed stream. Sequence numbers are
// per-direction: ours counts what we send, and the peer's counter is
// theirs — they are independent and MUST NOT be validated against each
// other.
type Channel struct {
	transport *Transport
	linkID    []byte

	mu       sync.Mutex
	outSeq   uint16
	handlers map[uint16]ChannelHandler
	fallback ChannelHandler
}

// OpenChannel returns the Channel for a link, creating it on first use.
func (t *Transport) OpenChannel(linkID []byte) (*Channel, error) {
	if t.linkManager.Get(linkID) == nil {
		return nil, fmt.Errorf("OpenChannel: unknown link_id %x", linkID)
	}
	key := hexOfBytes(linkID)
	t.channelMu.Lock()
	defer t.channelMu.Unlock()
	if t.channels == nil {
		t.channels = map[string]*Channel{}
	}
	if c, ok := t.channels[key]; ok {
		return c, nil
	}
	c := &Channel{
		transport: t,
		linkID:    append([]byte(nil), linkID...),
		handlers:  map[uint16]ChannelHandler{},
	}
	t.channels[key] = c
	return c, nil
}

// CloseChannel forgets a link's channel and its handlers.
func (t *Transport) CloseChannel(linkID []byte) {
	t.channelMu.Lock()
	delete(t.channels, hexOfBytes(linkID))
	t.channelMu.Unlock()
}

func (t *Transport) channelFor(linkID []byte) *Channel {
	t.channelMu.Lock()
	defer t.channelMu.Unlock()
	return t.channels[hexOfBytes(linkID)]
}

// Handle registers a handler for one message type, replacing any
// previous one. A nil handler removes it.
func (c *Channel) Handle(msgType uint16, h ChannelHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h == nil {
		delete(c.handlers, msgType)
		return
	}
	c.handlers[msgType] = h
}

// HandleDefault registers a handler for types with no specific one.
// Without it, an unregistered type is dropped — matching upstream, whose
// message_factories dict simply has no entry.
func (c *Channel) HandleDefault(h ChannelHandler) {
	c.mu.Lock()
	c.fallback = h
	c.mu.Unlock()
}

// Send emits one message, assigning the next outbound sequence number.
func (c *Channel) Send(msgType uint16, data []byte) (uint16, error) {
	link := c.transport.linkManager.Get(c.linkID)
	if link == nil {
		return 0, fmt.Errorf("channel send: unknown link_id %x", c.linkID)
	}
	link.mu.Lock()
	state := link.State
	signing, encryption := link.Signing, link.Encryption
	link.mu.Unlock()
	if state != LinkActive {
		return 0, fmt.Errorf("channel send: link is %s, want active", state)
	}

	c.mu.Lock()
	seq := c.outSeq
	c.outSeq++ // wraps at 65536 by construction (§6.8.1)
	c.mu.Unlock()

	body, err := PackChannelMessage(ChannelMessage{Type: msgType, Sequence: seq, Data: data})
	if err != nil {
		return 0, err
	}
	pkt, err := BuildLinkDataPacket(c.linkID, signing, encryption, body)
	if err != nil {
		return 0, err
	}
	pkt.Context = ContextChannel
	if err := c.transport.Broadcast(pkt); err != nil {
		return 0, err
	}
	return seq, nil
}

// SendStream emits a SMT_STREAM_DATA message carrying one chunk.
func (c *Channel) SendStream(streamID uint16, data []byte, eof bool) (uint16, error) {
	if len(data) > StreamMaxDataLen {
		return 0, fmt.Errorf("%w: stream chunk is %d bytes, limit is %d",
			ErrChannelMessageTooLarge, len(data), StreamMaxDataLen)
	}
	header, err := PackStreamHeader(streamID, eof, false)
	if err != nil {
		return 0, err
	}
	return c.Send(SMTStreamData, append(header, data...))
}

// dispatch delivers an inbound message to its handler.
func (c *Channel) dispatch(m ChannelMessage) {
	c.mu.Lock()
	h, ok := c.handlers[m.Type]
	if !ok {
		h = c.fallback
	}
	c.mu.Unlock()
	if h != nil {
		h(c, m)
	}
}

// handleChannel routes an inbound §6.8 message.
func (t *Transport) handleChannel(p *Packet) {
	l := t.linkManager.Get(p.DestHash)
	if l == nil {
		t.logger.Printf("channel: unknown link_id %x", p.DestHash)
		return
	}
	l.mu.Lock()
	signing, encryption := l.Signing, l.Encryption
	l.mu.Unlock()

	plaintext, err := LinkTokenDecrypt(p.Data, signing, encryption)
	if err != nil {
		t.logger.Printf("channel decrypt: %v", err)
		return
	}
	l.NoteInboundTraffic()
	m, err := ParseChannelMessage(plaintext)
	if err != nil {
		t.logger.Printf("channel parse: %v", err)
		return
	}
	c := t.channelFor(p.DestHash)
	if c == nil {
		// No channel opened on this link, so nothing is listening.
		// Upstream behaves the same way: a Channel exists only once the
		// application asks for one.
		t.logger.Printf("channel message type 0x%04x on link %x with no channel open", m.Type, p.DestHash[:4])
		return
	}
	c.dispatch(m)
}

func hexOfBytes(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&0x0f])
	}
	return string(out)
}
