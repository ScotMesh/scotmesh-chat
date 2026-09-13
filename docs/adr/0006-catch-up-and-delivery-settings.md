# 0006 — Catch-up since last disconnect; per-person delivery settings

- Status: Accepted, 2026-09-12
- Decided by: the maintainers

## Context

rrc-hub replays the last N messages on every join, and clients show them
stamped "now". RRC history on connect should be *only what was said since
you last disconnected*. People should also be able to stop LXMF delivery
without leaving the group and losing their name, for example when they're
already reading the room over RRC.

## Decision

**Catch-up cursors.** For each identity, room and way in, the store keeps the
ID of the last message delivered to that identity.

- RRC saves the cursor when the link closes, and replays everything after it
  when the identity next joins that room.
- Replayed messages are sent as the original envelopes (original `K_ID`, `K_TS`
  and extension keys) between two NOTICEs in that room: `— N messages
  since you were here (Sat 18:02) —` and `— end of history —`.
- Someone who has never been in the room gets the last 10 messages.
- If more than 50 were missed, they get the latest 50 and a `/history` pointer.
- Nothing missed means no replay at all.
- Cursors for different ways in are independent, so reading on the page or
  over LXMF doesn't use up the RRC catch-up.

**Delivery settings** are per identity, stored with the name:

- `lxmf = on | off | auto`.
  - `off` keeps the membership and name but sends nothing.
  - `auto` pauses LXMF delivery while the identity has an RRC link in the room
    and resumes when it closes.
- When delivery resumes, the member gets one message ("12 messages while
  paused — send /history 12") rather than a flood.
- Set with `/lxmf on|off|auto` from LXMF or RRC, or on the page's settings
  panel.

## Consequences

- Clients that keep their own history (MeshChatX) no longer show duplicates on
  reconnect.
- A hub restart loses no cursors, because they are written in the same
  transaction as the link-closed event.
- `/pause` and `/resume` from bridge 0.1 map to `/lxmf off` and `/lxmf on`, and
  stay as aliases.
