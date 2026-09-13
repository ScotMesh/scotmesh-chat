"""People interop (rc.4): profiles, whispers, moderation and /link over real
Reticulum links, with NomadNet's RRC client, Python LXMF apps and page
requests as NomadNet makes them.

usage: people_interop.py HUB_ADDRESS GROUP_ADDRESS PAGE_ADDRESS
env:   TEST_PORT (default 4299), OWNER_ID (an identity file listed in the
       hub's admins)

Prints PASS/FAIL per check; exits non-zero on any failure.
"""
import os
import re
import sys
import tempfile
import threading
import time

import RNS
import LXMF
from nomadnet import RRC as nrrc

PORT = int(os.environ.get("TEST_PORT", "4299"))
HUB, GROUP, PAGE = (bytes.fromhex(a) for a in sys.argv[1:4])
TOKEN = os.urandom(3).hex()
SUFFIX = TOKEN[:2]

tmp = tempfile.mkdtemp(prefix="people-interop-")
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


def page_say(identity, text):
    return fetch(identity=identity, data={"field_message": text, "var_action": "send"})


class Member:
    """An LXMF app, as Sideband or MeshChatX would be."""

    def __init__(self, display_name):
        self.identity = RNS.Identity()
        self.inbox = []
        self.lock = threading.Lock()
        self.router = LXMF.LXMRouter(identity=self.identity, storagepath=tempfile.mkdtemp(dir=tmp))
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

    def find(self, pattern):
        with self.lock:
            for m in self.inbox:
                found = re.search(pattern, m)
                if found:
                    return found
        return None

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


class RRCPerson:
    """NomadNet's RRC client."""

    def __init__(self, nick):
        self.nick = nick
        self.identity = RNS.Identity()
        self.events = []
        self.lock = threading.Lock()
        mgr = Manager(App(self.identity, nick))
        mgr.set_message_callback(self._on)
        self.hub = nrrc.RRCHub(mgr, HUB)
        self.hub._append_history = lambda room, msg: None
        self.hub._clean_history = lambda: None
        self.hub.nick_override = nick
        self.hub.connect()

    def _on(self, hub, msg):
        with self.lock:
            self.events.append((msg.kind, msg.room, msg.nick, msg.text, msg.src))

    def saw(self, needle, kind=None):
        with self.lock:
            return any(needle in (e[3] or "") and (kind is None or e[0] == kind) for e in self.events)

    def event(self, needle):
        with self.lock:
            for e in self.events:
                if needle in (e[3] or ""):
                    return e
        return None

    def say(self, text):
        self.hub.send_message("scotmesh", text)

    def last(self, n=4):
        with self.lock:
            return [e[:4] for e in self.events[-n:]]


for d in (HUB, GROUP, PAGE):
    RNS.Transport.request_path(d)
check("hub, group and page heard", wait_for(lambda: all(RNS.Identity.recall(d) for d in (HUB, GROUP, PAGE)), 90))

owner = RNS.Identity.from_file(os.environ["OWNER_ID"])
visitor = RNS.Identity()
alice = RRCPerson("Alice" + SUFFIX)
carl = RRCPerson("Carl" + SUFFIX)
for p in (alice, carl):
    check(f"{p.nick} on RRC", wait_for(lambda: p.hub.welcomed, 40))
    p.hub.join_room("scotmesh")
    wait_for(lambda: "scotmesh" in p.hub.rooms and "scotmesh" not in p.hub._pending_joins, 30)
ben = Member("Ben " + SUFFIX)
time.sleep(3)
ben.send("/join")
check("Ben joins the group", wait_for(lambda: ben.got("Welcome to ScotMesh"), 60), str(ben.last()))
ben_name = f"Ben_{SUFFIX}"

# --- profiles -----------------------------------------------------------------
ben_lxmf = RNS.Destination.hash(ben.identity, "lxmf", "delivery").hex()
page = fetch("/page/profile.mu", data={"var_id": ben.identity.hash.hex()})
check("Ben's profile page shows the name and a link to the LXMF address",
      f">{ben_name}" in page and f"`[Message on LXMF`lxmf@{ben_lxmf}]" in page, page[:1500])
alice.say(f"!profile {ben_name}")
check("!profile over RRC", wait_for(lambda: alice.saw(f"LXMF: {ben_lxmf}"), 30), str(alice.last()))

# --- whispers -----------------------------------------------------------------
alice.say(f"!whisper {ben_name} hello ben {TOKEN}")
check("a whisper from RRC reaches the LXMF app", wait_for(lambda: ben.got(f"{alice.nick} whispers: hello ben {TOKEN}"), 60), str(ben.last()))
check("the whisperer is told where it went", wait_for(lambda: alice.saw(f"Whispered to {ben_name} (by LXMF)."), 30), str(alice.last()))
check("nobody else sees the whisper", not carl.saw(f"hello ben {TOKEN}"))
ben.send(f"/r hi alice {TOKEN}")
check("/r from LXMF reaches RRC as a direct notice", wait_for(lambda: alice.saw(f"(whisper) hi alice {TOKEN}"), 60), str(alice.last()))
ev = alice.event(f"(whisper) hi alice {TOKEN}")
check("the direct notice comes from Ben's identity", bool(ev) and ev[4] == ben.identity.hash, str(ev))
fetch("/page/me.mu", identity=visitor, data={"var_action": "nick", "field_nick": "Vi" + SUFFIX})
page = page_say(visitor, f"/whisper {alice.nick} from the page {TOKEN}")
check("a whisper from the page", "Whispered to " + alice.nick in page, page[:1500])
check("reaches RRC", wait_for(lambda: alice.saw(f"(whisper) from the page {TOKEN}"), 30), str(alice.last()))
ben.send(f"/whisper Vi{SUFFIX} for your inbox {TOKEN}")
check("a whisper waits on the page", wait_for(lambda: f"whispers: for your inbox {TOKEN}" in fetch("/page/me.mu", identity=visitor), 60))

# --- ranks and moderation -------------------------------------------------------
fetch("/page/me.mu", identity=owner, data={"var_action": "nick", "field_nick": "Owner" + SUFFIX})
page = page_say(owner, f"/mod {alice.nick}")
check("the owner makes Alice a mod from the page", f"{alice.nick} is a mod now." in page, page[:1500])
check("Alice is told", wait_for(lambda: alice.saw("made you a mod"), 30), str(alice.last()))
alice.say(f"!rename Vi{SUFFIX} Violet{SUFFIX}")
check("a mod renames over RRC", wait_for(lambda: alice.saw(f"Renamed Vi{SUFFIX} to Violet{SUFFIX}."), 30), str(alice.last()))
check("the renamed visitor sees their new name", f"You are `F5d8`!Violet{SUFFIX}" in fetch(identity=visitor))
alice.say(f"!kick {ben_name} testing kicks")
check("a mod kicks over RRC", wait_for(lambda: alice.saw(f"Kicked {ben_name}."), 30), str(alice.last()))
check("the kicked LXMF member is told why", wait_for(lambda: ben.got(f"You were kicked from ScotMesh by {alice.nick}: testing kicks."), 60), str(ben.last()))
alice.hub.send_command(f"/ban {carl.nick} 1h spamming", room="scotmesh")
check("rrcd's /ban, forwarded by NomadNet, bans hub-wide", wait_for(lambda: alice.saw(f"Banned {carl.nick} (1 app), until"), 30), str(alice.last()))
check("the banned client is told why", wait_for(lambda: carl.saw(f"You were banned from ScotMesh by {alice.nick}"), 30), str(carl.last()))
alice.say(f"!bans")
check("!bans lists it", wait_for(lambda: alice.saw(f"{carl.nick} (1 app) — spamming"), 30), str(alice.last()))

# --- deleting ---------------------------------------------------------------------
alice.say(f"oops {TOKEN}")
anon_view = {"var_lines": "30", "var_flag": "1", "var_refresh": "0"}
check("the message is on the page", wait_for(lambda: f"oops {TOKEN}" in fetch(data=anon_view), 30))
alice.say(f"!delete {alice.nick}")
check("removing your own message", wait_for(lambda: alice.saw(f"Removed {alice.nick}'s last message"), 30), str(alice.last()))
check("it's gone from the page", f"oops {TOKEN}" not in fetch(data=anon_view))

# --- /link with approval ----------------------------------------------------------
page = page_say(owner, "/link")
code = re.search(r"send /link ([A-Z2-7]{4}-[A-Z2-7]{4})", page)
check("the owner gets a link code on the page", bool(code), page[:1500])
dana = Member("")
time.sleep(3)
dana.send("/join")
wait_for(lambda: dana.got("Welcome to ScotMesh"), 60)
if code:
    dana.send(f"/link {code.group(1)}")
    check("an owner's new app waits for approval", wait_for(lambda: dana.got("Waiting for approval from one of Owner"), 60), str(dana.last()))
    short = dana.identity.hash.hex()[:4]
    page = page_say(owner, f"/link approve {short}")
    check("approved from the page", f"is now one of Owner{SUFFIX}'s apps" in page, page[:1500])
    check("the new app is told", wait_for(lambda: dana.got(f"Approved: this app is now Owner{SUFFIX}."), 60), str(dana.last()))
    dana.send("/whoami")
    check("it shares the owner's name and is an admin", wait_for(lambda: dana.got(f"You are Owner{SUFFIX}") and dana.got("You are an admin of this hub."), 60), str(dana.last()))

# --- help ---------------------------------------------------------------------------
dana.send("/help")
check("/help comes in one-packet pages", wait_for(lambda: dana.find(r"Help 1/\d+ · chatting"), 60), str(dana.last()))

print(f"\n{'FAILED: ' + ', '.join(failures) if failures else 'all checks passed'}", flush=True)
os._exit(1 if failures else 0)
