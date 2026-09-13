// Package importer brings the state of the services the hub replaces into
// its store, once, at cutover: rrc-hub's nick registry, rooms, klines and
// history, and bridge 0.1's LXMF group members and the messages it carried
// from LXMF and the page.
//
// It runs as its own command while the hub is stopped, in one transaction,
// and can be run again safely: identities and names merge, messages are
// de-duplicated by origin.
package importer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fxamacker/cbor/v2"

	"github.com/ScotMesh/scotmesh-chat/rrc/wire"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// Sources are the directories to import from. Either may be empty.
type Sources struct {
	RRCHub string // rrc-hub's data directory: peers.toml, rooms.toml, klines.txt, history/
	Bridge string // bridge 0.1's data directory: log/state.json, log/messages.jsonl
	// Skip are identities whose names and messages are not imported: the
	// bridge's own RRC bot, whose room posts were copies of LXMF and page
	// messages imported from the bridge's log instead.
	Skip [][]byte
}

// Report says what was imported and what was not, and why.
type Report struct {
	Identities int
	Names      int
	Members    int
	Rooms      int
	Bans       int
	Messages   int
	Duplicates int
	Old        int // past retention
	Notes      []string
}

func (r *Report) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// String is the report for people.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "identities %d, names %d, group members %d, rooms %d, bans %d\n", r.Identities, r.Names, r.Members, r.Rooms, r.Bans)
	fmt.Fprintf(&b, "messages %d imported, %d already there, %d past retention\n", r.Messages, r.Duplicates, r.Old)
	for _, n := range r.Notes {
		b.WriteString("  - " + n + "\n")
	}
	return b.String()
}

// Import reads the sources and writes them to st in one transaction. With
// dryRun the transaction is rolled back and only the report is produced.
func Import(ctx context.Context, st *store.Store, src Sources, now time.Time, retention time.Duration, dryRun bool) (*Report, error) {
	rep := &Report{}
	var data sourceData
	if src.RRCHub != "" {
		if err := data.readRRCHub(src.RRCHub, rep); err != nil {
			return nil, err
		}
	}
	if src.Bridge != "" {
		if err := data.readBridge(src.Bridge, rep); err != nil {
			return nil, err
		}
	}
	skip := map[string]bool{}
	for _, s := range src.Skip {
		skip[hex.EncodeToString(s)] = true
	}

	errDryRun := errors.New("dry run")
	err := st.Update(ctx, func(tx *store.Tx) error {
		if err := data.write(tx, rep, skip, now, retention); err != nil {
			return err
		}
		if dryRun {
			return errDryRun
		}
		return tx.Audit(now.UnixMilli(), nil, "import", "", strings.TrimSpace(rep.String()))
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return rep, err
	}
	return rep, nil
}

type peer struct {
	id        []byte
	publicKey []byte
	nick      string
	lastSeen  int64
}

type member struct {
	id       []byte
	nick     string
	joinedAt int64
	lastSeen int64
	paused   bool
}

type roomData struct {
	name        string
	founder     []byte
	topic       string
	modes       string
	ops, voiced [][]byte
	bans        [][]byte
	lastUsed    int64
	registered  bool
}

type message struct {
	room   string
	action bool
	author []byte
	name   string
	body   string
	origin string
	saidAt int64
	hubAt  int64
}

type sourceData struct {
	peers    []peer
	members  []member
	rooms    []roomData
	bans     [][]byte
	messages []message
}

// --- rrc-hub ------------------------------------------------------------------

func (d *sourceData) readRRCHub(dir string, rep *Report) error {
	var peers struct {
		Peers map[string]struct {
			PublicKey  string  `toml:"public_key"`
			Nick       string  `toml:"nick"`
			LastSeenTS float64 `toml:"last_seen_ts"`
		} `toml:"peers"`
	}
	if _, err := toml.DecodeFile(filepath.Join(dir, "peers.toml"), &peers); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rrc-hub peers.toml: %w", err)
	}
	for k, p := range peers.Peers {
		id, err := hex.DecodeString(k)
		if err != nil || len(id) != store.IdentityLen {
			rep.note("rrc-hub peer %q skipped: not an identity hash", k)
			continue
		}
		pk, err := hex.DecodeString(p.PublicKey)
		if err != nil || len(pk) != 64 {
			pk = nil
		}
		d.peers = append(d.peers, peer{id: id, publicKey: pk, nick: p.Nick, lastSeen: int64(p.LastSeenTS * 1000)})
	}

	var rooms struct {
		Rooms map[string]struct {
			Founder       string   `toml:"founder"`
			Topic         string   `toml:"topic"`
			Moderated     bool     `toml:"moderated"`
			InviteOnly    bool     `toml:"invite_only"`
			TopicOpsOnly  bool     `toml:"topic_ops_only"`
			NoOutsideMsgs bool     `toml:"no_outside_msgs"`
			Private       bool     `toml:"private"`
			Operators     []string `toml:"operators"`
			Voiced        []string `toml:"voiced"`
			Bans          []string `toml:"bans"`
			LastUsedTS    float64  `toml:"last_used_ts"`
		} `toml:"rooms"`
	}
	if _, err := toml.DecodeFile(filepath.Join(dir, "rooms.toml"), &rooms); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rrc-hub rooms.toml: %w", err)
	}
	for name, r := range rooms.Rooms {
		rd := roomData{name: strings.ToLower(strings.TrimSpace(name)), topic: r.Topic, lastUsed: int64(r.LastUsedTS * 1000), registered: true}
		rd.founder = hashOrNil(r.Founder)
		for flag, on := range map[byte]bool{'m': r.Moderated, 'i': r.InviteOnly, 't': r.TopicOpsOnly, 'n': r.NoOutsideMsgs, 'p': r.Private} {
			if on {
				rd.modes += string(flag)
			}
		}
		rd.ops, rd.voiced, rd.bans = hashes(r.Operators, rep, name), hashes(r.Voiced, rep, name), hashes(r.Bans, rep, name)
		d.rooms = append(d.rooms, rd)
	}

	var klines struct {
		Banned []string `toml:"banned_identities"`
	}
	if _, err := toml.DecodeFile(filepath.Join(dir, "klines.txt"), &klines); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rrc-hub klines.txt: %w", err)
	}
	d.bans = append(d.bans, hashes(klines.Banned, rep, "klines")...)

	logs, err := filepath.Glob(filepath.Join(dir, "history", "*", "*.log"))
	if err != nil {
		return err
	}
	sort.Strings(logs)
	for _, f := range logs {
		roomBytes, err := hex.DecodeString(filepath.Base(filepath.Dir(f)))
		if err != nil {
			rep.note("history %s skipped: room directory is not hex", f)
			continue
		}
		if err := d.readHistory(f, string(roomBytes), rep); err != nil {
			return err
		}
	}
	return nil
}

// readHistory reads one rrc-hub history file: records of a 4-byte big-endian
// length and a CBOR map {1: hub ms, 2: id, 3: src, 4: nick, 5: type, 6: body,
// 7: client ms}. A torn record at the end (a crash mid-write) is reported
// and ignored.
func (d *sourceData) readHistory(path, room string, rep *Report) error {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied import path
	if err != nil {
		return fmt.Errorf("history %s: %w", path, err)
	}
	r := bytes.NewReader(raw)
	for {
		var n uint32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			if !errors.Is(err, io.EOF) {
				rep.note("history %s: torn length at the end, ignored", filepath.Base(path))
			}
			return nil
		}
		if n > 1<<20 || int64(n) > int64(r.Len()) {
			rep.note("history %s: torn or oversized record at the end, ignored", filepath.Base(path))
			return nil
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			// Unreachable: the length was checked against what is left.
			rep.note("history %s: short read, rest ignored: %v", filepath.Base(path), err)
			return nil //nolint:nilerr // a damaged tail is reported, not fatal
		}
		var rec map[uint64]any
		if err := cbor.Unmarshal(buf, &rec); err != nil {
			rep.note("history %s: unreadable record skipped", filepath.Base(path))
			continue
		}
		typ, _ := rec[5].(uint64)
		body, _ := rec[6].(string)
		src, _ := rec[3].([]byte)
		id, _ := rec[2].([]byte)
		if (typ != 20 && typ != 22) || body == "" || len(src) != store.IdentityLen {
			continue
		}
		nick, _ := rec[4].(string)
		hubAt, _ := rec[1].(uint64)
		saidAt, _ := rec[7].(uint64)
		if saidAt == 0 {
			saidAt = hubAt
		}
		d.messages = append(d.messages, message{room: room, action: typ == 22, author: src, name: nick, body: body,
			origin: "rrc-hub:" + hex.EncodeToString(id), saidAt: int64(saidAt), hubAt: int64(hubAt)}) //nolint:gosec // ms timestamps
	}
}

// --- bridge 0.1 ---------------------------------------------------------------

func (d *sourceData) readBridge(dir string, rep *Report) error {
	var state struct {
		Members map[string]struct {
			Identity string `json:"identity"`
			Nick     string `json:"nick"`
			Paused   bool   `json:"paused"`
			JoinedAt int64  `json:"joined_at"`
			LastSeen int64  `json:"last_seen"`
		} `json:"members"`
		Bans map[string]json.RawMessage `json:"bans"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "log", "state.json")) //nolint:gosec // operator-supplied import path
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bridge state.json: %w", err)
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &state); err != nil {
			return fmt.Errorf("bridge state.json: %w", err)
		}
	}
	for addr, m := range state.Members {
		id := hashOrNil(m.Identity)
		if id == nil {
			rep.note("bridge member %s skipped: its identity was never learned", addr)
			continue
		}
		d.members = append(d.members, member{id: id, nick: m.Nick, joinedAt: m.JoinedAt, lastSeen: m.LastSeen, paused: m.Paused})
	}
	for k := range state.Bans {
		if id := hashOrNil(k); id != nil {
			d.bans = append(d.bans, id)
		}
	}

	f, err := os.Open(filepath.Join(dir, "log", "messages.jsonl")) //nolint:gosec // operator-supplied import path
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bridge messages.jsonl: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var m struct {
			SaidAt         int64  `json:"said_at"`
			ReceivedAt     int64  `json:"received_at"`
			Source         string `json:"source"`
			OriginID       string `json:"origin_id"`
			AuthorIdentity string `json:"author_identity"`
			AuthorName     string `json:"author_name"`
			Body           string `json:"body"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			rep.note("bridge messages.jsonl: unreadable line skipped")
			continue
		}
		// Messages from RRC are in rrc-hub's own history.
		if m.Source == "rrc" || m.Body == "" {
			continue
		}
		id := hashOrNil(m.AuthorIdentity)
		if id == nil {
			rep.note("bridge message from %q skipped: no identity", m.AuthorName)
			continue
		}
		action := strings.HasPrefix(m.Body, "* ")
		body := m.Body
		if action {
			body = strings.TrimPrefix(body, "* ")
		}
		d.messages = append(d.messages, message{room: "", action: action, author: id, name: m.AuthorName, body: body,
			origin: "bridge:" + m.Source + ":" + m.OriginID, saidAt: m.SaidAt, hubAt: m.ReceivedAt})
	}
	return sc.Err()
}

func hashOrNil(s string) []byte {
	b, err := hex.DecodeString(strings.TrimSpace(strings.TrimPrefix(s, "0x")))
	if err != nil || len(b) != store.IdentityLen {
		return nil
	}
	return b
}

func hashes(list []string, rep *Report, where string) [][]byte {
	var out [][]byte
	for _, s := range list {
		if b := hashOrNil(s); b != nil {
			out = append(out, b)
		} else {
			rep.note("%s: %q is not an identity hash; skipped", where, s)
		}
	}
	return out
}

// --- writing ------------------------------------------------------------------

func (d *sourceData) write(tx *store.Tx, rep *Report, skip map[string]bool, now time.Time, retention time.Duration) error {
	nowMS := now.UnixMilli()
	touch := func(id, pk []byte, seen int64) error {
		if seen <= 0 {
			seen = nowMS
		}
		if _, ok, err := tx.Identity(id); err != nil {
			return err
		} else if !ok {
			rep.Identities++
		}
		return tx.TouchIdentity(id, pk, seen)
	}

	// Names: rrc-hub's registry first, then bridge nicks for identities it
	// doesn't know. Where two identities want lookalike names, the one seen
	// most recently keeps it.
	type claim struct {
		id       []byte
		name     string
		lastSeen int64
		source   string
	}
	var claims []claim
	for _, p := range d.peers {
		if skip[hex.EncodeToString(p.id)] {
			continue
		}
		if err := touch(p.id, p.publicKey, p.lastSeen); err != nil {
			return err
		}
		if p.nick != "" {
			claims = append(claims, claim{p.id, p.nick, p.lastSeen, "rrc-hub"})
		}
	}
	for _, m := range d.members {
		if err := touch(m.id, nil, m.lastSeen); err != nil {
			return err
		}
		if m.nick != "" {
			claims = append(claims, claim{m.id, m.nick, m.lastSeen, "bridge"})
		}
	}
	sort.SliceStable(claims, func(i, j int) bool { return claims[i].lastSeen > claims[j].lastSeen })
	named := map[string]bool{}
	for _, c := range claims {
		k := hex.EncodeToString(c.id)
		if named[k] {
			continue
		}
		clean, sk, err := hub.CleanName(strings.Join(strings.Fields(c.name), "_"))
		if err != nil {
			suggested := hub.SuggestName(c.name)
			if suggested == "" {
				rep.note("%s name %q for %s not imported: %v", c.source, c.name, k[:8], err)
				continue
			}
			rep.note("%s name %q for %s imported as %q", c.source, c.name, k[:8], suggested)
			clean, sk, _ = hub.CleanName(suggested)
		}
		if holder, ok, err := tx.NameBySkeleton(sk); err != nil {
			return err
		} else if ok && !bytes.Equal(holder.Identity, c.id) {
			rep.note("%s name %q for %s not imported: %s already holds %q", c.source, clean, k[:8], hex.EncodeToString(holder.Identity)[:8], holder.Name)
			continue
		}
		if err := tx.ClaimName(c.id, clean, sk, nowMS); err != nil {
			return err
		}
		named[k] = true
		rep.Names++
	}

	for _, m := range d.members {
		added, err := tx.AddMember(m.id, max(m.joinedAt, 1))
		if err != nil {
			return err
		}
		if added {
			rep.Members++
		}
		if m.paused {
			if err := tx.SetLXMFMode(m.id, store.LXMFOff); err != nil {
				return err
			}
		}
	}

	for _, r := range d.rooms {
		existing, ok, err := tx.Room(r.name)
		if err != nil {
			return err
		}
		room := store.Room{Name: r.name, Topic: r.topic, Founder: r.founder, Registered: true, CreatedAt: nowMS, LastUsed: max(r.lastUsed, 1)}
		if ok {
			room.CreatedAt = existing.CreatedAt
		}
		for i := 0; i < len(r.modes); i++ {
			room.SetMode(r.modes[i], true)
		}
		if r.founder != nil {
			if err := touch(r.founder, nil, 0); err != nil {
				return err
			}
		}
		if err := tx.PutRoom(&room); err != nil {
			return err
		}
		for _, set := range []struct {
			ids  [][]byte
			role store.Role
		}{{r.ops, store.RoleOp}, {r.voiced, store.RoleVoice}, {r.bans, store.RoleBan}} {
			for _, id := range set.ids {
				if err := touch(id, nil, 0); err != nil {
					return err
				}
				if err := tx.SetRole(r.name, id, set.role, true, nil, nowMS); err != nil {
					return err
				}
			}
		}
		rep.Rooms++
	}

	for _, id := range d.bans {
		if err := tx.AddBan(&store.Ban{Identity: id, Reason: "imported", BannedAt: nowMS}); err != nil {
			return err
		}
		rep.Bans++
	}

	// Messages in the order they happened, so hub IDs follow time.
	sort.SliceStable(d.messages, func(i, j int) bool { return d.messages[i].hubAt < d.messages[j].hubAt })
	cutoff := now.Add(-retention).UnixMilli()
	lastByRoom := map[string]int64{}
	for _, m := range d.messages {
		if skip[hex.EncodeToString(m.author)] {
			continue
		}
		if m.hubAt < cutoff {
			rep.Old++
			continue
		}
		room := m.room
		if room == "" {
			room = "scotmesh" // bridge 0.1 carried only #scotmesh
		}
		if r, err := wire.NormalizeRoom(room, 64); err == nil {
			room = r
		} else {
			rep.note("messages in room %q not imported: %v", room, err)
			continue
		}
		if err := touch(m.author, nil, m.hubAt); err != nil {
			return err
		}
		name := m.name
		if name == "" {
			name = hub.GuestName(m.author)
		}
		kind := store.KindMsg
		if m.action {
			kind = store.KindAction
		}
		sm := store.Message{Room: room, Kind: kind, Author: m.author, AuthorName: name, Body: m.body, Via: store.ViaImport,
			OriginID: []byte(m.origin), SaidAt: m.saidAt, HubAt: m.hubAt}
		msgID, dup, err := tx.AddMessage(&sm)
		if err != nil {
			return err
		}
		if dup {
			rep.Duplicates++
		} else {
			rep.Messages++
		}
		lastByRoom[room] = max(lastByRoom[room], msgID)
	}

	// People who used rrc-hub have seen its history in their clients: start
	// their catch-up after it, so nobody gets the whole week replayed.
	for _, p := range d.peers {
		if skip[hex.EncodeToString(p.id)] {
			continue
		}
		for room, last := range lastByRoom {
			if err := tx.SetCursor(p.id, room, store.ViaRRC, last, nowMS); err != nil {
				return err
			}
		}
	}
	return nil
}
