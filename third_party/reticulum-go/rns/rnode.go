package rns

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// RNodeInterface speaks the KISS host protocol (§8.1) to an RNode over
// any byte stream — a serial port, a BLE characteristic pair, a TCP
// bridge. The transport is supplied by the caller as an
// io.ReadWriteCloser rather than opened here, so this package does not
// take a serial-port dependency and the protocol can be exercised over a
// pipe.
//
// Note what is NOT here: the §8.3 air-frame header. That byte lives
// between RNodes on the LoRa side; the firmware adds it on TX and strips
// it on RX, so a KISS host never sees one. It is implemented separately
// (SplitAirFrames / AirFrameReassembler) for something talking LoRa
// directly to an RNode, such as a clean-room repeater firmware.
type RNodeInterface struct {
	port io.ReadWriteCloser

	inbox chan []byte
	done  chan struct{}

	mu     sync.Mutex // serialises writes
	closed atomic.Bool

	// lastRSSI / lastSNR hold the most recent link-quality readings the
	// firmware prefixed to a received frame.
	statMu   sync.Mutex
	lastRSSI int
	lastSNR  float64
	haveStat bool

	logger Logger
}

// NewRNodeInterface starts reading KISS frames from `port`.
func NewRNodeInterface(port io.ReadWriteCloser, logger Logger) *RNodeInterface {
	if logger == nil {
		logger = noopLogger{}
	}
	r := &RNodeInterface{
		port:   port,
		inbox:  make(chan []byte, 64),
		done:   make(chan struct{}),
		logger: logger,
	}
	go r.readLoop()
	return r
}

// Send frames a packet as CMD_DATA and writes it.
func (r *RNodeInterface) Send(packet []byte) error {
	if r.closed.Load() {
		return errors.New("rnode interface closed")
	}
	if len(packet) > RNodeMTU {
		// The radio cannot carry it: two LoRa frames is the whole
		// protocol (§8.3). Failing here beats handing the firmware
		// something it will silently truncate.
		return fmt.Errorf("packet is %d bytes, RNode carries at most %d", len(packet), RNodeMTU)
	}
	framed := EncodeKISS(CmdData, packet)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.port.Write(framed); err != nil {
		_ = r.Close()
		return fmt.Errorf("rnode write: %w", err)
	}
	return nil
}

// readLoop decodes frames until the port fails or is closed.
func (r *RNodeInterface) readLoop() {
	defer close(r.done)
	dec := NewKISSDecoder(r.port)
	for {
		cmd, payload, err := dec.NextFrame()
		if err != nil {
			if !r.closed.Load() {
				r.logger.Printf("rnode read: %v", err)
			}
			return
		}
		switch cmd {
		case CmdData:
			// Frames shorter than a Reticulum header are noise or
			// firmware chatter, dropped for the same reason the TCP
			// reader drops them.
			if len(payload) < header1MinLen {
				continue
			}
			select {
			case r.inbox <- append([]byte(nil), payload...):
			case <-r.done:
				return
			}
		case CmdStatRSSI:
			// Signed value is byte - 157 (§8.1).
			if len(payload) == 1 {
				r.setRSSI(int(payload[0]) - 157)
			}
		case CmdStatSNR:
			// Signed Q6.2: divide by 4 for dB.
			if len(payload) == 1 {
				r.setSNR(float64(int8(payload[0])) / 4.0)
			}
		default:
			// Other firmware commands (config, promiscuity, battery)
			// are not part of packet transport and are ignored rather
			// than treated as data.
		}
	}
}

func (r *RNodeInterface) setRSSI(v int) {
	r.statMu.Lock()
	r.lastRSSI = v
	r.haveStat = true
	r.statMu.Unlock()
}

func (r *RNodeInterface) setSNR(v float64) {
	r.statMu.Lock()
	r.lastSNR = v
	r.haveStat = true
	r.statMu.Unlock()
}

// LinkQuality returns the most recent RSSI (dBm) and SNR (dB) the
// firmware reported, and whether any reading has arrived.
func (r *RNodeInterface) LinkQuality() (rssi int, snr float64, ok bool) {
	r.statMu.Lock()
	defer r.statMu.Unlock()
	return r.lastRSSI, r.lastSNR, r.haveStat
}

// Inbox returns inbound Reticulum packets.
func (r *RNodeInterface) Inbox() <-chan []byte { return r.inbox }

// Done is closed when the reader exits.
func (r *RNodeInterface) Done() <-chan struct{} { return r.done }

// Close shuts the port. Idempotent.
func (r *RNodeInterface) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	return r.port.Close()
}
