#!/usr/bin/env bash
# update-docker.sh - rebuild the image from local sources and swap the container
# (works in Git Bash / Linux)
#
# Data safety: auths/, data/ and config.json are host bind mounts,
# so rebuilding the container never touches account-pool data.
# Flow: build new image (on failure the old container keeps running)
#   -> remove old container -> start new container -> readiness probe
#   -> on failure, roll back to the previous image and print its logs.
#
# Usage: ./update-docker.sh [image:tag]    # default: workbuddy2api-panel:local
set -euo pipefail

IMAGE="${1:-workbuddy2api-panel:local}"
NAME="workbuddy2api"
PORT="7863"
ROOT="$(cd "$(dirname "$0")" && pwd)"

# Convert Git Bash /e/... style path to E:/... (docker mounts need drive paths)
ROOT="$(cygpath -m "$ROOT" 2>/dev/null || echo "$ROOT")"

die() { echo "[ERR] $*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker not available (not installed or not in PATH)"
[ -f "$ROOT/config.json" ] || die "missing $ROOT/config.json - a missing mount file becomes a directory and breaks startup (run: cp config.example.json config.json)"
[ -d "$ROOT/auths" ] || die "missing $ROOT/auths directory"

# Remember the image of the current container (empty = no container, no rollback path)
OLD_IMAGE="$(docker inspect -f '{{.Config.Image}}' "$NAME" 2>/dev/null || true)"

run_container() { # $1 = image name
  docker run -d --name "$NAME" -p "$PORT:$PORT" -e TZ=Asia/Shanghai \
    -v "$ROOT/auths:/app/auths" \
    -v "$ROOT/data:/app/data" \
    -v "$ROOT/config.json:/app/config.json" \
    --restart unless-stopped "$1" >/dev/null
}

echo "[1/4] building image $IMAGE ..."
docker build -t "$IMAGE" "$ROOT"   # on failure set -e exits, old container untouched

echo "[2/4] removing old container ..."
docker rm -f "$NAME" >/dev/null 2>&1 || true

echo "[3/4] starting new container ..."
run_container "$IMAGE"

echo "[4/4] waiting for readiness (up to 60s) ..."
code="000"
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -m 3 -w '%{http_code}' "http://localhost:$PORT/healthz" 2>/dev/null || echo 000)"
  [ "$code" != "000" ] && break   # 200 = healthy / 503 = up but empty account pool, both count as started
  sleep 2
done

if [ "$code" = "200" ] || [ "$code" = "503" ]; then
  echo "Done: container $NAME now runs $IMAGE"
  echo "  healthz: $(curl -sf -m 3 "http://localhost:$PORT/healthz" || echo "503 - account pool empty, add/check accounts in the panel")"
  echo "  panel: http://localhost:$PORT/panel/"
  exit 0
fi

echo "[ERR] new container not ready (healthz code=$code), last 30 log lines:" >&2
docker logs --tail 30 "$NAME" >&2 || true
docker rm -f "$NAME" >/dev/null 2>&1 || true
if [ -n "$OLD_IMAGE" ]; then
  echo "rolling back to previous image $OLD_IMAGE ..." >&2
  run_container "$OLD_IMAGE"
  echo "rolled back. inspect the new image with: docker run --rm -it $IMAGE sh" >&2
else
  echo "no previous container to roll back to." >&2
fi
exit 1
