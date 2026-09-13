# 0003 — A single-writer hub core with adapters

- Status: Accepted, 2026-09-12

## Context

rrcd guards everything with one global lock, does disk I/O while holding it,
and its managers reach into each other's private state. rrc-hub's history
shows dispatcher stalls and lock-order problems. We now also have three ways
in with very different latency: an RRC link answers in milliseconds, while an
LXMF direct send can block for 15 seconds waiting for a proof.

## Decision

- `internal/hub` owns all chat state in one goroutine. Adapters send it
  requests (post, join, claim a name, change settings, run a command) over a
  bounded channel and get a reply. The core never blocks on network I/O.
- Every accepted message becomes an event that goes to each adapter's own
  bounded queue. A slow adapter drops from its own queue, counts the drops and
  logs them; it can't stall the core or the other adapters.
- Store writes happen in the core, one transaction per request. SQLite is
  local and fast; if it's slow we batch, never lock across the network.
- Adapters (`rrcsrv`, `lxmfgroup`, `nomadpage`) are the only code touching
  reticulum-go. Transport callbacks never send; they queue.

## Consequences

- There's one ordering of events, so every way in sees the same sequence and
  message IDs are monotonic.
- Commands, permissions and name rules are implemented once and tested without
  a network.
- Core throughput caps the whole hub. At ScotMesh's scale (tens of people)
  that's orders of magnitude of headroom, and the soak test measures it.
