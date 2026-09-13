# 0001 — Build our own hub by porting rrcd to Go

- Status: Accepted, 2026-09-12
- Decided by: the maintainers

## Context

ScotMesh chat runs rrc-hub alongside bridge 0.1. The bridge sits in the room
as a bot, so everyone reached through LXMF or the page appears as
`<bridge> <Name> text`. That means two services, two stores and two sets of
presence to keep in step. rrc-hub has no licence, so we can't fork or change it.

The options considered were:

1. Keep rrc-hub and improve the bridge.
2. Port rrcd (MIT, Python, by KC1AWV) to Go on reticulum-go and make it the one
   hub for all three ways in.
3. Run rrcd as it is and extend it in Python.

## Decision

Option 2. scotmesh-chat becomes the RRC hub, the LXMF group and the NomadNet
chat page in one Go process. We port rrcd's protocol behaviour, room model,
modes and commands. We do not port its structure: it has eight managers
sharing one lock. Identity, names, catch-up and the LXMF and page ways in are
built new. The operational lessons in rrc-hub's commit history are applied as
rules and tests, and none of its code is used.

## Consequences

- There is no bot. LXMF and page users appear in RRC under their own identity
  and name.
- Addresses stay the same: rrc-hub's hub identity moves to the new service at
  cutover, and the group and page keep theirs.
- We own the RRC server's correctness. That needs a wire codec checked against
  rrcd's error strings and golden frames, plus interop tests against NomadNet's
  RRC client, rrc-tui and MeshChatX.
- rrcd's copyright stays in NOTICE and in the headers of derived files.
- rrcd's known bugs, learned from running it, are deliberately not reproduced.
