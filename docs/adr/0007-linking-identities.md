# 0007 — Linking a second identity to your name (/link)

- Status: Accepted, 2026-09-13
- Amends: 0005 (names are claimed by one identity)

## Context

A name belongs to one Reticulum identity (ADR 0005). Most people run more
than one app or device: Sideband on a phone, and MeshChatX or NomadNet on a
computer. Each app makes its own identity unless the person restores the
same one on every device, and most people don't. At cutover, John had
`John_MM7CAN` on RRC (identity `895e4c7f…`) and `john_mm7can` on LXMF
(`33450a2d…`). Only one of those identities can hold the name, so the other
shows as `guest-895e`.

We still want nobody to be able to take a name they don't control, and we
still want no accounts or passwords.

## Decision

A name can be held by a **person**: one primary identity plus up to four
linked identities. Linking needs proof of control of both. The person asks
for a code from an identity that holds the name, then sends the code from
the other identity. Being able to send from both identities is the proof.

### How it works for people

| From | Command | What happens |
|---|---|---|
| the identity with the name | `/link` | The reply is a one-time code valid for 10 minutes: "On your other app, send `/link K7Q2-M9XD` to the group or in the room." |
| the other identity | `/link K7Q2-M9XD` | This identity now shares the name. Both identities get a notice. If it held a name of its own, the reply asks it to send `/link K7Q2-M9XD confirm`, which frees that name. |
| any linked identity | `/devices` | Lists the identities sharing the name, with the way in each was last seen and when. |
| any linked identity | `/unlink` | Unlinks this identity, which becomes a guest again. |
| the primary identity | `/unlink guest-895e` or a hash prefix | Removes another device. |
| any linked identity | `/leave` | Frees the name, removes all links, and leaves the group. The reply warns that it affects every linked device. |

On the page, the "Your name & settings" page gets a **Link another app**
section. It has a button that shows a code, a field to enter a code from the
other app, and the list of linked devices with an Unlink link next to each.

Codes are 8 base32 characters shown as `XXXX-XXXX` (40 bits):
- single use, 10 minutes, at most one live code per person;
- at most 3 codes per person per hour;
- at most 5 wrong entries per identity per 10 minutes, after which `/link`
  is refused for 10 minutes;
- case-insensitive and hyphen-optional, so they survive phone keyboards.

### What a link changes, and what it doesn't

- **Shared across the person:** the name, `/nick` (from any device) and
  `/leave`. Room operator and voice roles are also granted to the person, so
  an operator doesn't lose rights when they switch device.
- **Kept per identity:**
  - LXMF membership and delivery settings, since each app has its own LXMF
    address and one may be on while another is off;
  - RRC catch-up cursors and page read markers, since each device has seen
    different things;
  - message authorship: `K_SRC` stays the real identity hash, `K_NICK` is
    the shared name.
- **`/who`:** lists the person once, with the ways in they're on.
- **`/lxmf auto`:** pauses LXMF on an identity while *any* identity of the
  person is in the RRC room, which is the behaviour people expect with a
  phone and a laptop. That becomes per-person, not per-identity.
- **Bans:** a room ban or `/kline` given by name covers every identity of
  the person. Given by hash, it covers that identity; the operator is asked
  to confirm whether to cover the rest.
- **`/release Name`:** frees the name and dissolves the links.
- **Unlinking the primary:** the longest-linked remaining identity becomes
  primary.

### Store (migration 0004)

*Amended when building (2026-09-13).* A `people` table replaces the
`identity_links` table first proposed here. Each identity points at the
person it is part of, and the name, the rank and the preferences belong to
the person. Linking changes one column on one identity, and unlinking the
longest-linked identity needs no hand-over of names or roles, because
nothing is keyed by a "primary" identity any more. Store methods still take
an identity and resolve its person inside, so there is still exactly one
place where the lookup rule lives.

```sql
CREATE TABLE people (id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL,
                     role TEXT, share_lxmf INTEGER, whispers INTEGER, …) STRICT;
ALTER TABLE identities ADD COLUMN person INTEGER REFERENCES people (id);
ALTER TABLE identities ADD COLUMN person_since INTEGER NOT NULL DEFAULT 0;
-- names is rebuilt keyed by person
CREATE TABLE link_codes (code_hash BLOB PRIMARY KEY,  -- SHA-256 of the code
                         person INTEGER NOT NULL, issued_by BLOB NOT NULL,
                         issued_at INTEGER, expires_at INTEGER,
                         redeemed_by BLOB, redeemed_via TEXT) STRICT;
```

The migration makes every existing identity a person of its own. The
"primary" identity is simply the one that has been part of the person
longest (`person_since`). A test keeps a list of every table that points at
`people` in step with the schema, so a later table can't be missed when two
people merge.

### Import

The importer never links automatically, because two identities using the
same name is not proof they belong to the same person. Its report lists
likely pairs ("John_MM7CAN on 895e4c7f and john_mm7can on 33450a2d look like
one person; they can /link"), and the hub can send each identity a one-off
notice suggesting `/link`.

### Linking and rank (amended alongside ADR 0008)

Roles belong to the person, and linking adds an identity to a person.
Without rules, linking would be a way to gain rank or dodge a ban:

- **Roles flow only from the issuer.** The code is issued by the app that
  holds the name and the role. The redeeming app gives up its own name and
  role, and gets the person's.
- **No redeeming upwards.** If the redeeming app has a higher rank than the
  person it would join, the link is refused, with a plain explanation.
- **A role holder needs a second approval.** When a mod, admin or owner's
  code is redeemed, the link waits: their existing apps get a notice to
  `/link approve` or `/link deny` it, which expires with the code. A leaked
  or socially-engineered code otherwise hands over the role outright.
  Members link in one step.
- **Owner identities never redeem or gain owner.** Owners are the identities
  named in the config file. An owner's linked apps get admin in chat, not
  owner; owner-only power stays with the config identities.
- **Banned identities can't link or be linked.** Bans by name cover every
  linked app; a ban by hash on one app of someone with a role needs rank
  over that person.
- **Rank is compared person to person**, so a mod can't kick, ban, rename or
  unlink any app of an admin.
- Every link, approval, denial and unlink of someone with a role is
  audited, and notifies the person's other apps.

## Consequences

- People with two apps keep one name without restoring identities by hand.
- The ADR 0005 rule "nobody can hold a name they don't control" still holds:
  linking needs a code issued to the name's holder and sent from the second
  identity.
- More to test:
  - code expiry, reuse, guessing limits;
  - link, unlink and primary hand-over;
  - `/leave` from a secondary;
  - bans by name and by hash;
  - auto-pause across devices;
  - `/who` de-duplication;
  - linking over each way in, including the page.
- Operators need `/devices Name` to see who is linked. It is shown only to
  hub operators and to the person.
