package rns

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// §8.1: FEND delimits, FESC escapes, and the two reserved bytes take
// their transposed forms. Getting the escape wrong corrupts exactly the
// packets that happen to contain 0xC0 or 0xDB — rare enough to pass a
// casual test and fail in production.
func TestKISSEscaping(t *testing.T) {
	payload := []byte{0x01, KissFEND, 0x02, KissFESC, 0x03}
	framed := EncodeKISS(CmdData, payload)

	want := []byte{
		KissFEND, CmdData,
		0x01, KissFESC, KissTFEND,
		0x02, KissFESC, KissTFESC,
		0x03, KissFEND,
	}
	if !bytes.Equal(framed, want) {
		t.Errorf("framed\n got %x\nwant %x", framed, want)
	}

	cmd, got, err := NewKISSDecoder(bytes.NewReader(framed)).NextFrame()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cmd != CmdData {
		t.Errorf("cmd = %#x, want %#x", cmd, CmdData)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload round trip\n got %x\nwant %x", got, payload)
	}
}

// Over BLE a frame is split across notifications with no alignment to
// frame boundaries (§8.1), so the decoder must accumulate across reads.
// A decoder assuming one read yields one frame drops most traffic.
func TestKISSDecoderReassemblesAcrossReads(t *testing.T) {
	a := EncodeKISS(CmdData, []byte("first frame"))
	b := EncodeKISS(CmdStatRSSI, []byte{0xA0})
	stream := append(append([]byte(nil), a...), b...)

	// A reader that yields 3 bytes at a time, splitting frames anywhere.
	d := NewKISSDecoder(&chunkyReader{data: stream, chunk: 3})

	cmd, payload, err := d.NextFrame()
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if cmd != CmdData || string(payload) != "first frame" {
		t.Errorf("frame 1 = (%#x, %q)", cmd, payload)
	}
	cmd, payload, err = d.NextFrame()
	if err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if cmd != CmdStatRSSI || !bytes.Equal(payload, []byte{0xA0}) {
		t.Errorf("frame 2 = (%#x, %x)", cmd, payload)
	}
}

type chunkyReader struct {
	data  []byte
	chunk int
}

func (c *chunkyReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := c.chunk
	if n > len(c.data) {
		n = len(c.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// Empty frames — two consecutive FENDs on an idle line — are skipped,
// since the delimiter doubles as terminator and start marker.
func TestKISSSkipsEmptyFrames(t *testing.T) {
	stream := []byte{KissFEND, KissFEND, KissFEND}
	stream = append(stream, EncodeKISS(CmdData, []byte("x"))...)
	cmd, payload, err := NewKISSDecoder(bytes.NewReader(stream)).NextFrame()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cmd != CmdData || string(payload) != "x" {
		t.Errorf("got (%#x, %q)", cmd, payload)
	}
}

// Without a ceiling a peer that streams non-FEND bytes forever drives
// allocation until the process dies — remotely, before any parse.
func TestKISSBoundsFrameLength(t *testing.T) {
	junk := bytes.Repeat([]byte{0x41}, MaxKISSFrameLen+16)
	stream := append([]byte{KissFEND}, junk...)
	_, _, err := NewKISSDecoder(bytes.NewReader(stream)).NextFrame()
	if !errors.Is(err, ErrKISSFrameTooLong) {
		t.Errorf("err = %v, want ErrKISSFrameTooLong", err)
	}
}

// §8.3: sequence id in the high nibble, FLAG_SPLIT in bit 0.
func TestAirFrameHeaderLayout(t *testing.T) {
	h := AirFrameHeader(0xA0, true)
	if AirFrameSequence(h) != 0x0A {
		t.Errorf("sequence = %#x, want 0xA", AirFrameSequence(h))
	}
	if !AirFrameIsSplit(h) {
		t.Error("split flag not set")
	}
	if h&0x0E != 0 {
		t.Errorf("reserved bits set in %#x", h)
	}
	if AirFrameIsSplit(AirFrameHeader(0x30, false)) {
		t.Error("split flag set on a single frame")
	}
}

// A payload up to SINGLE_MTU-1 rides one frame; larger splits into two
// halves sharing a sequence. Two frames is the whole protocol — there is
// no chunking beyond it.
func TestSplitAndReassembleAirFrames(t *testing.T) {
	t.Run("single frame", func(t *testing.T) {
		packet := bytes.Repeat([]byte{0x11}, RNodeSingleMTU-RNodeHeaderLen)
		frames, err := SplitAirFrames(packet, 0x50)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) != 1 {
			t.Fatalf("split into %d frames, want 1", len(frames))
		}
		got, err := NewAirFrameReassembler().Push(frames[0])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, packet) {
			t.Error("single frame did not round trip")
		}
	})

	t.Run("split frame", func(t *testing.T) {
		packet := bytes.Repeat([]byte{0x22}, RNodeMTU)
		frames, err := SplitAirFrames(packet, 0x70)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) != 2 {
			t.Fatalf("split into %d frames, want 2", len(frames))
		}
		if !AirFrameIsSplit(frames[0][0]) {
			t.Error("first half is not flagged split")
		}
		if AirFrameIsSplit(frames[1][0]) {
			t.Error("second half is flagged split")
		}
		if AirFrameSequence(frames[0][0]) != AirFrameSequence(frames[1][0]) {
			t.Error("the two halves carry different sequence ids")
		}
		for _, f := range frames {
			if len(f) > RNodeSingleMTU {
				t.Errorf("air frame is %d bytes, over the %d LoRa limit", len(f), RNodeSingleMTU)
			}
		}

		r := NewAirFrameReassembler()
		if out, err := r.Push(frames[0]); err != nil || out != nil {
			t.Fatalf("first half = (%v, %v), want (nil, nil)", out, err)
		}
		out, err := r.Push(frames[1])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, packet) {
			t.Error("split packet did not reassemble")
		}
	})

	t.Run("over the MTU", func(t *testing.T) {
		if _, err := SplitAirFrames(bytes.Repeat([]byte{1}, RNodeMTU+1), 0); err == nil {
			t.Error("accepted a packet past the two-frame maximum")
		}
	})
}

// A second half from a different transmission must not be glued to a
// buffered first — that would produce a packet neither sender sent.
func TestAirFrameSequenceMismatchDiscardsBoth(t *testing.T) {
	first, err := SplitAirFrames(bytes.Repeat([]byte{0x33}, RNodeMTU), 0x10)
	if err != nil {
		t.Fatal(err)
	}
	other, err := SplitAirFrames(bytes.Repeat([]byte{0x44}, RNodeMTU), 0x90)
	if err != nil {
		t.Fatal(err)
	}

	r := NewAirFrameReassembler()
	if _, err := r.Push(first[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Push(other[1]); err == nil {
		t.Error("glued a second half from a different transmission")
	}
	// The buffered half must be gone, so a later single frame is not
	// mistaken for its continuation.
	single, err := SplitAirFrames([]byte("plain"), 0x20)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Push(single[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "plain" {
		t.Errorf("after a mismatch, a single frame gave %q", out)
	}
}
