"""Chat page interop: page requests over real Reticulum links, as NomadNet
and MeshChatX make them, against scotmesh-chat on a private network.

usage: page_interop.py HUB_ADDRESS PAGE_ADDRESS
env:   TEST_PORT (default 4299)
"""
import os
import sys
import tempfile
import time

import RNS
from nomadnet import RRC as nrrc

PORT = int(os.environ.get("TEST_PORT", "4299"))
HUB, PAGE = (bytes.fromhex(a) for a in sys.argv[1:3])
TOKEN = os.urandom(3).hex()

tmp = tempfile.mkdtemp(prefix="page-interop-")
with open(os.path.join(tmp, "config"), "w") as f:
    f.write(f"""[reticulum]
  enable_transport = No
  share_instance = No
[logging]
  loglevel = 2
[interfaces]
  [[test]]
    type = TCPClientInterface
    enabled = yes
    target_host = 127.0.0.1
    target_port = {PORT}
""")
reticulum = RNS.Reticulum(configdir=tmp, loglevel=RNS.LOG_ERROR)
failures = []


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + (f"  ({detail})" if detail and not ok else ""), flush=True)
    if not ok:
        failures.append(name)


def wait_for(pred, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        if pred():
            return True
        time.sleep(0.25)
    return False


def fetch(path="/page/index.mu", identity=None, data=None):
    dest = RNS.Destination(RNS.Identity.recall(PAGE), RNS.Destination.OUT, RNS.Destination.SINGLE, "nomadnetwork", "node")
    link = RNS.Link(dest)
    if not wait_for(lambda: link.status == RNS.Link.ACTIVE, 30):
        return ""
    if identity:
        link.identify(identity)
        time.sleep(1)
    box = {}
    link.request(path, data=data, response_callback=lambda r: box.update(r=r.response),
                 failed_callback=lambda r: box.update(r=None), timeout=30)
    wait_for(lambda: "r" in box, 40)
    link.teardown()
    r = box.get("r") or b""
    return r.decode("utf-8", "replace") if isinstance(r, (bytes, bytearray)) else str(r)


class App:
    def __init__(self, identity, nick):
        self.identity = identity
        self.rns = reticulum
        self.storagepath = tempfile.mkdtemp(dir=tmp)
        self.peer_settings = {"display_name": nick}
        self.rrc_history_per_room_cap = 500
        self.rrc_filter_loaded_history = True
        self.rrc_ephemeral_notices = 600
        self.rrc_max_accepted_resource_size = 262144


class Manager(nrrc.RRCManager):
    def save(self):
        pass

    def load(self):
        self._loaded = True


for d in (HUB, PAGE):
    RNS.Transport.request_path(d)
check("hub and page heard", wait_for(lambda: all(RNS.Identity.recall(d) for d in (HUB, PAGE)), 90))

events = []
mgr = Manager(App(RNS.Identity(), "Rrc" + TOKEN[:2]))
mgr.set_message_callback(lambda hub, msg: events.append((msg.kind, msg.nick, msg.text, msg.src)))
rrc = nrrc.RRCHub(mgr, HUB)
rrc._append_history = lambda room, msg: None
rrc._clean_history = lambda: None
rrc.nick_override = "Rrc" + TOKEN[:2]
rrc.connect()
wait_for(lambda: rrc.welcomed, 40)
rrc.join_room("scotmesh")
wait_for(lambda: "scotmesh" in rrc.rooms and "scotmesh" not in rrc._pending_joins, 30)
rrc.send_message("scotmesh", f"from rrc {TOKEN}")
time.sleep(2)

page = fetch()
check("anonymous visitors get the refreshing conversation", "`{" in page and ":/page/messages.mu`30`pid=chat|lines=30|m=0}" in page, page[:1500])
page = fetch("/page/messages.mu", data={"var_lines": "30", "var_m": "0", "var_pid": "chat"})
check("and read it", f"from rrc {TOKEN}" in page, page[:400])
page = fetch(data={"var_lines": "30", "var_flag": "1", "var_refresh": "0"})
check("with refresh off, the conversation is on the page itself", f"from rrc {TOKEN}" in page and "ScotMesh Chat" in page, page[:400])
check("anonymous visitors are told how to post", "To post, identify yourself" in page, page[-400:])

visitor = RNS.Identity()
page = fetch(data={"field_message": "hi"} | {"var_action": "send"}, identity=None)
check("an anonymous post is refused", "Identify to post" in page, page[-500:])

page = fetch("/page/me.mu", identity=visitor, data={"var_action": "nick", "field_nick": "Morag" + TOKEN[:2]})
check("claim a name on the settings page", f"You are now Morag{TOKEN[:2]}" in page, page[:800])

# The conversation refreshes as a partial, fetched over a link of its own
# that doesn't identify; a token on the identified page says who's looking.
import re  # noqa: E402
page = fetch(identity=visitor)
partial = re.search(r"`\{[0-9a-f]{32}:/page/messages\.mu`30`pid=chat\|lines=30\|m=(\d+)\|t=([0-9a-f]{32})\}", page)
check("the identified page carries a refreshing partial", bool(partial), page[:1500])
if partial:
    part = fetch("/page/messages.mu", data={"var_lines": "30", "var_m": partial.group(1), "var_t": partial.group(2), "var_pid": "chat"})
    check("the partial serves the conversation", f"from rrc {TOKEN}" in part and "Say:" not in part, part[:800])
page = fetch(identity=visitor, data={"var_action": "view", "var_lines": "30", "var_flag": "1", "var_refresh": "0"})
check("refresh can be turned off from the page", "`!`F2c8[off]`f`!" in page, page[:1500])
page = fetch(identity=visitor, data={"var_action": "send", "field_message": f"from the page {TOKEN}"})
check("an identified visitor posts", f"from the page {TOKEN}" in page, page[-800:])
check("RRC gets the page post with the visitor's identity and name",
      wait_for(lambda: any(f"from the page {TOKEN}" in e[2] and e[1] == f"Morag{TOKEN[:2]}" and e[3] == visitor.hash for e in events), 30),
      str(events[-3:]))

page = fetch(identity=visitor, data={"var_action": "send", "field_message": "/whoami"})
check("a command typed on the page runs, with a reply only the visitor sees", "Only you see this:" in page and f"You are Morag{TOKEN[:2]}" in page, page[:1500])
page = fetch(identity=visitor, data={"var_action": "send", "field_message": "!help 2"})
check("!help 2 on the page", "Help 2/" in page, page[:1500])
time.sleep(2)
check("commands from the page are never said", not any(e[0] == "msg" and ("/whoami" in e[2] or "!help" in e[2]) for e in events), str(events[-3:]))

rrc.send_message("scotmesh", f"while the page was closed {TOKEN}")
time.sleep(2)
page = fetch(identity=visitor)
check("the page marks what's new since the last visit", "new since your last visit" in page, page[-800:])
page = fetch("/page/me.mu", identity=RNS.Identity(), data={"var_action": "nick", "field_nick": "M0rag" + TOKEN[:2]})
check("a lookalike name is refused on the page", "is taken by someone else" in page, page[:800])
page = fetch("/page/help.mu")
check("the help page names the hub and group", sys.argv[1] in page, page[:800])

print(f"\n{'FAILED: ' + ', '.join(failures) if failures else 'all checks passed'}", flush=True)
os._exit(1 if failures else 0)
