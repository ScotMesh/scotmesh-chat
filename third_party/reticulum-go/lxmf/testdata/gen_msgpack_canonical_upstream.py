"""Generate lxmf/testdata/msgpack_canonical_upstream.json from upstream RNS.

Not part of the Go test run — this regenerates the fixture that
msgpack_canonical_test.go checks our encoder against. Run it in a
throwaway venv at the versions pinned in
reticulum-specifications/tools/requirements.txt:

    python3 -m venv /tmp/rnsvenv
    /tmp/rnsvenv/bin/pip install rns==1.5.0 lxmf==1.1.1
    /tmp/rnsvenv/bin/python gen_msgpack_canonical_upstream.py > msgpack_canonical_upstream.json

RNS.vendor.umsgpack is the encoder upstream LXMF uses for every signing
input, so its output IS the canonical form a stamped message must match
(SPEC §5.6.1). The integer boundaries are where an encoder that picks an
envelope by static type rather than by value diverges.
"""
import json
import RNS
from RNS.vendor import umsgpack

INTS = [0, 1, 127, 128, 255, 256, 65535, 65536, 2**32 - 1, 2**32,
        -1, -32, -33, -128, -129, -32768, -32769]

out = {
    "_source": f"RNS {RNS.__version__} RNS.vendor.umsgpack",
    "ints": {str(v): umsgpack.packb(v).hex() for v in INTS},
    "float_zero_hex": umsgpack.packb(0.0).hex(),
    "float_frac_hex": umsgpack.packb(1.5).hex(),
    "bytes_ab_hex": umsgpack.packb(b"ab").hex(),
    # The exact shape that broke: an LXMF fields map with an integer key.
    "fields_map_hex": umsgpack.packb({6: b"x"}).hex(),
    # A full 4-element LXMF payload with an integer-keyed fields map —
    # what a stamp-requiring recipient re-packs before verifying.
    "payload_hex": umsgpack.packb([0.0, b"", b"hi", {6: [b"webpd"]}]).hex(),
}
print(json.dumps(out, indent=2))
