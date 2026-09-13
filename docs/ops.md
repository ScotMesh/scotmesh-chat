# Running scotmesh-chat

This is a generic guide for operating the hub. It doesn't cover any one
deployment's server names, addresses or credentials — keep those in your
own private runbook.

## Files

| Path | What |
|---|---|
| `/opt/scotmesh-chat/scotmesh-chat` | the binary; `.previous` is the one before the last install |
| `/etc/scotmesh-chat/config.toml` | config (`deploy/config.example.toml` explains each key; `docs/configuration.md` lists every key and its `SCOTMESH_CHAT_*` environment variable) |
| `$data_dir/hub.db` | all state (see `docs/store.md`) |
| `$data_dir/identities/` | `rrc-hub`, `group`, `page`: **these are the addresses**; back them up offline |
| `$data_dir/backups/` | nightly `hub-YYYY-MM-DD.db`, 7 kept |
| `$data_dir/health.json` | counters, addresses and `backbone_connected`, rewritten every 30 s |
| `$data_dir/announces.json` | the transport's announce cache |

`$data_dir` is `data_dir` in the config (default `/var/lib/scotmesh-chat`), or
`SCOTMESH_CHAT_DATA_DIR` in a container.

## Everyday checks

```
systemctl status scotmesh-chat
journalctl -u scotmesh-chat --since today | grep -E 'level=(WARN|ERROR)'
jq . "$data_dir"/health.json
scotmesh-chat healthcheck            # exit code only: for a container HEALTHCHECK or a script
```

In `health.json`, watch for:
- `backbone_connected`: `false` for more than a few minutes means the
  backbone link is down.
- `hub.internal_errors`: anything above 0 is a bug, so report it with the
  journal lines around it.
- `hub.dropped_*` and `rrc.frames_dropped`: a subscriber or link falling
  behind. A few are fine; steady growth is not.
- `lxmf.gave_up` compared with `lxmf.sent`: members nobody can reach.

As a hub operator (an identity in `admins`), `/stats` shows the same counters
from any way in.

## Install or upgrade

```
make build                                   # on a workstation, or use the Docker image
scp bin/scotmesh-chat deploy/* you@your-host:/tmp/scotmesh-chat/
ssh you@your-host 'cd /tmp/scotmesh-chat && sudo ./install.sh'
```

`install.sh` refuses a binary that can't read the live config, keeps the old
binary as `scotmesh-chat.previous`, and restarts the service. The unit also
checks the config before every start. Running as a container instead is
just `docker run` (or `docker compose up`) with the new image tag; see
`deploy/docker-compose.example.yml`.

### A release with migrations

Try the migrations on a copy of the live database first, with the new binary.
`check-db` opens the copy, which migrates it, and prints the counts; compare
identities, names, messages and members with the live ones, and throw the copy
away:

```
ssh you@your-host 'sudo cp /var/lib/scotmesh-chat/hub.db /tmp/hub-copy.db &&
  sudo /tmp/scotmesh-chat/scotmesh-chat check-db /tmp/hub-copy.db; sudo rm -f /tmp/hub-copy.db*'
```

Then stop the hub, keep a copy of the database as it was
(`backups/hub-before-<version>.db`), and install. The migrations run as the
hub starts. After starting, send `/help` and `/whoami` from each way in, and
check the journal for errors.

## Roll back a release

```
sudo cp -p /opt/scotmesh-chat/scotmesh-chat.previous /opt/scotmesh-chat/scotmesh-chat
sudo systemctl restart scotmesh-chat
```

A build refuses to open a database with a newer schema than it knows. If a
release added a migration, restore the copy taken before the upgrade as well
(`backups/hub-before-<version>.db`, as above); anything said since is lost.

## Restore a backup

```
sudo systemctl stop scotmesh-chat
sudo -u scotmesh-chat cp /var/lib/scotmesh-chat/backups/hub-YYYY-MM-DD.db /var/lib/scotmesh-chat/hub.db
sudo rm -f /var/lib/scotmesh-chat/hub.db-wal /var/lib/scotmesh-chat/hub.db-shm
sudo systemctl start scotmesh-chat
```

## Migrating from rrc-hub and bridge 0.1

`scotmesh-chat import` brings an existing [rrcd](https://github.com/kc1awv/rrcd)
hub's and/or a bridge 0.1 deployment's state into this hub's store, so a
cutover keeps names, rooms, bans and history. See
`scotmesh-chat import --help` for its flags (`--rrc-hub`, `--bridge`,
`--skip`, `--dry-run`).

**Rule: the old service stops before the new one first announces an
address.** Two processes announcing one identity split the network's paths.

1. Rehearse with `--dry-run` against copies of both data directories, and
   read the report.
2. Stop the old service(s); copy their data directories somewhere the new
   host can read them (rrc-hub's `hub_identity`, `peers.toml`, `rooms.toml`,
   `klines.txt`, `history`; bridge 0.1's whole data directory).
3. If you're keeping rrc-hub's address as the new hub's RRC identity,
   install its identity file at `identities/rrc-hub` before starting; the
   bridge's group and page identities, if kept, go at `identities/group`
   and `identities/page`.
4. Install the new binary and config (`install.sh`), then run
   `scotmesh-chat import` for real.
5. Start the hub, and check the journal shows the addresses you expect.
6. Smoke test over the real backbone: join the RRC room and confirm
   catch-up; send `/whoami` to the LXMF group; open the chat page and post.
   `health.json`'s `hub.internal_errors` should be 0.

Keep the old data directories until you're confident the cutover holds —
rolling back means stopping the new service, restoring the old one's data
and identities, and starting it again.
