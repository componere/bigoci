#!/usr/bin/env bash
# Run one stage on the client box: cross-compile the harness, ship it with
# the spec, execute, and bring the JSONL home. GHCR credentials pass through
# the environment (GHCR_USERNAME / GHCR_TOKEN) and never touch a file or an
# argument list. Re-runs of the same spec on one client resume automatically;
# another spec or client gets a distinct run ID.
#
# Usage: run.sh <spec.json> <results.jsonl>
set -euo pipefail

cd "$(dirname "$0")"
source hosts.env

SPEC="${1:?usage: run.sh <spec.json> <results.jsonl>}"
OUT="${2:?usage: run.sh <spec.json> <results.jsonl>}"
SSH_USER="${SSH_USER:-ubuntu}"
KNOWN_HOSTS="$PWD/known_hosts.$CLIENT_ID"
SSH_OPTS=(-o UserKnownHostsFile="$KNOWN_HOSTS" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)
WORKDIR="/home/$SSH_USER/bench"
CLIENT_RUN_ID="$(printf '%s' "$CLIENT_ID" | tr '[:upper:]' '[:lower:]')"
if ! SPEC_RUN_ID="$(jq -er '.run_id | strings' "$SPEC")"; then
  echo "spec must contain a string run_id" >&2
  exit 2
fi
if [[ ! "$SPEC_RUN_ID" =~ ^[a-z0-9._-]+$ ]]; then
  echo "spec run_id must use lowercase letters, digits, dot, dash, and underscore" >&2
  exit 2
fi
RUN_ID="${BENCH_RUN_ID:-${SPEC_RUN_ID}-latitude-${CLIENT_RUN_ID}}"
if [[ ! "$RUN_ID" =~ ^[a-z0-9._-]+$ ]]; then
  echo "BENCH_RUN_ID must use lowercase letters, digits, dot, dash, and underscore" >&2
  exit 2
fi

echo "== cross-compiling the harness for linux/amd64 =="
if [[ -n "$(git -C .. status --porcelain --untracked-files=normal)" ]]; then
  echo "repository changes are present; commit them before a provenance-bearing benchmark build" >&2
  exit 1
fi
HARNESS_COMMIT="$(git -C .. rev-parse HEAD)"
(cd .. && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -ldflags "-X main.injectedCommit=$HARNESS_COMMIT" -o bin/bench-linux .)

echo "== shipping binary and spec =="
ssh "${SSH_OPTS[@]}" "$SSH_USER@$CLIENT_IP" "mkdir -p $WORKDIR"
scp "${SSH_OPTS[@]}" ../bin/bench-linux "$SSH_USER@$CLIENT_IP:$WORKDIR/bench"
scp "${SSH_OPTS[@]}" "$SPEC" "$SSH_USER@$CLIENT_IP:$WORKDIR/spec.json"

REMOTE_OUT="$WORKDIR/results-$RUN_ID.jsonl"
if [[ -f "$OUT" ]]; then
  echo "== shipping existing results for resume =="
  scp "${SSH_OPTS[@]}" "$OUT" "$SSH_USER@$CLIENT_IP:$REMOTE_OUT"
fi

# Endpoint overrides only for targets the spec actually names — the harness
# refuses overrides for unknown targets, which is the typo guard.
REMOTE_ARGS=(./bench run -spec spec.json -out "$REMOTE_OUT" -resume -run-id "$RUN_ID")
if jq -e '.targets | any(.name == "zot")' "$SPEC" >/dev/null; then
  REMOTE_ARGS+=(-endpoint "zot=$REGISTRY_IP:5000")
fi
if jq -e '.targets | any(.name == "dist")' "$SPEC" >/dev/null; then
  REMOTE_ARGS+=(-endpoint "dist=$REGISTRY_IP:5001")
fi
printf -v REMOTE_COMMAND '%q ' "${REMOTE_ARGS[@]}"
printf -v REMOTE_WORKDIR '%q' "$WORKDIR"

echo "== running $RUN_ID (interrupt-safe: re-run this script to resume) =="
# The credentials travel over stdin into the remote shell's variables —
# interpolating them into the ssh argument string would print them in the
# local and remote process listings for the whole run.
set +e
printf '%s\n%s\n' "${GHCR_USERNAME:-}" "${GHCR_TOKEN:-}" | ssh "${SSH_OPTS[@]}" "$SSH_USER@$CLIENT_IP" \
	"read -r GHCR_USERNAME && read -r GHCR_TOKEN && export GHCR_USERNAME GHCR_TOKEN; \
	 cd $REMOTE_WORKDIR && $REMOTE_COMMAND"
RUN_STATUS=$?
set -e
if ((RUN_STATUS != 0)); then
  echo "run exited non-zero; collecting partial results before returning status $RUN_STATUS" >&2
fi

echo "== collecting results =="
mkdir -p "$(dirname "$OUT")"
scp "${SSH_OPTS[@]}" "$SSH_USER@$CLIENT_IP:$REMOTE_OUT" "$OUT"

echo "results in $OUT; summarize with: (cd .. && go run . summarize -in $OUT)"
exit "$RUN_STATUS"
