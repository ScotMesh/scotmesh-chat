#!/bin/bash
# Install or upgrade scotmesh-chat on this machine (run as root from the folder
# holding scotmesh-chat, scotmesh-chat.service and config.example.toml).
# The previous binary is kept as scotmesh-chat.previous for rollback.
set -euo pipefail
id scotmesh-chat >/dev/null 2>&1 || useradd --system --home /var/lib/scotmesh-chat --shell /usr/sbin/nologin scotmesh-chat
install -d -o scotmesh-chat -g scotmesh-chat -m 700 /var/lib/scotmesh-chat
install -d -m 755 /opt/scotmesh-chat /etc/scotmesh-chat
[ -e /etc/scotmesh-chat/config.toml ] || install -m 644 config.example.toml /etc/scotmesh-chat/config.toml
# Refuse to install a binary that can't read the config it will run with.
./scotmesh-chat --config /etc/scotmesh-chat/config.toml --check-config
[ -e /opt/scotmesh-chat/scotmesh-chat ] && cp -p /opt/scotmesh-chat/scotmesh-chat /opt/scotmesh-chat/scotmesh-chat.previous
install -m 755 scotmesh-chat /opt/scotmesh-chat/scotmesh-chat.new
mv /opt/scotmesh-chat/scotmesh-chat.new /opt/scotmesh-chat/scotmesh-chat
install -m 644 scotmesh-chat.service /etc/systemd/system/scotmesh-chat.service
systemctl daemon-reload
systemctl enable scotmesh-chat >/dev/null
systemctl restart scotmesh-chat
sleep 5
systemctl is-active scotmesh-chat
