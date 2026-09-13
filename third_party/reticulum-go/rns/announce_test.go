package rns

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestBuildAndVerifyAnnounceNoRatchet(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	appData, err := EncodeLXMFAppData([]byte("Forwarder"), nil)
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := BuildAnnounce(id, FullName("lxmf", "delivery"), appData, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Round-trip via packet codec, parse back, verify.
	wire, err := pkt.Pack()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePacket(wire)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PacketType != PacketAnnounce {
		t.Fatalf("packet_type = %d", parsed.PacketType)
	}
	if parsed.ContextFlag {
		t.Error("context_flag should be false (no ratchet)")
	}

	a, err := ParseAnnounce(parsed)
	if err != nil {
		t.Fatalf("ParseAnnounce: %v", err)
	}
	if err := a.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Display name decodes correctly out of app_data.
	name, err := DecodeLXMFAppDataDisplayName(a.AppData)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(name, []byte("Forwarder")) {
		t.Errorf("display_name = %s, want Forwarder", name)
	}
}

func TestBuildAndVerifyAnnounceWithRatchet(t *testing.T) {
	id, _ := NewIdentity()

	// Use any 32-byte X25519 pub as the ratchet — for the verify path we
	// only care that bytes round-trip and signed_data includes them.
	ratchet := bytesOfLen(t, 32)
	pkt, err := BuildAnnounce(id, FullName("lxmf", "delivery"), nil, ratchet)
	if err != nil {
		t.Fatal(err)
	}
	if !pkt.ContextFlag {
		t.Error("context_flag should be true when ratchet is included")
	}

	wire, _ := pkt.Pack()
	parsed, _ := ParsePacket(wire)
	a, err := ParseAnnounce(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.RatchetPub, ratchet) {
		t.Errorf("ratchet round-trip failed\n got %x\nwant %x", a.RatchetPub, ratchet)
	}
	if err := a.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	id, _ := NewIdentity()
	pkt, _ := BuildAnnounce(id, FullName("lxmf", "delivery"), nil, nil)
	a, _ := ParseAnnounce(pkt)

	tampered := append([]byte(nil), a.Signature...)
	tampered[0] ^= 0x01
	a.Signature = tampered

	if err := a.Verify(); err == nil {
		t.Error("Verify accepted a tampered signature")
	}
}

func TestVerifyRejectsTamperedAppData(t *testing.T) {
	id, _ := NewIdentity()
	appData, _ := EncodeLXMFAppData([]byte("Forwarder"), nil)
	pkt, _ := BuildAnnounce(id, FullName("lxmf", "delivery"), appData, nil)
	a, _ := ParseAnnounce(pkt)

	a.AppData = append([]byte(nil), a.AppData...)
	a.AppData[0] ^= 0x01

	if err := a.Verify(); err == nil {
		t.Error("Verify accepted tampered app_data")
	}
}

func TestVerifyRejectsForgedDestHash(t *testing.T) {
	id, _ := NewIdentity()
	pkt, _ := BuildAnnounce(id, FullName("lxmf", "delivery"), nil, nil)
	a, _ := ParseAnnounce(pkt)

	// Replace dest_hash with a fake one — sig is still valid over the
	// ORIGINAL signed_data, but Verify should reject because the recomputed
	// dest_hash won't match the (tampered) outer one. Per SPEC §4.5 step 3.
	a.DestHash = make([]byte, IdentityHashLen)
	for i := range a.DestHash {
		a.DestHash[i] = 0xFF
	}

	if err := a.Verify(); err == nil {
		t.Error("Verify accepted a forged dest_hash")
	}
}

func TestParseAnnounceCapturesTransportIDForHeader2(t *testing.T) {
	id, _ := NewIdentity()
	pkt, err := BuildAnnounce(id, FullName("lxmf", "delivery"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Convert the announce packet to HEADER_2 by hand, simulating a relay
	// that inserted its own identity as transport_id.
	pkt.HeaderType = HeaderType2
	pkt.TransportType = NetworkTransport
	pkt.TransportID = newDummyHash(0xCC)
	pkt.Hops = 2

	wire, err := pkt.Pack()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePacket(wire)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseAnnounce(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(a.TransportID, newDummyHash(0xCC)) {
		t.Errorf("TransportID not captured: got %x", a.TransportID)
	}
	if a.Hops != 2 {
		t.Errorf("Hops = %d, want 2", a.Hops)
	}
	if err := a.Verify(); err != nil {
		t.Errorf("Verify on HEADER_2 announce: %v", err)
	}
}

func TestParseAnnounceTransportIDIsNilForHeader1(t *testing.T) {
	id, _ := NewIdentity()
	pkt, _ := BuildAnnounce(id, FullName("lxmf", "delivery"), nil, nil)
	wire, _ := pkt.Pack()
	parsed, _ := ParsePacket(wire)
	a, _ := ParseAnnounce(parsed)
	if a.TransportID != nil {
		t.Errorf("TransportID should be nil for HEADER_1 announce, got %x", a.TransportID)
	}
}

func TestRandomHashTimestamp(t *testing.T) {
	id, _ := NewIdentity()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	pkt, err := buildAnnounce(id, FullName("lxmf", "delivery"), nil, nil, ContextNone,
		func() time.Time { return now },
		func(p []byte) (int, error) { return len(p), nil }, // deterministic zero entropy
	)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := ParseAnnounce(pkt)

	emitted, err := a.EmittedAt()
	if err != nil {
		t.Fatal(err)
	}
	if !emitted.Equal(now) {
		t.Errorf("emitted_at = %v, want %v", emitted, now)
	}
}

func TestEncodeLXMFAppDataEmitsBinNotStr(t *testing.T) {
	// SPEC §4.3 wire example: display_name = "Reticulum5", stamp_cost = nil
	//   0x92                fixarray, 2 elements
	//   0xc4 0x0a           bin8, length 10
	//   "Reticulum5"        10 bytes ASCII
	//   0xc0                nil
	got, err := EncodeLXMFAppData([]byte("Reticulum5"), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x92, 0xc4, 0x0a}
	want = append(want, "Reticulum5"...)
	want = append(want, 0xc0)
	if !bytes.Equal(got, want) {
		t.Errorf("LXMF app_data wire bytes mismatch\n got %x\nwant %x", got, want)
	}
}

func TestDecodeLXMFAppDataAcceptsLegacyForms(t *testing.T) {
	// 1-element msgpack array (just bin name).
	one := []byte{0x91, 0xc4, 0x04, 'S', 'o', 'l', 'o'}
	if got, err := DecodeLXMFAppDataDisplayName(one); err != nil || string(got) != "Solo" {
		t.Errorf("1-element form: got %q err %v", got, err)
	}

	// 3-element msgpack array (name, stamp_cost, capability_flags).
	three := []byte{0x93, 0xc4, 0x04, 'M', 'u', 'l', 't', 0xc0, 0x90}
	if got, err := DecodeLXMFAppDataDisplayName(three); err != nil || string(got) != "Mult" {
		t.Errorf("3-element form: got %q err %v", got, err)
	}

	// Empty.
	if got, err := DecodeLXMFAppDataDisplayName(nil); err != nil || got != nil {
		t.Errorf("empty: got %v err %v", got, err)
	}
}

// TestPlausibleEmittedAtRejectsOutOfRange is the regression for the
// v1.14.0 announce-cache breakage. EmittedAt decodes 40 bits of
// peer-supplied data, so its range reaches year 36812 — and time.Time
// refuses to marshal a year outside [0,9999]. Caching one such peer
// failed the entire cache save, so the announce cache silently stopped
// persisting and every peer had to be re-learned after a restart.
func TestPlausibleEmittedAtRejectsOutOfRange(t *testing.T) {
	mk := func(tsSecs uint64) *Announce {
		rh := make([]byte, 10)
		// random_hash = 5 random bytes || 5 bytes big-endian seconds
		for i := 0; i < 5; i++ {
			rh[9-i] = byte(tsSecs >> (8 * i))
		}
		return &Announce{RandomHash: rh}
	}

	// Top of the 40-bit range: year 36812. Must be refused.
	if ts, ok := plausibleEmittedAt(mk(1<<40 - 1)); ok {
		t.Errorf("year-%d timestamp accepted; it cannot even be marshalled", ts.Year())
	}
	// Pre-Reticulum epoch nonsense.
	if _, ok := plausibleEmittedAt(mk(0)); ok {
		t.Error("zero timestamp accepted")
	}
	// Far future beyond allowed skew.
	if _, ok := plausibleEmittedAt(mk(uint64(time.Now().Add(72 * time.Hour).Unix()))); ok {
		t.Error("far-future timestamp accepted")
	}
	// A normal, current announce must be accepted.
	now := uint64(time.Now().Add(-time.Minute).Unix())
	if got, ok := plausibleEmittedAt(mk(now)); !ok {
		t.Error("current timestamp rejected")
	} else if got.Unix() != int64(now) {
		t.Errorf("decoded %d, want %d", got.Unix(), now)
	}

	// Whatever we accept must survive a JSON round trip, since it is
	// stored in the announce cache.
	accepted, _ := plausibleEmittedAt(mk(now))
	if _, err := json.Marshal(struct {
		T time.Time `json:"t"`
	}{accepted}); err != nil {
		t.Errorf("accepted timestamp is not marshalable: %v", err)
	}
}

// TestDecodeLXMFAppDataStampCost covers the announce field that decides
// whether a sender must do proof-of-work for us (SPEC §4.3 element [1],
// applied per §5.7.4).
func TestDecodeLXMFAppDataStampCost(t *testing.T) {
	withCost := func(c int) []byte {
		data, err := EncodeLXMFAppData([]byte("Reticulum5"), &c)
		if err != nil {
			t.Fatalf("EncodeLXMFAppData: %v", err)
		}
		return data
	}
	noCost, err := EncodeLXMFAppData([]byte("Reticulum5"), nil)
	if err != nil {
		t.Fatalf("EncodeLXMFAppData: %v", err)
	}

	for _, c := range []struct {
		name string
		in   []byte
		want int
	}{
		{"round-trips our own encoder", withCost(8), 8},
		{"explicit zero", withCost(0), 0},
		{"msgpack nil means no demand", noCost, 0},
		{"empty app_data", nil, 0},
		// Legacy "original announce format": app_data is the raw UTF-8
		// display name with no array around it, so there is no element
		// [1] to read (§4.3).
		{"legacy raw-UTF8 name", []byte("Reticulum5"), 0},
		// 1-element array — a name-only announce.
		{"name-only array", []byte{0x91, 0xc4, 0x01, 'n'}, 0},
		// 3-element array: [name, stamp_cost, capability_flags]. The
		// cost is still element [1].
		{"3-element array", []byte{0x93, 0xc4, 0x01, 'n', 0x0c, 0x00}, 12},
		// Upstream's setter maps anything < 1 to None — a negative cost
		// means "no stamp required", not a malformed announce
		// (LXMF/LXMRouter.py:385-386).
		{"negative means no stamp", []byte{0x92, 0xc4, 0x01, 'n', 0xff}, 0},
		// 254 is the largest value set_inbound_stamp_cost accepts.
		{"upper bound accepted", []byte{0x92, 0xc4, 0x01, 'n', 0xcc, 0xfe}, 254},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := DecodeLXMFAppDataStampCost(c.in)
			if err != nil {
				t.Fatalf("DecodeLXMFAppDataStampCost: %v", err)
			}
			if got != c.want {
				t.Errorf("stamp_cost = %d, want %d", got, c.want)
			}
		})
	}
}

// TestDecodeLXMFAppDataStampCostRejectsGarbage: a stamp_cost we cannot
// read is an error rather than a silent 0. Treating it as "no stamp"
// would send an unstamped message to a peer that may enforce stamps, and
// the drop happens on their side where we never see it (§5.7.4).
func TestDecodeLXMFAppDataStampCostRejectsGarbage(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
	}{
		// [bin "n", str "high"] — element [1] is text, not an integer.
		{"stamp_cost is a string", []byte{0x92, 0xc4, 0x01, 'n', 0xa4, 'h', 'i', 'g', 'h'}},
		// 255 is where upstream's setter returns False, so no conformant
		// peer announces it (LXMF/LXMRouter.py:387-389).
		{"stamp_cost at the upstream refusal point", []byte{0x92, 0xc4, 0x01, 'n', 0xcc, 0xff}},
		// uint64 max, reached by the fuzzer through a uint64 envelope.
		{"absurd stamp_cost", []byte{0x92, 0xc4, 0x01, 'n', 0xcf, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := DecodeLXMFAppDataStampCost(c.in)
			if err == nil {
				t.Errorf("accepted malformed stamp_cost, returned %d", got)
			}
			if got != 0 {
				t.Errorf("cost = %d on error, want 0", got)
			}
		})
	}
}

// FuzzDecodeLXMFAppDataStampCost fuzzes the announce field that decides
// how much proof-of-work we do for a peer. app_data is fully
// attacker-controlled and this decoder runs on every announce we recall
// before a send, so it must never panic and never return a cost that
// isn't a plausible bit count.
func FuzzDecodeLXMFAppDataStampCost(f *testing.F) {
	cost := 8
	withCost, _ := EncodeLXMFAppData([]byte("peer"), &cost)
	noCost, _ := EncodeLXMFAppData([]byte("peer"), nil)
	f.Add(withCost)
	f.Add(noCost)
	f.Add([]byte("Reticulum5"))                                 // legacy raw-UTF8
	f.Add([]byte{0x92, 0xc4, 0x01, 'n', 0xcf, 0xff, 0xff, 0xff, // uint64 max cost
		0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0x93, 0xc4, 0x01, 'n', 0x0c, 0x00}) // 3-element form
	f.Add([]byte{0x92, 0xc4, 0x01, 'n', 0xff})       // negative fixint
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := DecodeLXMFAppDataStampCost(data)
		if err != nil {
			if got != 0 {
				t.Errorf("cost = %d alongside error %v; callers read the value on error paths too", got, err)
			}
			return
		}
		// An accepted cost must be one a conformant peer could have
		// announced: upstream's set_inbound_stamp_cost stores only 1..254,
		// mapping anything below to None. Outside that, a decode path
		// produced a number no peer can legitimately be asking for.
		if got < 0 || got > maxAnnouncedStampCost {
			t.Errorf("accepted stamp_cost %d, outside the announceable range [0, %d]", got, maxAnnouncedStampCost)
		}
	})
}

// SPEC §4.5: an announce must fit the 500-byte Reticulum MTU. Upstream
// refuses to emit one that doesn't (Packet.py:238) and, since RNS 1.5.2,
// drops one that arrives as a protocol violation (Transport.py:1804).
func TestBuildAnnounceRejectsOverMTUAppData(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	ratchet := make([]byte, ratchetPubLen)

	// Largest app_data that still fits: 500 - 19 (HEADER_1) - 180 (body
	// with ratchet) = 301.
	const maxAppData = ReticulumMTU - header1MinLen - announceMinWithRatch

	pkt, err := BuildAnnounce(id, "lxmf.delivery", make([]byte, maxAppData), ratchet)
	if err != nil {
		t.Fatalf("BuildAnnounce at the %d-byte app_data limit: %v", maxAppData, err)
	}
	wire, err := pkt.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(wire) != ReticulumMTU {
		t.Errorf("announce at the limit packed to %d bytes, want exactly %d", len(wire), ReticulumMTU)
	}

	if _, err := BuildAnnounce(id, "lxmf.delivery", make([]byte, maxAppData+1), ratchet); err == nil {
		t.Errorf("BuildAnnounce with %d-byte app_data = nil error, want refusal", maxAppData+1)
	}
}

// SPEC §4.6: app_data is opaque bytes to RNS, and the §4.3 msgpack
// array is LXMF's own convention. A promiscuous listener sees announces
// on other aspects whose app_data is not msgpack at all, and the shape
// does not identify the protocol — so the display_name lookup must key
// on name_hash. These are the three dispatch outcomes §4.6 tabulates,
// as CBOR payloads (verified upstream by tools/verify_app_data_dispatch.py):
// two of them look like success, which is what makes shape-dispatch unsafe.
func TestLXMFDisplayNameIsKeyedOnNameHashNotShape(t *testing.T) {
	rrcHub := NameHash(FullName("rrc", "hub"))
	for _, c := range []struct {
		name    string
		appData []byte
		// what the unkeyed decoder makes of it — the trap §4.6 warns about
		unkeyed string
	}{
		{"CBOR 3-entry map (0xa3), not UTF-8 decodable", []byte{0xa3, 0x01, 0x02}, "\xa3\x01\x02"},
		{"CBOR 7-char text string glues the header on", append([]byte{0x67}, "hubname"...), "ghubname"},
		{"CBOR 16-item array lands in msgpack fixarray(0)", []byte{0x90}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &Announce{NameHash: rrcHub, AppData: c.appData}
			if got := LXMFDisplayNameFromAnnounce(a); got != nil {
				t.Errorf("display name %q read off a non-LXMF announce; want nil", got)
			}
			// Lock in that the unkeyed decoder really does return
			// confident nonsense here, so the gate above is load-bearing
			// and not a no-op someone can inline away.
			raw, _ := DecodeLXMFAppDataDisplayName(c.appData)
			if string(raw) != c.unkeyed {
				t.Errorf("unkeyed decode = %q, want %q — §4.6's dispatch table has moved", raw, c.unkeyed)
			}
		})
	}

	// The same app_data on the lxmf.delivery aspect still resolves.
	want := []byte("alice")
	appData, err := EncodeLXMFAppData(want, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &Announce{NameHash: NameHash(FullName("lxmf", "delivery")), AppData: appData}
	if got := LXMFDisplayNameFromAnnounce(a); !bytes.Equal(got, want) {
		t.Errorf("display name = %q, want %q", got, want)
	}
	if got := LXMFDisplayNameFromAnnounce(nil); got != nil {
		t.Errorf("nil announce = %q, want nil", got)
	}
}
