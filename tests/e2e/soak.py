"""Soak: steady traffic on all three ways in against one hub, for hours.

usage: soak.py HUB GROUP PAGE HEALTH_JSON DURATION_MINUTES OUT_DIR
env:   TEST_PORT (default 4299)

People:
  - two observers that never leave: one on RRC, one in the LXMF group;
  - RRC regulars who post and reconnect now and then;
  - LXMF members who post;
  - page visitors who identify and post;
  - one LXMF member who has gone away for good (exercises the away and
    propagation path).

Every post carries a unique token. At the end the observers must have seen
every token exactly once (RRC also through catch-up after its own
reconnects, which the observer never does). health.json is sampled every
minute into OUT_DIR/health.csv, and the run fails on internal errors,
dropped events, growing goroutines or memory.
"""
import csv
import json
import os
import random
import sys
import tempfile
import threading
import time

import RNS
import LXMF
from nomadnet import RRC as nrrc

PORT = int(os.environ.get("TEST_PORT", "4299"))
HUB, GROUP, PAGE = (bytes.fromhex(a) for a in sys.argv[1:4])
HEALTH = sys.argv[4]
DURATION = float(sys.argv[5]) * 60
OUT = sys.argv[6]
os.makedirs(OUT, exist_ok=True)
rng = random.Random(1)

tmp = tempfile.mkdtemp(prefix="soak-")
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
lock = threading.Lock()
posted = {}  # token -> (way, author)


def log(msg):
    line = time.strftime("%H:%M:%S ") + msg
    print(line, flush=True)
    with open(os.path.join(OUT, "soak.log"), "a") as f:
        f.write(line + "\n")


def wait_for(pred, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        if pred():
            return True
        time.sleep(0.25)
    return False


def new_token(way, author):
    t = "tok" + os.urandom(5).hex()
    with lock:
        posted[t] = (way, author)
    return t


class App:
    def __init__(self, identity, nick):
        self.identity = identity
        self.rns = reticulum
        self.storagepath = tempfile.mkdtemp(dir=tmp)
        self.peer_settings = {"display_name": nick}
        self.rrc_history_per_room_cap = 100000
        self.rrc_filter_loaded_history = True
        self.rrc_ephemeral_notices = 600
        self.rrc_max_accepted_resource_size = 262144


class Manager(nrrc.RRCManager):
    def save(self):
        pass

    def load(self):
        self._loaded = True


class RRCPerson:
    def __init__(self, nick):
        self.nick = nick
        self.identity = RNS.Identity()
        self.seen = {}
        self.hub = None
        self.connect()

    def connect(self):
        mgr = Manager(App(self.identity, self.nick))
        mgr.set_message_callback(self._on)
        self.hub = nrrc.RRCHub(mgr, HUB)
        self.hub._append_history = lambda room, msg: None
        self.hub._clean_history = lambda: None
        self.hub.nick_override = self.nick
        self.hub.connect()
        if wait_for(lambda: self.hub.welcomed, 60):
            self.hub.join_room("scotmesh")
            wait_for(lambda: "scotmesh" in self.hub.rooms and "scotmesh" not in self.hub._pending_joins, 30)

    def _on(self, hub, msg):
        if msg.kind in ("msg", "action"):
            for word in msg.text.split():
                if word.startswith("tok"):
                    with lock:
                        self.seen[word] = self.seen.get(word, 0) + 1

    def ok(self):
        return self.hub.welcomed and "scotmesh" in self.hub.rooms

    def post(self):
        if not self.ok():
            return
        t = new_token("rrc", self.nick)
        self.hub.send_message("scotmesh", f"rrc says {t}")

    def reconnect(self):
        self.hub.disconnect()
        time.sleep(rng.uniform(5, 30))
        self.connect()


class LXMFPerson:
    def __init__(self, name, join=True):
        self.name = name
        self.identity = RNS.Identity()
        self.seen = {}
        self.inbox = 0
        self.router = LXMF.LXMRouter(identity=self.identity, storagepath=tempfile.mkdtemp(dir=tmp))
        self.dest = self.router.register_delivery_identity(self.identity, display_name=name)
        self.router.register_delivery_callback(self._on)
        self.router.announce(self.dest.hash)
        if join:
            time.sleep(2)
            self.send("/join")

    def _on(self, m):
        text = m.content_as_string()
        with lock:
            self.inbox += 1
            for word in text.split():
                if word.startswith("tok"):
                    self.seen[word] = self.seen.get(word, 0) + 1

    def send(self, text):
        ident = RNS.Identity.recall(GROUP)
        if ident is None:
            return
        dest = RNS.Destination(ident, RNS.Destination.OUT, RNS.Destination.SINGLE, "lxmf", "delivery")
        self.router.handle_outbound(LXMF.LXMessage(dest, self.dest, text, desired_method=LXMF.LXMessage.DIRECT))

    def post(self):
        t = new_token("lxmf", self.name)
        self.send(f"lxmf says {t}")


def page_post(identity, name):
    ident = RNS.Identity.recall(PAGE)
    if ident is None:
        return
    dest = RNS.Destination(ident, RNS.Destination.OUT, RNS.Destination.SINGLE, "nomadnetwork", "node")
    link = RNS.Link(dest)
    if not wait_for(lambda: link.status == RNS.Link.ACTIVE, 30):
        return
    link.identify(identity)
    time.sleep(1)
    box = {}
    t = new_token("page", name)
    link.request("/page/index.mu", data={"var_action": "send", "field_message": f"page says {t}"},
                 response_callback=lambda r: box.update(r=True), failed_callback=lambda r: box.update(r=False), timeout=30)
    if not wait_for(lambda: "r" in box, 40) or not box["r"]:
        with lock:
            posted.pop(t, None)  # the request never arrived; don't count it
    link.teardown()


def sample_health(writer):
    try:
        h = json.load(open(HEALTH))
    except (OSError, ValueError):
        return None
    c = h["counters"]
    row = {
        "time": int(time.time()),
        "rss_mb": round(c["process"].get("rss_bytes", 0) / 1e6, 1),
        "heap_mb": round(c["process"]["heap_bytes"] / 1e6, 1),
        "goroutines": c["process"]["goroutines"],
        "posts": c["hub"]["posts"],
        "internal_errors": c["hub"]["internal_errors"] + c.get("rrc", {}).get("internal_errors", 0),
        "dropped": sum(v for k, v in c["hub"].items() if k.startswith("dropped_")),
        "frames_dropped": c.get("rrc", {}).get("frames_dropped", 0),
        "lxmf_sent": c.get("lxmf", {}).get("sent", 0),
        "lxmf_propagated": c.get("lxmf", {}).get("propagated", 0),
        "lxmf_gave_up": c.get("lxmf", {}).get("gave_up", 0),
    }
    writer.writerow(row)
    return row


for d in (HUB, GROUP, PAGE):
    RNS.Transport.request_path(d)
if not wait_for(lambda: all(RNS.Identity.recall(d) for d in (HUB, GROUP, PAGE)), 120):
    log("FAIL addresses not heard")
    os._exit(1)

obs_rrc = RRCPerson("ObserverR")
obs_lxmf = LXMFPerson("ObserverL")
regulars = [RRCPerson(f"Regular{i}") for i in range(4)]
members = [LXMFPerson(f"Member{i}") for i in range(3)]
gone = LXMFPerson("GoneAway")
time.sleep(20)
gone.router.exit_handler()
RNS.Transport.deregister_destination(gone.dest)
visitors = [(RNS.Identity(), f"Visitor{i}") for i in range(2)]
log("people in place; soaking for %.0f minutes" % (DURATION / 60))

f = open(os.path.join(OUT, "health.csv"), "w", newline="")
writer = csv.DictWriter(f, fieldnames=["time", "rss_mb", "heap_mb", "goroutines", "posts", "internal_errors", "dropped",
                                        "frames_dropped", "lxmf_sent", "lxmf_propagated", "lxmf_gave_up"])
writer.writeheader()
start = time.time()
samples = []
next_sample = 0
while time.time() - start < DURATION:
    now = time.time()
    if now >= next_sample:
        row = sample_health(writer)
        f.flush()
        if row:
            samples.append(row)
            log("rss %.1f MB, heap %.1f MB, goroutines %d, posts %d, errors %d, dropped %d" %
                (row["rss_mb"], row["heap_mb"], row["goroutines"], row["posts"], row["internal_errors"], row["dropped"]))
        next_sample = now + 60
    roll = rng.random()
    if roll < 0.35:
        rng.choice(regulars).post()
    elif roll < 0.60:
        rng.choice(members).post()
    elif roll < 0.68:
        threading.Thread(target=page_post, args=rng.choice(visitors), daemon=True).start()
    elif roll < 0.70:
        threading.Thread(target=rng.choice(regulars).reconnect, daemon=True).start()
    if not obs_rrc.ok():
        log("observer RRC link dropped; reconnecting (its count may include catch-up)")
        obs_rrc.connect()
    time.sleep(rng.uniform(3, 9))

log("traffic stopped; letting deliveries settle for 5 minutes")
time.sleep(300)
sample_health(writer)
f.close()

failures = []
with lock:
    tokens = dict(posted)
    rrc_seen, lxmf_seen = dict(obs_rrc.seen), dict(obs_lxmf.seen)
missing_rrc = [t for t in tokens if rrc_seen.get(t, 0) == 0]
dup_rrc = [t for t in tokens if rrc_seen.get(t, 0) > 1]
missing_lxmf = [t for t, (way, author) in tokens.items() if lxmf_seen.get(t, 0) == 0]
dup_lxmf = [t for t in tokens if lxmf_seen.get(t, 0) > 1]
log(f"posts {len(tokens)}: RRC observer missing {len(missing_rrc)}, duplicated {len(dup_rrc)}; "
    f"LXMF observer missing {len(missing_lxmf)}, duplicated {len(dup_lxmf)}")
if missing_rrc or dup_rrc:
    failures.append("RRC observer did not see every post exactly once")
if missing_lxmf or dup_lxmf:
    failures.append("LXMF observer did not see every post exactly once")
if samples:
    if max(s["internal_errors"] for s in samples) > 0:
        failures.append("internal errors")
    if samples[-1]["dropped"] > 0 or samples[-1]["frames_dropped"] > 0:
        failures.append("dropped events or frames")
    quarter = max(1, len(samples) // 4)
    early = sum(s["rss_mb"] for s in samples[quarter:2 * quarter]) / quarter
    late = sum(s["rss_mb"] for s in samples[-quarter:]) / quarter
    g_early = sum(s["goroutines"] for s in samples[quarter:2 * quarter]) / quarter
    g_late = sum(s["goroutines"] for s in samples[-quarter:]) / quarter
    log(f"RSS second quarter {early:.1f} MB, last quarter {late:.1f} MB; goroutines {g_early:.0f} -> {g_late:.0f}")
    if late > 40:
        failures.append(f"RSS {late:.1f} MB over the 40 MB target")
    if late > early * 1.25 + 2:
        failures.append("memory grows")
    if g_late > g_early * 1.25 + 10:
        failures.append("goroutines grow")
json.dump({"posts": len(tokens), "missing_rrc": missing_rrc[:20], "dup_rrc": dup_rrc[:20], "missing_lxmf": missing_lxmf[:20],
           "dup_lxmf": dup_lxmf[:20], "failures": failures}, open(os.path.join(OUT, "result.json"), "w"), indent=2)
log("SOAK " + ("FAILED: " + "; ".join(failures) if failures else "PASSED"))
os._exit(1 if failures else 0)
