"""Generate rns/testdata/request_response_upstream.json from upstream RNS.

The §11 envelopes are plain umsgpack, and RNS bundles its own umsgpack
(RNS/vendor/umsgpack.py) which is what every signing/packing input uses.
Pack with that, not with a third-party msgpack, so the bytes are what a
real peer emits.
"""
import hashlib, json, sys
import RNS
import RNS.vendor.umsgpack as umsgpack

def path_hash(p):
    return hashlib.sha256(p.encode("utf-8")).digest()[:16]

TS = 1756300000.5  # pinned so the vector is reproducible

# A plain GET: data is None.
get_path = "/page/index.mu"
get_env = umsgpack.packb([TS, path_hash(get_path), None])

# A form post: data is a native dict — the §11.1 "packed exactly once"
# case. Pre-packing it would make element [2] decode as bytes.
form_path = "/page/guestbook.mu"
form_data = {"field_message": "hello", "var_username": "alice"}
form_env = umsgpack.packb([TS, path_hash(form_path), form_data])

# The mistake §11.1 warns about, for a test that asserts we do NOT do it.
form_env_double = umsgpack.packb([TS, path_hash(form_path), umsgpack.packb(form_data)])

# An LXMF propagation /get round: data is a list.
get_msgs_path = "/get"
get_msgs_env = umsgpack.packb([TS, path_hash(get_msgs_path), [None, ["aabb", "ccdd"]]])

# A response envelope.
req_id = bytes.fromhex("00112233445566778899aabbccddeeff")
resp_env = umsgpack.packb([req_id, {"status": "ok", "n": 3}])

json.dump({
    "_about": "SPEC §11 REQUEST/RESPONSE envelopes packed by upstream RNS's bundled umsgpack (RNS/vendor/umsgpack.py), the same encoder every RNS signing input uses.",
    "rns_version_at_generation": RNS.__version__,
    "timestamp": TS,
    "get_path": get_path,
    "get_path_hash_hex": path_hash(get_path).hex(),
    "get_envelope_hex": get_env.hex(),
    "form_path": form_path,
    "form_path_hash_hex": path_hash(form_path).hex(),
    "form_data": form_data,
    "form_envelope_hex": form_env.hex(),
    "form_envelope_double_packed_hex": form_env_double.hex(),
    "_trap": "form_envelope_double_packed_hex is what an implementation emits if it msgpacks `data` before putting it in the envelope. Element [2] then decodes as bytes rather than a map, and every upstream handler drops the request silently (§11.1).",
    "get_messages_path": get_msgs_path,
    "get_messages_envelope_hex": get_msgs_env.hex(),
    "response_request_id_hex": req_id.hex(),
    "response_envelope_hex": resp_env.hex(),
}, sys.stdout, indent=2)
print()
