# 0009 — Profiles, LXMF address sharing and whispers

- Status: Accepted, 2026-09-13

## Context

Someone reading the chat page or an RRC room has no way to find out more
about another person — whether they use LXMF, when they were last seen, or
how to reach them privately — short of asking in the open room. rrcd and
the LXMF group have no concept of a private, one-to-one message either.

## Decision

### Profiles

On the chat page, every name in the conversation links to a profile:

- the name, the role badge if they have one, and how long they've held it;
- where they are now (RRC, LXMF group, page) and when they were last seen;
- their LXMF address, as an `lxmf@<address>` link NomadNet opens as a
  conversation, if they share it (see below);
- a whisper box (see below);
- their last few messages;
- for mods and up: the moderation buttons from ADR 0008, each needing a
  second confirming click.

`/profile Name` (alias `/whois`) on RRC and LXMF returns the same profile
as text.

### Sharing

Whether an LXMF address is shown on someone's profile is their choice:

- `/share on|off` from any way in, or a toggle on the settings page. The
  default is on.
- Mods, admins and owners always show as shared, so people can reach hub
  staff.
- If someone has no address of their own (RRC-only), the `lxmf.delivery`
  address their identity would use is shown instead, labelled "if they use
  LXMF with this identity".

### Whispers

A message to one person, that only they see:

- **Sending:** `/whisper Name text` (aliases `/w`, `/msg`, `/tell`); `/r
  text` replies to the last person who whispered to you. The chat and
  profile pages have a whisper box.
- **Delivery:**
  - **RRC:** a direct notice to their links, from their identity and name.
  - **LXMF**, if they're a group member: a direct message from the group,
    delivered even if group delivery is off or paused, unless they've
    turned whispers off.
  - **Page:** a Whispers panel on their settings page, with a "new" marker.
  - **Offline:** stored, and delivered on their next RRC catch-up, through
    the durable LXMF queue, or shown on the page next time they look.
- **Privacy:** kept for a limited time (`internal/hub/whispers.go`'s
  `WhisperRetention`) and never shown in `/history`, catch-up or the page
  conversation. Mods can't read them; the mod log records only that a
  whisper happened, not its content. `/ignore Name` and `/unignore Name`
  block whispers from someone, from any way in.
- **Limits:** the ordinary per-identity post rate applies. You can't
  whisper while banned, or to someone who has banned you.

## Consequences

- People can find out how to reach each other without asking in the open
  room, without being forced to share anything they don't want to.
- Whispers give a private channel without adding accounts, direct messages
  between arbitrary Reticulum identities, or a second protocol.
- Tests cover sharing's default and the forced-on case for staff, whisper
  delivery on each way in (including while offline), and that whispers
  never leak into history, catch-up or the mod log.
