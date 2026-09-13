package rns

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// fakePort is a bidirectional byte stream standing in for a serial port.
type fakePort struct {
	mu      sync.Mutex
	toHost  chan []byte
	written []byte
	closed  bool
	pending []byte
}

func newFakePort() *fakePort { return &fakePort{toHost: make(chan []byte, 16)} }

func (p *fakePort) Read(b []byte) (int, error) {
	for {
		p.mu.Lock()
		if len(p.pending) > 0 {
			n := copy(b, p.pending)
			p.pending = p.pending[n:]
			p.mu.Unlock()
			return n, nil
		}
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return 0, io.EOF
		}
		chunk, ok := <-p.toHost
		if !ok {
			return 0, io.EOF
		}
		p.mu.Lock()
		p.pending = append(p.pending, chunk...)
		p.mu.Unlock()
	}
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	p.written = append(p.written, b...)
	return len(b), nil
}

func (p *fakePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.toHost)
	}
	return nil
}

func (p *fakePort) feed(b []byte) { p.toHost <- b }

func (p *fakePort) sent() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.written...)
}

func TestRNodeSendFramesAsCmdData(t *testing.T) {
	port := newFakePort()
	r := NewRNodeInterface(port, noopLogger{})
	defer r.Close()

	packet := bytes.Repeat([]byte{0x5A}, header1MinLen+4)
	if err := r.Send(packet); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := EncodeKISS(CmdData, packet)
	if got := port.sent(); !bytes.Equal(got, want) {
		t.Errorf("wrote\n got %x\nwant %x", got, want)
	}
}

// Two LoRa frames is the whole protocol (§8.3), so an oversize packet is
// refused rather than handed to firmware that would truncate it.
func TestRNodeRefusesOversizePackets(t *testing.T) {
	port := newFakePort()
	r := NewRNodeInterface(port, noopLogger{})
	defer r.Close()
	if err := r.Send(bytes.Repeat([]byte{1}, RNodeMTU+1)); err == nil {
		t.Error("accepted a packet past the RNode MTU")
	}
}

func TestRNodeReceivesDataFrames(t *testing.T) {
	port := newFakePort()
	r := NewRNodeInterface(port, noopLogger{})
	defer r.Close()

	packet := bytes.Repeat([]byte{0x33}, header1MinLen+2)
	port.feed(EncodeKISS(CmdData, packet))

	select {
	case got := <-r.Inbox():
		if !bytes.Equal(got, packet) {
			t.Errorf("received %x, want %x", got, packet)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no packet arrived")
	}
}

// The firmware prefixes received frames with link-quality readings
// (§8.1). They are stats, not packets: surfacing them as data would
// inject two-byte garbage into the Transport.
func TestRNodeDecodesLinkQualityStats(t *testing.T) {
	port := newFakePort()
	r := NewRNodeInterface(port, noopLogger{})
	defer r.Close()

	if _, _, ok := r.LinkQuality(); ok {
		t.Error("link quality reported before any reading arrived")
	}

	// RSSI: signed value is byte - 157. SNR: signed Q6.2, /4 for dB.
	port.feed(EncodeKISS(CmdStatRSSI, []byte{100})) // -57 dBm
	port.feed(EncodeKISS(CmdStatSNR, []byte{40}))   // +10 dB
	packet := bytes.Repeat([]byte{0x44}, header1MinLen)
	port.feed(EncodeKISS(CmdData, packet))

	select {
	case got := <-r.Inbox():
		if !bytes.Equal(got, packet) {
			t.Errorf("received %x", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no packet arrived after the stat frames")
	}

	rssi, snr, ok := r.LinkQuality()
	if !ok {
		t.Fatal("no link quality recorded")
	}
	if rssi != -57 {
		t.Errorf("RSSI = %d dBm, want -57 (byte 100 - 157)", rssi)
	}
	if snr != 10.0 {
		t.Errorf("SNR = %v dB, want 10 (Q6.2 40/4)", snr)
	}
}

// Unknown firmware commands are ignored rather than treated as packets.
func TestRNodeIgnoresUnknownCommands(t *testing.T) {
	port := newFakePort()
	r := NewRNodeInterface(port, noopLogger{})
	defer r.Close()

	port.feed(EncodeKISS(0x42, bytes.Repeat([]byte{0xFF}, 40)))
	packet := bytes.Repeat([]byte{0x55}, header1MinLen)
	port.feed(EncodeKISS(CmdData, packet))

	select {
	case got := <-r.Inbox():
		if !bytes.Equal(got, packet) {
			t.Errorf("an unknown command was delivered as a packet: %x", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no packet arrived")
	}
}

func TestRNodeCloseIsIdempotentAndStopsSends(t *testing.T) {
	port := newFakePort()
	r := NewRNodeInterface(port, noopLogger{})
	if err := r.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	if err := r.Send(bytes.Repeat([]byte{1}, header1MinLen)); err == nil {
		t.Error("Send succeeded on a closed interface")
	}
	select {
	case <-r.Done():
	case <-time.After(2 * time.Second):
		t.Error("Done was not closed")
	}
}

func TestRNodeSatisfiesInterface(t *testing.T) {
	var _ Interface = NewRNodeInterface(newFakePort(), noopLogger{})
}
