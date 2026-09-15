# ScotMesh patches to reticulum-go

This directory is github.com/thatSFguy/reticulum-go **v0.7.2**
(`b0e1ca80cc3776e0a30cd56f47d37c0feed98245`, MIT) with the patches below.
scotmesh-chat builds against it through a `replace` in the top-level `go.mod`.
Each patch is its own commit on top of the unmodified import, so
`git log -- third_party/reticulum-go` is the full diff against upstream.
They are written to be offered upstream; once upstream has them, this copy goes.

`CLAUDE.md` (upstream's own AI-agent guide, not a patch) was removed: it
names a contributor's local filesystem path and a workflow file that isn't
vendored here. Nothing else in the vendored tree was touched by that commit.

## 1. Per-destination link hooks, SendOnLink, eviction close

**Why.** A chat hub is the *responder* on its clients' links. Upstream gives a
responder:

- one process-wide slot each for link data, identify, resource and close
  callbacks (`LinkManager.SetDefault…Handler`). A process serving several
  destinations (RRC hub, LXMF delivery, NomadNet page) cannot tell whose link a
  callback is about.
- `LocalDestination.OnLinkPlaintext`, which has no link ID.
- no way to send on a link it did not open: `SendOverLink` finds or opens a link
  by the *responder's* destination hash.
- an eviction path (`makeRoomForResponderLocked`) that drops the oldest responder
  link from the table without firing the closed callback. The peer then stays
  "present" for ever.

**What.**

- `LinkHooks{OnEstablished, OnIdentified, OnData, OnResource, OnClosed}` and
  `LocalDestination.LinkHooks`. The hooks are installed on the link before it
  enters the manager. A set hook replaces the manager-level handler for that
  destination's links; `OnLinkPlaintext` keeps precedence over `OnData`.
- `Link.LocalDestHash()`.
- `Transport.SendOnLink(linkID, plaintext)`: one DATA packet, no proof wait, never
  opens a link, `ErrLinkPayloadTooLarge` above `LinkMDU`.
- Eviction marks the link Closed, zeroes its keys and fires the closed callback
  (`TeardownTimeout`) after the manager lock is released.
- `HandleLinkData` reads the manager default handler under the manager lock
  (it was read unlocked).

**Tests.** `rns/link_hooks_test.go`. The full upstream suite passes with `-race`.

## 2. Plain addressing for destinations on the relaying node

**Why.** `KnownIdentity.TransportID` is captured from any announce that
arrived as HEADER_2, and every link request, Resource and opportunistic
LXMF packet to that destination was then sent HEADER_2 through it, even at
hops 0. An announce with hops 0 over HEADER_2 comes from a destination
attached to the relay itself: a local client of its rnsd, such as lxmd's
propagation node. Python RNS counts that as one hop and sends HEADER_1.
rnsd 1.5.2 forwards a HEADER_2 packet addressed to itself for a hops-0
destination to the local client without stripping the transport header, and
the link request is never answered. In production every upload to the
ScotMesh propagation node failed with `ErrLinkHandshakeTimeout`, while a
stock Python client on the same backbone linked to it in 0.1 s.

**What.** `KnownIdentity.RouteTransportID()` returns the transport_id only
when hops is at least 1. `acquireLinkTo`, the Resource path of
`SendOverLink` and `Delivery.SendWithID` use it instead of reading
`TransportID` directly.

**Tests.** `rns/route_header_test.go`: the link request for a hops-0
destination is HEADER_1 (failed before the change) and a hops-1 destination
is still HEADER_2 through the relay.
