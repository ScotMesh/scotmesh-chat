// Package store keeps all of the hub's state in one SQLite database.
//
// The hub core is the only writer (ADR 0003): it runs each request in one
// Update transaction. Adapters may read concurrently through View, which
// sees a consistent snapshot thanks to WAL mode. Durability is
// synchronous=FULL, so a committed claim, ban or cursor survives power loss
// (ADR 0004).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// IdentityLen is the length of a Reticulum identity hash in bytes.
const IdentityLen = 16

// Store is the database. It is safe for concurrent use; see the package
// comment for who may write.
type Store struct {
	w    *sql.DB // one connection: writes are serialised
	r    *sql.DB // a small pool for reads
	path string
}

// Open opens or creates the database at path and brings its schema up to
// date.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := ensureDir(path); err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	w.SetMaxOpenConns(1)
	s := &Store{w: w, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open %s for reading: %w", path, err)
	}
	r.SetMaxOpenConns(4)
	s.r = r
	return s, nil
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(FULL)"} {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		// Take the write lock at BEGIN, so a transaction never fails to
		// upgrade from read to write halfway through.
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

// Close closes the database.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if _, err := s.w.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, s.w.Close())
	return errors.Join(errs...)
}

// Update runs fn in a write transaction. It commits if fn returns nil and
// rolls back otherwise.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	return runTx(ctx, s.w, fn)
}

// View runs fn in a read-only transaction on a consistent snapshot.
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	return runTx(ctx, s.r, fn)
}

func runTx(ctx context.Context, db *sql.DB, fn func(*Tx) error) (err error) {
	sqlTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
		if err != nil {
			err = errors.Join(err, ignoreDone(sqlTx.Rollback()))
			return
		}
		if cerr := sqlTx.Commit(); cerr != nil {
			err = fmt.Errorf("commit: %w", cerr)
		}
	}()
	return fn(&Tx{tx: sqlTx, ctx: ctx})
}

func ignoreDone(err error) error {
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

// Backup writes a consistent copy of the database to path, which must not
// exist.
func (s *Store) Backup(ctx context.Context, path string) error {
	if err := ensureDir(path); err != nil {
		return err
	}
	if _, err := s.w.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("backup to %s: %w", path, err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error { return s.migrateTo(ctx, 0) }

// migrateTo brings the schema up to version target, or the latest if target
// is 0. Tests use it to build a database as an older release left it.
func (s *Store) migrateTo(ctx context.Context, target int) error {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	type step struct {
		version int
		name    string
	}
	var steps []step
	for _, e := range entries {
		n, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(n)
		if !ok || err != nil || v < 1 {
			return fmt.Errorf("migration %s: name must start with a positive number and '_'", e.Name())
		}
		steps = append(steps, step{v, e.Name()})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].version < steps[j].version })
	for i, st := range steps {
		if st.version != i+1 {
			return fmt.Errorf("migrations are not numbered 1..n: %s", st.name)
		}
	}

	var current int
	if err := s.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(steps) {
		return fmt.Errorf("database schema version %d is newer than this build (%d); refusing to run", current, len(steps))
	}
	if target <= 0 || target > len(steps) {
		target = len(steps)
	}
	for _, st := range steps[current:max(current, target)] {
		body, err := migrations.ReadFile("migrations/" + st.name)
		if err != nil {
			return err
		}
		err = runTx(ctx, s.w, func(tx *Tx) error {
			if _, err := tx.tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", st.version))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", st.name, err)
		}
	}
	return nil
}

// SchemaVersion returns the database's schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.r.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

func ensureDir(path string) error {
	dir := filepath.Dir(path)
	if err := mkdirAll(dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

// Tx is one transaction. Its methods must not be used after the function
// passed to Update or View returns.
type Tx struct {
	tx  *sql.Tx
	ctx context.Context
}

func (t *Tx) exec(q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(t.ctx, q, args...)
}

func (t *Tx) queryRow(q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(t.ctx, q, args...)
}

func (t *Tx) query(q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(t.ctx, q, args...)
}

// Millis converts a time to the store's representation.
func Millis(t time.Time) int64 { return t.UnixMilli() }

func checkID(id []byte) error {
	if len(id) != IdentityLen {
		return fmt.Errorf("identity hash must be %d bytes, got %d", IdentityLen, len(id))
	}
	return nil
}

// Summary counts the rows that matter for an upgrade's before-and-after
// check, with the schema version under "schema_version".
func (s *Store) Summary(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	out["schema_version"] = int64(v)
	tables := []string{"identities", "names", "messages", "members", "deliveries", "bans", "rooms", "room_roles", "audit"}
	if v >= 4 {
		tables = append(tables, "people", "link_codes", "whispers", "whisper_deliveries", "ignores")
	}
	for _, table := range tables {
		var n int64
		if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil { //nolint:gosec // table names are fixed above
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		out[table] = n
	}
	return out, nil
}
