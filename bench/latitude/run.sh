#!/usr/bin/env bash
# Run one stage on the client box: cross-compile the harness, ship it with
# the spec, execute, and bring the JSONL home. GHCR credentials pass through
# the environment (GHCR_USERNAME / GHCR_TOKEN) and never touch a file or an
# argument list. Re-runs of an interrupted stage resume automatically.
#
# Usage: run.sh <spec.json> <results.jsonl>
set -euo pipefail

cd "$(dirname "$0")"
source hosts.env

SPEC="${1:?usage: run.sh <spec.json> <results.jsonl>}"
OUT="${2:?usage: run.sh <spec.json> <results.jsonl>}"
SSH_USER="${SSH_USER:-ubuntu}"
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)
WORKDIR="/home/$SSH_USER/bench"

echo "== cross-compiling the harness for linux/amd64 =="
(cd .. && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/bench-linux .)

echo "== shipping binary and spec =="
ssh "${SSH_OPTS[@]}" "$SSH_USER@$CLIENT_IP" "mkdir -p $WORKDIR"
scp "${SSH_OPTS[@]}" ../bin/bench-linux "$SSH_USER@$CLIENT_IP:$WORKDIR/bench"
scp "${SSH_OPTS[@]}" "$SPEC" "$SSH_USER@$CLIENT_IP:$WORKDIR/spec.json"

REMOTE_OUT="$WORKDIR/$(basename "$OUT")"
if [[ -f "$OUT" ]]; then
  echo "== shipping existing results for resume =="
  scp "${SSH_OPTS[@]}" "$OUT" "$SSH_USER@$CLIENT_IP:$REMOTE_OUT"
fi

# Endpoint overrides only for targets the spec actually names — the harness
# refuses overrides for unknown targets, which is the typo guard.
ENDPOINTS=""
grep -q '"name": "zot"' "$SPEC" && ENDPOINTS="$ENDPOINTS -endpoint zot=$REGISTRY_IP:5000"
grep -q '"name": "dist"' "$SPEC" && ENDPOINTS="$ENDPOINTS -endpoint dist=$REGISTRY_IP:5001"

echo "== running (interrupt-safe: re-run this script to resume) =="
# The remote command sees the credentials only through its environment. -t
# ties the remote process to this terminal, so a local Ctrl-C reaches it.
ssh "${SSH_OPTS[@]}" -t "$SSH_USER@$CLIENT_IP" \
  "cd $WORKDIR && \
   GHCR_USERNAME='${GHCR_USERNAME:-}' GHCR_TOKEN='${GHCR_TOKEN:-}' \
   ./bench run -spec spec.json -out $REMOTE_OUT -resume$ENDPOINTS" \
  || echo "run exited non-zero; partial results still collected"

echo "== collecting results =="
mkdir -p "$(dirname "$OUT")"
scp "${SSH_OPTS[@]}" "$SSH_USER@$CLIENT_IP:$REMOTE_OUT" "$OUT"

echo "results in $OUT; summarize with: (cd .. && go run . summarize -in $OUT)"
