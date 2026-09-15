package rrcsrv

// Per-link handling (HELLO, JOIN, PART, MSG/NOTICE/ACTION, keepalive) ports
// rrcd 0.3.2's router.py and session.py, Copyright (c) 2025 S. Miller,
// KC1AWV, MIT License — see the package doc in server.go for the lessons
// applied on top.

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/thatSFguy/reticulum-go/rns"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
)

func (s *Server) established(link []byte) {
	s.mu.Lock()
	key := hex.EncodeToString(link)
	if _, ok := s.sessions[key]; ok {
		s.mu.Unlock()
		return
	}
	ss := newSession(link, s.now(), s.cfg.SendQueue, s.cfg.RatePerMinute)
	s.sessions[key] = ss
	s.mu.Unlock()
	s.stats.LinksAccepted.Add(1)
	go s.writer(ss)
}

// sessionFor returns the session for a link, creating it if the established
// callback was lost.
func (s *Server) sessionFor(link []byte) *session {
	if ss := s.session(link); ss != nil {
		return ss
	}
	s.established(link)
	return s.session(link)
}

func (s *Server) identified(ctx context.Context, link, pubKey []byte) {
	ss := s.sessionFor(link)
	id := rns.IdentityHashFromPublicKey(pubKey)
	ss.mu.Lock()
	if ss.identity != nil {
		ss.mu.Unlock()
		return
	}
	ss.identity, ss.pubKey = id, pubKey
	pending := ss.pending
	ss.pending = nil
	ss.mu.Unlock()

	s.mu.Lock()
	k := hex.EncodeToString(id)
	if s.byIdentity[k] == nil {
		s.byIdentity[k] = map[string]*session{}
	}
	s.byIdentity[k][hex.EncodeToString(link)] = ss
	s.mu.Unlock()

	if _, err := s.hub.Identify(ctx, id, pubKey); err != nil {
		s.refuseLink(ss, err)
		return
	}
	for _, frame := range pending {
		s.data(ctx, link, frame)
	}
}

// refuseLink tells a link why it can't stay, then closes it.
func (s *Server) refuseLink(ss *session, err error) {
	var ue *hub.UserError
	text := "internal error"
	if errors.As(err, &ue) {
		text = ue.Text
		if strings.Contains(text, "banned") {
			text = "banned"
		}
	} else {
		s.stats.InternalErrors.Add(1)
		s.log.Error("rrc: identify failed", "link", short(ss.link), "err", err)
	}
	s.sendError(ss, "", text)
	ss.closeAfterFlush()
}

func (s *Server) closed(ctx context.Context, link []byte) {
	s.mu.Lock()
	key := hex.EncodeToString(link)
	ss := s.sessions[key]
	delete(s.sessions, key)
	if ss != nil && ss.identity != nil {
		ik := hex.EncodeToString(ss.identity)
		delete(s.byIdentity[ik], key)
		if len(s.byIdentity[ik]) == 0 {
			delete(s.byIdentity, ik)
		}
	}
	s.mu.Unlock()
	if ss == nil {
		return
	}
	s.stats.LinksClosed.Add(1)
	ss.stop()
	id, rooms := ss.id(), ss.roomList()
	if id != nil && len(rooms) > 0 {
		if err := s.hub.Disconnect(ctx, id, rooms); err != nil && !errors.Is(err, hub.ErrClosed) {
			s.log.Error("rrc: recording a disconnect", "link", short(link), "err", err)
		}
	}
}

func (s *Server) resource(link, body []byte) {
	// rrcd accepts RESOURCE_ENVELOPE announcements but does nothing useful
	// with what follows (its forwarding path cannot work). No client needs
	// it for chat, whose messages fit a packet; we log and drop.
	s.log.Debug("rrc: resource received and ignored", "link", short(link), "bytes", len(body))
}

// data handles one inbound frame, in rrcd's order.
func (s *Server) data(ctx context.Context, link, frame []byte) {
	s.stats.FramesIn.Add(1)
	ss := s.sessionFor(link)
	now := s.now()

	ss.mu.Lock()
	if ss.identity == nil {
		if len(ss.pending) < maxPending {
			ss.pending = append(ss.pending, frame)
		}
		ss.mu.Unlock()
		return
	}
	ss.lastHeard = now
	ss.pingSent = time.Time{}
	ss.mu.Unlock()

	if !ss.take(now, s.cfg.RatePerMinute) {
		s.stats.RateLimited.Add(1)
		ss.mu.Lock()
		tell := now.Sub(ss.errorAt) > 10*time.Second
		if tell {
			ss.errorAt = now
		}
		ss.mu.Unlock()
		if tell {
			s.sendError(ss, "", "rate limited")
		}
		return
	}

	lim := wire.DefaultLimits()
	lim.MaxFrameBytes = maxFrame
	env, err := wire.Decode(frame, lim)
	if err != nil {
		s.stats.BadFrames.Add(1)
		s.sendError(ss, "", "bad message: "+err.Error())
		return
	}

	ss.mu.Lock()
	welcomed := ss.welcomed
	ss.mu.Unlock()
	switch {
	case env.Type == wire.TypePong:
		return
	case env.Type == wire.TypePing:
		pong := s.hubEnvelope(wire.TypePong, "")
		pong.Body = env.Body
		s.send(ss, pong)
		return
	case env.Type == wire.TypeResourceEnvelope:
		if _, err := wire.ParseResourceEnvelope(env.Body); err != nil {
			s.sendError(ss, env.Room, err.Error())
		}
		return
	case env.Type == wire.TypeHello:
		s.hello(ctx, ss, env)
		return
	case !welcomed:
		s.sendError(ss, "", "send HELLO first")
		return
	}

	switch env.Type {
	case wire.TypeJoin:
		s.join(ctx, ss, env)
	case wire.TypePart:
		s.part(ctx, ss, env)
	case wire.TypeMsg, wire.TypeNotice, wire.TypeAction:
		s.message(ctx, ss, env)
	default:
		// Unknown types after WELCOME are ignored, as rrcd does.
	}
}

func (s *Server) hello(ctx context.Context, ss *session, env *wire.Envelope) {
	h := wire.ParseHello(env.Body)
	id := ss.id()

	ss.mu.Lock()
	rehello := ss.welcomed
	oldRooms := ss.rooms
	ss.rooms = map[string]bool{}
	ss.welcomed = false
	ss.caps = h.Caps
	ss.mu.Unlock()
	if rehello && len(oldRooms) > 0 {
		// A re-HELLO starts the session afresh. Unlike rrcd, the rooms are
		// left properly, so others see PARTED.
		rooms := make([]string, 0, len(oldRooms))
		for r := range oldRooms {
			rooms = append(rooms, r)
		}
		if err := s.hub.Disconnect(ctx, id, rooms); err != nil {
			s.log.Error("rrc: leaving rooms on re-HELLO", "err", err)
		}
	}

	ss.mu.Lock()
	pubKey := ss.pubKey
	ss.mu.Unlock()
	person, err := s.hub.Identify(ctx, id, pubKey)
	if err != nil {
		s.refuseLink(ss, err)
		return
	}
	nick := env.Nick
	if nick == "" {
		nick = h.LegacyNick
	}
	var note string
	if n, ok := wire.NormalizeNick(nick, s.cfg.MaxNickBytes); ok && n != person.Name {
		if p, err := s.hub.ClaimName(ctx, id, n); err != nil {
			note = s.userText(err)
		} else {
			person = p
		}
		ss.mu.Lock()
		ss.triedNick[n] = true
		ss.mu.Unlock()
	}

	body, err := wire.Welcome{
		Hub:     s.cfg.HubName,
		Version: s.cfg.Version,
		Caps:    map[wire.Key]bool{wire.CapAction: true, wire.CapDirectNotice: true},
		Limits: wire.HubLimits{
			MaxNickBytes:       s.cfg.MaxNickBytes,
			MaxRoomNameBytes:   s.cfg.MaxRoomNameBytes,
			MaxMsgBodyBytes:    s.cfg.MaxMsgBodyBytes,
			MaxRoomsPerSession: s.cfg.MaxRoomsPerSession,
			RatePerMinute:      s.cfg.RatePerMinute,
		},
	}.Encode()
	if err != nil {
		s.log.Error("rrc: building WELCOME", "err", err)
		return
	}
	welcome := s.hubEnvelope(wire.TypeWelcome, "")
	welcome.Body = body
	if !s.send(ss, welcome) {
		// rrcd marks the session welcomed even when WELCOME could not be
		// sent; a client that never saw it will say HELLO again.
		return
	}
	ss.mu.Lock()
	ss.welcomed = true
	ss.nick = person.Name
	ss.mu.Unlock()

	var lines []string
	lines = append(lines, s.cfg.Greeting...)
	lines = append(lines, "Commands start with / or, in NomadNet, with ! (try !help).")
	if note != "" {
		lines = append(lines, note)
	} else if !person.Claimed {
		lines = append(lines, "You are "+person.Name+". Set a nickname in your app, or send /nick YourName, to claim a name.")
	}
	if len(lines) > 0 {
		s.sendFrames(ss, s.noticeFrames(wire.TypeNotice, "", strings.Join(lines, "\n")))
	}
	s.sendMissedWhispers(ctx, ss)
}

// sendMissedWhispers sends whispers that arrived while none of the person's
// apps was on RRC.
func (s *Server) sendMissedWhispers(ctx context.Context, ss *session) {
	whispers, err := s.hub.UnsentRRCWhispers(ctx, ss.id())
	if err != nil {
		s.log.Error("rrc: missed whispers", "err", err)
		return
	}
	for i := range whispers {
		s.sendFrames(ss, s.whisperFrames(&whispers[i], ss.id()))
		if err := s.hub.WhisperSentRRC(ctx, whispers[i].ID); err != nil {
			s.log.Error("rrc: recording a whisper sent", "err", err)
		}
	}
}

func (s *Server) join(ctx context.Context, ss *session, env *wire.Envelope) {
	key, _ := env.BodyString()
	if room, err := wire.NormalizeRoom(env.Room, s.cfg.MaxRoomNameBytes); err == nil {
		// The hub counts links per room, so a JOIN for a room this link is
		// already in must not reach it twice; the client just hears JOINED.
		if ss.inRoom(room) {
			s.send(ss, s.hubEnvelope(wire.TypeJoined, room))
			return
		}
		ss.mu.Lock()
		n := len(ss.rooms)
		ss.mu.Unlock()
		if n >= s.cfg.MaxRoomsPerSession {
			s.sendError(ss, env.Room, "too many rooms")
			return
		}
	}
	// Hold room messages for this link while the hub commits the join.
	// Anything said in the meantime either is in the catch-up replay (the hub
	// counts it as delivered) or comes after it; nothing is lost and nothing
	// is sent twice.
	pending, _ := wire.NormalizeRoom(env.Room, s.cfg.MaxRoomNameBytes)
	if pending != "" {
		ss.mu.Lock()
		ss.joining[pending] = nil
		ss.mu.Unlock()
	}
	res, err := s.hub.Join(ctx, hub.JoinRequest{Identity: ss.id(), Room: env.Room, Key: key})
	if err != nil {
		if pending != "" {
			ss.mu.Lock()
			delete(ss.joining, pending)
			ss.mu.Unlock()
		}
		s.sendError(ss, env.Room, s.userText(err))
		return
	}
	room := res.Room.Name
	defer s.finishJoin(ss, room, res.SeenUpTo)

	joined := s.hubEnvelope(wire.TypeJoined, room)
	if s.cfg.IncludeJoinedMemberList {
		ids := make([][]byte, 0, len(res.Members))
		for _, m := range res.Members {
			ids = append(ids, m.Identity)
		}
		if err := joined.SetBody(ids); err != nil {
			s.log.Error("rrc: JOINED body", "err", err)
		}
		if b, err := encode(joined); err != nil || len(b) > maxFrame {
			joined.Body = nil // a long member list would not fit; /who has it
		}
	}
	s.send(ss, joined)

	topic := res.Room.Topic
	if topic == "" {
		topic = "(none)"
	}
	state := "unregistered"
	if res.Room.Registered {
		state = "registered"
	}
	info := "room " + room + ": " + state + "; mode=" + modeString(res.Room.Modes, res.Room.Key != "", res.Room.Registered) + "; topic=" + topic
	s.sendFrames(ss, s.noticeFrames(wire.TypeNotice, room, info))

	if len(res.Replay) > 0 {
		s.sendFrames(ss, s.noticeFrames(wire.TypeNotice, room, res.ReplayNote))
		for i := range res.Replay {
			s.sendFrames(ss, s.messageFrames(&res.Replay[i]))
		}
		s.sendFrames(ss, s.noticeFrames(wire.TypeNotice, room, "— end of history —"))
	}
}

// finishJoin makes the link a member of the room after JOINED and the replay
// have been queued, then sends what arrived meanwhile that the replay didn't
// cover.
func (s *Server) finishJoin(ss *session, room string, seenUpTo int64) {
	ss.mu.Lock()
	held := ss.joining[room]
	delete(ss.joining, room)
	ss.rooms[room] = true
	ss.mu.Unlock()
	for _, b := range held {
		if b.id > seenUpTo {
			s.sendFrames(ss, b.frames)
		}
	}
}

func modeString(modes string, key, registered bool) string {
	const order = "ikmnprt"
	var b strings.Builder
	for i := 0; i < len(order); i++ {
		m := order[i]
		switch {
		case m == 'k' && key, m == 'r' && registered, m != 'k' && m != 'r' && strings.IndexByte(modes, m) >= 0:
			b.WriteByte(m)
		}
	}
	if b.Len() == 0 {
		return "(none)"
	}
	return "+" + b.String()
}

func (s *Server) part(ctx context.Context, ss *session, env *wire.Envelope) {
	if env.Room == "" {
		s.sendError(ss, "", "PART requires room name")
		return
	}
	room, err := wire.NormalizeRoom(env.Room, s.cfg.MaxRoomNameBytes)
	if err != nil {
		s.sendError(ss, env.Room, err.Error())
		return
	}
	// Only this link's own membership is given up; rrcd answers PARTED
	// whether or not the link was in the room.
	ss.mu.Lock()
	in := ss.rooms[room]
	delete(ss.rooms, room)
	ss.mu.Unlock()
	if in {
		if _, err := s.hub.Part(ctx, ss.id(), room); err != nil {
			s.sendError(ss, env.Room, s.userText(err))
		}
	}
	parted := s.hubEnvelope(wire.TypeParted, room)
	if err := parted.SetBody([][]byte{ss.id()}); err != nil {
		s.log.Error("rrc: PARTED body", "err", err)
	}
	s.send(ss, parted)
}

func (s *Server) message(ctx context.Context, ss *session, env *wire.Envelope) {
	id := ss.id()
	text, isText := env.BodyString()
	if isText && env.Type == wire.TypeMsg {
		// "!me waves" from a client that keeps /me to itself.
		if action, ok := hub.ActionText(text); ok && strings.HasPrefix(strings.TrimSpace(text), "!") {
			env = env.Clone()
			env.Type = wire.TypeAction
			env.Body = wire.TextBody(action)
			text = action
		}
	}
	if isText && (env.Type == wire.TypeMsg || env.Type == wire.TypeNotice) && hub.IsCommand(text) {
		s.command(ctx, ss, env, text)
		return
	}
	if env.Type == wire.TypeNotice {
		s.notice(ctx, ss, env)
		return
	}
	if env.Room == "" {
		s.sendError(ss, "", "message requires room name")
		return
	}
	if !isText {
		s.sendError(ss, env.Room, "message body must be text")
		return
	}
	if len(text) > s.cfg.MaxMsgBodyBytes {
		s.sendError(ss, "", "message too large: "+strconv.Itoa(len(text))+" bytes > "+strconv.Itoa(s.cfg.MaxMsgBodyBytes)+" bytes")
		return
	}
	s.maybeClaimNick(ctx, ss, env.Nick)

	var ext []byte
	if len(env.Ext) > 0 {
		b, err := cbor.Marshal(env.Ext)
		if err == nil {
			ext = b
		}
	}
	_, err := s.hub.Post(ctx, hub.PostRequest{
		Via: hub.ViaRRC, Identity: id, Room: env.Room, Action: env.Type == wire.TypeAction,
		Body: text, OriginID: env.ID, SaidAt: int64(min(env.TS, uint64(1)<<62)), Ext: ext, //nolint:gosec // clamped
	})
	if err != nil {
		s.sendError(ss, env.Room, s.userText(err))
	}
	// The hub's MessageEvent delivers the message to everyone in the room,
	// the sender's own links included, as rrcd echoes.
}

// maybeClaimNick tries a K_NICK that differs from the name the hub gave, and
// explains a refusal once per nick per session.
func (s *Server) maybeClaimNick(ctx context.Context, ss *session, nick string) {
	n, ok := wire.NormalizeNick(nick, s.cfg.MaxNickBytes)
	ss.mu.Lock()
	current, tried := ss.nick, ss.triedNick[n]
	if ok {
		ss.triedNick[n] = true
	}
	ss.mu.Unlock()
	if !ok || n == current || tried {
		return
	}
	p, err := s.hub.ClaimName(ctx, ss.id(), n)
	if err != nil {
		s.sendFrames(ss, s.noticeFrames(wire.TypeNotice, "", s.userText(err)))
		return
	}
	ss.mu.Lock()
	ss.nick = p.Name
	ss.mu.Unlock()
}

func (s *Server) command(ctx context.Context, ss *session, env *wire.Envelope, text string) {
	room := env.Room
	if room != "" {
		if r, err := wire.NormalizeRoom(room, s.cfg.MaxRoomNameBytes); err == nil {
			room = r
		}
	}
	reply, err := s.hub.Command(ctx, hub.CommandRequest{Via: hub.ViaRRC, Identity: ss.id(), Room: room, Text: text})
	if err != nil {
		s.sendError(ss, "", s.userText(err))
		return
	}
	if reply.PartRoom != "" && ss.inRoom(reply.PartRoom) {
		// /leave: the same as a PART from this link.
		part := s.hubEnvelope(wire.TypePart, reply.PartRoom)
		part.Src = ss.id()
		s.part(ctx, ss, part)
	}
	if len(reply.History) > 0 {
		histRoom := reply.History[0].Room
		s.sendFrames(ss, s.noticeFrames(wire.TypeNotice, histRoom, reply.HistoryNote))
		for i := range reply.History {
			s.sendFrames(ss, s.messageFrames(&reply.History[i]))
		}
		s.sendFrames(ss, s.noticeFrames(wire.TypeNotice, histRoom, "— end of history —"))
	}
	if len(reply.Lines) == 0 {
		return
	}
	// Command replies carry no room: MeshChatX files a NOTICE with a room
	// under that room and it vanishes from the conversation you typed in.
	t := wire.TypeNotice
	if reply.Error && len(reply.Lines) == 1 {
		t = wire.TypeError
	}
	if reply.Whole {
		// rrcd sends /list as one multi-line NOTICE, and MeshChatX and
		// NomadNet before 1.4.3 find no rooms in it any other way. Too long
		// for one frame, it goes line by line, which NomadNet 1.4.3 rejoins.
		e := s.hubEnvelope(t, "")
		e.Body = wire.TextBody(reply.Text())
		if len(reply.Text()) <= s.bodyBudget(e) {
			if frame, err := encode(e); err == nil {
				s.sendFrame(ss, frame)
				return
			}
		}
		s.log.Debug("rrc: reply too long for one notice; sending it line by line", "link", short(ss.link), "bytes", len(reply.Text()))
	}
	s.sendFrames(ss, s.noticeFrames(t, "", reply.Text()))
}

// notice handles a NOTICE that is not a command: a direct notice to one
// identity (K_DST), or a notice to a room, which is passed to the room's RRC
// members without being stored.
func (s *Server) notice(ctx context.Context, ss *session, env *wire.Envelope) {
	id := ss.id()
	if env.Dst != nil {
		if env.Room != "" {
			s.sendError(ss, "", "direct notice must not include room")
			return
		}
		// The same checks /whisper applies: ignore, whisper preference, hub
		// ban, rate limit. Without this a direct NOTICE reached its target
		// with none of them, including someone who had ignored the sender.
		if err := s.hub.MayNotifyDirect(ctx, id, env.Dst); err != nil {
			s.sendError(ss, "", s.userText(err))
			return
		}
		targets := s.sessionsOf(env.Dst)
		if len(targets) == 0 {
			s.sendError(ss, "", "destination not connected")
			return
		}
		out := env.Clone()
		out.Src = id
		out.Nick = s.nameOf(ctx, id)
		frame, err := encode(out)
		if err != nil {
			s.sendError(ss, "", "message too large")
			return
		}
		for _, t := range targets {
			s.sendFrame(t, frame)
		}
		return
	}
	if env.Room == "" {
		return // rrcd drops a roomless, targetless NOTICE
	}
	room, err := wire.NormalizeRoom(env.Room, s.cfg.MaxRoomNameBytes)
	if err != nil || !ss.inRoom(room) {
		s.sendError(ss, env.Room, "no outside messages (+n)")
		return
	}
	// The same checks a room MSG applies: room ban, +m/voice, rate limit.
	if err := s.hub.MayNotice(ctx, id, room); err != nil {
		s.sendError(ss, room, s.userText(err))
		return
	}
	out := env.Clone()
	out.Src, out.Room, out.Nick = id, room, s.nameOf(ctx, id)
	frame, err := encode(out)
	if err != nil {
		return
	}
	for _, t := range s.sessionsIn(room) {
		s.sendFrame(t, frame)
	}
}

func (s *Server) nameOf(ctx context.Context, id []byte) string {
	p, err := s.hub.PersonOf(ctx, id)
	if err != nil {
		return hub.GuestName(id)
	}
	return p.Name
}

// keepalive pings silent links and closes those that stay silent, and
// closes links that never finish HELLO.
func (s *Server) keepalive(_ context.Context) {
	now := s.now()
	s.mu.Lock()
	all := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		all = append(all, ss)
	}
	s.mu.Unlock()
	for _, ss := range all {
		ss.mu.Lock()
		welcomed, heard, pinged, started := ss.welcomed, ss.lastHeard, ss.pingSent, ss.start
		ss.mu.Unlock()
		switch {
		case !welcomed:
			if now.Sub(started) > s.cfg.HelloTimeout {
				s.log.Debug("rrc: no HELLO in time; closing", "link", short(ss.link))
				ss.stop()
				s.tr.TeardownLink(ss.link)
			}
		case !pinged.IsZero() && now.Sub(pinged) > s.cfg.PingTimeout:
			s.log.Debug("rrc: link silent after PING; closing", "link", short(ss.link))
			ss.stop()
			s.tr.TeardownLink(ss.link)
		case pinged.IsZero() && now.Sub(heard) > s.cfg.PingInterval:
			ping := s.hubEnvelope(wire.TypePing, "")
			if err := ping.SetBody(uint64(now.UnixMilli())); err == nil && s.send(ss, ping) { //nolint:gosec // after 1970
				ss.mu.Lock()
				ss.pingSent = now
				ss.mu.Unlock()
			}
		}
	}
}

// --- sending ------------------------------------------------------------------

func (s *Server) send(ss *session, e *wire.Envelope) bool {
	frame, err := encode(e)
	if err != nil {
		s.log.Error("rrc: building a frame", "type", e.Type.String(), "err", err)
		return false
	}
	return s.sendFrame(ss, frame)
}

func (s *Server) sendFrames(ss *session, frames [][]byte) {
	for _, f := range frames {
		s.sendFrame(ss, f)
	}
}

func (s *Server) sendFrame(ss *session, frame []byte) bool {
	if ss.enqueue(frame) {
		return true
	}
	s.stats.FramesDropped.Add(1)
	return false
}

func (s *Server) sendError(ss *session, room, text string) {
	s.sendFrames(ss, s.noticeFrames(wire.TypeError, room, text))
}

func (s *Server) userText(err error) string {
	var ue *hub.UserError
	if errors.As(err, &ue) {
		return ue.Text
	}
	s.stats.InternalErrors.Add(1)
	s.log.Error("rrc: hub request failed", "err", err)
	return "internal error, sorry; the hub operator has been told"
}
