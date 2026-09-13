"""RRC interop: NomadNet's own RRC client against scotmesh-chat on a private network.

usage: rrc_interop.py HUB_ADDRESS
env:   TEST_PORT (default 4299)  the rnsd TCP server of the test network
       RESTART_CMD               shell command that restarts the hub (optional)

Everything goes over real Reticulum links. Prints PASS/FAIL per check; exits
non-zero on any failure.
"""
import os
import subprocess
import sys
import tempfile
import threading
import time

import RNS
from nomadnet import RRC as nrrc

PORT = int(os.environ.get("TEST_PORT", "4299"))
HUB = bytes.fromhex(sys.argv[1])
TOKEN = os.urandom(3).hex()

tmp = tempfile.mkdtemp(prefix="rrc-interop-")
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
        time.sleep(0.2)
    return False


class App:
    """The parts of NomadNetworkApp that RRCManager and RRCHub read."""

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


class Person:
    def __init__(self, nick, identity=None):
        self.nick = nick
        self.identity = identity or RNS.Identity()
        self.events = []  # (kind, room, nick, text, src)
        self.lock = threading.Lock()
        self.connect()

    def connect(self):
        mgr = Manager(App(self.identity, self.nick))
        mgr.set_message_callback(self._on)
        self.hub = nrrc.RRCHub(mgr, HUB)
        self.hub._append_history = lambda room, msg: None
        self.hub._clean_history = lambda: None
        self.hub.nick_override = self.nick
        self.hub.connect()

    def _on(self, hub, msg):
        with self.lock:
            self.events.append((msg.kind, msg.room, msg.nick, msg.text, msg.src))

    def texts(self, kind=None, room=None):
        with self.lock:
            return [e[3] for e in self.events if (kind is None or e[0] == kind) and (room is None or e[1] == room)]

    def saw(self, needle, kind=None):
        return any(needle in t for t in self.texts(kind))

    def mark(self):
        with self.lock:
            return len(self.events)

    def since(self, mark, kind=None):
        with self.lock:
            return [e for e in self.events[mark:] if kind is None or e[0] == kind]

    def join(self, room="scotmesh"):
        self.hub.join_room(room)
        return wait_for(lambda: room in self.hub.rooms and room not in self.hub._pending_joins, 30)

    def say(self, text, room="scotmesh"):
        self.hub.send_message(room, text)

    def disconnect(self):
        self.hub.disconnect()


RNS.Transport.request_path(HUB)
check("hub announce heard", wait_for(lambda: RNS.Identity.recall(HUB) is not None, 60))

alice = Person("Alice" + TOKEN[:2])
check("Alice welcomed", wait_for(lambda: alice.hub.welcomed, 40))
check("Alice joins #scotmesh", alice.join())

bob = Person("Bob" + TOKEN[:2])
check("Bob welcomed", wait_for(lambda: bob.hub.welcomed, 40))
m = alice.mark()
check("Bob joins #scotmesh", bob.join())
check("Alice sees Bob join", wait_for(lambda: any("joined" in e[3] and "Bob" in e[3] for e in alice.since(m, "system")), 20),
      str(alice.since(m)))

alice.say(f"hello bob {TOKEN}")
check("Bob gets Alice's message", wait_for(lambda: bob.saw(f"hello bob {TOKEN}", "msg"), 20))
ev = [e for e in bob.events if e[0] == "msg" and TOKEN in e[3]]
check("message carries Alice's identity and name", bool(ev) and ev[0][4] == alice.identity.hash and ev[0][2] == alice.nick,
      str(ev[:1]))

bob.hub.send_command("/who", room="scotmesh")
check("/who reply is parsed by NomadNet into the member list",
      wait_for(lambda: alice.identity.hash in bob.hub.members.get("scotmesh", set()) or
               any(p for p in bob.hub.prefix_members.get("scotmesh", {})), 20),
      str(bob.hub.members.get("scotmesh")))

carol = Person("A1ice" + TOKEN[:2])  # a lookalike of Alice's name
check("lookalike name refused with an explanation", wait_for(lambda: carol.saw("is taken by someone else"), 40), str(carol.texts()[-3:]))

# Catch-up: Alice leaves, three messages, Alice comes back with the same identity.
alice.disconnect()
time.sleep(3)
for i in range(3):
    bob.say(f"while you were away {i} {TOKEN}")
    time.sleep(0.5)
time.sleep(2)
alice.events.clear()
alice.connect()
check("Alice reconnects", wait_for(lambda: alice.hub.welcomed, 40))
alice.join()
check("Alice gets exactly the 3 missed messages",
      wait_for(lambda: sum(f"while you were away" in t for t in alice.texts("msg")) == 3, 30),
      str(alice.texts("msg")))
check("catch-up is bracketed", alice.saw("3 messages since you were here") and wait_for(lambda: alice.saw("end of history"), 10),
      str(alice.texts()))
check("nothing from before the disconnect is replayed", not any(f"hello bob {TOKEN}" in t for t in alice.texts("msg")))

m = alice.mark()
alice.hub.send_command("/nick", room="scotmesh")
check("command errors reach the client", wait_for(lambda: any("usage: /nick" in e[3] for e in alice.since(m)), 20),
      str(alice.since(m)))

# NomadNet's client keeps / to itself and forwards only rrcd's commands; !
# reaches the hub as an ordinary message and is answered, never relayed.
m, mb = alice.mark(), bob.mark()
alice.say("!help")
check("!help from NomadNet's client gets help page 1", wait_for(lambda: any("· chatting" in e[3] for e in alice.since(m)), 20),
      str(alice.since(m)))
alice.say("!whoami")
check("!whoami answers with the name", wait_for(lambda: any(f"You are {alice.nick}" in e[3] for e in alice.since(m)), 20), str(alice.since(m)))
time.sleep(2)
check("! commands are never relayed to the room", not any("!help" in e[3] or "!whoami" in e[3] for e in bob.since(mb)), str(bob.since(mb)))
alice.say(f"!!! not a command {TOKEN}")
check("! that isn't a command is said", wait_for(lambda: bob.saw(f"!!! not a command {TOKEN}", "msg"), 20))
alice.hub.send_command("/kick scotmesh nobody", room="scotmesh")
check("rrcd's forms still reach the hub", wait_for(lambda: any("target 'nobody' not found" in e[3] or "not authorized" in e[3] for e in alice.since(m)), 20),
      str(alice.since(m)))

long = ("the Ochils node hears Stirling " * 12)[:340]
bob.say(long)
check("a 340-byte message arrives whole", wait_for(lambda: alice.saw(long.strip(), "msg"), 20))

if os.environ.get("RESTART_CMD"):
    alice.disconnect()
    time.sleep(2)
    bob.say(f"before restart {TOKEN}")
    time.sleep(2)
    subprocess.run(os.environ["RESTART_CMD"], shell=True, check=True)
    time.sleep(8)
    alice.events.clear()
    alice.connect()
    check("Alice reconnects after a hub restart", wait_for(lambda: alice.hub.welcomed, 60))
    alice.join()
    check("catch-up survives the restart", wait_for(lambda: alice.saw(f"before restart {TOKEN}", "msg"), 30), str(alice.texts()))

print(f"\n{'FAILED: ' + ', '.join(failures) if failures else 'all checks passed'}", flush=True)
os._exit(1 if failures else 0)
