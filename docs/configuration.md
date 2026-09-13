# Configuration reference

scotmesh-chat is configured from three layers, each overriding the one
before: the built-in defaults, an optional TOML file, and `SCOTMESH_CHAT_*`
environment variables. A container commonly skips the file entirely and
sets everything through its environment; `deploy/config.example.toml` is a
worked TOML example with commentary.

Every environment variable's name is its TOML path, uppercased with `_` in
place of `.`, prefixed `SCOTMESH_CHAT_`: `page.banner_file` is
`SCOTMESH_CHAT_PAGE_BANNER_FILE`. Both are as strict as each other: an
unknown TOML key or an unknown `SCOTMESH_CHAT_*` variable is a startup
error, so a typo can't silently leave a setting at its default.

## Finding the config file

`--config` names it; failing that, `SCOTMESH_CHAT_CONFIG`; failing that,
`/etc/scotmesh-chat/config.toml` if it happens to exist. A path named
explicitly (by the flag or the environment variable) **must** exist —
that's the point of naming one. Only the last, unnamed case is allowed to be
missing, which is what lets a container run from its environment alone.

Check a config (file, environment, or both) without starting the hub:

```
scotmesh-chat --config /path/to/config.toml --check-config
```

## Value types

- **String, integer, boolean:** as themselves — `SCOTMESH_CHAT_HUB_NAME=Chat`,
  `SCOTMESH_CHAT_GROUP_ENABLED=false`.
- **Duration:** a Go duration string — `SCOTMESH_CHAT_RRC_ANNOUNCE_INTERVAL=30m`.
- **List of strings** (`admins`, `rrc.greeting`): a JSON array
  (`["one","two"]`), or — since a JSON array is awkward on a command line —
  a comma-separated list (`one,two`) when the value doesn't start with `[`.
- **List of objects** (`hub.rooms`): a JSON array of objects, e.g.
  `[{"name":"lounge","topic":"General chat"}]`.

## Top level

| Key | Env var | Default | Meaning |
|---|---|---|---|
| `data_dir` | `SCOTMESH_CHAT_DATA_DIR` | `/var/lib/scotmesh-chat` | identities, the database, backups, `health.json` |
| `backbone` | `SCOTMESH_CHAT_BACKBONE` | *(required)* | `host:port` of a Reticulum TCP server |
| `admins` | `SCOTMESH_CHAT_ADMINS` | none | identity hashes (32 hex characters) of hub owners |
| `log_level` | `SCOTMESH_CHAT_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `log_format` | `SCOTMESH_CHAT_LOG_FORMAT` | `text` | `text` or `json` (a container's log collector usually wants `json`) |
| `timezone` | `SCOTMESH_CHAT_TIMEZONE` | `UTC` | IANA zone the nightly backup runs on (e.g. `Europe/London`) |

## `[hub]`

| Key | Env var | Default | Meaning |
|---|---|---|---|
| `name` | `SCOTMESH_CHAT_HUB_NAME` | `Chat` | shown in greetings and announces |
| `group_room` | `SCOTMESH_CHAT_HUB_GROUP_ROOM` | `chat` | the room the LXMF group and the page carry |
| `retention` | `SCOTMESH_CHAT_HUB_RETENTION` | `168h` (7 days) | how long messages are kept |
| `posts_per_minute` | `SCOTMESH_CHAT_HUB_POSTS_PER_MINUTE` | 30 | per identity, across every way in; 0 or unset means the built-in default |
| `max_body_bytes` | `SCOTMESH_CHAT_HUB_MAX_BODY_BYTES` | 2000 | longest stored message; 0 or unset means the built-in default |
| `replay_max` | `SCOTMESH_CHAT_HUB_REPLAY_MAX` | 50 | catch-up cap on reconnect; 0 or unset means the built-in default |
| `replay_first_visit` | `SCOTMESH_CHAT_HUB_REPLAY_FIRST_VISIT` | 10 | catch-up for someone never seen in the room; 0 or unset means the built-in default |
| `rooms` | `SCOTMESH_CHAT_HUB_ROOMS` | one room named `chat`, no topic | rooms that always exist and are never pruned: `[{"name":"...","topic":"..."}]` |

## `[group]` — the LXMF group

| Key | Env var | Default | Meaning |
|---|---|---|---|
| `enabled` | `SCOTMESH_CHAT_GROUP_ENABLED` | `true` | |
| `identity` | `SCOTMESH_CHAT_GROUP_IDENTITY` | `identities/group` | file, relative to `data_dir`; this is the group's address — back it up |
| `display_name` | `SCOTMESH_CHAT_GROUP_DISPLAY_NAME` | `Chat – send /join` | announced app data |
| `name` | `SCOTMESH_CHAT_GROUP_NAME` | `Chat` | how replies refer to the group |
| `propagation_node` | `SCOTMESH_CHAT_GROUP_PROPAGATION_NODE` | none | where messages wait for members who are away (32 hex characters) |
| `announce_interval` | `SCOTMESH_CHAT_GROUP_ANNOUNCE_INTERVAL` | `30m` | minimum `10m`: announces are costly on radio |
| `max_stamp_cost` | `SCOTMESH_CHAT_GROUP_MAX_STAMP_COST` | 16 | caps the proof-of-work a stranger's announce can demand |
| `join_digest` | `SCOTMESH_CHAT_GROUP_JOIN_DIGEST` | 0 (use the built-in default) | how much catch-up a `/join` reply includes |

## `[page]` — the NomadNet chat page

| Key | Env var | Default | Meaning |
|---|---|---|---|
| `enabled` | `SCOTMESH_CHAT_PAGE_ENABLED` | `true` | |
| `identity` | `SCOTMESH_CHAT_PAGE_IDENTITY` | `identities/page` | file, relative to `data_dir`; this is the page's address — back it up |
| `node_name` | `SCOTMESH_CHAT_PAGE_NODE_NAME` | `Chat` | announced as the NomadNet node's name |
| `title` | `SCOTMESH_CHAT_PAGE_TITLE` | `Chat` | shown at the top of the page |
| `wiki_url` | `SCOTMESH_CHAT_PAGE_WIKI_URL` | none | a NomadNet page about the chat, `hash:/page/x.mu`; omit to leave the link out |
| `announce_interval` | `SCOTMESH_CHAT_PAGE_ANNOUNCE_INTERVAL` | `30m` | minimum `10m` |
| `banner_file` | `SCOTMESH_CHAT_PAGE_BANNER_FILE` | none (the built-in Saltire) | a Micron file shown at the top of the page; see `docs/banner.md` |

## `[rrc]` — the RRC hub

| Key | Env var | Default | Meaning |
|---|---|---|---|
| `enabled` | `SCOTMESH_CHAT_RRC_ENABLED` | `true` | |
| `identity` | `SCOTMESH_CHAT_RRC_IDENTITY` | `identities/rrc-hub` | file, relative to `data_dir`; this is the hub's address — back it up |
| `greeting` | `SCOTMESH_CHAT_RRC_GREETING` | none | lines shown to a client on connect |
| `announce_interval` | `SCOTMESH_CHAT_RRC_ANNOUNCE_INTERVAL` | `30m` | minimum `10m` |
| `include_joined_member_list` | `SCOTMESH_CHAT_RRC_INCLUDE_JOINED_MEMBER_LIST` | `false` | rrcd compatibility: list members in the JOINED reply |
| `max_msg_body_bytes` | `SCOTMESH_CHAT_RRC_MAX_MSG_BODY_BYTES` | 0 (use the built-in default) | longest RRC message body accepted |
| `rate_per_minute` | `SCOTMESH_CHAT_RRC_RATE_PER_MINUTE` | 0 (use the built-in default) | frames per minute, per link |

## Example: a container from the environment alone

```
docker run -d \
  -e SCOTMESH_CHAT_BACKBONE=rns.example.net:4242 \
  -e SCOTMESH_CHAT_HUB_NAME="Highland Mesh" \
  -e SCOTMESH_CHAT_HUB_GROUP_ROOM=highland \
  -e SCOTMESH_CHAT_ADMINS=0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f \
  -e SCOTMESH_CHAT_LOG_FORMAT=json \
  -v scotmesh-chat-data:/data \
  ghcr.io/a13xb0/scotmesh-chat:latest
```

See `deploy/docker-compose.example.yml` for a fuller example, including a
custom banner file.
