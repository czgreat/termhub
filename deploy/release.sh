#!/usr/bin/env bash
# Builds termhub from this checkout and (re)deploys the Hub on a Docker host.
#
#   bash deploy/release.sh user@host [dir]   deploy over SSH (default dir: termhub, under the remote home)
#   bash deploy/release.sh local [dir]       deploy on this machine (default dir: ~/termhub)
#
# Needs on the build machine: git, Go 1.27+, Node.js 22.13+ with npm, and ssh/scp
# for a remote target. Needs on the target: Docker with the compose plugin.
# Environment: TH_GO (go binary, default "go"), TH_ARCH (amd64 or arm64,
# default amd64), GOPROXY, TH_ALLOW_DIRTY=1 (deploy uncommitted changes).
#
# What it does: npm run build → cross-compile the Linux Hub with the web UI
# embedded, and the Windows agent → copy the Hub, deploy/Dockerfile.prebuilt
# and the compose file to the target → docker build there (pulls no base
# image: the image is the static binary on scratch) → docker compose up -d →
# wait for the health check → print the log lines the first login needs.
# The first run on a target only creates .env and hub.env from the examples
# and stops, so you can fill them in. The data directory is never touched.
# The agent ends up in dist/termhub-agent.exe (see docs/部署指南.md).
set -euo pipefail

target="${1:-}"
if [ -z "$target" ]; then
  sed -n '2,6p' "$0" >&2
  exit 2
fi
if [ "$target" = local ]; then dir="${2:-$HOME/termhub}"; else dir="${2:-termhub}"; fi
GO="${TH_GO:-go}"
ARCH="${TH_ARCH:-amd64}"
NPM=npm
command -v npm.cmd >/dev/null 2>&1 && NPM=npm.cmd   # Git Bash on Windows

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
version="0.1.0-$(git rev-parse --short HEAD)"
if [ -n "$(git status --porcelain)" ]; then
  if [ "${TH_ALLOW_DIRTY:-}" != 1 ]; then
    echo "working tree not clean: commit first (the version is the commit), or set TH_ALLOW_DIRTY=1" >&2
    exit 1
  fi
  version="$version-dirty"
fi

# on: run a shell command on the target; put: copy local files into a target directory
on() { if [ "$target" = local ]; then bash -c "$1"; else ssh -o BatchMode=yes "$target" "$1"; fi; }
put() {
  local to="$1"; shift
  if [ "$target" = local ]; then cp -r "$@" "$to/"; else scp -q -r -o BatchMode=yes "$@" "$target:$to/"; fi
}
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
D="$(q "$dir")"

echo "== target $target:$dir"
on "command -v docker >/dev/null" || { echo "Docker is needed on the target" >&2; exit 1; }
on "docker compose version >/dev/null 2>&1" ||
  { echo "docker compose does not work for this user on the target: install the compose plugin, or add the user to the docker group" >&2; exit 1; }
on "mkdir -p $D/data"
if ! on "test -f $D/.env && test -f $D/hub.env"; then
  on "test -f $D/.env" || put "$dir" deploy/.env.example
  on "test -f $D/hub.env" || put "$dir" deploy/hub.env.example
  on "cd $D && { test -f .env || mv .env.example .env; } && { test -f hub.env || mv hub.env.example hub.env; } && chmod 600 hub.env"
  echo "created $dir/.env and $dir/hub.env on the target from the examples."
  echo "fill in TH_BIND_ADDR (.env) and TH_PUBLIC_URL (hub.env), then run this again."
  exit 1
fi

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
echo "== web UI"
(cd web && { [ -d node_modules ] || "$NPM" ci; } && TH_WEB_VERSION="$version" "$NPM" run build >/dev/null)
echo "== hub $version (linux/$ARCH)"
GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/termhub" ./cmd/termhub
echo "== agent $version (dist/termhub-agent.exe)"
mkdir -p dist
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w -X main.version=$version" -o dist/termhub-agent.exe ./cmd/termhub-agent
cp deploy/Dockerfile.prebuilt "$stage/Dockerfile"
# the unprivileged user and the empty data directory, so the image needs no base
mkdir -p "$stage/etc" "$stage/data"
printf 'nonroot:x:65532:65532:nonroot:/:/sbin/nologin\n' > "$stage/etc/passwd"
printf 'nonroot:x:65532:\n' > "$stage/etc/group"
printf '' > "$stage/data/.keep"

echo "== copy"
on "rm -rf $D/build && mkdir -p $D/build"
put "$dir/build" "$stage/termhub" "$stage/Dockerfile" "$stage/etc" "$stage/data"
put "$dir" deploy/docker-compose.yml

echo "== image and container"
# .env keeps your settings; only the TERMHUB_* lines are rewritten here. The
# container runs as the user that owns ./data, never root: logged in as root,
# ./data goes to the image's unprivileged user 65532 instead.
on "set -e
  cd $D/build && docker build -q -t 'localhost/termhub:$version' -t localhost/termhub:latest . >/dev/null
  cd $D
  uid=\$(id -u); gid=\$(id -g)
  if [ \"\$uid\" = 0 ]; then uid=65532; gid=65532; chown -R 65532:65532 data; fi
  { grep -v '^TERMHUB_' .env || true; printf 'TERMHUB_VERSION=%s\nTERMHUB_UID=%s\nTERMHUB_GID=%s\n' '$version' \"\$uid\" \"\$gid\"; } > .env.tmp
  mv .env.tmp .env
  docker compose up -d --remove-orphans
  for i in \$(seq 1 60); do
    s=\$(docker inspect -f '{{.State.Health.Status}}' termhub 2>/dev/null || echo none)
    [ \"\$s\" = healthy ] && break
    sleep 2
  done
  echo \"health: \$s\"
  if [ \"\$s\" != healthy ]; then
    docker logs --tail 40 termhub 2>&1 || true
    echo 'Hub did not become healthy; deployment failed' >&2
    exit 1
  fi
  docker logs termhub 2>&1 | grep -E 'setup_token|cert_fingerprint|version' | tail -5
"
echo "== done. Open TH_PUBLIC_URL in a browser; the agent is dist/termhub-agent.exe"
