package hub

import "github.com/ScotMesh/scotmesh-chat/internal/store"

// Event is something every adapter may need to show. Adapters ignore the
// kinds that don't apply to them.
type Event interface{ event() }

// MessageEvent is a new message in a room.
type MessageEvent struct {
	Message store.Message
	// LXMFTo are the group members who should get it by LXMF: members with
	// delivery on, or on auto and not in the room over RRC, except the
	// author. Empty for rooms other than the group room.
	LXMFTo [][]byte
}

// JoinedEvent says an identity is now in a room (its first link, for RRC).
type JoinedEvent struct {
	Room     string
	Identity []byte
	Name     string
	Via      Via
}

// PartedEvent says an identity has left a room (its last link, for RRC).
type PartedEvent struct {
	Room     string
	Identity []byte
	Name     string
	Via      Via
}

// NameEvent says an identity's display name changed. Old or New may be a guest name.
type NameEvent struct {
	Identity []byte
	Old, New string
}

// RoomNoticeEvent is a line everyone in a room should see, such as a topic
// or mode change.
type RoomNoticeEvent struct {
	Room string
	Text string
}

// NoticeEvent is a line for one identity wherever they are connected, such
// as an invitation.
type NoticeEvent struct {
	Identity []byte
	Room     string // the room it is about, or ""
	Text     string
	// ToLXMF sends it by LXMF even though they are no longer a group member:
	// they were one until this change (a kick or a ban tells them why).
	ToLXMF bool
}

// RemovedEvent says an operator put an identity out of a room (kick or
// room ban). RRC adapters drop the identity's links from the room and send
// Reason as an ERROR.
type RemovedEvent struct {
	Room     string
	Identity []byte
	Reason   string
}

// BannedEvent says an identity was banned from the hub. Adapters disconnect it
// everywhere, immediately.
type BannedEvent struct {
	Identity []byte
	Reason   string
}

// GroupEvent says an identity joined or left the LXMF group.
type GroupEvent struct {
	Identity []byte
	Name     string
	Joined   bool
}

// LXMFResumeEvent says LXMF delivery to a member has resumed after a pause, and
// Missed messages were said meanwhile.
type LXMFResumeEvent struct {
	Identity []byte
	Missed   int
}

func (MessageEvent) event()    {}
func (JoinedEvent) event()     {}
func (PartedEvent) event()     {}
func (NameEvent) event()       {}
func (RoomNoticeEvent) event() {}
func (NoticeEvent) event()     {}
func (RemovedEvent) event()    {}
func (BannedEvent) event()     {}
func (GroupEvent) event()      {}
func (LXMFResumeEvent) event() {}
