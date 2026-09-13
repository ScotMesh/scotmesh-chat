# 0005 — Names are claimed by one identity across every way in

- Status: Accepted, 2026-09-12
- Decided by: the maintainers
- Amended by [0008](0008-roles-and-moderation.md): freeing your name moved
  from `/leave` to `/forget`, since `/leave` now means "leave what's here"
  (the room on RRC, the group on LXMF and the page) everywhere. The rest of
  this record — one name per identity, lookalikes refused, no accounts —
  is unchanged.

## Context

Nicks on rrc-hub are per link and can be squatted with lookalikes. The bridge
shows `<bridge>` in front of everyone it carries. The rules for this hub:

- A name is claimed by one Reticulum identity and never shared.
- It is the same name on RRC, the LXMF group and the page.
- It is freed only by `/leave`.
- There are no accounts and no device linking.

## Decision

- The person is the Reticulum identity (16-byte hash). RRC uses the identity
  from LINKIDENTIFY, LXMF the source identity of the message, and the page the
  identity the browser identified with.
- A name is claimed the first time an identity uses one: the RRC HELLO nick or
  `/nick`, the LXMF `/join Name` or announce display name, or the page name
  field.
- Names compare by skeleton: NFKC, case fold, `I l 1 |` → `l`, `O 0` → `o`,
  separators removed. If a name's skeleton matches someone else's claim, it is
  refused.
- Refused or nameless identities show as `guest-xxxx` (the first 4 hex of the
  identity) until they pick a free name.
- `/nick` swaps names in one transaction. `/leave` frees the name and ends
  memberships. `/release Name` is admin-only and audited.
- Names never expire.
- In RRC, `K_SRC` is always the author's real identity hash and `K_NICK` the
  claimed name, including for LXMF and page authors.

## Consequences

- Someone using two apps with two identities holds two names. That's by design,
  and `/leave` on one frees the name for the other.
- Messages keep the name they were said under, even after `/nick` or `/leave`.
- The name rules are one table-tested package used by all three ways in.
