# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- `[page] banner_file` (or `SCOTMESH_CHAT_PAGE_BANNER_FILE`): a Micron file
  shown at the top of the chat page in place of the Saltire, which stays the
  built-in default. Pointing it at an empty file removes the banner
  entirely. See `docs/banner.md`.
- Every setting can now come from a `SCOTMESH_CHAT_*` environment variable
  instead of (or on top of) the TOML file, so the hub can run as a
  container configured purely through its environment. `docs/configuration.md`
  lists every variable. `[log_format]` (`text` or `json`) and `[timezone]`
  (the nightly backup's clock) are new settings that came out of this work.
- `scotmesh-chat healthcheck`: reads `health.json` and exits non-zero if it's
  stale or the backbone is disconnected. Meant as a container `HEALTHCHECK`.
  `health.json` gained `backbone_connected`, and the backbone's up/down
  transitions are now logged at info/warn (previously debug-only).
- `Dockerfile`, `.dockerignore` and `deploy/docker-compose.example.yml` for
  running the hub as a container.

### Changed
- A config file that's named explicitly (`--config`, or
  `SCOTMESH_CHAT_CONFIG`) but doesn't exist is now a startup error, instead
  of silently running on defaults. Those defaults are also no longer
  ScotMesh-specific (no backbone, generic hub name and room): only
  `deploy/config.example.toml` carries ScotMesh's own values now. A
  container with no config file at all, configured purely through the
  environment, is unaffected — see above.
- The nightly backup writes to a temp file and renames it into place, so an
  interrupted backup can't be mistaken for a complete one, and it no longer
  counts manual `hub-before-*.db` copies towards its retention count.

### Fixed
- RRC clients' room lists are no longer empty. The `/list` reply went out
  one NOTICE per line, but MeshChatX and NomadNet (before 1.4.3) only read
  the rooms when the header and the rooms arrive in one NOTICE, as rrcd
  sends it. It's now one NOTICE whenever it fits a packet.

## [1.0.0-rc.4] - 2026-09-13

People, moderation and one command set (ADR 0007, ADR 0008, ADR 0009).

### Added
- The ScotMesh chat hub (ADR 0001), replacing rrc-hub and bridge 0.1 with
  one service.
- `rrc/wire`: RRC v1 codec ported from rrcd, with golden frames and fuzzing.
- `internal/store`: SQLite state (identities, names, rooms, messages,
  catch-up cursors, LXMF deliveries, bans, audit).
- `internal/hub`: the chat core. Names are bound to identities, with lookalike
  checks. Catch-up covers what you missed since your last disconnect. LXMF
  delivery can be on, off or auto. rrcd's room model and one command table.
- `internal/rrcsrv`: the RRC hub server (rrc.hub) on the hub core.
- `internal/app` and a new `cmd/scotmesh-chat`: strict TOML config (unknown
  keys are an error), slog, staggered announces, persisted announce cache.
  The binary now runs the hub; bridge 0.1 is tag `v0.1.1`.
- `internal/lxmfgroup`: the LXMF group. `Name: message` delivery comes from a
  durable queue, goes direct first with the propagation node as fallback,
  `/join` uses your announced name, and resuming delivery tells you how much
  you missed.
- `internal/nomadpage`: the chat page with the Saltire header, a
  new-since-your-last-visit marker, posting for identified visitors, a
  name-and-settings page and a how-to-join page.
- `tests/e2e/run_all.sh`: rnsd, an lxmd propagation node and the hub on a
  private network. RRC (18), page (10) and LXMF group (26) checks with stock
  clients.

### Fixed
- LXMF messages from the hub now always fit one packet. MeshChatX's "Block
  Attachments from Strangers" setting (on by default) refused anything
  bigger, so `/help`, the join digest and long messages never reached
  MeshChatX users who hadn't added the group as a contact. Longer text goes in
  numbered parts, `(1/3)`. A retry carries on from the part that failed
  (migration 0003, `deliveries.parts_done`). The propagation fallback's own
  error is logged too.
- Bridge 0.1: errors from file writes, path requests and RRC sends are logged
  rather than dropped.

### Changed
- One command set on RRC, LXMF and the chat page: the same names, arguments
  and replies everywhere, with rooms written `#room` and the group `lxmf`,
  both defaulting to where you typed. A test runs one script on all three and
  requires identical replies.
- Commands work with `!` as well as `/`, so NomadNet's RRC client, which keeps
  `/` to itself, reaches the hub (`!help`). `!` followed by anything that
  isn't a command is still said.
- Commands typed into the chat page's Say box run, with a reply only the
  visitor sees.
- `/leave` leaves what you're in: the room over RRC, the group from LXMF and
  the page (`/leave lxmf` from anywhere). Freeing your name is `/forget`.
- Joining the LXMF group turns delivery on, your own RRC and page messages
  included.
- rrcd's room operator commands move under `/room` (`/room kick #den Rab`).
  The old forms still work and say the new one.
- `/link` shares one name between apps (ADR 0007): `/link` gives a
  one-time code, `/link CODE` on the other app links it, `/devices` lists
  them and `/unlink` takes one out. People with a role approve new apps from
  one they already have; nobody redeems upwards; an owner's own identity
  never joins anyone. The settings page has a "Your apps" section. With
  linked apps, LXMF on auto pauses while any of them is in the room, `/who`
  lists the person once, and `/forget` unlinks them all.
- `/profile Name` (`/whois`) shows someone's name, rank, whether they're
  here, and their LXMF address if they share it. `/share on|off` (and a
  toggle on the settings page) chooses; mods and up always share.
- Moderation, by rank (owner, admin, mod): `/kick` (out of every room and
  the group, name freed), `/ban Name [30m|1h|1d|7d|perm] [reason]` (every app,
  name freed; timed bans run out on the minute), `/unban`, `/bans`,
  `/rename`, `/delete Name [n]` (anyone for their own messages), `/modlog`,
  and `/mod`, `/admin`, `/demote`. Nobody acts on an equal or higher rank.
  `/kline` still works as rrcd's form of these.
- Whispers (ADR 0009): `/whisper Name text` (`/w`, `/msg`, `/tell`) and `/r`
  reach one person on RRC (a direct notice from the sender), in their LXMF
  app (even with group delivery off) and on the settings page, now or when
  they're next on; kept 7 days and never in history. `/whispers on|off`,
  `/whispers lxmf on|off`, `/ignore`, `/unignore` and `/ignored` control them,
  from any way in or the settings page.
- The chat page: names open a profile page (who they are, where they are,
  a "Message on LXMF" link, a whisper box, their recent messages, and for
  mods and up kick, ban, rename, remove and role buttons, each confirmed
  first). Buttons under the title choose how many messages to show, hide the
  saltire, and set auto-refresh (off, 10s, 30s, 60s); they're saved with your
  identity (`/page lines|flag|refresh` from anywhere) or kept in the links
  for visitors who haven't identified. The conversation refreshes as a
  NomadNet partial, and your own messages carry a remove link. Links to the
  wiki's chat page on the chat and help pages (`[page] wiki_url`).
- `tests/e2e`: a people suite (32 checks) runs profiles, whispers between
  RRC, LXMF and the page, promotion, rename, kick, timed ban, deleting and
  /link with approval through NomadNet's RRC client, Python LXMF apps and
  page requests over real links. The page suite checks the refreshing
  partial with its token. Totals: RRC 23, page 18, group 34, people 32.
- `scotmesh-chat check-db COPY` migrates a copy of the database and prints
  its counts, for trying an upgrade first; `scotmesh-chat help-wiki` prints
  the wiki's Chat commands page from the command table.
- `/help` comes in pages that each fit one LXMF packet: four for members
  (chatting, you, whispers, people and rooms), more for mods, admins and
  owners. `/help <command>` explains one command.
- `third_party/reticulum-go`: v0.7.2, with per-destination link hooks,
  `Transport.SendOnLink` and a closed callback for evicted links.
- CI: lint (golangci-lint v2), race tests, coverage gates, fuzzing,
  govulncheck, reproducible builds. `make check` runs the same locally.
- `cmd/scotmesh-chat` exits through `run()` so deferred cleanup runs.

### Removed
- Bridge 0.1 (`internal/chatlog`, `core`, `group`, `node`, `page`, `rrc`,
  `text`, the Python reference and its tests). The hub replaces it; `v0.1.1`
  remains tagged for rollback.

## [0.1.1] - 2026-09-12

### Fixed
- The RRC hub itself is no longer listed among the people in the room.

## [0.1.0] - 2026-09-12

### Added
- Bridge between the #scotmesh RRC room, an LXMF group and a NomadNet chat
  page, with offline delivery through the propagation node.

[Unreleased]: https://github.com/ScotMesh/scotmesh-chat/compare/v1.0.0-rc.4...HEAD
[1.0.0-rc.4]: https://github.com/ScotMesh/scotmesh-chat/compare/v0.1.1...v1.0.0-rc.4
[0.1.1]: https://github.com/ScotMesh/scotmesh-chat/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/ScotMesh/scotmesh-chat/releases/tag/v0.1.0
