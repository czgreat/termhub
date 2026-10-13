#!/usr/bin/env bash
# Installs termhub-agent on a Linux machine for the user who runs this script
# (docs/Linux节点.md). Run it as that user, not as root, from the folder
# that holds the termhub-agent binary:
#
#   bash install-agent.sh --hub https://<hub>:27443 --pin <fingerprint> [--name <label>]
#
# The token is asked for (or read from standard input when that is not a
# terminal); it never appears in the process list or the shell history, so
# there is no --token option. What it does:
#   1. copies the binary to ~/.local/share/termhub/termhub-agent (0700 folder);
#   2. enrols: one real connection to the Hub; nothing is written if it fails;
#   3. writes the systemd user unit termhub-agent.service and starts it;
#   4. turns on lingering, so the agent runs from boot without a login.
# The CLIs (Claude Code, Codex) and their logins are not touched.
set -euo pipefail

hub="" pin="" name="" exe="./termhub-agent" linger=1
while [ $# -gt 0 ]; do
  case "$1" in
    --hub) hub="$2"; shift 2 ;;
    --pin) pin="$2"; shift 2 ;;
    --token) echo "--token is not accepted: a token on the command line ends up in the shell history; paste it when asked" >&2; exit 2 ;;
    --name) name="$2"; shift 2 ;;
    --exe) exe="$2"; shift 2 ;;
    --no-linger) linger=0; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$hub" ] || { echo "usage: bash install-agent.sh --hub https://<hub>:27443 --pin <fingerprint> [--name <label>]" >&2; exit 2; }
[ "$(id -u)" -ne 0 ] || { echo "run this as the user who will use the node, not as root" >&2; exit 1; }
[ -x "$exe" ] || { echo "no executable termhub-agent at $exe (use --exe)" >&2; exit 1; }
command -v systemctl >/dev/null || { echo "systemd is required" >&2; exit 1; }

dir="${XDG_DATA_HOME:-$HOME/.local/share}/termhub"
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
install -d -m 700 "$dir"
chmod 700 "$dir"
install -m 755 "$exe" "$dir/termhub-agent.new"
mv -f "$dir/termhub-agent.new" "$dir/termhub-agent"
echo "installed $("$dir/termhub-agent" version)"

token=""
if [ -t 0 ]; then
  read -r -s -p "node token (thn_...): " token
  echo
else
  read -r token || true # a last line without a newline still counts
fi
[ -n "$token" ] || { echo "no token given" >&2; exit 1; }
args=(enroll --hub "$hub" --token -)
[ -n "$pin" ] && args+=(--pin "$pin")
[ -n "$name" ] && args+=(--name "$name")
printf '%s\n' "$token" | "$dir/termhub-agent" "${args[@]}"
token=""

install -d -m 755 "$unit_dir"
cat > "$unit_dir/termhub-agent.service" <<EOF
[Unit]
Description=termhub agent: connects this machine to the termhub Hub
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart="$dir/termhub-agent" run
Restart=always
RestartSec=5
# Stopping or restarting the agent must not end the session host it started,
# nor the terminals and CLIs under it (docs/M3 第 3 节).
KillMode=process

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable termhub-agent.service >/dev/null
systemctl --user restart termhub-agent.service

if [ "$linger" = 1 ]; then
  if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]; then
    loginctl enable-linger "$USER" 2>/dev/null || sudo loginctl enable-linger "$USER" ||
      echo "could not turn on lingering: the agent then runs only while $USER is logged in. As an administrator: sudo loginctl enable-linger $USER" >&2
  fi
  echo "linger: $(loginctl show-user "$USER" -p Linger --value 2>/dev/null || echo unknown)"
fi
sleep 2
systemctl --user --no-pager --lines=0 status termhub-agent.service | sed -n 1,4p
echo "logs: $dir/agent/logs/agent.log and $dir/host/logs/host.log"
