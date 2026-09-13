// Package wire is the RRC v1 wire format: the CBOR envelope, its validation,
// and the bodies of the handshake and resource messages.
//
// It has no Reticulum dependency, so clients, tools and tests can use it as
// well as the hub. Behaviour follows rrcd 0.3.2 (constants.py, envelope.py,
// util.py), which this package ports; validation error texts are identical,
// because clients show them to people. Where this package deliberately
// differs from rrcd, the difference is noted at the declaration.
//
// Derived from rrcd, Copyright (c) 2025 S. Miller, KC1AWV, MIT License.
package wire
