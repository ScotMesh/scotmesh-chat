"""Validate a Go-packed stamped LXMF message with real upstream LXMF.

Reads {"body_hex", "sender_pub_hex", "stamp_cost"} on stdin, prints a
JSON verdict. Driven by msgpack_canonical_interop_test.go under
`-tags interop_python`; run in a venv pinned to
reticulum-specifications/tools/requirements.txt (rns==1.5.0 lxmf==1.1.1).

This is the only admissible proof that our canonical encoding is right
(CLAUDE.md cardinal rule 3). LXMessage.unpack_from_bytes performs the
§5.7.1 strip-and-re-pack itself and validates the signature inline, so
signature_validated is upstream's own verdict on our bytes — not our
reimplementation of upstream's rules.
"""
import contextlib
import json
import sys
import tempfile

import RNS
from LXMF import LXMessage

inp = json.load(sys.stdin)
body = bytes.fromhex(inp["body_hex"])
pub = bytes.fromhex(inp["sender_pub_hex"])
source_hash = body[16:32]

# unpack_from_bytes validates the signature only when it can recall the
# source identity, so seed known_destinations the way an announce would.
RNS.Reticulum.storagepath = tempfile.mkdtemp(prefix="lxmf-interop-")
RNS.Identity.remember(None, source_hash, pub, app_data=None)

# RNS.log writes to stdout; keep it off the channel the verdict uses.
out = {}
try:
    with contextlib.redirect_stdout(sys.stderr):
        msg = LXMessage.unpack_from_bytes(body)
    out["signature_validated"] = bool(getattr(msg, "signature_validated", False))
    out["unverified_reason"] = getattr(msg, "unverified_reason", None)
    out["message_id_hex"] = msg.message_id.hex() if msg.message_id else None
    out["stamp_present"] = msg.stamp is not None
    cost = inp.get("stamp_cost") or 0
    if cost:
        with contextlib.redirect_stdout(sys.stderr):
            out["stamp_valid"] = bool(msg.validate_stamp(cost))
        out["stamp_value"] = msg.stamp_value
except Exception as e:
    out["error"] = f"{type(e).__name__}: {e}"

print(json.dumps(out))
