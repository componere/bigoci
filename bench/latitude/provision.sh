#!/usr/bin/env bash
# Provision the two-box benchmark pair: a client box and a registry box in
# the same site. Writes hosts.env beside this script for the other scripts
# to source. Servers bill by the hour from this moment — run destroy.sh as
# soon as the numbers are in.
#
# Usage: provision.sh <project-id-or-slug> <site> [plan]
set -euo pipefail

cd "$(dirname "$0")"

PROJECT="${1:?usage: provision.sh <project> <site> [plan]}"
SITE="${2:?usage: provision.sh <project> <site> [plan]}"
PLAN="${3:-m4-metal-small}"
OS="ubuntu_24_04_x64_lts"

# Without an SSH key on the server there is no way in, and a box nobody can
# reach still bills by the hour. Attach every key the project has.
SSH_KEYS="$(lsh ssh_keys list --project="$PROJECT" --json | jq -r '[.[].id] | join(",")')"
if [[ -z "$SSH_KEYS" ]]; then
  echo "the project has no SSH keys registered; add one before provisioning" >&2
  exit 1
fi

create() {
  local hostname="$1"
  lsh servers create \
    --project="$PROJECT" \
    --site="$SITE" \
    --plan="$PLAN" \
    --operating_system="$OS" \
    --hostname="$hostname" \
    --ssh_keys="$SSH_KEYS" \
    --billing=hourly \
    --json | jq -r '.id // empty'
}

echo "creating bigoci-bench-client and bigoci-bench-registry ($PLAN, $SITE, hourly)..."
CLIENT_ID="$(create bigoci-bench-client)"
REGISTRY_ID="$(create bigoci-bench-registry)"

if [[ -z "$CLIENT_ID" || -z "$REGISTRY_ID" ]]; then
  echo "server creation did not return IDs; check 'lsh servers list' before retrying" >&2
  exit 1
fi
echo "client=$CLIENT_ID registry=$REGISTRY_ID"

# Wait for both to deploy. A fresh bare-metal deploy takes a few minutes;
# poll well past that before declaring failure.
ip_of() {
  lsh servers get --id="$1" --json |
    jq -r '.. | objects | select(has("primary_ipv4")) | .primary_ipv4 // empty' | head -1
}
status_of() {
  lsh servers get --id="$1" --json |
    jq -r '.. | objects | select(has("status")) | .status // empty' | head -1
}

for _ in $(seq 1 90); do
  CLIENT_STATUS="$(status_of "$CLIENT_ID")"
  REGISTRY_STATUS="$(status_of "$REGISTRY_ID")"
  echo "client: ${CLIENT_STATUS:-?}  registry: ${REGISTRY_STATUS:-?}"
  if [[ "$CLIENT_STATUS" == "on" && "$REGISTRY_STATUS" == "on" ]]; then
    break
  fi
  sleep 20
done

CLIENT_IP="$(ip_of "$CLIENT_ID")"
REGISTRY_IP="$(ip_of "$REGISTRY_ID")"
if [[ -z "$CLIENT_IP" || -z "$REGISTRY_IP" ]]; then
  echo "could not resolve server IPs; inspect 'lsh servers get' output manually" >&2
  exit 1
fi

cat > hosts.env <<EOF
PROJECT=$PROJECT
CLIENT_ID=$CLIENT_ID
CLIENT_IP=$CLIENT_IP
REGISTRY_ID=$REGISTRY_ID
REGISTRY_IP=$REGISTRY_IP
EOF
echo "wrote hosts.env: client $CLIENT_IP, registry $REGISTRY_IP"
echo "next: ./setup-registry.sh"
