# Security policy

scotmesh-chat runs as the RRC hub, LXMF group and NomadNet page for a real
community, so a vulnerability here can affect people who trust the hub with
their identity, their messages and their moderation. Please report
security issues privately rather than as a public issue or pull request.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for this repository: go to the
**Security** tab → **Report a vulnerability**. That opens a private
advisory only the maintainers can see, where you can describe the issue
and, if you have one, a proof of concept.

If that isn't available, open a regular issue asking a maintainer to
contact you privately, without any details of the vulnerability itself.

Please include, where you can:

- what's affected (a way in — RRC, LXMF or the page — a command, a config
  option) and how to reproduce it;
- what you'd expect instead, and the practical impact (what an attacker
  gains, and who's exposed);
- a suggested fix, if you have one — not required.

## Scope

In scope: `cmd/`, `internal/`, `rrc/wire/`, and the ScotMesh patches to
`third_party/reticulum-go` (see `third_party/reticulum-go/SCOTMESH-PATCHES.md`
for exactly what's patched). A vulnerability in the *unmodified* parts of
`third_party/reticulum-go` should also go to
[thatSFguy/reticulum-go](https://github.com/thatSFguy/reticulum-go)
directly, since it affects every user of that library, not just this hub.

Out of scope: a specific ScotMesh deployment's infrastructure, credentials
or identity keys — those aren't part of this repository.

## What to expect

We'll acknowledge a report as soon as we can and aim to keep you posted as
we work out impact and a fix. Once a fix is out, we'll credit the report in
the release notes unless you'd rather stay anonymous.
