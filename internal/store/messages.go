package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Via is the way a message or a reader came in.
type Via string

// Ways in.
const (
	ViaRRC    Via = "rrc"
	ViaLXMF   Via = "lxmf"
	ViaPage   Via = "page"
	ViaImport Via = "import" // messages brought over from rrc-hub or bridge 0.1
)

// Kind is what a message is.
type Kind string

// Message kinds kept in history. Notices and command traffic are not stored.
const (
	KindMsg    Kind = "msg"
	KindAction Kind = "action"
)

// Message is one stored chat message.
type Message struct {
	ID         int64
	Room       string
	Kind       Kind
	Author     []byte
	AuthorName string
	Body       string
	Via        Via
	OriginID   []byte // nil when the way in has no message ID
	SaidAt     int64
	HubAt      int64
	Ext        []byte // RRC extension fields as a CBOR map, or nil
}

// AddMessage stores a message and returns its ID. If a message with the same
// Via and OriginID is already stored, nothing is written and dup is true,
// with the existing message's ID.
func (t *Tx) AddMessage(m *Message) (id int64, dup bool, err error) {
	if err := checkID(m.Author); err != nil {
		return 0, false, err
	}
	if m.OriginID != nil {
		err := t.queryRow(`SELECT id FROM messages WHERE via = ? AND origin_id = ?`, string(m.Via), m.OriginID).Scan(&id)
		if err == nil {
			return id, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("check duplicate message: %w", err)
		}
	}
	var origin, ext any
	if m.OriginID != nil {
		origin = m.OriginID
	}
	if m.Ext != nil {
		ext = m.Ext
	}
	res, err := t.exec(`
		INSERT INTO messages (room, kind, author, author_name, body, via, origin_id, said_at, hub_at, ext)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Room, string(m.Kind), m.Author, m.AuthorName, m.Body, string(m.Via), origin, m.SaidAt, m.HubAt, ext)
	if err != nil {
		return 0, false, fmt.Errorf("add message: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	m.ID = id
	return id, false, nil
}

const messageCols = `id, room, kind, author, author_name, body, via, origin_id, said_at, hub_at, ext`

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		var m Message
		var kind, via string
		if err := rows.Scan(&m.ID, &m.Room, &kind, &m.Author, &m.AuthorName, &m.Body, &via, &m.OriginID, &m.SaidAt, &m.HubAt, &m.Ext); err != nil {
			return nil, err
		}
		m.Kind, m.Via = Kind(kind), Via(via)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Message returns one message by ID.
func (t *Tx) Message(id int64) (Message, bool, error) {
	rows, err := t.query(`SELECT `+messageCols+` FROM messages WHERE id = ?`, id)
	if err != nil {
		return Message{}, false, err
	}
	ms, err := scanMessages(rows)
	if err != nil || len(ms) == 0 {
		return Message{}, false, err
	}
	return ms[0], true, nil
}

// MessagesAfter returns up to limit messages in room with ID > afterID,
// oldest first.
func (t *Tx) MessagesAfter(room string, afterID int64, limit int) ([]Message, error) {
	rows, err := t.query(`SELECT `+messageCols+` FROM messages WHERE room = ? AND id > ? ORDER BY id LIMIT ?`, room, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("messages after %d: %w", afterID, err)
	}
	return scanMessages(rows)
}

// LatestMessages returns the last limit messages in room with ID < beforeID
// (0 means no bound), oldest first.
func (t *Tx) LatestMessages(room string, beforeID int64, limit int) ([]Message, error) {
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	rows, err := t.query(`
		SELECT `+messageCols+` FROM (
			SELECT `+messageCols+` FROM messages WHERE room = ? AND id < ? ORDER BY id DESC LIMIT ?
		) ORDER BY id`, room, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("latest messages: %w", err)
	}
	return scanMessages(rows)
}

// LatestMessagesByPerson returns a person's last limit messages in room, from
// any of their apps, oldest first.
func (t *Tx) LatestMessagesByPerson(person int64, room string, limit int) ([]Message, error) {
	rows, err := t.query(`
		SELECT `+messageCols+` FROM (
			SELECT `+prefixed("m.", messageCols)+` FROM messages m JOIN identities i ON i.id = m.author
			WHERE i.person = ? AND m.room = ? ORDER BY m.id DESC LIMIT ?
		) ORDER BY id`, person, room, limit)
	if err != nil {
		return nil, fmt.Errorf("messages by person %d: %w", person, err)
	}
	return scanMessages(rows)
}

// DeleteMessages removes messages with their LXMF deliveries not yet
// finished, and returns how many of those deliveries were cancelled.
func (t *Tx) DeleteMessages(ids []int64) (int64, error) {
	var cancelled int64
	for _, id := range ids {
		res, err := t.exec(`DELETE FROM deliveries WHERE message_id = ? AND state = 'queued'`, id)
		if err != nil {
			return cancelled, fmt.Errorf("cancel deliveries of %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return cancelled, err
		}
		cancelled += n
		if _, err := t.exec(`DELETE FROM deliveries WHERE message_id = ?`, id); err != nil {
			return cancelled, err
		}
		if _, err := t.exec(`DELETE FROM messages WHERE id = ?`, id); err != nil {
			return cancelled, fmt.Errorf("delete message %d: %w", id, err)
		}
	}
	return cancelled, nil
}

// CountAfter counts messages in room with ID > afterID.
func (t *Tx) CountAfter(room string, afterID int64) (int, error) {
	var n int
	err := t.queryRow(`SELECT count(*) FROM messages WHERE room = ? AND id > ?`, room, afterID).Scan(&n)
	return n, err
}

// LastMessageID returns the newest message ID in room, or 0.
func (t *Tx) LastMessageID(room string) (int64, error) {
	var id sql.NullInt64
	err := t.queryRow(`SELECT max(id) FROM messages WHERE room = ?`, room).Scan(&id)
	return id.Int64, err
}

// PruneMessages deletes messages whose hub time is before cutoff, along with
// their unfinished deliveries, and returns how many messages went.
func (t *Tx) PruneMessages(cutoff int64) (int64, error) {
	if _, err := t.exec(`DELETE FROM deliveries WHERE message_id IN (SELECT id FROM messages WHERE hub_at < ?)`, cutoff); err != nil {
		return 0, fmt.Errorf("prune deliveries: %w", err)
	}
	res, err := t.exec(`DELETE FROM messages WHERE hub_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune messages: %w", err)
	}
	return res.RowsAffected()
}

// Cursor is the last message delivered to an identity in a room via a way in.
type Cursor struct {
	MessageID int64
	UpdatedAt int64
}

// Cursor returns the cursor for identity in room via a way in.
func (t *Tx) Cursor(identity []byte, room string, via Via) (Cursor, bool, error) {
	var c Cursor
	err := t.queryRow(`SELECT message_id, updated_at FROM cursors WHERE identity = ? AND room = ? AND via = ?`, identity, room, string(via)).
		Scan(&c.MessageID, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return Cursor{}, false, fmt.Errorf("cursor: %w", err)
	}
	return c, true, nil
}

// SetCursor records the last message delivered. A cursor never moves back.
func (t *Tx) SetCursor(identity []byte, room string, via Via, messageID, now int64) error {
	if err := checkID(identity); err != nil {
		return err
	}
	_, err := t.exec(`
		INSERT INTO cursors (identity, room, via, message_id, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (identity, room, via) DO UPDATE SET
			message_id = max(message_id, excluded.message_id), updated_at = excluded.updated_at`,
		identity, room, string(via), messageID, now)
	if err != nil {
		return fmt.Errorf("set cursor: %w", err)
	}
	return nil
}
