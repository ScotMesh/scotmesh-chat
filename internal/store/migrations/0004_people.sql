-- 0004: people. A person is one or more identities that share a name, a hub
-- role and their preferences: an identity joins another's person with /link
-- (ADR 0007). Roles and moderation are ADR 0008; profiles, sharing and
-- whispers are ADR 0009. Everything a person owns hangs off people.id, so
-- linking or unlinking an identity never has to move a name or a role.

CREATE TABLE people (
    id            INTEGER PRIMARY KEY,
    created_at    INTEGER NOT NULL,
    -- hub role; owners are the config file's admins and are not stored
    role          TEXT NOT NULL DEFAULT '' CHECK (role IN ('', 'mod', 'admin')),
    role_by       BLOB CHECK (role_by IS NULL OR length(role_by) = 16),
    role_at       INTEGER NOT NULL DEFAULT 0,
    -- preferences, reachable from RRC, LXMF and the page
    share_lxmf    INTEGER NOT NULL DEFAULT 1 CHECK (share_lxmf IN (0, 1)),
    whispers      INTEGER NOT NULL DEFAULT 1 CHECK (whispers IN (0, 1)),
    whispers_lxmf INTEGER NOT NULL DEFAULT 1 CHECK (whispers_lxmf IN (0, 1)),
    page_lines    INTEGER NOT NULL DEFAULT 30 CHECK (page_lines IN (10, 20, 30, 50, 100)),
    page_flag     INTEGER NOT NULL DEFAULT 1 CHECK (page_flag IN (0, 1)),
    page_refresh  INTEGER NOT NULL DEFAULT 30 CHECK (page_refresh IN (0, 10, 30, 60))
) STRICT;

-- Every identity so far is a person of its own, numbered by its rowid.
ALTER TABLE identities ADD COLUMN person INTEGER REFERENCES people (id);
ALTER TABLE identities ADD COLUMN person_since INTEGER NOT NULL DEFAULT 0;
INSERT INTO people (id, created_at) SELECT rowid, first_seen FROM identities;
UPDATE identities SET person = rowid, person_since = first_seen;
CREATE INDEX identities_person ON identities (person, person_since);

-- ALTER TABLE can't add NOT NULL to a column; these triggers do its job.
CREATE TRIGGER identities_person_insert BEFORE INSERT ON identities
    WHEN NEW.person IS NULL BEGIN SELECT RAISE(ABORT, 'identities.person is required'); END;
CREATE TRIGGER identities_person_update BEFORE UPDATE OF person ON identities
    WHEN NEW.person IS NULL BEGIN SELECT RAISE(ABORT, 'identities.person is required'); END;

-- Names belong to people now, not identities.
CREATE TABLE names_by_person (
    person      INTEGER PRIMARY KEY REFERENCES people (id),
    name        TEXT NOT NULL,
    skeleton    TEXT NOT NULL UNIQUE,
    claimed_at  INTEGER NOT NULL
) STRICT;
INSERT INTO names_by_person (person, name, skeleton, claimed_at)
    SELECT i.person, n.name, n.skeleton, n.claimed_at FROM names n JOIN identities i ON i.id = n.identity;
DROP TABLE names;
ALTER TABLE names_by_person RENAME TO names;

-- One-time /link codes, stored as SHA-256 hashes. A code issued by someone
-- with a role waits, once redeemed, for approval from one of their apps.
CREATE TABLE link_codes (
    code_hash    BLOB PRIMARY KEY CHECK (length(code_hash) = 32),
    person       INTEGER NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    issued_by    BLOB NOT NULL CHECK (length(issued_by) = 16),
    issued_at    INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    redeemed_by  BLOB CHECK (redeemed_by IS NULL OR length(redeemed_by) = 16),
    redeemed_via TEXT CHECK (redeemed_via IS NULL OR redeemed_via IN ('rrc', 'lxmf', 'page'))
) STRICT;
CREATE INDEX link_codes_person ON link_codes (person);

-- Timed bans, and the name someone had when they were banned (for /bans).
ALTER TABLE bans ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0; -- 0: never
ALTER TABLE bans ADD COLUMN name TEXT NOT NULL DEFAULT '';

-- Whispers: kept 7 days, never in history, catch-up or the page conversation.
CREATE TABLE whispers (
    id          INTEGER PRIMARY KEY,
    from_id     BLOB NOT NULL CHECK (length(from_id) = 16),
    from_name   TEXT NOT NULL,
    to_person   INTEGER NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    to_name     TEXT NOT NULL,
    body        TEXT NOT NULL,
    via         TEXT NOT NULL CHECK (via IN ('rrc', 'lxmf', 'page')),
    said_at     INTEGER NOT NULL,
    rrc_at      INTEGER NOT NULL DEFAULT 0, -- when it went to one of their RRC links
    read_at     INTEGER NOT NULL DEFAULT 0  -- when they saw it on the page
) STRICT;
CREATE INDEX whispers_to ON whispers (to_person, id);
CREATE INDEX whispers_from ON whispers (from_id, id);
CREATE INDEX whispers_said ON whispers (said_at);

-- LXMF copies of whispers, on the same durable terms as group deliveries.
CREATE TABLE whisper_deliveries (
    whisper_id  INTEGER NOT NULL REFERENCES whispers (id) ON DELETE CASCADE,
    identity    BLOB NOT NULL CHECK (length(identity) = 16),
    state       TEXT NOT NULL CHECK (state IN ('queued', 'sent', 'propagated', 'proven', 'failed')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    parts_done  INTEGER NOT NULL DEFAULT 0 CHECK (parts_done >= 0),
    next_at     INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (whisper_id, identity)
) STRICT;
CREATE INDEX whisper_deliveries_due ON whisper_deliveries (state, next_at);

-- A person's recent messages, for their profile.
CREATE INDEX messages_author ON messages (author, id);

-- People someone doesn't want whispers from.
CREATE TABLE ignores (
    person      INTEGER NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    ignored     INTEGER NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    at          INTEGER NOT NULL,
    PRIMARY KEY (person, ignored),
    CHECK (person != ignored)
) STRICT;
