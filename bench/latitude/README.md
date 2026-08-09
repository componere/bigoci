# Latitude measurement runbook

This directory drives one paid measurement session on two Latitude bare-metal
servers: a client box that runs the harness and a registry box that serves
zot and CNCF Distribution. The scripts are thin glue over `lsh`, `ssh`, and
`docker`. An operator runs them by hand; nothing here runs in CI, on a
schedule, or unattended.

The servers bill by the hour from the moment `provision.sh` returns.
Everything below is ordered so the paid window stays as short as possible.

## Prerequisites

- `lsh` authenticated (`lsh auth status`), plus `jq` and `iperf3` locally
  irrelevant — only the boxes need iperf3; the scripts install it.
- An SSH key registered with Latitude that your local agent can use.
- Docker running locally for the smoke test.
- For the GHCR stage only: `GHCR_USERNAME` and `GHCR_TOKEN` (a PAT with
  `write:packages`) exported in the shell that runs `run.sh`.

## Order of operations

1. **Smoke test locally, before spending money.** From `bench/` (port 5555
   because macOS parks AirPlay on 5000, and the image's default config
   refuses anonymous access — mount ours):

   ```sh
   docker run -d --rm --name bench-zot -p 5555:5000 \
     -v "$PWD/latitude/zot-config.json:/etc/zot/config.json:ro" \
     ghcr.io/project-zot/zot:v2.1.20
   go run . run -spec specs/smoke.json -out results/smoke.jsonl -endpoint zot=localhost:5555
   go run . summarize -in results/smoke.jsonl
   docker stop bench-zot
   ```

   A grid with sane numbers proves the harness end to end. Do not provision
   until it does.

2. **Provision the pair** (same site, hourly billing):

   ```sh
   ./provision.sh <project> DAL
   ```

   Writes `hosts.env`. If deploy polling times out, check
   `lsh servers list --project=<project>` before retrying — never leave a
   half-provisioned pair running unnoticed.

3. **Set up the registry box and measure the link:**

   ```sh
   ./setup-registry.sh
   ```

   Starts zot on `:5000` and Distribution on `:5001` with disk-backed
   volumes, then runs a 4-stream iperf3 between the boxes. The result lands
   in `link.txt`; every throughput number in the run is read against it.

4. **Run the stages, summarizing between them:**

   ```sh
   ./run.sh ../specs/stage1.json ../results/stage1.jsonl
   (cd .. && go run . summarize -in results/stage1.jsonl)
   ```

   Edit `specs/stage2.json`'s `part_sizes` to the stage-1 winners, then run
   stage2, stage3-dist, and stage4-ghcr the same way. The GHCR stage needs
   the credentials exported (see prerequisites) and pushes to a throwaway
   private package.

   An interrupted stage is not lost: `run.sh` ships the collected rows back
   up and passes `-resume`, so re-running the script continues where it
   stopped. Resume assumes the registry box still holds what earlier
   iterations pushed — it is valid within one provisioned session, not
   across sessions.

5. **Sanity-check before teardown.** Compare the fastest grid cells against
   `link.txt`; a cell that beats the link is a bug, not a result. Confirm
   every stage's JSONL is copied back and summarized.

6. **Destroy the pair:**

   ```sh
   ./destroy.sh
   ```

   Verifies the server list afterward. If the GHCR stage ran, delete the
   `bigoci-bench` package from the account's package settings — the run
   pushed real gigabytes into it.

## Costs and expectations

Two `m4-metal-small` boxes are about $1.62/hour combined. The full staged
matrix fits in one afternoon: roughly 30 minutes for stage 1, 50 for
stage 2, 15 for stage 3, and up to 40 for the GHCR stage, plus provisioning
and setup overhead. Registry disk grows to roughly 700 GiB across the run;
the boxes carry 2x960 GB NVMe, so nothing is cleaned between stages —
teardown is the cleanup.
