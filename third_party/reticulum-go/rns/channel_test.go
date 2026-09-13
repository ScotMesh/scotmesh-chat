package rns

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// The §6.8.1 header is three big-endian uint16s — Python's
// struct.pack(">HHH", ...). Little-endian or a different field order
// produces a body that parses to nonsense rather than failing loudly.
func TestChannelWireFormat(t *testing.T) {
	m := ChannelMessage{Type: 0x1234, Sequence: 0xABCD, Data: []byte("hello")}
	body, err := PackChannelMessage(m)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	want := append([]byte{0x12, 0x34, 0xAB, 0xCD, 0x00, 0x05}, []byte("hello")...)
	if !bytes.Equal(body, want) {
		t.Errorf("wire form\n got %x\nwant %x", body, want)
	}
	if got := binary.BigEndian.Uint16(body[4:6]); int(got) != len(m.Data) {
		t.Errorf("length field = %d, want %d", got, len(m.Data))
	}

	back, err := ParseChannelMessage(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if back.Type != m.Type || back.Sequence != m.Sequence || !bytes.Equal(back.Data, m.Data) {
		t.Errorf("round trip gave %+v, want %+v", back, m)
	}
}

func TestChannelEmptyPayload(t *testing.T) {
	body, err := PackChannelMessage(ChannelMessage{Type: 1, Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != ChannelHeaderLen {
		t.Fatalf("empty message is %d bytes, want the %d-byte header alone", len(body), ChannelHeaderLen)
	}
	m, err := ParseChannelMessage(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Data) != 0 {
		t.Errorf("payload = %q, want empty", m.Data)
	}
}

// The declared length must match the frame. Trusting the header would
// let a peer describe a 65535-byte payload in a short packet and have
// the receiver read past it, or silently truncate a mismatched message.
func TestChannelParseRejectsLengthMismatch(t *testing.T) {
	body, err := PackChannelMessage(ChannelMessage{Type: 1, Sequence: 1, Data: []byte("abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("declares more than it carries", func(t *testing.T) {
		bad := append([]byte(nil), body...)
		binary.BigEndian.PutUint16(bad[4:6], 0xFFFF)
		if _, err := ParseChannelMessage(bad); !errors.Is(err, ErrChannelMalformed) {
			t.Errorf("err = %v, want ErrChannelMalformed", err)
		}
	})
	t.Run("declares less than it carries", func(t *testing.T) {
		bad := append([]byte(nil), body...)
		binary.BigEndian.PutUint16(bad[4:6], 2)
		if _, err := ParseChannelMessage(bad); !errors.Is(err, ErrChannelMalformed) {
			t.Errorf("err = %v, want ErrChannelMalformed", err)
		}
	})
	t.Run("shorter than the header", func(t *testing.T) {
		if _, err := ParseChannelMessage([]byte{1, 2, 3}); !errors.Is(err, ErrChannelMalformed) {
			t.Errorf("err = %v, want ErrChannelMalformed", err)
		}
	})
}

func TestChannelRejectsOversizePayload(t *testing.T) {
	_, err := PackChannelMessage(ChannelMessage{Data: bytes.Repeat([]byte{1}, ChannelMDU+1)})
	if !errors.Is(err, ErrChannelMessageTooLarge) {
		t.Errorf("err = %v, want ErrChannelMessageTooLarge", err)
	}
	// Exactly the MDU must be accepted — an off-by-one here silently
	// costs a byte of every message.
	if _, err := PackChannelMessage(ChannelMessage{Data: bytes.Repeat([]byte{1}, ChannelMDU)}); err != nil {
		t.Errorf("a full-MDU payload was rejected: %v", err)
	}
}

// §6.8.2: the stream header packs the id into 14 bits with eof and
// compressed as the top two.
func TestStreamHeaderWireFormat(t *testing.T) {
	for _, c := range []struct {
		name       string
		id         uint16
		eof, comp  bool
		wantHeader []byte
	}{
		{"plain", 0x0001, false, false, []byte{0x00, 0x01}},
		{"eof", 0x0001, true, false, []byte{0x80, 0x01}},
		{"compressed", 0x0001, false, true, []byte{0x40, 0x01}},
		{"both flags", 0x3fff, true, true, []byte{0xff, 0xff}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, err := PackStreamHeader(c.id, c.eof, c.comp)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(h, c.wantHeader) {
				t.Errorf("header = %x, want %x", h, c.wantHeader)
			}
			id, eof, comp, data, err := ParseStreamHeader(append(h, []byte("payload")...))
			if err != nil {
				t.Fatal(err)
			}
			if id != c.id || eof != c.eof || comp != c.comp {
				t.Errorf("parsed id=%d eof=%t comp=%t, want %d/%t/%t", id, eof, comp, c.id, c.eof, c.comp)
			}
			if string(data) != "payload" {
				t.Errorf("payload = %q", data)
			}
		})
	}
}

func TestStreamHeaderRejectsOversizeID(t *testing.T) {
	if _, err := PackStreamHeader(StreamIDMax+1, false, false); err == nil {
		t.Error("accepted a stream id past the 14-bit maximum")
	}
	if _, _, _, _, err := ParseStreamHeader([]byte{0x01}); err == nil {
		t.Error("accepted a stream payload shorter than its header")
	}
}

// The stream chunk size is 423 bytes at default MTU, not 417: upstream
// used to subtract the channel envelope twice, fixed in RNS 1.5.0.
func TestStreamMaxDataLenMatchesUpstream(t *testing.T) {
	if ChannelMDU != LinkMDU-ChannelHeaderLen {
		t.Errorf("ChannelMDU = %d, want link.mdu - 6", ChannelMDU)
	}
	if StreamMaxDataLen != 423 {
		t.Errorf("StreamMaxDataLen = %d, want 423 (RNS 1.5.0; 417 was the double-subtraction bug)", StreamMaxDataLen)
	}
}

// End to end over a link: send, dispatch by type, and confirm the
// per-direction sequence advances.
func TestChannelSendAndDispatch(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)

	c, err := tp.OpenChannel(link.ID)
	if err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}
	var got []ChannelMessage
	c.Handle(0x0042, func(_ *Channel, m ChannelMessage) { got = append(got, m) })

	for i := 0; i < 3; i++ {
		seq, err := c.Send(0x0042, []byte{byte(i)})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if int(seq) != i {
			t.Errorf("sequence = %d, want %d — the counter must advance per emission", seq, i)
		}
		deliverToSelf(t, tp, rd.take(t))
	}
	if len(got) != 3 {
		t.Fatalf("handler saw %d messages, want 3", len(got))
	}
	for i, m := range got {
		if int(m.Sequence) != i || !bytes.Equal(m.Data, []byte{byte(i)}) {
			t.Errorf("message %d = %+v", i, m)
		}
	}
}

// An unregistered type goes to the default handler, or is dropped —
// matching upstream, whose message_factories dict simply has no entry.
func TestChannelUnregisteredTypeGoesToDefault(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)
	c, err := tp.OpenChannel(link.ID)
	if err != nil {
		t.Fatal(err)
	}

	var registered, def int
	c.Handle(0x0001, func(*Channel, ChannelMessage) { registered++ })
	if _, err := c.Send(0x0002, []byte("x")); err != nil {
		t.Fatal(err)
	}
	deliverToSelf(t, tp, rd.take(t))
	if registered != 0 {
		t.Error("an unregistered type reached the wrong handler")
	}

	c.HandleDefault(func(*Channel, ChannelMessage) { def++ })
	if _, err := c.Send(0x0002, []byte("x")); err != nil {
		t.Fatal(err)
	}
	deliverToSelf(t, tp, rd.take(t))
	if def != 1 {
		t.Errorf("default handler saw %d messages, want 1", def)
	}
}

func TestChannelStreamRoundTrip(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)
	c, err := tp.OpenChannel(link.ID)
	if err != nil {
		t.Fatal(err)
	}

	var gotID uint16
	var gotEOF bool
	var gotData []byte
	c.Handle(SMTStreamData, func(_ *Channel, m ChannelMessage) {
		id, eof, _, data, err := ParseStreamHeader(m.Data)
		if err != nil {
			t.Errorf("stream header: %v", err)
			return
		}
		gotID, gotEOF, gotData = id, eof, data
	})

	if _, err := c.SendStream(77, []byte("chunk"), true); err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	deliverToSelf(t, tp, rd.take(t))
	if gotID != 77 || !gotEOF || string(gotData) != "chunk" {
		t.Errorf("stream round trip gave id=%d eof=%t data=%q", gotID, gotEOF, gotData)
	}

	if _, err := c.SendStream(1, bytes.Repeat([]byte{1}, StreamMaxDataLen+1), false); !errors.Is(err, ErrChannelMessageTooLarge) {
		t.Errorf("oversize chunk err = %v, want ErrChannelMessageTooLarge", err)
	}
}

// A channel is per link, and closing it stops delivery.
func TestChannelLifecycle(t *testing.T) {
	link, tp, iface := makeActiveTestLink(t)
	rd := reader(iface)

	c1, err := tp.OpenChannel(link.ID)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := tp.OpenChannel(link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Error("OpenChannel returned a second channel for the same link")
	}
	if _, err := tp.OpenChannel(bytes.Repeat([]byte{0xEE}, IdentityHashLen)); err == nil {
		t.Error("opened a channel on an unknown link")
	}

	var seen int
	c1.Handle(1, func(*Channel, ChannelMessage) { seen++ })
	if _, err := c1.Send(1, []byte("a")); err != nil {
		t.Fatal(err)
	}
	raw := rd.take(t)
	tp.CloseChannel(link.ID)
	deliverToSelf(t, tp, raw)
	if seen != 0 {
		t.Error("a message was dispatched after the channel was closed")
	}
}
