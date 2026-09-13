#!/usr/bin/env python3
"""Regenerates golden.json: RRC frames produced by the reference encoders.

  - cbor2 is the encoder rrcd uses (rrcd/codec.py);
  - nomadnet.vendor.cbor is the encoder NomadNet's RRC client uses.

Run with a Python that has cbor2 and nomadnet installed:
    python3 rrc/wire/testdata/gen_golden.py > rrc/wire/testdata/golden.json
Frames use fixed IDs and timestamps so the file is stable.
"""
import json

import cbor2
from nomadnet.vendor import cbor as nncbor

SRC = bytes.fromhex("178c1f390b8f332f9c8681515c12107b")
HUB = bytes.fromhex("5c1b0a0e3d2f4a7b8c9d0e1f2a3b4c5d")
DST = bytes.fromhex("629ab93b4f74537940069f536f03bb7f")
MID = bytes.fromhex("0102030405060708")
TS = 1789247861447


def env(t, extra=None, src=SRC):
    e = {0: 1, 1: t, 2: MID, 3: TS, 4: src}
    e.update(extra or {})
    return e


valid = [
    ("nomadnet HELLO", "nomadnet",
     env(1, {6: {0: "nomadnet", 1: "0.1", 2: {0: True, 1: True}}, 7: "Alex"})),
    ("nomadnet MSG", "nomadnet", env(20, {5: "scotmesh", 6: "evening all", 7: "Rab"})),
    ("nomadnet ACTION", "nomadnet", env(22, {5: "scotmesh", 6: "waves", 7: "Ellen"})),
    ("nomadnet JOIN with key", "nomadnet", env(10, {5: "scotmesh", 6: "sekrit"})),
    ("MSG with reply and reaction extensions", "cbor2",
     env(20, {5: "scotmesh", 6: "+1", 7: "Rab", 64: bytes.fromhex("a1a2a3a4a5a6a7a8"), 65: "👍", 66: 1})),
    ("direct NOTICE", "cbor2", env(21, {6: "psst", 7: "Alex", 8: DST})),
    ("rrcd WELCOME", "cbor2",
     env(2, src=HUB, extra={6: {0: "ScotMesh", 1: "0.3.2", 2: {1: True, 2: True, 0: True},
                              3: {0: 32, 1: 64, 2: 350, 3: 32, 4: 240}}})),
    ("rrcd JOINED with member list", "cbor2", env(11, src=HUB, extra={5: "scotmesh", 6: [SRC, DST], 7: "Alex"})),
    ("rrcd PING with float body", "cbor2", env(30, src=HUB, extra={6: 12345.678})),
    ("RESOURCE_ENVELOPE", "cbor2",
     env(50, {6: {0: MID, 1: "motd", 2: 1024, 3: bytes(32), 4: "utf-8"}})),
    ("unknown key 9 is dropped", "cbor2", env(20, {5: "scotmesh", 6: "hi", 9: "ignored"})),
    ("empty room and nick", "cbor2", env(21, {5: "", 6: "/who", 7: ""})),
]

invalid = [
    ("not a map", cbor2.dumps([1, 2, 3]), "envelope must be a CBOR map (dict)"),
    ("string key", cbor2.dumps({**env(20), "1": 20}), "envelope keys must be integers"),
    ("negative key", cbor2.dumps({**env(20), -1: 0}), "envelope keys must be unsigned integers"),
    ("missing timestamp", cbor2.dumps({0: 1, 1: 20, 2: MID, 4: SRC}), "missing envelope key 3"),
    ("missing src", cbor2.dumps({0: 1, 1: 20, 2: MID, 3: TS}), "missing envelope key 4"),
    ("version 2", cbor2.dumps(env(20) | {0: 2}), "unsupported version 2"),
    ("version as text", cbor2.dumps(env(20) | {0: "1"}), "protocol version must be an integer"),
    ("version as bool", cbor2.dumps(env(20) | {0: True}), "protocol version must be an integer"),
    ("type as text", cbor2.dumps(env(20) | {1: "MSG"}), "message type must be an integer"),
    ("id as text", cbor2.dumps(env(20) | {2: "abc"}), "message id must be bytes"),
    ("timestamp as text", cbor2.dumps(env(20) | {3: "now"}), "timestamp must be an integer"),
    ("negative timestamp", cbor2.dumps(env(20) | {3: -5}), "timestamp must be unsigned"),
    ("src as text", cbor2.dumps(env(20) | {4: "me"}), "sender identity must be bytes"),
    ("room as int", cbor2.dumps(env(20) | {5: 7}), "room name must be a string"),
    ("nick as int", cbor2.dumps(env(20) | {7: 123}), "nickname must be a string"),
    ("dst as text", cbor2.dumps(env(21) | {8: "you"}), "destination identity must be bytes"),
]


def expect(e):
    out = {"type": e[1], "id": e[2].hex(), "ts": e[3], "src": e[4].hex()}
    if e.get(5):
        out["room"] = e[5]
    if e.get(7):
        out["nick"] = e[7]
    if 8 in e:
        out["dst"] = e[8].hex()
    if 6 in e:
        out["body"] = cbor2.dumps(e[6], canonical=True).hex()
    ext = {str(k): cbor2.dumps(v, canonical=True).hex() for k, v in e.items() if k >= 64}
    if ext:
        out["ext"] = ext
    return out


def canonical_of(e):
    """The frame our encoder must produce: absent-when-empty fields dropped, unknown keys 9-63 dropped."""
    c = {k: v for k, v in e.items() if k <= 8 or k >= 64}
    for k in (5, 7):
        if c.get(k) == "":
            del c[k]
    return cbor2.dumps(c, canonical=True).hex()


print(json.dumps({
    "valid": [
        {"name": n, "encoder": enc,
         "frame": (nncbor.dumps(e) if enc == "nomadnet" else cbor2.dumps(e)).hex(),
         "canonical": canonical_of(e), "expect": expect(e)}
        for n, enc, e in valid
    ],
    "invalid": [{"name": n, "frame": f.hex(), "error": err} for n, f, err in invalid],
    "announce": {"hub": "ScotMesh", "app_data": cbor2.dumps({"proto": "rrc", "v": 1, "hub": "ScotMesh"}).hex()},
}, indent=2, ensure_ascii=False))
