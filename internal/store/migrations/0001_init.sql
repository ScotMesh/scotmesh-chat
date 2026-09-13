-- 0001: the hub's state. Times are milliseconds since the Unix epoch.
-- Identity hashes are 16-byte BLOBs. See docs/store.md.

CREATE TABLE identities (
    id          BLOB PRIMARY KEY CHECK (length(id) = 16),
    public_key  BLOB CHECK (public_key IS NULL OR length(public_key) = 64),
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    lxmf_mode   TEXT NOT NULL DEFAULT 'on' CHECK (lxmf_mode IN ('on', 'off', 'auto'))
) STRICT;

-- One name per identity; a skeleton (the lookalike-folded form) is unique.
CREATE TABLE names (
    identity    BLOB PRIMARY KEY REFERENCES identities (id),
    name        TEXT NOT NULL,
    skeleton    TEXT NOT NULL UNIQUE,
    claimed_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE rooms (
    name        TEXT PRIMARY KEY,
    topic       TEXT NOT NULL DEFAULT '',
    founder     BLOB CHECK (founder IS NULL OR length(founder) = 16),
    registered  INTEGER NOT NULL DEFAULT 0 CHECK (registered IN (0, 1)),
    modes       TEXT NOT NULL DEFAULT '',   -- letters from "ikmnpt"; +r is `registered`
    room_key    TEXT NOT NULL DEFAULT '',   -- +k
    created_at  INTEGER NOT NULL,
    last_used   INTEGER NOT NULL
) STRICT;

CREATE TABLE room_roles (
    room        TEXT NOT NULL REFERENCES rooms (name) ON DELETE CASCADE,
    identity    BLOB NOT NULL CHECK (length(identity) = 16),
    role        TEXT NOT NULL CHECK (role IN ('op', 'voice', 'ban')),
    set_by      BLOB,
    set_at      INTEGER NOT NULL,
    PRIMARY KEY (room, identity, role)
) STRICT;

CREATE TABLE invites (
    room        TEXT NOT NULL REFERENCES rooms (name) ON DELETE CASCADE,
    identity    BLOB NOT NULL CHECK (length(identity) = 16),
    expires_at  INTEGER NOT NULL,
    PRIMARY KEY (room, identity)
) STRICT;

-- Every message once, in hub order. author_name is the name it was said under.
CREATE TABLE messages (
    id          INTEGER PRIMARY KEY,
    room        TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('msg', 'action')),
    author      BLOB NOT NULL CHECK (length(author) = 16),
    author_name TEXT NOT NULL,
    body        TEXT NOT NULL,
    via         TEXT NOT NULL CHECK (via IN ('rrc', 'lxmf', 'page', 'import')),
    origin_id   BLOB,                       -- RRC K_ID, LXMF message hash
    said_at     INTEGER NOT NULL,           -- the author's clock
    hub_at      INTEGER NOT NULL,           -- ours
    ext         BLOB                        -- RRC extension fields, CBOR map
) STRICT;
CREATE UNIQUE INDEX messages_origin ON messages (via, origin_id) WHERE origin_id IS NOT NULL;
CREATE INDEX messages_room ON messages (room, id);
CREATE INDEX messages_hub_at ON messages (hub_at);

-- Last message delivered to an identity, per room and way in (ADR 0006).
CREATE TABLE cursors (
    identity    BLOB NOT NULL CHECK (length(identity) = 16),
    room        TEXT NOT NULL,
    via         TEXT NOT NULL CHECK (via IN ('rrc', 'lxmf', 'page')),
    message_id  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (identity, room, via)
) STRICT;

-- The LXMF group.
CREATE TABLE members (
    identity    BLOB PRIMARY KEY REFERENCES identities (id),
    joined_at   INTEGER NOT NULL,
    away_until  INTEGER NOT NULL DEFAULT 0
) STRICT;

-- LXMF deliveries not yet finished. 'queued' is "not yet", 'failed' is final.
CREATE TABLE deliveries (
    message_id  INTEGER NOT NULL,
    identity    BLOB NOT NULL CHECK (length(identity) = 16),
    state       TEXT NOT NULL CHECK (state IN ('queued', 'sent', 'propagated', 'proven', 'failed')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    next_at     INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (message_id, identity)
) STRICT;
CREATE INDEX deliveries_due ON deliveries (state, next_at);

-- Hub-wide bans (klines).
CREATE TABLE bans (
    identity    BLOB PRIMARY KEY CHECK (length(identity) = 16),
    reason      TEXT NOT NULL DEFAULT '',
    banned_by   BLOB,
    banned_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE audit (
    id          INTEGER PRIMARY KEY,
    at          INTEGER NOT NULL,
    actor       BLOB,
    action      TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE meta (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL
) STRICT;
