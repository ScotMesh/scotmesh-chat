package rns

import (
	"errors"
	"fmt"
	"io"
)

// SPEC §8.1 — KISS framing, used on serial, BLE and the RNode host
// channel. Distinct from the HDLC framing TCP uses (§8.2): different
// delimiter, different escape, and a command byte at the head of every
// frame that HDLC has no equivalent of.
const (
	// KissFEND delimits a frame.
	KissFEND = 0xC0
	// KissFESC escapes a literal delimiter or escape byte.
	KissFESC = 0xDB
	// KissTFEND is what an escaped FEND becomes.
	KissTFEND = 0xDC
	// KissTFESC is what an escaped FESC becomes.
	KissTFESC = 0xDD

	// CmdData carries a Reticulum packet in either direction.
	CmdData = 0x00
	// CmdStatRSSI prefixes a received frame with the signal strength of
	// the LoRa frame that carried it. Payload is one byte; the signed
	// value is byte - 157.
	CmdStatRSSI = 0x23
	// CmdStatSNR likewise carries signal-to-noise as a signed Q6.2 —
	// divide by 4 for dB.
	CmdStatSNR = 0x24
)

// MaxKISSFrameLen bounds an assembled frame, for the same reason
// MaxHDLCFrameLen exists: without it a peer that streams non-FEND bytes
// forever drives allocation until the process dies, remotely and before
// any parse or crypto. Generous against the 508-byte RNode MTU.
const MaxKISSFrameLen = 8192

// ErrKISSFrameTooLong is returned when a frame exceeds MaxKISSFrameLen.
var ErrKISSFrameTooLong = errors.New("KISS frame exceeds the maximum length")

// EncodeKISS frames a payload under `cmd`, escaping the two reserved
// bytes (§8.1).
func EncodeKISS(cmd byte, data []byte) []byte {
	out := make([]byte, 0, len(data)+8)
	out = append(out, KissFEND, cmd)
	for _, b := range data {
		switch b {
		case KissFEND:
			out = append(out, KissFESC, KissTFEND)
		case KissFESC:
			out = append(out, KissFESC, KissTFESC)
		default:
			out = append(out, b)
		}
	}
	return append(out, KissFEND)
}

// KISSDecoder reassembles KISS frames from a byte stream.
//
// Streaming matters here beyond tidiness: over BLE a frame is split
// across notifications with no alignment to frame boundaries (§8.1), so
// a decoder that assumes one read yields one frame drops most traffic.
type KISSDecoder struct {
	r       io.Reader
	buf     []byte
	pending []byte
	inFrame bool
	escaped bool
	maxLen  int
}

// NewKISSDecoder wraps a stream.
func NewKISSDecoder(r io.Reader) *KISSDecoder {
	return &KISSDecoder{r: r, buf: make([]byte, 1024), maxLen: MaxKISSFrameLen}
}

// NextFrame returns the next complete frame's command byte and payload.
//
// Empty frames — two consecutive FENDs — are skipped rather than
// returned, matching how the delimiter doubles as both terminator and
// start marker on an idle line.
func (d *KISSDecoder) NextFrame() (cmd byte, payload []byte, err error) {
	for {
		for len(d.pending) > 0 {
			b := d.pending[0]
			d.pending = d.pending[1:]

			if b == KissFEND {
				if d.inFrame && len(d.buf) > 0 {
					frame := d.buf
					d.buf = nil
					d.inFrame = false
					d.escaped = false
					if len(frame) < 1 {
						continue // delimiter with no command byte
					}
					return frame[0], frame[1:], nil
				}
				// Start (or restart) a frame.
				d.inFrame = true
				d.buf = nil
				d.escaped = false
				continue
			}
			if !d.inFrame {
				continue // noise between frames
			}
			switch {
			case d.escaped:
				switch b {
				case KissTFEND:
					b = KissFEND
				case KissTFESC:
					b = KissFESC
				}
				d.escaped = false
			case b == KissFESC:
				d.escaped = true
				continue
			}
			if len(d.buf) >= d.maxLen {
				d.inFrame = false
				d.buf = nil
				return 0, nil, ErrKISSFrameTooLong
			}
			d.buf = append(d.buf, b)
		}

		chunk := make([]byte, 1024)
		n, err := d.r.Read(chunk)
		if n > 0 {
			d.pending = append(d.pending, chunk[:n]...)
			continue
		}
		if err != nil {
			return 0, nil, err
		}
	}
}

// SPEC §8.3 — the RNode LoRa air-frame header.
//
// This byte lives BETWEEN RNodes on the air, never on the KISS host
// channel: the firmware adds it on TX and strips it on RX before handing
// the payload up. It matters only to something talking LoRa directly to
// an RNode — a clean-room repeater firmware — which must build and parse
// it bit-exactly or its transmissions are invisible and its receptions
// mistake the header for the first payload byte.
const (
	// AirFrameSplitFlag marks the first half of a two-frame packet.
	AirFrameSplitFlag = 0x01
	// AirFrameSeqMask is the high nibble: a random sequence id per TX.
	AirFrameSeqMask = 0xF0
	// AirFrameSeqUnset is the sentinel for "no first half buffered".
	AirFrameSeqUnset = 0xFF

	// RNodeMTU is the largest reassembled packet: two LoRa frames'
	// payloads.
	RNodeMTU = 508
	// RNodeSingleMTU is the largest LoRa frame, header included.
	RNodeSingleMTU = 255
	// RNodeHeaderLen is the per-frame air-frame header overhead.
	RNodeHeaderLen = 1
)

// AirFrameHeader builds the §8.3 header byte: the sequence id in the
// high nibble, and FLAG_SPLIT set iff the payload needs two frames.
func AirFrameHeader(seq byte, split bool) byte {
	h := seq & AirFrameSeqMask
	if split {
		h |= AirFrameSplitFlag
	}
	return h
}

// AirFrameIsSplit reports whether a header marks a split packet.
func AirFrameIsSplit(header byte) bool { return header&AirFrameSplitFlag != 0 }

// AirFrameSequence extracts the sequence id (0..15).
func AirFrameSequence(header byte) byte { return header >> 4 }

// SplitAirFrames splits a Reticulum packet into LoRa air frames, each
// carrying the §8.3 header.
//
// A payload up to SINGLE_MTU - HEADER_L rides one frame; anything larger
// is split into two halves sharing a sequence id, the first flagged.
// Beyond RNodeMTU it cannot be carried at all — two frames is the whole
// protocol, not a chunking scheme.
func SplitAirFrames(packet []byte, seq byte) ([][]byte, error) {
	if len(packet) == 0 {
		return nil, errors.New("empty packet")
	}
	if len(packet) > RNodeMTU {
		return nil, fmt.Errorf("packet is %d bytes, RNode carries at most %d", len(packet), RNodeMTU)
	}
	max := RNodeSingleMTU - RNodeHeaderLen
	if len(packet) <= max {
		return [][]byte{append([]byte{AirFrameHeader(seq, false)}, packet...)}, nil
	}
	first := append([]byte{AirFrameHeader(seq, true)}, packet[:max]...)
	second := append([]byte{AirFrameHeader(seq, false)}, packet[max:]...)
	return [][]byte{first, second}, nil
}

// AirFrameReassembler glues split air frames back into packets.
type AirFrameReassembler struct {
	seq   byte
	first []byte
}

// NewAirFrameReassembler returns a reassembler with no half buffered.
func NewAirFrameReassembler() *AirFrameReassembler {
	return &AirFrameReassembler{seq: AirFrameSeqUnset}
}

// Push feeds one received air frame and returns a complete packet when
// one is ready.
//
// A second half whose sequence does not match the buffered first is
// dropped along with the buffered half: the two came from different
// transmissions, and concatenating them would produce a packet neither
// sender sent.
func (a *AirFrameReassembler) Push(frame []byte) ([]byte, error) {
	if len(frame) < RNodeHeaderLen+1 {
		return nil, fmt.Errorf("air frame is %d bytes, too short to carry a header and payload", len(frame))
	}
	header, payload := frame[0], frame[1:]

	if AirFrameIsSplit(header) {
		a.seq = AirFrameSequence(header)
		a.first = append([]byte(nil), payload...)
		return nil, nil
	}
	if a.seq == AirFrameSeqUnset {
		return append([]byte(nil), payload...), nil // ordinary single frame
	}
	if AirFrameSequence(header) != a.seq {
		a.seq = AirFrameSeqUnset
		a.first = nil
		return nil, fmt.Errorf("air frame sequence %d does not match the buffered %d; both discarded",
			AirFrameSequence(header), a.seq)
	}
	out := append(a.first, payload...)
	a.seq = AirFrameSeqUnset
	a.first = nil
	return out, nil
}
