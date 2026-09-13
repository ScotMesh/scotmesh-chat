# 0008 — Hub roles (owner, admin, mod) and moderation

- Status: Accepted, 2026-09-13

## Context

rrcd's moderation is per room: an operator of `#den` has no standing in
`#lounge`. A hub carrying one conversation across RRC, an LXMF group and a
chat page needs moderation that follows a person everywhere, not just in
whichever room they happen to be op of, while still keeping rrcd's own
per-room roles (founder, granted op, voice) for rooms that want them.

## Decision

### Roles

| Role | Who | Can |
|---|---|---|
| owner | the `admins` list in the config file; can't be changed from chat | everything an admin can, plus demote admins |
| admin | promoted by an owner or admin | everything a mod can, plus make or remove mods and admins |
| mod | promoted by an owner or admin | kick, ban and unban, rename, see the mod log |
| member | everyone else | profile, whisper, their own settings |

- **Rank rule:** nobody acts on someone of equal or higher rank, compared
  person to person (a linked person's rank is the highest of any of its
  identities, see ADR 0007). Owners are exempt from the rule, but nobody can
  act on an owner from chat.
- **Room operators:** hub mods and admins are room operators in every room.
  A room's own founder and granted ops (rrcd's model) are unchanged, but a
  room operator with no hub rank can't kick, ban or deop hub staff — that
  needs the rank rule above, not just standing in the room.
- Roles belong to the person: with `/link` (ADR 0007), every linked
  identity of a mod is a mod.

### Moderation actions

Each action is available as a command on RRC and LXMF, and as a button on
the profile page, each needing a second confirming click.

| Action | Command | Effect |
|---|---|---|
| Kick | `/kick Name [reason]` | Removes them from every room and the LXMF group, releases their name, and breaks their links. They get a notice with the reason and can come back as a guest. |
| Ban | `/ban Name [1h\|1d\|7d\|perm] [reason]` | Bans every linked identity across the hub, releases the name, and disconnects them everywhere. The default is permanent. |
| Unban | `/unban Name\|hash` | Lifts the ban. |
| Ban list | `/bans` | Lists current bans with reason, who issued each and when it expires. |
| Rename | `/rename Name NewName` | The normal name rules apply (lookalikes, reserved words). The person is told and the change is audited. |
| Promote | `/mod Name`, `/admin Name` | Grants the role. The person is told. |
| Demote | `/demote Name` | Takes one role step down, to member. |
| Delete messages | `/delete Name [n]` | Removes their last n messages (default 1) in the room from history, catch-up and the page, and cancels LXMF copies not yet sent. Anyone can delete their own; mods and up can delete lower ranks'. |
| Mod log | `/modlog [n]` | The last actions, with who did what to whom and why. |

- **Name clash with rrcd:** rrcd's room-level `/kick <room> …`,
  `/ban <room> …`, `/invite`, `/op`, `/voice`, `/mode` and `/register` move
  under `/room` (for example `/room kick <room> <name>`). `/topic` stays as
  it is. `/kline` stays as an alias of `/ban`. Old forms keep working for
  one release, with a hint pointing at the new one.
- **Timed bans** expire through the minute maintenance tick.
- **Every action** is written to the audit table, with the reason.

### Freeing your name

Freeing your name moved from `/leave` (ADR 0005's original wording) to
`/forget`, because `/leave [#room|lxmf]` now means "leave what's here" —
the room on RRC, the group on LXMF and the page — everywhere, and that
needed a name that didn't already mean something else. `/forget` frees
your name for anyone, removes your links, and leaves the group.

## Consequences

- Moderation follows a person across every way in, not just the room they
  happen to be in.
- rrcd's per-room operators keep working exactly as before, for rooms that
  want them, but they're capped: they can't be used against hub staff.
- Tests cover the rank rule for every role and action pair, room operators
  acting against hub ranks (refused unless the actor also outranks the
  target hub-wide), and the rrcd-compatible `/room` forms alongside the old
  ones.
