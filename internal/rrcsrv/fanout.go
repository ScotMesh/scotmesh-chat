package rrcsrv

// Turning hub events into frames for the sessions they concern follows
// rrcd 0.3.2's session.py broadcast behaviour, Copyright (c) 2025 S.
// Miller, KC1AWV, MIT License — see the package doc in server.go.

import (
	"bytes"
	"context"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
)

// fanOut turns hub events into frames for the sessions they concern.
func (s *Server) fanOut(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-s.events.Events():
			if !ok {
				return
			}
			s.deliver(ctx, ev)
		}
	}
}

func (s *Server) deliver(ctx context.Context, ev hub.Event) {
	switch e := ev.(type) {
	case hub.WhisperEvent:
		sent := false
		for _, id := range e.To {
			for _, ss := range s.sessionsOf(id) {
				s.sendFrames(ss, s.whisperFrames(&e.Whisper, id))
				sent = true
			}
		}
		if sent {
			if err := s.hub.WhisperSentRRC(ctx, e.Whisper.ID); err != nil && ctx.Err() == nil {
				s.log.Error("rrc: recording a whisper sent", "err", err)
			}
		}
	case hub.MessageEvent:
		frames := s.messageFrames(&e.Message)
		for _, ss := range s.allSessions() {
			s.offerMessage(ss, e.Message.Room, e.Message.ID, frames)
		}
	case hub.JoinedEvent:
		s.presenceChange(wire.TypeJoined, e.Room, e.Identity, e.Name)
	case hub.PartedEvent:
		s.presenceChange(wire.TypeParted, e.Room, e.Identity, e.Name)
	case hub.GroupEvent:
		// LXMF members are in the group room too; RRC clients keep member
		// lists from JOINED and PARTED.
		t := wire.TypeParted
		if e.Joined {
			t = wire.TypeJoined
		}
		s.presenceChange(t, s.groupRoomHint(), e.Identity, e.Name)
	case hub.NameEvent:
		for _, ss := range s.sessionsOf(e.Identity) {
			ss.mu.Lock()
			ss.nick = e.New
			ss.mu.Unlock()
		}
	case hub.RoomNoticeEvent:
		frames := s.noticeFrames(wire.TypeNotice, e.Room, e.Text)
		for _, ss := range s.sessionsIn(e.Room) {
			s.sendFrames(ss, frames)
		}
	case hub.NoticeEvent:
		frames := s.noticeFrames(wire.TypeNotice, "", e.Text)
		for _, ss := range s.sessionsOf(e.Identity) {
			s.sendFrames(ss, frames)
		}
	case hub.RemovedEvent:
		for _, ss := range s.sessionsOf(e.Identity) {
			ss.mu.Lock()
			in := ss.rooms[e.Room]
			delete(ss.rooms, e.Room)
			ss.mu.Unlock()
			if in {
				s.sendError(ss, e.Room, e.Reason)
			}
		}
	case hub.BannedEvent:
		for _, ss := range s.sessionsOf(e.Identity) {
			ss.mu.Lock()
			ss.rooms = map[string]bool{}
			ss.mu.Unlock()
			reason := "banned"
			if e.Reason != "" {
				reason = e.Reason
			}
			s.sendError(ss, "", reason)
			ss.closeAfterFlush()
		}
	}
}

// presenceChange sends JOINED or PARTED with [identity] and the name to the
// room's other members, as rrcd does.
func (s *Server) presenceChange(t wire.Type, room string, id []byte, name string) {
	if room == "" {
		return
	}
	e := s.hubEnvelope(t, room)
	e.Nick = name
	if err := e.SetBody([][]byte{id}); err != nil {
		return
	}
	frame, err := encode(e)
	if err != nil {
		return
	}
	for _, ss := range s.sessionsIn(room) {
		if bytes.Equal(ss.id(), id) {
			continue
		}
		s.sendFrame(ss, frame)
	}
}

// groupRoomHint is the room LXMF group changes appear in.
func (s *Server) groupRoomHint() string { return s.cfg.GroupRoom }
