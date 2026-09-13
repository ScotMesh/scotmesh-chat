package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Whisper is a message to one person (ADR 0009). It is kept 7 days and never
// appears in history, catch-up or the page conversation.
type Whisper struct {
	ID       int64
	From     []byte // the identity that sent it
	FromName string // the name it was sent under
	ToPerson int64
	ToName   string // the name it was sent to
	Body     string
	Via      Via
	SaidAt   int64
	RRCAt    int64 // when it went to one of their RRC links; 0 not yet
	ReadAt   int64 // when they saw it on the page; 0 not yet
}

// AddWhisper stores a whisper and sets its ID.
func (t *Tx) AddWhisper(w *Whisper) error {
	if err := checkID(w.From); err != nil {
		return err
	}
	res, err := t.exec(`INSERT INTO whispers (from_id, from_name, to_person, to_name, body, via, said_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.From, w.FromName, w.ToPerson, w.ToName, w.Body, string(w.Via), w.SaidAt)
	if err != nil {
		return fmt.Errorf("add whisper: %w", err)
	}
	w.ID, err = res.LastInsertId()
	return err
}

const whisperColumns = `id, from_id, from_name, to_person, to_name, body, via, said_at, rrc_at, read_at`

func (t *Tx) whispers(q string, args ...any) ([]Whisper, error) {
	rows, err := t.query(`SELECT `+whisperColumns+` FROM whispers `+q, args...)
	if err != nil {
		return nil, fmt.Errorf("whispers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Whisper
	for rows.Next() {
		var w Whisper
		var via string
		if err := rows.Scan(&w.ID, &w.From, &w.FromName, &w.ToPerson, &w.ToName, &w.Body, &via, &w.SaidAt, &w.RRCAt, &w.ReadAt); err != nil {
			return nil, err
		}
		w.Via = Via(via)
		out = append(out, w)
	}
	return out, rows.Err()
}

// Whisper returns one whisper.
func (t *Tx) Whisper(id int64) (Whisper, bool, error) {
	ws, err := t.whispers(`WHERE id = ?`, id)
	if err != nil || len(ws) == 0 {
		return Whisper{}, false, err
	}
	return ws[0], true, nil
}

// WhispersTo returns the latest whispers to a person, oldest first.
func (t *Tx) WhispersTo(person int64, limit int) ([]Whisper, error) {
	ws, err := t.whispers(`WHERE to_person = ? ORDER BY id DESC LIMIT ?`, person, limit)
	for i, j := 0, len(ws)-1; i < j; i, j = i+1, j-1 {
		ws[i], ws[j] = ws[j], ws[i]
	}
	return ws, err
}

// UnsentRRCWhispers returns whispers to a person that haven't gone to an RRC
// link yet, oldest first.
func (t *Tx) UnsentRRCWhispers(person int64) ([]Whisper, error) {
	return t.whispers(`WHERE to_person = ? AND rrc_at = 0 ORDER BY id`, person)
}

// MarkWhisperRRC records that a whisper went to an RRC link.
func (t *Tx) MarkWhisperRRC(id, at int64) error {
	_, err := t.exec(`UPDATE whispers SET rrc_at = ? WHERE id = ? AND rrc_at = 0`, at, id)
	return err
}

// MarkWhispersRead records that a person has seen their whispers up to id on
// the page, and returns how many were new.
func (t *Tx) MarkWhispersRead(person, upTo, at int64) (int64, error) {
	res, err := t.exec(`UPDATE whispers SET read_at = ? WHERE to_person = ? AND id <= ? AND read_at = 0`, at, person, upTo)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UnreadWhispers counts a person's whispers not yet seen on the page.
func (t *Tx) UnreadWhispers(person int64) (int, error) {
	var n int
	err := t.queryRow(`SELECT count(*) FROM whispers WHERE to_person = ? AND read_at = 0`, person).Scan(&n)
	return n, err
}

// LastWhisperTo returns the most recent whisper to a person: /r replies to
// its sender.
func (t *Tx) LastWhisperTo(person int64) (Whisper, bool, error) {
	ws, err := t.whispers(`WHERE to_person = ? ORDER BY id DESC LIMIT 1`, person)
	if err != nil || len(ws) == 0 {
		return Whisper{}, false, err
	}
	return ws[0], true, nil
}

// PruneWhispers deletes whispers said before cutoff, with their deliveries.
func (t *Tx) PruneWhispers(cutoff int64) (int64, error) {
	res, err := t.exec(`DELETE FROM whispers WHERE said_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Ignore makes person ignore whispers from another person. Ignoring again
// keeps the original time.
func (t *Tx) Ignore(person, ignored, at int64) error {
	if person == ignored {
		return errors.New("a person can't ignore themselves")
	}
	_, err := t.exec(`INSERT INTO ignores (person, ignored, at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, person, ignored, at)
	if err != nil {
		return fmt.Errorf("ignore: %w", err)
	}
	return nil
}

// Unignore lifts an ignore and reports whether there was one.
func (t *Tx) Unignore(person, ignored int64) (bool, error) {
	res, err := t.exec(`DELETE FROM ignores WHERE person = ? AND ignored = ?`, person, ignored)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// IsIgnoring reports whether person ignores whispers from another person.
func (t *Tx) IsIgnoring(person, ignored int64) (bool, error) {
	var one int
	err := t.queryRow(`SELECT 1 FROM ignores WHERE person = ? AND ignored = ?`, person, ignored).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Ignored lists the people a person ignores, earliest first.
func (t *Tx) Ignored(person int64) ([]int64, error) {
	rows, err := t.query(`SELECT ignored FROM ignores WHERE person = ? ORDER BY at, ignored`, person)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var p int64
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
