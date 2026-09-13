# Architecture decision records

One file per decision that is hard to undo. The format is Context → Decision →
Consequences. A superseded record stays, marked as superseded, with a link to its
replacement.

| # | Decision | Status |
|---|---|---|
| [0001](0001-own-hub-port-rrcd.md) | Build our own hub by porting rrcd to Go | Accepted |
| [0002](0002-vendored-reticulum-go.md) | Vendor reticulum-go with patches | Accepted |
| [0003](0003-single-writer-core.md) | A single-writer hub core with adapters | Accepted |
| [0004](0004-sqlite-store.md) | SQLite (pure Go) for all state | Accepted |
| [0005](0005-names-bound-to-identities.md) | Names are claimed by one identity across every way in | Accepted |
| [0006](0006-catch-up-and-delivery-settings.md) | Catch-up since last disconnect; per-person delivery settings | Accepted |
| [0007](0007-linking-identities.md) | Linking a second identity to your name (/link) | Accepted |
| [0008](0008-roles-and-moderation.md) | Hub roles (owner, admin, mod) and moderation | Accepted |
| [0009](0009-profiles-and-sharing.md) | Profiles, LXMF address sharing and whispers | Accepted |
