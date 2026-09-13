# 0004 — SQLite (pure Go) for all state

- Status: Accepted, 2026-09-12

## Context

Bridge 0.1 keeps a JSON-lines log and a state.json. rrcd rewrites TOML
non-atomically, and rrc-hub had torn records after crashes. The hub needs
relational queries: messages since a cursor, names by skeleton, members of a
room, deliveries not yet proven.

## Decision

Use `modernc.org/sqlite` (pure Go, no cgo) in WAL mode with `synchronous=FULL`,
plus schema migrations embedded in the binary. Each hub request is one
transaction. Retention deletes messages older than the configured age. Nightly,
`VACUUM INTO` writes an online backup next to the database. Importers bring in
rrc-hub's history and nick registry, and bridge 0.1's log and members.

## Consequences

- Claims, bans and cursors survive `kill -9` and power loss (tested).
- The binary grows by a few MB. The memory target (under 40 MB RSS) is measured
  in the soak.
- Operators can inspect state with the `sqlite3` CLI; the schema is
  documented in `docs/store.md`.
