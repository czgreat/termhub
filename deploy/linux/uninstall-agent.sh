#!/usr/bin/env bash
# Removes termhub-agent from a Linux node: stops the unit, ends the session
# host and with it every termhub session on this machine, removes the unit
# and the installation folder. Lingering is left as it is (other services of
# the user may need it): sudo loginctl disable-linger <user> turns it off.
# The CLIs, their logins and their history are not touched.
set -euo pipefail

dir="${XDG_DATA_HOME:-$HOME/.local/share}/termhub"
unit="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/termhub-agent.service"
systemctl --user disable --now termhub-agent.service 2>/dev/null || true
# SIGTERM: the host ends its sessions itself (up to 15 s), whatever ignores
# SIGHUP included. Only what is still there after that gets SIGKILL.
pkill -TERM -u "$(id -u)" -f "^$dir/termhub-agent host" 2>/dev/null || true
for _ in $(seq 20); do
  pgrep -u "$(id -u)" -f "^$dir/termhub-agent host" >/dev/null || break
  sleep 1
done
pkill -KILL -u "$(id -u)" -f "^$dir/termhub-agent host" 2>/dev/null || true
rm -f "$unit"
systemctl --user daemon-reload
rm -rf "$dir"
echo "removed. Delete the node in the Hub too (管理 → 节点)."
