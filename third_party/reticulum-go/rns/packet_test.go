package rns

import (
	"bytes"
	"errors"
	"testing"
)

func newDummyHash(seed byte) []byte {
	out := make([]byte, addressHashLen)
	for i := range out {
		out[i] = seed
	}
	return out
}

func TestPackUnpackHeader1(t *testing.T) {
	p := &Packet{
		HeaderType:      HeaderType1,
		ContextFlag:     false,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationSingle,
		PacketType:      PacketAnnounce,
		Hops:            0,
		DestHash:        newDummyHash(0xAB),
		Context:         ContextNone,
		Data:            []byte("hello"),
	}
	wire, err := p.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != header1MinLen+len("hello") {
		t.Errorf("wire length = %d, want %d", len(wire), header1MinLen+5)
	}

	parsed, err := ParsePacket(wire)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.HeaderType != HeaderType1 {
		t.Errorf("header_type = %d", parsed.HeaderType)
	}
	if parsed.ContextFlag {
		t.Error("context_flag should be false")
	}
	if parsed.TransportType != BroadcastTransport {
		t.Errorf("transport_type = %d", parsed.TransportType)
	}
	if parsed.DestinationType != DestinationSingle {
		t.Errorf("dest_type = %d", parsed.DestinationType)
	}
	if parsed.PacketType != PacketAnnounce {
		t.Errorf("packet_type = %d", parsed.PacketType)
	}
	if !bytes.Equal(parsed.DestHash, p.DestHash) {
		t.Errorf("dest_hash mismatch")
	}
	if parsed.TransportID != nil {
		t.Errorf("HEADER_1 should not have transport_id, got %x", parsed.TransportID)
	}
	if !bytes.Equal(parsed.Data, []byte("hello")) {
		t.Errorf("data = %x", parsed.Data)
	}
}

func TestPackUnpackHeader2(t *testing.T) {
	p := &Packet{
		HeaderType:      HeaderType2,
		ContextFlag:     true, // announce-with-ratchet
		TransportType:   NetworkTransport,
		DestinationType: DestinationSingle,
		PacketType:      PacketData,
		Hops:            3,
		TransportID:     newDummyHash(0x11),
		DestHash:        newDummyHash(0x22),
		Context:         ContextPathResponse,
		Data:            []byte{0x01, 0x02, 0x03},
	}
	wire, err := p.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != header2MinLen+3 {
		t.Errorf("wire length = %d, want %d", len(wire), header2MinLen+3)
	}

	parsed, err := ParsePacket(wire)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.HeaderType != HeaderType2 {
		t.Errorf("header_type = %d", parsed.HeaderType)
	}
	if !parsed.ContextFlag {
		t.Error("context_flag should be true")
	}
	if !bytes.Equal(parsed.TransportID, p.TransportID) {
		t.Errorf("transport_id mismatch")
	}
	if !bytes.Equal(parsed.DestHash, p.DestHash) {
		t.Errorf("dest_hash mismatch")
	}
	if parsed.Hops != 3 {
		t.Errorf("hops = %d", parsed.Hops)
	}
	if parsed.Context != ContextPathResponse {
		t.Errorf("context = 0x%02x", parsed.Context)
	}
}

func TestFlagBitsRoundTripExhaustive(t *testing.T) {
	for hdr := byte(0); hdr <= 1; hdr++ {
		for cf := 0; cf < 2; cf++ {
			for tr := byte(0); tr <= 1; tr++ {
				for dt := byte(0); dt <= 3; dt++ {
					for pt := byte(0); pt <= 3; pt++ {
						f := packFlags(hdr, cf == 1, tr, dt, pt)
						// packFlags must never set bit 7 (ifac_flag).
						if f&0x80 != 0 {
							t.Errorf("packFlags set bit 7 for (%d,%d,%d,%d,%d) -> 0x%02x",
								hdr, cf, tr, dt, pt, f)
						}
						gotIFAC, gotHdr, gotCF, gotTr, gotDt, gotPt := unpackFlags(f)
						if gotIFAC {
							t.Errorf("unpackFlags read ifac_flag=true from packFlags output 0x%02x", f)
						}
						if gotHdr != hdr || (gotCF && cf == 0) || (!gotCF && cf == 1) ||
							gotTr != tr || gotDt != dt || gotPt != pt {
							t.Errorf("flags round-trip failed for (%d,%d,%d,%d,%d) -> 0x%02x -> (%d,%v,%d,%d,%d)",
								hdr, cf, tr, dt, pt, f,
								gotHdr, gotCF, gotTr, gotDt, gotPt)
						}
					}
				}
			}
		}
	}
}

// TestUnpackFlagsReadsIFACBit confirms unpackFlags pulls bit 7 as
// ifac_flag and bit 6 as a 1-bit header_type, consistent with the SPEC
// §2.1 corrected layout (and upstream RNS/Packet.py:246 parse masks).
func TestUnpackFlagsReadsIFACBit(t *testing.T) {
	cases := []struct {
		flags    byte
		ifac     bool
		hdr      byte
		expected string
	}{
		{0x00, false, 0, "all zero"},
		{0x40, false, 1, "header_type=1, ifac=0"},
		{0x80, true, 0, "ifac=1, header_type=0"},
		{0xC0, true, 1, "ifac=1, header_type=1"},
	}
	for _, tc := range cases {
		ifac, hdr, _, _, _, _ := unpackFlags(tc.flags)
		if ifac != tc.ifac || hdr != tc.hdr {
			t.Errorf("%s (flags=0x%02x): got ifac=%v hdr=%d, want ifac=%v hdr=%d",
				tc.expected, tc.flags, ifac, hdr, tc.ifac, tc.hdr)
		}
	}
}

// TestParseRejectsIFACSealed confirms inbound IFAC-sealed packets are
// rejected with errIFACUnsupported instead of being mis-parsed as
// header_type=2 or 3 (which earlier spec wording, briefly published in
// 8c4d550, would have produced). The local clone of upstream
// reticulum-specifications corrected this in 0c2021e.
func TestParseRejectsIFACSealed(t *testing.T) {
	p := &Packet{
		HeaderType:      HeaderType1,
		DestinationType: DestinationSingle,
		PacketType:      PacketData,
		DestHash:        newDummyHash(0xAB),
		Context:         ContextNone,
		Data:            []byte("payload"),
	}
	wire, err := p.Pack()
	if err != nil {
		t.Fatal(err)
	}
	wire[0] |= 0x80 // set ifac_flag

	if _, err := ParsePacket(wire); !errors.Is(err, errIFACUnsupported) {
		t.Errorf("ParsePacket(ifac-sealed) error = %v, want errIFACUnsupported", err)
	}
}

func TestHashablePartHeader1Strips(t *testing.T) {
	// HEADER_1: hashable = (flags & 0x0F) || raw[2:]
	// flags' high nibble (header_type + context_flag + transport_type) and
	// the hops byte at offset 1 are stripped.
	p := &Packet{
		HeaderType:      HeaderType1,
		TransportType:   NetworkTransport, // bit 4 set in raw flags, masked off in hashable
		DestinationType: DestinationSingle,
		PacketType:      PacketData,
		Hops:            7, // also stripped
		DestHash:        newDummyHash(0xAA),
		Context:         ContextNone,
		Data:            []byte("payload"),
	}
	hp, err := p.HashablePart()
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := p.Pack()

	// Expected: low nibble of flags || dest_hash || context || data
	expected := append([]byte{wire[0] & 0x0F}, wire[2:]...)
	if !bytes.Equal(hp, expected) {
		t.Errorf("hashable part mismatch\n got %x\nwant %x", hp, expected)
	}
}

func TestHashablePartHeader2StripsTransportID(t *testing.T) {
	p := &Packet{
		HeaderType:      HeaderType2,
		TransportType:   NetworkTransport,
		DestinationType: DestinationSingle,
		PacketType:      PacketData,
		Hops:            2,
		TransportID:     newDummyHash(0x11),
		DestHash:        newDummyHash(0x22),
		Context:         ContextNone,
		Data:            []byte("body"),
	}
	hp, err := p.HashablePart()
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := p.Pack()

	// Expected: low nibble of flags || dest_hash || context || data (transport_id slot at [2:18] stripped)
	expected := append([]byte{wire[0] & 0x0F}, wire[18:]...)
	if !bytes.Equal(hp, expected) {
		t.Errorf("hashable part mismatch\n got %x\nwant %x", hp, expected)
	}
}

func TestUint40RoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 0xFFFFFFFFFF, 1735689600 /* 2025-01-01 */} {
		b := BigEndianUint40(v)
		got, err := DecodeBigEndianUint40(b[:])
		if err != nil {
			t.Fatalf("decode %x: %v", b, err)
		}
		if got != v {
			t.Errorf("uint40 round-trip: got %d, want %d", got, v)
		}
	}
}

func TestParseRejectsTruncated(t *testing.T) {
	if _, err := ParsePacket([]byte{0x00, 0x00}); err == nil {
		t.Error("expected error on truncated packet")
	}
}

// SPEC §2.4: valid wire hops are 0–127; since RNS 1.3.8 receivers drop
// hops >= 128 (PATHFINDER_M) as malformed.
func TestParseRejectsHopsOutOfBound(t *testing.T) {
	p := &Packet{
		HeaderType:      HeaderType1,
		TransportType:   BroadcastTransport,
		DestinationType: DestinationSingle,
		PacketType:      PacketData,
		Hops:            127,
		DestHash:        newDummyHash(0xAB),
		Context:         ContextNone,
		Data:            []byte("x"),
	}
	wire, err := p.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePacket(wire); err != nil {
		t.Errorf("hops=127 should parse, got %v", err)
	}

	for _, hops := range []byte{128, 200, 255} {
		wire[1] = hops
		if _, err := ParsePacket(wire); err == nil {
			t.Errorf("hops=%d should be rejected as malformed", hops)
		}
	}
}

// TestPackRefusesHopsAtTheLimit covers the emit half of SPEC §2.4, added
// in RNS 1.5.0: Packet.send() returns False without packing at
// hops >= PATHFINDER_M (Packet.py:288) and Transport._outbound rejects
// the same before interface selection (Transport.py:1305). Before this
// the bound was enforced only on receive, so we would happily emit a
// packet every conformant peer drops as malformed.
func TestPackRefusesHopsAtTheLimit(t *testing.T) {
	base := func(hops byte) *Packet {
		return &Packet{
			HeaderType:      HeaderType1,
			TransportType:   BroadcastTransport,
			DestinationType: DestinationSingle,
			PacketType:      PacketData,
			Hops:            hops,
			DestHash:        bytes.Repeat([]byte{0x11}, addressHashLen),
			Data:            []byte("x"),
		}
	}
	if _, err := base(maxWireHops - 1).Pack(); err != nil {
		t.Errorf("hops %d refused, want accepted: %v", maxWireHops-1, err)
	}
	for _, hops := range []byte{maxWireHops, maxWireHops + 1, 255} {
		if _, err := base(hops).Pack(); err == nil {
			t.Errorf("Pack accepted hops=%d, want refusal (SPEC §2.4)", hops)
		}
	}
	// The receive side already rejected these; both halves must agree.
	raw, err := base(maxWireHops - 1).Pack()
	if err != nil {
		t.Fatal(err)
	}
	raw[1] = maxWireHops
	if _, err := ParsePacket(raw); err == nil {
		t.Error("ParsePacket accepted hops at the limit")
	}
}

// Upstream RNS 1.5.2 raises on an empty data field in Packet.unpack
// (Packet.py:275), where 1.4.2 and 1.5.0 accepted it. The HEADER_2 case
// is the one that framing never caught: at 35 bytes it clears
// TCPInterface's `frame_len <= HEADER_MINSIZE` (19) bound.
func TestParseRejectsZeroLengthData(t *testing.T) {
	for _, tc := range []struct {
		name string
		pkt  *Packet
	}{
		{"HEADER_1", &Packet{
			HeaderType:      HeaderType1,
			TransportType:   BroadcastTransport,
			DestinationType: DestinationSingle,
			PacketType:      PacketData,
			DestHash:        make([]byte, addressHashLen),
			Context:         ContextNone,
			Data:            nil,
		}},
		{"HEADER_2", &Packet{
			HeaderType:      HeaderType2,
			TransportType:   NetworkTransport,
			DestinationType: DestinationSingle,
			PacketType:      PacketData,
			TransportID:     make([]byte, addressHashLen),
			DestHash:        make([]byte, addressHashLen),
			Context:         ContextNone,
			Data:            nil,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := tc.pkt.Pack()
			if err != nil {
				t.Fatalf("Pack: %v", err)
			}
			if _, err := ParsePacket(wire); err == nil {
				t.Errorf("ParsePacket(%d-byte zero-data frame) = nil error, want rejection", len(wire))
			}
		})
	}
}
