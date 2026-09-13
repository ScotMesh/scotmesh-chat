# Draft: pull request to thatSFguy/reticulum-go

**Not posted.** This is the draft PR text for the patch in
`third_party/reticulum-go` (see `SCOTMESH-PATCHES.md`), to be rebased onto
upstream `main` before it's sent.

---

**Title:** rns: per-destination link hooks, SendOnLink, and a closed callback for evicted links

Hi, and thanks for reticulum-go. We've built a chat hub on it (an RRC hub,
an LXMF group and a NomadNet page in one process), and ran into three things
that anyone serving links as a *responder* will hit. This PR is what we're
running.

**1. A responder can't tell whose link a callback is about.** The link data,
identify, resource and close handlers on `LinkManager` are single
process-wide slots. `LocalDestination.OnLinkPlaintext` has no link ID. With
more than one destination serving links in a process, every consumer has to
guess.

This adds `LinkHooks{OnEstablished, OnIdentified, OnData, OnResource,
OnClosed}` on `LocalDestination`. They are installed on the link inside
`AcceptIncomingLinkRequest` before it enters the manager, so no packet can
reach a link without them. A set hook replaces the manager-level handler for
that destination's links; nil hooks leave today's behaviour unchanged, and
`OnLinkPlaintext` keeps precedence over `OnData`. `OnEstablished` does not
fire again for a retransmitted LINKREQUEST. `Link.LocalDestHash()` is added
too.

**2. A responder can't send on a link it didn't open.** `SendOverLink` finds
or opens a link by the responder's destination hash, which is the
initiator's view. `Transport.SendOnLink(linkID, plaintext)` sends one DATA
packet on an existing link by ID. It doesn't wait for the proof, matching
upstream's `Packet(link, data).send()` without a receipt callback. It never
opens a link, and it returns `ErrLinkPayloadTooLarge` above `LinkMDU`.

**3. Evicted links never report closing.** `makeRoomForResponderLocked`
removes the least-recently-active responder link from the table but never
fires the closed callback, so a consumer tracking presence keeps the peer
for ever. The evicted link is now marked Closed, its keys are zeroed, and
the closed callback (hook or manager-level) fires with `TeardownTimeout`
after `lm.mu` is released. The ordering follows `closeLink`.

Also, `HandleLinkData` read `lm.defaultOnInboundData` without `lm.mu`. It
now takes the lock.

**Tests:** `rns/link_hooks_test.go` covers hooks on a real LINKREQUEST
between two in-process managers, OnEstablished firing exactly once, data and
identify going to hooks instead of manager handlers, manager handlers still
serving destinations without hooks, SendOnLink decrypting at the initiator
plus its refusals, OnClosed firing once, and eviction reporting closed. The
full suite passes with `-race`.

Beyond the unit tests, we've run it against NomadNet's RRC client and Python
LXMF over real links. Happy to rework names or shape to fit how you'd like
the API to look.
