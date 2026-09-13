package wire

// Derived from rrcd router.py (_handle_pre_welcome, _extract_caps),
// messages.py (queue_welcome) and router.py (_handle_resource_envelope),
// Copyright (c) 2025 S. Miller, KC1AWV, MIT License.

import (
	"bytes"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// Hello is what a client says about itself in HELLO. Every field is
// optional and parsed leniently: a malformed body yields a zero Hello, as
// rrcd ignores it.
type Hello struct {
	Name       string
	Version    string
	Caps       map[Key]bool
	LegacyNick string // body key 64, from pre-spec clients
}

// ParseHello reads a HELLO body.
func ParseHello(body cbor.RawMessage) Hello {
	var h Hello
	if len(body) == 0 {
		return h
	}
	var m map[any]any
	if err := decMode.Unmarshal(body, &m); err != nil {
		return h
	}
	// m's keys are the raw uint64 the CBOR decoder produced for a map[any]any,
	// not our Key type, so indexing needs the same conversion back.
	get := func(k Key) any { return m[uint64(k)] }
	h.Name, _ = get(HelloName).(string)
	h.Version, _ = get(HelloVersion).(string)
	h.LegacyNick, _ = get(HelloLegacyNick).(string)
	if caps, ok := get(HelloCaps).(map[any]any); ok {
		h.Caps = make(map[Key]bool, len(caps))
		for k, v := range caps {
			ku, ok := k.(uint64)
			if !ok {
				continue
			}
			on, isBool := v.(bool)
			h.Caps[Key(ku)] = !isBool || on // advisory: anything but false counts
		}
	}
	return h
}

// Encode returns the HELLO body as CBOR, for a client identifying itself to
// a hub. Empty fields are omitted, matching what ParseHello expects.
func (h Hello) Encode() (cbor.RawMessage, error) {
	body := map[Key]any{}
	if h.Name != "" {
		body[HelloName] = h.Name
	}
	if h.Version != "" {
		body[HelloVersion] = h.Version
	}
	if len(h.Caps) > 0 {
		caps := make(map[Key]bool, len(h.Caps))
		for k, v := range h.Caps {
			if v {
				caps[k] = true
			}
		}
		body[HelloCaps] = caps
	}
	if h.LegacyNick != "" {
		body[HelloLegacyNick] = h.LegacyNick
	}
	b, err := encMode.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode HELLO body: %w", err)
	}
	return b, nil
}

// HubLimits are the limits a hub advertises in WELCOME, in bytes and counts.
type HubLimits struct {
	MaxNickBytes       int
	MaxRoomNameBytes   int
	MaxMsgBodyBytes    int
	MaxRoomsPerSession int
	RatePerMinute      int
}

// Welcome is the hub's WELCOME body.
type Welcome struct {
	Hub     string
	Version string
	Caps    map[Key]bool
	Limits  HubLimits
}

// Encode returns the WELCOME body as CBOR.
func (w Welcome) Encode() (cbor.RawMessage, error) {
	caps := make(map[Key]bool, len(w.Caps))
	for k, v := range w.Caps {
		if v {
			caps[k] = true
		}
	}
	body := map[Key]any{
		WelcomeHub:     w.Hub,
		WelcomeVersion: w.Version,
		WelcomeCaps:    caps,
		WelcomeLimits: map[Key]int{
			LimitMaxNickBytes:       w.Limits.MaxNickBytes,
			LimitMaxRoomNameBytes:   w.Limits.MaxRoomNameBytes,
			LimitMaxMsgBodyBytes:    w.Limits.MaxMsgBodyBytes,
			LimitMaxRoomsPerSession: w.Limits.MaxRoomsPerSession,
			LimitRatePerMinute:      w.Limits.RatePerMinute,
		},
	}
	b, err := encMode.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode WELCOME body: %w", err)
	}
	return b, nil
}

// ParseWelcome reads a WELCOME body, for a client reading a hub's reply to
// HELLO. Parsed leniently, like ParseHello: a malformed body yields a zero
// Welcome.
func ParseWelcome(body cbor.RawMessage) Welcome {
	var w Welcome
	if len(body) == 0 {
		return w
	}
	var m map[any]any
	if err := decMode.Unmarshal(body, &m); err != nil {
		return w
	}
	// m's keys are the raw uint64 the CBOR decoder produced for a map[any]any,
	// not our Key type, so indexing needs the same conversion back.
	get := func(k Key) any { return m[uint64(k)] }
	w.Hub, _ = get(WelcomeHub).(string)
	w.Version, _ = get(WelcomeVersion).(string)
	if caps, ok := get(WelcomeCaps).(map[any]any); ok {
		w.Caps = make(map[Key]bool, len(caps))
		for k, v := range caps {
			ku, ok := k.(uint64)
			if !ok {
				continue
			}
			on, isBool := v.(bool)
			w.Caps[Key(ku)] = !isBool || on // advisory: anything but false counts
		}
	}
	if lim, ok := get(WelcomeLimits).(map[any]any); ok {
		geti := func(k Key) int {
			v, _ := lim[uint64(k)].(uint64)
			return int(v) //nolint:gosec // wire sizes fit an int
		}
		w.Limits = HubLimits{
			MaxNickBytes:       geti(LimitMaxNickBytes),
			MaxRoomNameBytes:   geti(LimitMaxRoomNameBytes),
			MaxMsgBodyBytes:    geti(LimitMaxMsgBodyBytes),
			MaxRoomsPerSession: geti(LimitMaxRoomsPerSession),
			RatePerMinute:      geti(LimitRatePerMinute),
		}
	}
	return w
}

// ResourceEnvelope announces a Resource transfer that follows on the link.
type ResourceEnvelope struct {
	ID       []byte
	Kind     string
	Size     uint64
	SHA256   []byte // optional
	Encoding string // optional
}

// ParseResourceEnvelope validates a RESOURCE_ENVELOPE body with rrcd's
// error texts.
func ParseResourceEnvelope(body cbor.RawMessage) (ResourceEnvelope, error) {
	var re ResourceEnvelope
	var m map[any]cbor.RawMessage
	if len(body) == 0 || body[0]>>5 != 5 || decMode.Unmarshal(body, &m) != nil {
		return re, invalid("invalid resource envelope body")
	}
	f := make(map[Key]cbor.RawMessage, len(m))
	for k, v := range m {
		if ku, ok := k.(uint64); ok {
			f[Key(ku)] = v
		}
	}
	var ok bool
	if re.ID, ok = decodeBytes(f[ResID]); !ok {
		return re, invalid("resource envelope missing id")
	}
	if re.Kind, ok = decodeString(f[ResKind]); !ok || re.Kind == "" {
		return re, invalid("resource envelope missing kind")
	}
	size, ok := decodeInt(f[ResSize])
	if !ok || size < 0 {
		return re, invalid("resource envelope invalid size")
	}
	re.Size = uint64(size)
	if raw, present := f[ResSHA256]; present {
		if re.SHA256, ok = decodeBytes(raw); !ok {
			return re, invalid("resource envelope invalid sha256")
		}
	}
	if raw, present := f[ResEncoding]; present {
		re.Encoding, _ = decodeString(raw) // rrcd ignores a non-string encoding
	}
	return re, nil
}

// Encode returns the RESOURCE_ENVELOPE body as CBOR.
func (re ResourceEnvelope) Encode() (cbor.RawMessage, error) {
	m := map[Key]any{ResID: nonNil(re.ID), ResKind: re.Kind, ResSize: re.Size}
	if re.SHA256 != nil {
		m[ResSHA256] = re.SHA256
	}
	if re.Encoding != "" {
		m[ResEncoding] = re.Encoding
	}
	b, err := encMode.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode RESOURCE_ENVELOPE body: %w", err)
	}
	return b, nil
}

// AnnounceAppData is the app_data a hub announces on rrc.hub: the CBOR map
// {"proto": "rrc", "v": 1, "hub": name}, with text keys, in that order,
// byte-for-byte what rrcd sends.
func AnnounceAppData(hubName string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(0xa3) // map of 3 pairs, insertion order as rrcd's dict
	for _, kv := range []struct {
		k string
		v any
	}{{"proto", "rrc"}, {"v", Version}, {"hub", hubName}} {
		kb, err := encMode.Marshal(kv.k)
		if err != nil {
			return nil, err
		}
		vb, err := encMode.Marshal(kv.v)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.Write(vb)
	}
	return buf.Bytes(), nil
}

// ParseAnnounceAppData reads rrc.hub announce app_data. ok is false for
// anything that is not an RRC v1 hub announce.
func ParseAnnounceAppData(appData []byte) (hubName string, ok bool) {
	var m map[string]any
	if decMode.Unmarshal(appData, &m) != nil {
		return "", false
	}
	if proto, _ := m["proto"].(string); proto != "rrc" {
		return "", false
	}
	if v, isInt := m["v"].(uint64); !isInt || v != Version {
		return "", false
	}
	name, _ := m["hub"].(string)
	return name, true
}
