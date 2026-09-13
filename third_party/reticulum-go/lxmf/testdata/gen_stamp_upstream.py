"""Generate lxmf/testdata/stamp_upstream.json from upstream LXMF.

Not part of the Go test run — this regenerates the fixture that
stamp_interop_test.go checks our workblock against. Run it in a throwaway
venv at the versions pinned in reticulum-specifications/tools/requirements.txt:

    python3 -m venv /tmp/rnsvenv
    /tmp/rnsvenv/bin/pip install rns==1.4.2 lxmf==1.1.1
    /tmp/rnsvenv/bin/python gen_stamp_upstream.py > stamp_upstream.json

The workblock is the only deterministic artifact of the stamp scheme —
the stamp search is randomized — so what gets pinned is its SHA-256 at
each round count, plus one upstream-generated stamp our validator must
accept.
"""
import hashlib, json, sys
import RNS
import LXMF
from LXMF import LXStamper

MATERIAL = hashlib.sha256(b"reticulum-go stamp interop vector").digest()
COST = 8

def digest(rounds):
    return hashlib.sha256(LXStamper.stamp_workblock(MATERIAL, rounds)).hexdigest()

wb = LXStamper.stamp_workblock(MATERIAL, LXStamper.WORKBLOCK_EXPAND_ROUNDS)
counter, stamp = 0, None
while True:
    candidate = counter.to_bytes(32, "big")
    if LXStamper.stamp_valid(candidate, COST, wb):
        stamp = candidate
        break
    counter += 1

SOURCE = f"upstream RNS {RNS.__version__} / LXMF {LXMF.__version__}, lxmf/testdata/gen_stamp_upstream.py"

out = {
    # Bound outside the f-string: nesting the same quote style inside an
    # f-string expression only parses on Python 3.12+ (PEP 701), and this
    # script is meant to run in whatever venv you happen to build.
    "_source": SOURCE,
    "material_hex": MATERIAL.hex(),
    "workblock_rounds": LXStamper.WORKBLOCK_EXPAND_ROUNDS,
    "workblock_len": len(wb),
    "workblock_sha256_hex": hashlib.sha256(wb).hexdigest(),
    "workblock_pn_rounds": LXStamper.WORKBLOCK_EXPAND_ROUNDS_PN,
    "workblock_pn_sha256_hex": digest(LXStamper.WORKBLOCK_EXPAND_ROUNDS_PN),
    "stamp_cost": COST,
    "stamp_hex": stamp.hex(),
    "stamp_value": LXStamper.stamp_value(wb, stamp),
}
json.dump(out, sys.stdout, indent=2, sort_keys=True)
print()
