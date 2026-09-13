# reticulum-go

Pure-Go implementation of the [Reticulum Network Stack](https://reticulum.network/) (RNS) and [LXMF](https://github.com/markqvist/LXMF) — no third-party RNS library, no cgo. Interoperates with the canonical Python `RNS`/`LXMF` and the wider client ecosystem (Sideband, NomadNet, MeshChat).

```go
import (
    "github.com/thatSFguy/reticulum-go/rns"
    "github.com/thatSFguy/reticulum-go/lxmf"
)
```

## Packages

| Package | Contents |
|---|---|
| `rns` | Identity & destinations, packet/announce wire formats, links (handshake, tokens, state), transport & path-finding, proofs, resources (sender/receiver/ADV), HDLC + TCP interfaces. |
| `lxmf` | LXMF messages, opportunistic & link delivery, delivery proofs, propagation (store-and-forward), stamps/tickets. Depends only on `rns`. |

Wire formats are tracked against [`reticulum-specifications`](https://github.com/thatSFguy/reticulum-specifications) (byte-level reference, with runtime verifiers). The test suites include upstream interop fixtures under each package's `testdata/`.

## Provenance

Extracted from the RNS/LXMF core shared by [`reticulum-group-chat`](https://github.com/thatSFguy/reticulum-group-chat) and [`reticulum-relay-chat`](https://github.com/thatSFguy/reticulum-relay-chat), reconciled into one canonical module (group-chat's newer stack as the base, with relay-chat's unique hardening forward-ported). Those repos, and [`semaphore`](https://github.com/thatSFguy/semaphore), now depend on this module rather than carrying their own copy.

## Requirements

Go 1.26.1+. Dependencies: `golang.org/x/crypto`, `github.com/vmihailenco/msgpack/v5`.

## License

[MIT](LICENSE) © 2026 thatSFguy
