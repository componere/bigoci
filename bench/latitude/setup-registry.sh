#!/usr/bin/env bash
# Prepare the pair: docker + both registries on the registry box, and a raw
# link measurement between the boxes so every throughput number in the run
# has a denominator. Idempotent — safe to re-run after a partial failure.
#
# Usage: setup-registry.sh   (after provision.sh wrote hosts.env)
set -euo pipefail

cd "$(dirname "$0")"
source hosts.env

SSH_USER="${SSH_USER:-ubuntu}"
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)

# Registry images, pinned. zot matches the repository's e2e pin; a benchmark
# against a floating tag would measure a moving target.
ZOT_IMAGE="ghcr.io/project-zot/zot:v2.1.20"
DIST_IMAGE="registry:2.8.3"

run_on() {
  local host="$1"
  shift
  ssh "${SSH_OPTS[@]}" "$SSH_USER@$host" "$@"
}

echo "== installing docker + iperf3 on the registry box =="
run_on "$REGISTRY_IP" 'command -v docker >/dev/null || curl -fsSL https://get.docker.com | sudo sh'
run_on "$REGISTRY_IP" 'command -v iperf3 >/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq iperf3)'

echo "== installing iperf3 on the client box =="
run_on "$CLIENT_IP" 'command -v iperf3 >/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq iperf3)'

echo "== starting zot (:5000) and distribution (:5001) =="
scp "${SSH_OPTS[@]}" zot-config.json "$SSH_USER@$REGISTRY_IP:/tmp/zot-config.json"
run_on "$REGISTRY_IP" "
  sudo mkdir -p /var/lib/bench-zot /var/lib/bench-dist
  sudo docker rm -f bench-zot bench-dist 2>/dev/null || true
  sudo docker run -d --name bench-zot --restart unless-stopped \
    -p 5000:5000 \
    -v /var/lib/bench-zot:/var/lib/registry \
    -v /tmp/zot-config.json:/etc/zot/config.json:ro \
    $ZOT_IMAGE
  sudo docker run -d --name bench-dist --restart unless-stopped \
    -p 5001:5000 \
    -v /var/lib/bench-dist:/var/lib/registry \
    $DIST_IMAGE
"

echo "== waiting for both registries to answer =="
for port in 5000 5001; do
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 3 "http://$REGISTRY_IP:$port/v2/" >/dev/null 2>&1; then
      echo "registry on :$port is answering"
      break
    fi
    sleep 2
  done
  curl -fsS --max-time 3 "http://$REGISTRY_IP:$port/v2/" >/dev/null
done

echo "== measuring the raw link (client -> registry) =="
run_on "$REGISTRY_IP" 'pkill -x iperf3 2>/dev/null || true; nohup iperf3 -s -1 >/tmp/iperf3.log 2>&1 & sleep 1'
run_on "$CLIENT_IP" "iperf3 -c $REGISTRY_IP -t 10 -P 4" | tee link.txt

echo "setup complete; link measurement saved to link.txt"
echo "next: ./run.sh ../specs/stage1.json ../results/stage1.jsonl"
