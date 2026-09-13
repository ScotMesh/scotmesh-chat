![ScotMesh Reticulum](https://raw.githubusercontent.com/ScotMesh/branding/main/networks/reticulum/readme-header.png)

# scotmesh-chat

ScotMesh's chat hub. It's one conversation with three ways in, and everyone
sees the same messages:

- **RRC room** `#scotmesh`: the hub *is* the RRC hub (`rrc.hub`), for
  MeshChatX, NomadNet, rrc-tui and WeeChat.
- **LXMF group**: send `/join` to *ScotMesh Chat* from Sideband, MeshChatX,
  NomadNet or Columba. Messages arrive as `Name: message`.
- **NomadNet chat page**: read it in NomadNet or MeshChatX, and post once you
  identify.

It's a single static Go binary on [reticulum-go](https://github.com/thatSFguy/reticulum-go)
that connects to a Reticulum backbone over TCP. No rnsd is needed on the
machine.

> **Status:** running in production on ScotMesh's own network. The bridge
> it replaces is tag `v0.1.1`. Contributors: read `CONTRIBUTING.md`;
> `make check` is the gate.

## Quick start

**Docker** is the fastest way to run it — one image, configured entirely
through `SCOTMESH_CHAT_*` environment variables, no config file needed:

```
docker run -d --name scotmesh-chat \
  -e SCOTMESH_CHAT_BACKBONE=rns.example.net:4242 \
  -e SCOTMESH_CHAT_HUB_NAME="Highland Mesh" \
  -e SCOTMESH_CHAT_ADMINS=0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f \
  -v scotmesh-chat-data:/data \
  ghcr.io/scotmesh/scotmesh-chat:latest
```

`SCOTMESH_CHAT_BACKBONE` is the only setting you must supply: `host:port` of
a Reticulum TCP server to connect to. The named volume holds the hub's
identities and database — back it up, since the identities are the hub's
addresses. For a fuller example (custom banner, more settings, no ports
published) copy `deploy/docker-compose.example.yml` to `docker-compose.yml`
and run `docker compose up -d`. Every TOML setting has an env var
equivalent; see `docs/configuration.md` for the full reference.

**Without Docker**, install the binary as a systemd service instead:

```
sudo ./deploy/install.sh    # from a folder with the binary, the unit and config.example.toml
scotmesh-chat --config /etc/scotmesh-chat/config.toml --check-config
```

Either way, identities live under `data_dir` (`identities/rrc-hub`,
`identities/group`, `identities/page`) — they're the hub's addresses, so
keep them backed up.

## What people get

- **One name per identity, everywhere.** Your Reticulum identity claims a
  name. It is the same on RRC, the group and the page, nobody else can take it
  or a lookalike of it (`A1ex` counts as `Alex`), and `/forget` frees it
  (ADR 0005, ADR 0008).
- **Catch-up since you left.** Reconnect to the RRC room and you get exactly
  what was said since you disconnected, with the original times. The page
  marks what's new since your last visit (ADR 0006).
- **Your LXMF settings.** `/lxmf off` stops group messages and keeps your
  name. `/lxmf auto` pauses them while you're in the RRC room with the same
  identity. When delivery resumes you get one "N messages missed" note.
- **No bot.** People reached through LXMF or the page appear in RRC under
  their own identity and name.
- **Reliable group delivery.** Each recipient's delivery is queued in the same
  transaction as the message. Members who don't answer directly get the
  message through the propagation node.
- **rrcd's room model.** Topics, modes (`+i +k +m +n +p +t`), ops, voice,
  bans, invites and registered rooms, with rrcd's reply texts so clients parse
  them.

## Layout

| Path | What |
|---|---|
| `cmd/scotmesh-chat` | the binary |
| `internal/app` | config (TOML, strict) and wiring |
| `internal/hub` | the chat core: names, rooms, catch-up, settings, commands (single writer) |
| `internal/store` | SQLite state (see `docs/store.md`) |
| `internal/rrcsrv` | the RRC hub server |
| `internal/lxmfgroup` | the LXMF group |
| `internal/nomadpage` | the NomadNet page |
| `rrc/wire` | the RRC v1 wire format (usable by clients) |
| `third_party/reticulum-go` | reticulum-go v0.7.2 with ScotMesh patches |
| `docs/adr` | decisions |
| `deploy` | systemd unit, example config, install script |
| `tests/e2e` | end-to-end tests on a private Reticulum network |

## Build and test

```
make check          # vet, lint, race tests, coverage gates, reticulum-go suite, govulncheck
make build          # bin/scotmesh-chat (linux/amd64, reproducible) and its sha256
PYTHON=/path/to/venv/bin/python tests/e2e/run_all.sh
```

The end-to-end runner starts rnsd and an lxmd propagation node on
127.0.0.1, then the hub. It drives stock clients: NomadNet's RRC client,
Python LXMF and page requests over real links. The Python needs `rns`, `lxmf`
and `nomadnet`.

## Licence

MIT. Parts are ported from [rrcd](https://github.com/kc1awv/rrcd) (MIT,
S. Miller KC1AWV); see `NOTICE`.
