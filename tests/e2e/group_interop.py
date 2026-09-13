"""LXMF group interop: stock Python LXMF clients and NomadNet's RRC client
against scotmesh-chat on a private network with a propagation node.

usage: group_interop.py HUB_ADDRESS GROUP_ADDRESS PROPAGATION_NODE
env:   TEST_PORT (default 4299)

Prints PASS/FAIL per check; exits non-zero on any failure.
"""
import os
import subprocess
import sys
import tempfile
import threading
import time

PORT = int(os.environ.get("TEST_PORT", "4299"))
HUB, GROUP, PN = (bytes.fromhex(a) for a in sys.argv[1:4])
TOKEN = os.urandom(3).hex()
HERE = os.path.dirname(os.path.abspath(__file__))


def rns_config(path):
    os.makedirs(path, exist_ok=True)
    with open(os.path.join(path, "config"), "w") as f:
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
    return path


def wait_for(pred, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        if pred():
            return True
        time.sleep(0.25)
    return False


# --- Eve's second life: a separate process that collects from the propagation node ---
TOKEN_ARG = os.environ.get("EVE_TOKEN", "")
if len(sys.argv) > 4 and sys.argv[4] == "eve-sync":
    import RNS
    import LXMF
    workdir = sys.argv[5]
    RNS.Reticulum(configdir=rns_config(os.path.join(workdir, "eve-sync-rns")), loglevel=RNS.LOG_ERROR)
    identity = RNS.Identity.from_file(os.path.join(workdir, "eve.id"))
    inbox = []
    router = LXMF.LXMRouter(identity=identity, storagepath=os.path.join(workdir, "eve-lxmf"))
    router.register_delivery_identity(identity, display_name="Eve")
    router.register_delivery_callback(lambda m: inbox.append(m.content_as_string()))
    RNS.Transport.request_path(PN)
    wait_for(lambda: RNS.Identity.recall(PN), 60)
    router.set_outbound_propagation_node(PN)
    for _ in range(6):
        router.request_messages_from_propagation_node(identity)
        if wait_for(lambda: any(f"for eve {TOKEN_ARG}" in m for m in inbox), 20):
            break
    ok = any(f"for eve {TOKEN_ARG}" in m for m in inbox)
    print(("PASS" if ok else "FAIL") + " Eve collects the group message from the propagation node" + ("" if ok else f"  ({inbox[-3:]})"), flush=True)
    os._exit(0 if ok else 1)

import RNS  # noqa: E402
import LXMF  # noqa: E402
from nomadnet import RRC as nrrc  # noqa: E402

tmp = tempfile.mkdtemp(prefix="group-interop-")
reticulum = RNS.Reticulum(configdir=rns_config(os.path.join(tmp, "rns")), loglevel=RNS.LOG_ERROR)
failures = []


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + (f"  ({detail})" if detail and not ok else ""), flush=True)
    if not ok:
        failures.append(name)


class Member:
    def __init__(self, display_name, identity=None, storage=None):
        self.identity = identity or RNS.Identity()
        self.inbox = []
        self.lock = threading.Lock()
        self.router = LXMF.LXMRouter(identity=self.identity, storagepath=storage or tempfile.mkdtemp(dir=tmp))
        self.dest = self.router.register_delivery_identity(self.identity, display_name=display_name)
        self.router.register_delivery_callback(self._got)
        self.router.announce(self.dest.hash)

    def _got(self, m):
        with self.lock:
            self.inbox.append(m.content_as_string())

    def send(self, text):
        dest = RNS.Destination(RNS.Identity.recall(GROUP), RNS.Destination.OUT, RNS.Destination.SINGLE, "lxmf", "delivery")
        self.router.handle_outbound(LXMF.LXMessage(dest, self.dest, text, desired_method=LXMF.LXMessage.DIRECT))

    def got(self, needle):
        with self.lock:
            return any(needle in m for m in self.inbox)

    def count(self, needle):
        with self.lock:
            return sum(needle in m for m in self.inbox)

    def last(self, n=3):
        with self.lock:
            return self.inbox[-n:]


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


def rrc_person(nick, identity=None):
    identity = identity or RNS.Identity()
    events = []
    mgr = Manager(App(identity, nick))
    mgr.set_message_callback(lambda hub, msg: events.append((msg.kind, msg.room, msg.nick, msg.text, msg.src)))
    hub = nrrc.RRCHub(mgr, HUB)
    hub._append_history = lambda room, msg: None
    hub._clean_history = lambda: None
    hub.nick_override = nick
    hub.connect()
    return hub, events, identity


for d in (HUB, GROUP, PN):
    RNS.Transport.request_path(d)
check("hub, group and propagation node heard", wait_for(lambda: all(RNS.Identity.recall(d) for d in (HUB, GROUP, PN)), 90))

alice, alice_events, _ = rrc_person("Alice" + TOKEN[:2])
check("Alice on RRC", wait_for(lambda: alice.welcomed, 40))
alice.join_room("scotmesh")
check("Alice in #scotmesh", wait_for(lambda: "scotmesh" in alice.rooms and "scotmesh" not in alice._pending_joins, 30))

ben = Member("Ben Nevis " + TOKEN[:2])
cara = Member("Cara" + TOKEN[:2])
time.sleep(3)
ben.send("hello?")
check("a non-member is told how to join", wait_for(lambda: ben.got("Send /join to take part"), 60), str(ben.last()))
ben.send("/join")
check("Ben joins under his announced name", wait_for(lambda: ben.got(f"Welcome to ScotMesh, Ben_Nevis_{TOKEN[:2]}."), 60), str(ben.last()))
check("joining says delivery starts with your own messages", wait_for(lambda: ben.got("You'll get every message, your own from RRC and the page included."), 60), str(ben.last()))
check("RRC members see Ben join", wait_for(lambda: any("joined" in e[3] and "Ben_Nevis" in e[3] for e in alice_events), 30))
cara.send("/join")
check("Cara joins", wait_for(lambda: cara.got("Welcome to ScotMesh"), 60), str(cara.last()))

alice.send_message("scotmesh", f"hello group {TOKEN}")
check("RRC -> Ben as 'Name: message'", wait_for(lambda: ben.got(f"Alice{TOKEN[:2]}: hello group {TOKEN}"), 60), str(ben.last()))
check("RRC -> Cara", wait_for(lambda: cara.got(f"Alice{TOKEN[:2]}: hello group {TOKEN}"), 60), str(cara.last()))

ben.send(f"hello rrc {TOKEN}")
check("LXMF -> RRC", wait_for(lambda: any(f"hello rrc {TOKEN}" in e[3] for e in alice_events if e[0] == "msg"), 60))
ev = [e for e in alice_events if e[0] == "msg" and f"hello rrc {TOKEN}" in e[3]]
check("RRC sees Ben's real identity and name, no bridge prefix",
      bool(ev) and ev[0][4] == ben.identity.hash and ev[0][2] == f"Ben_Nevis_{TOKEN[:2]}" and ev[0][3] == f"hello rrc {TOKEN}", str(ev[:1]))
check("LXMF -> other member", wait_for(lambda: cara.got(f"Ben_Nevis_{TOKEN[:2]}: hello rrc {TOKEN}"), 60), str(cara.last()))
time.sleep(3)
check("the author does not get their own message back", ben.count(f"hello rrc {TOKEN}") == 0, str(ben.last()))

# Fay's client refuses every inbound LXMF Resource, as MeshChatX does for
# strangers by default ("Block Attachments from Strangers"). Everything the
# hub sends must still reach her, one packet at a time.
fay = Member("Fay" + TOKEN[:2])
fay_refused = []
fay.router.delivery_resource_advertised = lambda r: fay_refused.append(r.get_data_size()) and False
time.sleep(3)
fay.send("/join")
check("Fay (refuses Resources) gets the welcome and digest", wait_for(lambda: fay.got("Welcome to ScotMesh") and fay.got(f"hello rrc {TOKEN}"), 90), str(fay.last()))
fay.send("/help")
check("/help page 1 reaches Fay", wait_for(lambda: fay.got("Help 1/") and fay.got("· chatting"), 60), str(fay.last()))
fay.send("!help 3")
check("!help 3 reaches Fay", wait_for(lambda: fay.got("Help 3/"), 60), str(fay.last()))
fay.send("/help 2")
check("/help 2 reaches Fay", wait_for(lambda: fay.got("Help 2/"), 60), str(fay.last()))
tail = f" end {TOKEN}"
long_line = " ".join(f"word{i}" for i in range(400))[: alice.max_msg_body_bytes - len(tail)].rstrip() + tail  # as long as RRC allows
check("the RRC limit leaves room for a message longer than one LXMF packet", len(long_line) > 300, str(alice.max_msg_body_bytes))
alice.send_message("scotmesh", long_line)
check("a long RRC message reaches Fay in parts", wait_for(lambda: fay.got(f"end {TOKEN}") and fay.got("(1/"), 90), str(fay.last()))
check("the hub never offered Fay a Resource", not fay_refused, str(fay_refused))

# Settings: off, then on with the missed-count note.
cara.send("/lxmf off")
check("/lxmf off acknowledged", wait_for(lambda: cara.got("LXMF delivery is off."), 60))
alice.send_message("scotmesh", f"while cara is off 1 {TOKEN}")
alice.send_message("scotmesh", f"while cara is off 2 {TOKEN}")
check("Ben still gets messages", wait_for(lambda: ben.got(f"while cara is off 2 {TOKEN}"), 60))
time.sleep(5)
check("Cara gets nothing while off", not cara.got("while cara is off"), str(cara.last()))
cara.send("/lxmf on")
check("resume says how many were missed", wait_for(lambda: cara.got("2 messages were said while it was paused"), 60), str(cara.last()))
cara.send("/history 2")
check("/history over LXMF", wait_for(lambda: cara.got(f"while cara is off 1 {TOKEN}") and cara.got("the last 2 messages"), 60), str(cara.last()))

# auto: one identity on both; LXMF pauses while on RRC.
dee_id = RNS.Identity()
dee = Member("Dee" + TOKEN[:2], identity=dee_id)
time.sleep(3)
dee.send("/join")
check("Dee joins the group", wait_for(lambda: dee.got("Welcome to ScotMesh"), 60), str(dee.last()))
dee.send("/lxmf auto")
check("/lxmf auto acknowledged", wait_for(lambda: dee.got("LXMF delivery is auto, on"), 60), str(dee.last()))
dee_rrc, dee_events, _ = rrc_person("Dee" + TOKEN[:2], identity=dee_id)
check("Dee on RRC with the same identity", wait_for(lambda: dee_rrc.welcomed, 40))
dee_rrc.join_room("scotmesh")
wait_for(lambda: "scotmesh" in dee_rrc.rooms and "scotmesh" not in dee_rrc._pending_joins, 30)
time.sleep(2)
alice.send_message("scotmesh", f"dee is on rrc {TOKEN}")
check("Dee gets it on RRC", wait_for(lambda: any(f"dee is on rrc {TOKEN}" in e[3] for e in dee_events), 60))
time.sleep(5)
check("Dee's LXMF is paused while on RRC", not dee.got(f"dee is on rrc {TOKEN}"), str(dee.last()))
dee_rrc.disconnect()
check("LXMF resumes with the count when Dee leaves RRC", wait_for(lambda: dee.got("1 message was said while it was paused"), 60), str(dee.last()))

# Offline member: delivery goes to the propagation node, and Eve collects it later.
eve_storage = os.path.join(tmp, "eve-lxmf")
eve_id = RNS.Identity()
eve_id.to_file(os.path.join(tmp, "eve.id"))
eve = Member("Eve" + TOKEN[:2], identity=eve_id, storage=eve_storage)
time.sleep(3)
eve.send("/join")
check("Eve joins", wait_for(lambda: eve.got("Welcome to ScotMesh"), 60), str(eve.last()))
eve.router.exit_handler()
RNS.Transport.deregister_destination(eve.dest)
time.sleep(2)
alice.send_message("scotmesh", f"for eve {TOKEN}")
print("waiting for direct delivery to Eve to fail and go to the propagation node (up to 3 minutes)", flush=True)
time.sleep(150)
r = subprocess.run([sys.executable, __file__, *sys.argv[1:4], "eve-sync", tmp], env={**os.environ, "EVE_TOKEN": TOKEN})
if r.returncode != 0:
    failures.append("offline delivery via the propagation node")

print(f"\n{'FAILED: ' + ', '.join(failures) if failures else 'all checks passed'}", flush=True)
os._exit(1 if failures else 0)
