# 0002 — Vendor reticulum-go with patches

- Status: Accepted, 2026-09-12

## Context

A hub is the *responder* on its clients' links. reticulum-go v0.7.2 cannot send
on a link it did not open. Its link callbacks are process-wide slots with no
destination. And a link evicted under table pressure never reports as closed.
The hub needs all three fixed, and the fixes are small.

A GitHub fork would be public, since forks of public repos can't be private.
A private fork would need CI credentials to fetch the module.

## Decision

Keep an exact copy of v0.7.2 in `third_party/reticulum-go`, add patches as
separate commits, and point the module at it with a `replace` in `go.mod`.
`SCOTMESH-PATCHES.md` records each patch and why. CI runs the upstream suite
with `-race` on the patched copy. The patches are written to be offered
upstream, with the PR text reviewed by the maintainers before anything is posted.

## Consequences

- Builds stay self-contained and reproducible; no extra credentials needed.
- Upstream fixes are not picked up automatically. Upgrading means importing
  the new tag unmodified, reapplying the patches, and running both suites.
- Once upstream merges equivalent changes, the copy is deleted and the
  `replace` removed.
