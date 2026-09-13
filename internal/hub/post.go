package hub

import (
	"bytes"
	"context"
	"math"
	"strings"
	"time"
	_ "time/tzdata" // Europe/London without relying on the host's zoneinfo
	"unicode/utf8"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

var london = func() *time.Location {
	l, err := time.LoadLocation("Europe/London")
	if err != nil { // impossible with tzdata embedded
		panic(err)
	}
	return l
}()

// PostRequest is a message someone said.
type PostRequest struct {
	Via      Via
	Identity []byte
	Room     string // "" means the group room (LXMF, page)
	Action   bool   // /me
	Body     string
	OriginID []byte // RRC K_ID or LXMF message hash, for de-duplication
	SaidAt   int64  // the author's clock; 0 means now
	Ext      []byte // RRC extension fields (CBOR map), relayed and stored
}

// PostResult is the stored message.
type PostResult struct {
	Message   store.Message
	Duplicate bool // already stored; nothing was sent again
}

// bucket is a token bucket: capacity perMinute, refilling continuously.
type bucket struct {
	tokens float64
	at     time.Time
}

func (h *Hub) allowPost(id []byte) bool {
	now := h.now()
	perMin := float64(h.cfg.PostsPerMinute)
	b := h.rate[hexID(id)]
	if b == nil {
		b = &bucket{tokens: perMin, at: now}
		h.rate[hexID(id)] = b
	}
	b.tokens = min(perMin, b.tokens+now.Sub(b.at).Minutes()*perMin)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Post stores a message and sends it to everyone. Refusals use rrcd's
// wording where rrcd has one.
func (h *Hub) Post(ctx context.Context, req PostRequest) (PostResult, error) {
	var res PostResult
	err := h.update(ctx, func(tx *store.Tx, out *outbox) error {
		var err error
		res, err = h.post(tx, out, req)
		return err
	})
	if err != nil {
		h.stats.Refusals.Add(1)
	}
	return res, err
}

func (h *Hub) post(tx *store.Tx, out *outbox, req PostRequest) (PostResult, error) {
	var res PostResult
	id := req.Identity
	room := req.Room
	if room == "" {
		room = h.cfg.GroupRoom
	}
	room, err := h.normRoom(room)
	if err != nil {
		return res, err
	}
	body := strings.TrimRight(req.Body, " \t\r\n")
	if strings.TrimSpace(body) == "" {
		return res, refuse("message is empty")
	}
	if !utf8.ValidString(body) {
		return res, refuse("message is not valid text")
	}
	if len(body) > h.cfg.MaxBodyBytes {
		return res, refuse("message too large: %d bytes > %d bytes", len(body), h.cfg.MaxBodyBytes)
	}
	if err := h.checkHubBan(tx, id); err != nil {
		return res, err
	}

	r, exists, err := tx.Room(room)
	if err != nil {
		return res, err
	}
	if !exists {
		return res, refuse("no such room")
	}
	switch req.Via {
	case ViaRRC:
		if !h.rrcPresent(room, id) && r.HasMode('n') {
			return res, refuse("no outside messages (+n)")
		}
	case ViaLXMF:
		member, err := tx.IsMember(id)
		if err != nil {
			return res, err
		}
		if !member {
			return res, refuse("send /join first to take part in the group")
		}
		if room != h.cfg.GroupRoom {
			return res, refuse("the group only carries #%s", h.cfg.GroupRoom)
		}
	case ViaPage:
		if room != h.cfg.GroupRoom {
			return res, refuse("the page only carries #%s", h.cfg.GroupRoom)
		}
	}
	if err := h.checkRoomBan(tx, room, id); err != nil {
		return res, err
	}
	if r.HasMode('m') {
		voiced, err := h.isVoiced(tx, &r, id)
		if err != nil {
			return res, err
		}
		if !voiced {
			return res, refuse("room is moderated (+m)")
		}
	}
	if !h.allowPost(id) {
		return res, refuse("rate limited")
	}

	now := h.nowMS()
	if err := tx.TouchIdentity(id, nil, now); err != nil {
		return res, err
	}
	name, err := h.displayName(tx, id)
	if err != nil {
		return res, err
	}
	kind := store.KindMsg
	if req.Action {
		kind = store.KindAction
	}
	saidAt := req.SaidAt
	if saidAt <= 0 {
		saidAt = now
	}
	m := store.Message{Room: room, Kind: kind, Author: id, AuthorName: name, Body: body, Via: req.Via,
		OriginID: req.OriginID, SaidAt: saidAt, HubAt: now, Ext: req.Ext}
	msgID, dup, err := tx.AddMessage(&m)
	if err != nil {
		return res, err
	}
	if dup {
		h.stats.Duplicates.Add(1)
		stored, _, err := tx.Message(msgID)
		return PostResult{Message: stored, Duplicate: true}, err
	}
	r.LastUsed = now
	if err := tx.PutRoom(&r); err != nil {
		return res, err
	}
	ev := MessageEvent{Message: m}
	if room == h.cfg.GroupRoom {
		if ev.LXMFTo, err = h.lxmfRecipients(tx, id, req.Via); err != nil {
			return res, err
		}
		// Queued in the same transaction as the message, so a restart can
		// never lose a delivery that was promised.
		for _, to := range ev.LXMFTo {
			if err := tx.PutDelivery(&store.Delivery{MessageID: m.ID, Identity: to, State: store.DeliveryQueued, NextAt: now, UpdatedAt: now}); err != nil {
				return res, err
			}
		}
	}
	out.emit(ev)
	h.stats.Posts.Add(1)
	return PostResult{Message: m}, nil
}

// MayNotice reports whether id may send an RRC NOTICE into room right now,
// applying the same room-entry gates Post does for a stored message (hub
// ban, room ban, +m/voice, the post rate limit) without writing anything:
// unlike MSG, rrcd's NOTICE is never archived. Without this, a client-sent
// NOTICE reached a room with none of Post's rules applied — including to
// someone banned or muted from it.
func (h *Hub) MayNotice(ctx context.Context, id []byte, room string) error {
	var err error
	if derr := h.do(ctx, func() {
		room, err = h.normRoom(room)
		if err != nil {
			return
		}
		err = h.st.View(ctx, func(tx *store.Tx) error {
			if verr := h.checkHubBan(tx, id); verr != nil {
				return verr
			}
			r, exists, verr := tx.Room(room)
			if verr != nil {
				return verr
			}
			if !exists {
				return refuse("no such room")
			}
			if verr := h.checkRoomBan(tx, room, id); verr != nil {
				return verr
			}
			if r.HasMode('m') {
				voiced, verr := h.isVoiced(tx, &r, id)
				if verr != nil {
					return verr
				}
				if !voiced {
					return refuse("room is moderated (+m)")
				}
			}
			return nil
		})
		if err == nil && !h.allowPost(id) {
			err = refuse("rate limited")
		}
	}); derr != nil {
		return derr
	}
	return err
}

func (h *Hub) lxmfRecipients(tx *store.Tx, author []byte, via Via) ([][]byte, error) {
	members, err := tx.Members()
	if err != nil {
		return nil, err
	}
	var to [][]byte
	for _, m := range members {
		if paused, err := h.lxmfPaused(tx, m.LXMFMode, m.Identity); err != nil {
			return nil, err
		} else if paused {
			continue
		}
		// The author's own apps get their message only if they asked for
		// it, and never the LXMF app it came from, which already shows it.
		if bytes.Equal(m.Identity, author) && via == ViaLXMF {
			continue
		}
		if !m.LXMFMine {
			if same, err := h.samePerson(tx, m.Identity, author); err != nil {
				return nil, err
			} else if same {
				continue
			}
		}
		if banned, err := tx.IsBanned(m.Identity); err != nil {
			return nil, err
		} else if banned {
			continue
		}
		to = append(to, m.Identity)
	}
	return to, nil
}

// History returns up to n messages from a room before beforeID (0: the
// latest), oldest first. It is rate limited per identity.
func (h *Hub) History(ctx context.Context, id []byte, room string, n int, beforeID int64) ([]store.Message, error) {
	var msgs []store.Message
	err := h.update(ctx, func(tx *store.Tx, _ *outbox) error {
		var err error
		msgs, err = h.history(tx, id, room, n, beforeID)
		return err
	})
	return msgs, err
}

func (h *Hub) history(tx *store.Tx, id []byte, room string, n int, beforeID int64) ([]store.Message, error) {
	if room == "" {
		room = h.cfg.GroupRoom
	}
	room, err := h.normRoom(room)
	if err != nil {
		return nil, err
	}
	if last, ok := h.historyAt[hexID(id)]; ok && h.now().Sub(last) < h.cfg.HistoryCooldown {
		wait := h.cfg.HistoryCooldown - h.now().Sub(last)
		return nil, refuse("please wait %d seconds before asking for history again", int(math.Ceil(wait.Seconds())))
	}
	r, ok, err := tx.Room(room)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, refuse("no such room")
	}
	if err := h.checkRoomBan(tx, room, id); err != nil {
		return nil, err
	}
	if ok, err := h.mayReadRoom(tx, &r, id); err != nil {
		return nil, err
	} else if !ok {
		return nil, refuse("room %s is private", room)
	}
	if n <= 0 {
		n = 20
	}
	n = min(n, h.cfg.HistoryMax)
	h.historyAt[hexID(id)] = h.now()
	return tx.LatestMessages(room, beforeID, n)
}

// Recent returns the latest messages in a room for display, without rate
// limiting: the page renders from it.
func (h *Hub) Recent(ctx context.Context, room string, n int, beforeID int64) ([]store.Message, error) {
	var msgs []store.Message
	err := h.st.View(ctx, func(tx *store.Tx) error {
		var err error
		msgs, err = tx.LatestMessages(room, beforeID, n)
		return err
	})
	return msgs, err
}
