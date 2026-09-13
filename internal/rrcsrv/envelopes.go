package rrcsrv

// Framing replies and splitting long text to fit a link follow rrcd
// 0.3.2's messages.py, Copyright (c) 2025 S. Miller, KC1AWV, MIT License —
// see the package doc in server.go.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// maxFrame is the largest envelope that fits one link DATA packet.
const maxFrame = rns.LinkMDU

// hubEnvelope is an envelope from the hub itself.
func (s *Server) hubEnvelope(t wire.Type, room string) *wire.Envelope {
	return &wire.Envelope{Type: t, ID: wire.NewID(), TS: uint64(s.now().UnixMilli()), Src: s.hubHash, Room: room} //nolint:gosec // after 1970
}

func encode(e *wire.Envelope) ([]byte, error) {
	b, err := e.Encode()
	if err != nil {
		return nil, err
	}
	if len(b) > maxFrame {
		return nil, fmt.Errorf("%s envelope is %d bytes, over the %d-byte link limit", e.Type, len(b), maxFrame)
	}
	return b, nil
}

// noticeFrames turns text into NOTICE frames: one per line, each line split
// so every frame fits the link. MeshChatX shows only the first line of a
// multi-line NOTICE, and rrcd sends one per line too.
func (s *Server) noticeFrames(t wire.Type, room, text string) [][]byte {
	var frames [][]byte
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		e := s.hubEnvelope(t, room)
		for _, part := range s.fitText(e, line) {
			e.ID = wire.NewID()
			e.Body = wire.TextBody(part)
			b, err := encode(e)
			if err != nil {
				s.log.Error("rrc: building a notice", "err", err)
				continue
			}
			frames = append(frames, b)
		}
	}
	return frames
}

// fitText splits text so that e, carrying any one piece as its body, fits
// the link and the hub's message body limit.
func (s *Server) fitText(e *wire.Envelope, text string) []string {
	return splitText(text, s.bodyBudget(e))
}

// bodyBudget is the longest text body e can carry in one frame.
func (s *Server) bodyBudget(e *wire.Envelope) int {
	probe := *e
	probe.ID = make([]byte, max(len(e.ID), 8))
	probe.Body = wire.TextBody("")
	empty, err := probe.Encode()
	if err != nil {
		return 16
	}
	// A text string's CBOR header grows from 1 to 3 bytes past 23 bytes.
	return max(min(maxFrame-len(empty)-2, s.cfg.MaxMsgBodyBytes), 16)
}

// splitText cuts text into pieces of at most limit bytes.
func splitText(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var out []string
	for len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if sp := strings.LastIndexByte(text[:cut], ' '); sp > limit/2 {
			cut = sp + 1
		}
		out = append(out, strings.TrimRight(text[:cut], " "))
		text = strings.TrimLeft(text[cut:], " ")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// whisperFrames builds the direct NOTICE frames (K_DST) that carry a whisper
// to one of the recipient's links: from the sender's identity and name, with
// "(whisper)" leading the text so it reads right in a client that shows it
// as a plain notice.
func (s *Server) whisperFrames(w *store.Whisper, to []byte) [][]byte {
	e := &wire.Envelope{Type: wire.TypeNotice, TS: uint64(max(w.SaidAt, 0)), Src: w.From, Nick: w.FromName, Dst: to} //nolint:gosec // checked non-negative
	var frames [][]byte
	for _, part := range s.fitText(e, "(whisper) "+w.Body) {
		e.ID = wire.NewID()
		e.Body = wire.TextBody(part)
		b, err := encode(e)
		if err != nil {
			s.log.Error("rrc: building a whisper", "whisper", w.ID, "err", err)
			continue
		}
		frames = append(frames, b)
	}
	return frames
}

// messageFrames builds the MSG or ACTION frames for a stored message, as
// every client sees it: the author's real identity as K_SRC, the name they
// hold as K_NICK, the author's clock as K_TS, and the original K_ID and
// extension fields when it came over RRC. A body too long for one frame is
// sent in numbered parts.
func (s *Server) messageFrames(m *store.Message) [][]byte {
	t := wire.TypeMsg
	if m.Kind == store.KindAction {
		t = wire.TypeAction
	}
	e := &wire.Envelope{Type: t, ID: messageID(m, 0), TS: uint64(max(m.SaidAt, 0)), Src: m.Author, Room: m.Room, Nick: m.AuthorName} //nolint:gosec // checked non-negative
	if len(m.Ext) > 0 {
		var ext map[wire.Key]cbor.RawMessage
		if err := cbor.Unmarshal(m.Ext, &ext); err == nil {
			e.Ext = ext
		}
	}
	budget := s.bodyBudget(e)
	parts := []string{m.Body}
	if len(m.Body) > budget {
		// Extensions (a reply target, a reaction) belong to the whole
		// message, not to a part; the parts carry " (1/3)" instead.
		e.Ext = nil
		parts = splitText(m.Body, s.bodyBudget(e)-len(" (00/00)"))
	}
	frames := make([][]byte, 0, len(parts))
	for i, p := range parts {
		body := p
		if len(parts) > 1 {
			e.ID = messageID(m, i+1)
			body = fmt.Sprintf("%s (%d/%d)", p, i+1, len(parts))
		}
		e.Body = wire.TextBody(body)
		b, err := encode(e)
		if err != nil {
			s.log.Error("rrc: building a message frame", "message", m.ID, "err", err)
			continue
		}
		frames = append(frames, b)
	}
	return frames
}

// messageID is the K_ID a message carries: its original ID if it came over
// RRC, otherwise one derived from the hub's message ID, so a replay or a
// part carries the same ID every time.
func messageID(m *store.Message, part int) []byte {
	if part == 0 && m.Via == store.ViaRRC && len(m.OriginID) > 0 {
		return m.OriginID
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(m.ID)) //nolint:gosec // IDs are positive
	sum := sha256.Sum256(append([]byte(fmt.Sprintf("scotmesh-chat/%s/%d/", m.Room, part)), buf[:]...))
	return sum[:8]
}
