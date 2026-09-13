package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// Config is the hub's behaviour. Zero values are replaced by Defaults.
type Config struct {
	HubName string
	// Admins are server operators: identity hashes (16 bytes).
	Admins [][]byte
	// Rooms are created at start if missing, registered, and never pruned.
	Rooms []RoomConfig
	// GroupRoom is the room the LXMF group and the page belong to.
	GroupRoom string
	// GroupAddress is the LXMF group's address in hex, for replies that
	// say where to send /join.
	GroupAddress string

	Retention         time.Duration // how long messages are kept
	ReplayFirstVisit  int           // catch-up for someone never seen in the room
	ReplayMax         int           // most messages replayed on reconnect
	HistoryMax        int           // most messages /history returns
	HistoryCooldown   time.Duration // between /history calls per identity
	PostsPerMinute    int           // per identity, across every way in
	MaxBodyBytes      int           // longest stored message
	MaxTopicBytes     int           // longest /topic
	MaxRoomNameBytes  int
	MaxRoomsPerPerson int
	InviteTTL         time.Duration
	PruneRoomsAfter   time.Duration // unused registered rooms (not default rooms)
}

// RoomConfig is a room that always exists.
type RoomConfig struct {
	Name  string
	Topic string
}

// Defaults returns the configuration the hub runs with when a field is unset.
func Defaults() Config {
	return Config{
		HubName:           "ScotMesh",
		Rooms:             []RoomConfig{{Name: "scotmesh", Topic: "ScotMesh - Scotland's Reticulum community"}},
		GroupRoom:         "scotmesh",
		Retention:         7 * 24 * time.Hour,
		ReplayFirstVisit:  10,
		ReplayMax:         50,
		HistoryMax:        100,
		HistoryCooldown:   15 * time.Second,
		PostsPerMinute:    30,
		MaxBodyBytes:      2000,
		MaxTopicBytes:     300,
		MaxRoomNameBytes:  64,
		MaxRoomsPerPerson: 32,
		InviteTTL:         15 * time.Minute,
		PruneRoomsAfter:   30 * 24 * time.Hour,
	}
}

func (c *Config) validate() error {
	d := Defaults()
	if c.HubName == "" {
		c.HubName = d.HubName
	}
	if c.GroupRoom == "" {
		c.GroupRoom = d.GroupRoom
	}
	setInt := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	setDur := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	setDur(&c.Retention, d.Retention)
	setInt(&c.ReplayFirstVisit, d.ReplayFirstVisit)
	setInt(&c.ReplayMax, d.ReplayMax)
	setInt(&c.HistoryMax, d.HistoryMax)
	setDur(&c.HistoryCooldown, d.HistoryCooldown)
	setInt(&c.PostsPerMinute, d.PostsPerMinute)
	setInt(&c.MaxBodyBytes, d.MaxBodyBytes)
	setInt(&c.MaxTopicBytes, d.MaxTopicBytes)
	setInt(&c.MaxRoomNameBytes, d.MaxRoomNameBytes)
	setInt(&c.MaxRoomsPerPerson, d.MaxRoomsPerPerson)
	setDur(&c.InviteTTL, d.InviteTTL)
	setDur(&c.PruneRoomsAfter, d.PruneRoomsAfter)
	if len(c.Rooms) == 0 {
		c.Rooms = d.Rooms
	}

	var errs []error
	for i, a := range c.Admins {
		if len(a) != store.IdentityLen {
			errs = append(errs, fmt.Errorf("admin %d: identity hash must be %d bytes", i+1, store.IdentityLen))
		}
	}
	group, err := wire.NormalizeRoom(c.GroupRoom, c.MaxRoomNameBytes)
	if err != nil {
		errs = append(errs, fmt.Errorf("group room: %w", err))
	}
	c.GroupRoom = group
	groupListed := false
	for i := range c.Rooms {
		n, err := wire.NormalizeRoom(c.Rooms[i].Name, c.MaxRoomNameBytes)
		if err != nil {
			errs = append(errs, fmt.Errorf("room %q: %w", c.Rooms[i].Name, err))
			continue
		}
		c.Rooms[i].Name = n
		groupListed = groupListed || n == group
	}
	if !groupListed && group != "" {
		errs = append(errs, fmt.Errorf("group room %q must be one of the configured rooms", group))
	}
	return errors.Join(errs...)
}

func (c *Config) isDefaultRoom(name string) bool {
	for _, r := range c.Rooms {
		if r.Name == name {
			return true
		}
	}
	return false
}

func (h *Hub) ensureDefaultRooms(ctx context.Context) error {
	now := h.nowMS()
	return h.st.Update(ctx, func(tx *store.Tx) error {
		for _, rc := range h.cfg.Rooms {
			r, ok, err := tx.Room(rc.Name)
			if err != nil {
				return err
			}
			if ok && r.Registered {
				continue
			}
			if !ok {
				r = store.Room{Name: rc.Name, CreatedAt: now}
			}
			// A configured room belongs to the config, not to whoever
			// happened to create it (by chance, or ahead of the operator
			// adding it here) or was in it before: the configured topic
			// wins, there's no founder, and changing the topic needs a
			// room op (rather than being open to everyone, which is how a
			// fresh room with no modes at all behaves). Not +n: the LXMF
			// group and the page post into this room from outside RRC,
			// which +n would block.
			r.Topic = rc.Topic
			r.Founder = nil
			r.SetMode('t', true)
			r.Registered = true
			r.LastUsed = now
			if err := tx.PutRoom(&r); err != nil {
				return err
			}
		}
		return nil
	})
}
