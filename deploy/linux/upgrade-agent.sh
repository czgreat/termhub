#!/usr/bin/env bash
# Replaces termhub-agent on a Linux node and restarts only the "run" process:
# the session host and every terminal under it keep running (docs/M3 第 3 节,
# KillMode=process in the unit). Run as the node's user:
#
#   bash upgrade-agent.sh --exe ./termhub-agent [--sha <sha256>]
#
# The old binary stays as termhub-agent.old-<version> for a rollback: stop the
# unit, move it back, start the unit.
set -euo pipefail

exe="./termhub-agent" sha=""
while [ $# -gt 0 ]; do
  case "$1" in
    --exe) exe="$2"; shift 2 ;;
    --sha) sha="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
dir="${XDG_DATA_HOME:-$HOME/.local/share}/termhub"
[ -x "$dir/termhub-agent" ] || { echo "not installed in $dir; use install-agent.sh" >&2; exit 1; }
[ -x "$exe" ] || { echo "no executable at $exe" >&2; exit 1; }
if [ -n "$sha" ] && [ "$(sha256sum "$exe" | cut -d' ' -f1)" != "$sha" ]; then
  echo "sha256 mismatch" >&2; exit 1
fi

hostpid() { pgrep -u "$(id -u)" -f "^$dir/termhub-agent host" | head -1 || true; }
old="$("$dir/termhub-agent" version | awk '{print $2}')"
before="$(hostpid)"
install -m 755 "$exe" "$dir/termhub-agent.new"
"$dir/termhub-agent.new" version >/dev/null
cp -p "$dir/termhub-agent" "$dir/termhub-agent.old-$old"
mv -f "$dir/termhub-agent.new" "$dir/termhub-agent" # the running host keeps its own copy
systemctl --user restart termhub-agent.service
sleep 2
echo "agent $old -> $("$dir/termhub-agent" version | awk '{print $2}')"
echo "host before: ${before:-none}  after: $(hostpid)"
systemctl --user is-active termhub-agent.service
