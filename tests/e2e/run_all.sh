#!/usr/bin/env bash
# Runs every end-to-end test on a private Reticulum network: rnsd and an
# lxmd propagation node on 127.0.0.1, a fresh scotmesh-chat, stock clients.
#   PYTHON  python with rns, lxmf and nomadnet (default: python3)
#   WORK    scratch directory (default: a new temp dir)
#   ONLY    rrc | group | page | people | soak, to run one suite (soak only runs when asked)
#   SOAK_MINUTES  length of the soak (default 60)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
PYTHON=${PYTHON:-python3}
BIN=$(dirname "$PYTHON")
WORK=${WORK:-$(mktemp -d -t scotmesh-e2e-XXXX)}
PORT=${TEST_PORT:-4299}
mkdir -p "$WORK/rnsd" "$WORK/lxmd/cfg" "$WORK/lxmd/rns" "$WORK/hub"

cat > "$WORK/rnsd/config" <<CFG
[reticulum]
  enable_transport = Yes
  share_instance = No
[logging]
  loglevel = 3
[interfaces]
  [[Test backbone]]
    type = TCPServerInterface
    enabled = yes
    listen_ip = 127.0.0.1
    listen_port = $PORT
CFG
sed "s/listen_ip.*//; s/listen_port.*//; s/TCPServerInterface/TCPClientInterface\n    target_host = 127.0.0.1\n    target_port = $PORT/; s/enable_transport = Yes/enable_transport = No/" "$WORK/rnsd/config" > "$WORK/lxmd/rns/config"
cat > "$WORK/lxmd/cfg/config" <<CFG
[propagation]
enable_node = yes
node_name = Test Propagation
announce_interval = 1
announce_at_start = yes
autopeer = no
propagation_cost = 0
[lxmf]
display_name = TestPN
announce_at_start = no
[logging]
loglevel = 4
CFG

pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; [ -f "$WORK/hub.pid" ] && kill "$(cat "$WORK/hub.pid")" 2>/dev/null || true; }
trap cleanup EXIT

"$BIN/rnsd" --config "$WORK/rnsd" >"$WORK/rnsd.log" 2>&1 & pids+=($!)
# The hub gives up if the backbone refuses its first connection, so wait
# until rnsd is listening (it can take a while after a previous run).
for _ in $(seq 60); do (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null && break; sleep 0.5; done
"$BIN/lxmd" --config "$WORK/lxmd/cfg" --rnsconfig "$WORK/lxmd/rns" >"$WORK/lxmd.log" 2>&1 & pids+=($!)
for _ in $(seq 30); do [ -f "$WORK/lxmd/cfg/identity" ] && break; sleep 1; done
pn=$("$PYTHON" -c "import RNS,sys; i=RNS.Identity.from_file(sys.argv[1]); print(RNS.Destination.hash(i,'lxmf','propagation').hex())" "$WORK/lxmd/cfg/identity")

# An owner for the tests that need one.
owner=$("$PYTHON" -c "import RNS,sys; i=RNS.Identity(); i.to_file(sys.argv[1]); print(i.hash.hex())" "$WORK/owner.id")
export OWNER_ID="$WORK/owner.id"

cat > "$WORK/hub/config.toml" <<CFG
data_dir = "$WORK/hub/data"
backbone = "127.0.0.1:$PORT"
admins = ["$owner"]
log_level = "debug"
[group]
propagation_node = "$pn"
announce_interval = "10m"
[rrc]
greeting = ["Welcome to the test hub."]
announce_interval = "10m"
CFG
(cd "$repo" && go build -o "$WORK/scotmesh-chat" ./cmd/scotmesh-chat)
start_hub() { "$WORK/scotmesh-chat" --config "$WORK/hub/config.toml" >>"$WORK/hub.log" 2>&1 & echo $! > "$WORK/hub.pid"; }
start_hub
for _ in $(seq 30); do
  hubaddr=$(grep -o 'msg="RRC hub" address=[0-9a-f]*' "$WORK/hub.log" | head -1 | sed 's/.*address=//' || true)
  groupaddr=$(grep -o 'msg="LXMF group" address=[0-9a-f]*' "$WORK/hub.log" | head -1 | sed 's/.*address=//' || true)
  pageaddr=$(grep -o 'msg="chat page" address=[0-9a-f]*' "$WORK/hub.log" | head -1 | sed 's/.*address=//' || true)
  [ -n "$hubaddr" ] && [ -n "$groupaddr" ] && [ -n "$pageaddr" ] && break
  sleep 1
done
echo "hub $hubaddr, group $groupaddr, page $pageaddr, propagation node $pn (work dir $WORK)"
if [ -z "$hubaddr" ] || [ -z "$groupaddr" ] || [ -z "$pageaddr" ]; then
  echo "the hub didn't start; its log ends:"; tail -5 "$WORK/hub.log"; exit 1
fi
export TEST_PORT=$PORT
export RESTART_CMD="kill \$(cat $WORK/hub.pid); sleep 2; $WORK/scotmesh-chat --config $WORK/hub/config.toml >>$WORK/hub.log 2>&1 & echo \$! > $WORK/hub.pid"

status=0
want() { { [ -z "${ONLY:-}" ] && [ "$1" != soak ]; } || [ "${ONLY:-}" = "$1" ]; }
if want rrc; then
  echo "== RRC interop"; "$PYTHON" "$here/rrc_interop.py" "$hubaddr" || status=1
fi
if want page; then
  echo "== chat page interop"; "$PYTHON" "$here/page_interop.py" "$hubaddr" "$pageaddr" || status=1
fi
if want group; then
  echo "== LXMF group interop"; "$PYTHON" "$here/group_interop.py" "$hubaddr" "$groupaddr" "$pn" || status=1
fi
if want people; then
  echo "== people interop"; "$PYTHON" "$here/people_interop.py" "$hubaddr" "$groupaddr" "$pageaddr" || status=1
fi
if want soak; then
  echo "== soak (${SOAK_MINUTES:-60} minutes)"
  "$PYTHON" "$here/soak.py" "$hubaddr" "$groupaddr" "$pageaddr" "$WORK/hub/data/health.json" "${SOAK_MINUTES:-60}" "$WORK/soak" || status=1
fi
exit $status
