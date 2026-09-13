package wire

import "strconv"

// Derived from rrcd constants.py, Copyright (c) 2025 S. Miller, KC1AWV, MIT License.

// Version is the RRC protocol version carried in every envelope.
const Version = 1

// HubAspect is the Reticulum destination name every RRC hub announces on.
const HubAspect = "rrc.hub"

// Key is an envelope map key: an envelope field (KeyV, KeyRoom, ...), a
// HELLO or WELCOME field, a capability, or a resource envelope field,
// depending which map it's in. A defined type rather than a plain uint64
// alias, so a map of them reads as map[wire.Key]... in godoc rather than
// map[uint64]..., and passing a bare int needs an explicit conversion.
type Key uint64

// Envelope keys.
const (
	KeyV    Key = 0 // protocol version
	KeyT    Key = 1 // message type
	KeyID   Key = 2 // message ID, bytes
	KeyTS   Key = 3 // timestamp, milliseconds since the Unix epoch
	KeySrc  Key = 4 // sender identity hash, bytes
	KeyRoom Key = 5 // room name
	KeyBody Key = 6 // body, any CBOR value
	KeyNick Key = 7 // sender nickname
	KeyDst  Key = 8 // destination identity hash for a direct NOTICE

	// ExtensionMin is the first key of the extension range. Extension fields
	// (reply 64, reaction 65, reaction op 66, and future ones) are carried
	// verbatim; see Envelope.Ext.
	ExtensionMin Key = 64
)

// Type is an envelope message type.
type Type uint64

// Message types.
const (
	TypeHello            Type = 1
	TypeWelcome          Type = 2
	TypeJoin             Type = 10
	TypeJoined           Type = 11
	TypePart             Type = 12
	TypeParted           Type = 13
	TypeMsg              Type = 20
	TypeNotice           Type = 21
	TypeAction           Type = 22
	TypePing             Type = 30
	TypePong             Type = 31
	TypeError            Type = 40
	TypeResourceEnvelope Type = 50
)

var typeNames = map[Type]string{
	TypeHello: "HELLO", TypeWelcome: "WELCOME", TypeJoin: "JOIN", TypeJoined: "JOINED",
	TypePart: "PART", TypeParted: "PARTED", TypeMsg: "MSG", TypeNotice: "NOTICE",
	TypeAction: "ACTION", TypePing: "PING", TypePong: "PONG", TypeError: "ERROR",
	TypeResourceEnvelope: "RESOURCE_ENVELOPE",
}

// String returns the protocol name of the type, or "TYPE(n)" for an unknown one.
func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return "TYPE(" + strconv.FormatUint(uint64(t), 10) + ")"
}

// HELLO body keys.
const (
	HelloName       Key = 0
	HelloVersion    Key = 1
	HelloCaps       Key = 2
	HelloLegacyNick Key = 64 // pre-spec clients put the nick here; K_NICK wins
)

// WELCOME body keys.
const (
	WelcomeHub     Key = 0
	WelcomeVersion Key = 1
	WelcomeCaps    Key = 2
	WelcomeLimits  Key = 3
)

// Limit keys inside the WELCOME limits map.
const (
	LimitMaxNickBytes       Key = 0
	LimitMaxRoomNameBytes   Key = 1
	LimitMaxMsgBodyBytes    Key = 2
	LimitMaxRoomsPerSession Key = 3
	LimitRatePerMinute      Key = 4
)

// Capability keys, in HELLO and WELCOME caps maps.
const (
	CapResourceEnvelope Key = 0
	CapAction           Key = 1
	CapDirectNotice     Key = 2
)

// RESOURCE_ENVELOPE body keys.
const (
	ResID       Key = 0
	ResKind     Key = 1
	ResSize     Key = 2
	ResSHA256   Key = 3
	ResEncoding Key = 4
)

// Resource kinds.
const (
	ResourceKindNotice = "notice"
	ResourceKindMOTD   = "motd"
	ResourceKindBlob   = "blob"
)
