# The store

All hub state lives in one SQLite database, `data_dir/hub.db` (ADR 0004). The
schema is `internal/store/migrations/*.sql`, and `PRAGMA user_version` is the
schema version. A build refuses to open a database newer than itself.

- **Times** are milliseconds since the Unix epoch (UTC).
- **Identity hashes** are 16-byte BLOBs. In the `sqlite3` CLI they read
  better as `hex(id)`.
- **Writes** come from the hub core only, in one transaction per request
  (`BEGIN IMMEDIATE`). Adapters read on a separate read-only connection pool.
- **Durability** is WAL with `synchronous=FULL`. `TestKillDuringWrites` kills a
  writer with SIGKILL mid-stream and checks every reported commit survived.

| Table | Holds |
|---|---|
| `people` | A person is one or more identities linked with `/link` (ADR 0007). Each has a rank (`mod`, `admin`; owners come from the config) and preferences: sharing, whispers, and the chat page's length, flag and refresh. |
| `identities` | Every identity the hub has seen, its public key when known, its LXMF settings (`lxmf_mode` on, off or auto, and `lxmf_mine`), and the person it is part of, since when. |
| `names` | One claimed name per person. `skeleton` is the lookalike-folded form and is unique (ADR 0005). |
| `link_codes` | One-time `/link` codes, hashed, with expiry, and who redeemed a code waiting for approval. |
| `rooms` | Room topic, founder, registration, modes (`ikmnpt`) and key. |
| `room_roles` | `op`, `voice` and `ban` per room and identity. |
| `invites` | Invitations with expiry. |
| `messages` | Every MSG and ACTION once, in hub order, with the name it was said under, the author's and the hub's clocks, the way in, and RRC extension fields. `(via, origin_id)` de-duplicates. |
| `cursors` | The last message delivered to an identity per room and way in, for catch-up (ADR 0006). |
| `members` | The LXMF group, with `away_until` while direct delivery is failing. |
| `deliveries` | LXMF deliveries in flight. `queued` means not yet sent; `failed` is final. `parts_done` counts the parts of a split message that have arrived. |
| `bans` | Hub-wide bans, one row per identity, with the name they had and an expiry (0 means permanent). |
| `whispers` | Whispers to a person, kept 7 days, with when each went to RRC and was read on the page (ADR 0009). |
| `whisper_deliveries` | LXMF copies of whispers, on the same terms as `deliveries`. |
| `ignores` | People someone doesn't want whispers from. |
| `audit` | Administrative actions. |
| `meta` | Key/value settings, such as the import markers. |

## Handy queries

```sql
-- who holds which name, and with which identities
SELECT n.name, n.person, hex(i.id), datetime(n.claimed_at/1000, 'unixepoch')
  FROM names n JOIN identities i ON i.person = n.person ORDER BY n.name, i.person_since;
-- mods and admins
SELECT p.role, n.name FROM people p LEFT JOIN names n ON n.person = p.id WHERE p.role != '';
-- the last 20 messages in #scotmesh
SELECT id, datetime(hub_at/1000, 'unixepoch'), author_name, via, body FROM messages
 WHERE room = 'scotmesh' ORDER BY id DESC LIMIT 20;
-- LXMF deliveries that are stuck
SELECT message_id, hex(identity), state, attempts FROM deliveries WHERE state = 'queued';
```

## Backups

The hub writes `VACUUM INTO data_dir/backups/hub-YYYY-MM-DD.db` nightly and
keeps 7. A backup is an ordinary database: to restore, stop the service,
copy the backup over `hub.db`, remove `hub.db-wal` and `hub.db-shm`, and start
the service.
