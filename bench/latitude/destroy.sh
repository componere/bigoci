#!/usr/bin/env bash
# Tear the benchmark pair down. Billing stops when the servers are gone, so
# this runs the moment the numbers are summarized and sanity-checked — and
# never before that check, because a destroyed box cannot re-run a suspect
# stage.
#
# Usage: destroy.sh
set -euo pipefail

cd "$(dirname "$0")"
source hosts.env

for id in "$CLIENT_ID" "$REGISTRY_ID"; do
  echo "destroying $id..."
  lsh servers destroy --id="$id" --no-input
done

echo "== remaining servers (should not list the bench pair) =="
lsh servers list --project="$PROJECT"

echo "if a GHCR stage ran, delete the throwaway package too:"
echo "  https://github.com/settings > Packages > bigoci-bench"
rm -f hosts.env "known_hosts.$CLIENT_ID"
