#!/usr/bin/env bash
# Run one stage on the client box: cross-compile the harness, ship it with
# the spec, execute, and bring the JSONL home. GHCR credentials pass through
# the environment (GHCR_USERNAME / GHCR_TOKEN) and never touch a file or an
# argument list. Re-runs on the same client resume automatically; a new
# client gets a distinct run ID and cannot silently reuse retained results.
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
RUN_ID="${BENCH_RUN_ID:-latitude-${CLIENT_ID}}"
if [[ ! "$RUN_ID" =~ ^[a-z0-9._-]+$ ]]; then
  echo "BENCH_RUN_ID must use lowercase letters, digits, dot, dash, and underscore" >&2
  exit 2
fi

echo "== cross-compiling the harness for linux/amd64 =="
if ! git -C .. diff --quiet || ! git -C .. diff --cached --quiet; then
  echo "tracked changes are present; commit them before a provenance-bearing benchmark build" >&2
  exit 1
fi
HARNESS_COMMIT="$(git -C .. rev-parse --short=12 HEAD)"
(cd .. && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -ldflags "-X main.injectedCommit=$HARNESS_COMMIT" -o bin/bench-linux .)

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

echo "== running $RUN_ID (interrupt-safe: re-run this script to resume) =="
# The credentials travel over stdin into the remote shell's variables —
# interpolating them into the ssh argument string would print them in the
# local and remote process listings for the whole run.
printf '%s\n%s\n' "${GHCR_USERNAME:-}" "${GHCR_TOKEN:-}" | ssh "${SSH_OPTS[@]}" "$SSH_USER@$CLIENT_IP" \
	"read -r GHCR_USERNAME && read -r GHCR_TOKEN && export GHCR_USERNAME GHCR_TOKEN; \
	 cd $WORKDIR && ./bench run -spec spec.json -out $REMOTE_OUT -resume -run-id $RUN_ID$ENDPOINTS" \
  || echo "run exited non-zero; partial results still collected"

echo "== collecting results =="
mkdir -p "$(dirname "$OUT")"
scp "${SSH_OPTS[@]}" "$SSH_USER@$CLIENT_IP:$REMOTE_OUT" "$OUT"

echo "results in $OUT; summarize with: (cd .. && go run . summarize -in $OUT)"
