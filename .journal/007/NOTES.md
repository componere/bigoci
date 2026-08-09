---
id: 007
title: Phase 6 — benchmark harness and measured defaults
started: 2026-08-09
---

## 2026-08-09 09:43 — Kickoff
Goal for the session: execute phase 6 of `.journal/002/PLAN.md` —
Measurement. Build the benchmark harness from the design's Testing section
(wall-clock throughput, push and pull, warm and cold, matrix over part
sizes × worker counts × file sizes against zot/Distribution), run the
matrix on real hardware, then set evidence-based final defaults (keep or
change 512 MiB / 4 workers) and record a data-backed verdict on the
design's open question: adaptive worker count on 429/503.

Current state of the world: phases 1–5 are complete and gated (sessions
002–006; master at e49ce48). Push/pull, the reference CLI, per-part
retries, pull resume, auth via the Docker credential ecosystem, presigned
redirects, and the GHCR conformance job are all shipped and proven.
Relevant seams awaiting this phase: the retry tag carries `after` but no
overload kind (extend only if self-tuning needs the 429/503 distinction),
and a CDN capping open-ended ranges is an accepted cost pending phase-6
measurement. Release PR #11 (0.1.0) stays open until the first release is
cut deliberately.

Plan (from PLAN.md phase 6): two PRs — (1) `feat(bench): throughput
harness against local registries` with a repeatable moon invocation and a
nightly volume job for multi-GB runs, per-commit CI staying small; (2)
`chore: set measured defaults` + docs updated in the same PR if defaults
change. Manual gates: harness reproduced on a real machine outside CI with
sanity-checked numbers; chosen defaults demonstrably beating the naive
alternatives in the recorded matrix; the adaptive-concurrency decision
recorded with data in design.md's Open Questions.

## 2026-08-09 13:00 — Design approved
Explored the repo (design doc spec, defaults at options.go:24/:32, moon/CI
patterns, e2e zot scaffolding) and the `lsh` CLI surface (plans, pricing,
SSH keys, create/destroy). Josh decided three methodology questions: two
servers in ONE site (client box + registry box running zot AND CNCF
Distribution); GHCR subset rows from the client box (throwaway private
package, PAT at run time, session-006 style); NO nightly CI job — manual
only, PLAN.md's nightly criterion gets a dated amendment.

Governing design preserved as `.journal/007/DESIGN.md` (approved plan).
Shape: third module `bench/` mirroring `cli/` (stdlib flag, local replace,
never released), one binary with `run`/`summarize`, checked-in JSON specs
as the matrix, JSONL rows out, `-resume` for paid runs, status-counting
transport for the 429/503 open question, thin operator-run lsh/ssh scripts
under `bench/latitude/`. Target servers: two m4-metal-small ($0.81/hr,
2×960GB NVMe, Ubuntu 24.04), ~3–4 h total ≈ $5–7. PR 1 = harness +
integration; measurement between PRs; PR 2 = measured defaults + docs.

Next: implementation worktree off master, build PR 1.

## 2026-08-09 13:19 — PR 1 up: the harness is built and smoke-proven
Built the whole `bench/` module in `.wt/feat-bench-harness` and opened
PR #32 (feat(bench): throughput harness against local registries, commit
8a85002, signed, verified identity). Shape as designed: spec-driven matrix
(specs/ JSON), `run`/`summarize` subcommands, JSONL rows, `-resume`,
429/503-counting transport, latitude/ runbook + 4 scripts, moon `bench`
project wired into root:check, CI cache keys + dependabot + release-please
excludes updated.

Proven, not just tested: smoke ran against a real zot v2.1.20 container —
full 4-cell matrix, verify=all, warm-push measurably faster than cold
(HEAD-skip visible: ~1.4 GB/s vs ~0.5–0.9), resume skipped all 12 rows on
re-run, summarize rendered the grids. root:check green (after a
golangci cache clean — the phantom-findings bite recurred exactly as
TECH_NOTES warns).

Learned the hard way (now tested): repository names must be entirely
lowercase while cell IDs read "MiB" — repository() lowercases on the way
in. And macOS parks AirPlay on port 5000; the zot image's default config
refuses anonymous — both now in the READMEs. lint traps for a new module:
nolintlint flags directives for rules that don't fire (errcheck ignores
defers; gosec G304 seems preset-excluded), golines rewraps >120 cols,
goconst/mnd want constants for repeated strings and bare 2s.

Waiting on PR #32 CI; then the measurement session (task 2).

## 2026-08-09 13:25 — PR #32 merged; boxes provisioned (billing running)
PR #32 squash-merged as 92e7d52 (CI green on 962f63c, which added the
provision-time SSH-keys fix). Measurement session started:

- Client: sv_bBmw0K2Mra9VR, m4-metal-small, DAL, 162.43.191.111 (SSH ok,
  12 threads, 879G NVMe root).
- Registry: sv_BoQ45AvrraMYA, f4-metal-small, DAL, 207.188.7.141 — DAL ran
  OUT OF m4-metal-small stock between the two creates (422
  SERVERS_OUT_OF_STOCK); same-site sibling plan chosen over
  destroy-and-move since the client is the measured side.
- provision.sh bug found live: `lsh servers create --json` returns an
  ARRAY, so `jq '.id'` dies (client was created, script aborted before the
  registry). Registry created by hand; hosts.env written by hand. Fix
  queued for a follow-up PR with the rest of the session's runbook
  learnings.
- GHCR stage will use the gh CLI token (scopes verified: write:packages +
  delete:packages) against the throwaway package, per the approved plan's
  session-006-style gate.

Both boxes billing since ~13:23. Next: setup-registry.sh (docker, zot
:5000, dist :5001, iperf3 link), then stages 1-4.

## 2026-08-09 14:39 — Stage 1 complete: 225 rows, zero errors
Registry-box saga first: the f4-metal-small created at 13:24
(sv_BoQ45AvrraMYA) EVAPORATED server-side — status "on" with an IP, then
404 ~15 minutes later, SSH never answered. A second f4-metal-small create
(sv_8mop5gZx9Njxv, 206.223.227.181) deployed clean in ~7 min. Lesson for
the runbook: poll `servers get` for not_found, not just readiness — a
Latitude create can be accepted and then reaped.

Link measured (iperf3, 4 streams): 9.42 Gbit/s aggregate ≈ 1178 MB/s
ceiling, ~2.35 Gbit/s per stream. Saved as results/link.txt.

Stage 1 (zot, 2GiB file, p{64,128,256,512M,1G} × w{1,2,4,8,16}, N=3,
all three scenarios): 225/225 rows clean, JSONL + summary preserved in
.journal/007/results/. Headlines:
- cold-push peaks ~950-1070 MB/s at p64-256MiB with w>=4 (near link
  ceiling); p512MiB caps ~800; p1GiB ~625 — but at 2GiB those are 4- and
  2-part files, so parallelism confounds part size; stage 2 at 16GiB
  disentangles.
- warm-push is FLAT ~1125 MB/s at every shape with wall ~1.91s: a warm
  push still reads+hashes the whole source, so it measures the client's
  single-pass sha256+read floor, not the network. The "hash floor" is
  real and worth a docs paragraph.
- cold-pull is flat ~650 MB/s for w>=2 at every part size (w1 ~585) —
  pull is bound by something part-shape-independent (~55% of link).
- w1 cold-push ~620-730 MB/s: the design's 85-90 MB/s-per-connection
  S3 expectation is far exceeded on a same-DC path; single-connection
  is not the bottleneck story here.

Stage 2 launched 14:37: 16GiB × p{64,128,256,512MiB} × w{4,8}, N=3,
cold-push+cold-pull only (warm-push at 16GiB would just re-measure the
hash floor). ~25-30 min expected.

## 2026-08-09 15:24 — Measurement complete: 333 rows, 0 errors, 0 throttles
All four stages done; boxes destroyed at 15:20 (`lsh servers list` empty;
~2h paid ≈ $4); all six GHCR throwaway packages deleted (verified 0
remain). Raw JSONL + rendered summaries for every stage in
.journal/007/results/. Stage 4 ran with the token passed via ssh stdin —
run.sh's env interpolation would put it in the process listing; fix
queued.

Stage 2 (zot, 16GiB, the deciding matrix): cold-push medians —
256MiB/w8 1069.5, 256MiB/w4 1067.4, 512MiB/w8 1052.8, 512MiB/w4 1040.0,
128MiB/w8 1056.8, 64MiB/w8 948.6. Cold-pull ~897-925 everywhere (the
~650 at 2GiB was a short-transfer artifact). 64MiB pays real per-part
overhead at scale; w8 buys 1-3% over w4.

Stage 3 (Distribution, 8GiB): cold-push 512MiB/w4 907.1 ≈ 512MiB/w8
908.4 > 256MiB/w4 758.8 (dist favors bigger parts at low workers);
pull flat ~720. Registry variance is real; 512/4 lands near-best on both
local registries.

Stage 4 (GHCR over the internet, 1GiB, N=2): pushes ~78-99 MB/s in every
shape — the design's 85-90 MB/s per-connection S3 figure measured almost
exactly; pulls 86-155, scaling with workers at 256MiB (parallel presigned
CDN). ZERO 429/503 in every row (the counting transport proves the
instrument: it counts 4xx generally, and GHCR's protocol 401s appear on
anonymous probes — none here since authenticated).

DECISION (for PR 2): keep 512 MiB / 4 workers, now measured — 512MiB is
within ~2% of best at 16GiB on zot, best-at-w4 on Distribution, tied
within noise on GHCR, and keeps the retry-cost story; w4 is within 1-3%
of w8 everywhere and saturates ~90% of a 10G link. Adaptive workers:
NOT warranted for v1 — zero throttles in 333 rows incl. w8 against GHCR,
and fixed-4 lands near-best across a 40x per-connection spread
(~90 MB/s GHCR vs ~3.7 Gbit/s single-worker same-DC). WithWorkers stays
the escape hatch. Extra doc-worthy finding: warm-push measures the
client's single-pass read+sha256 floor (~1125 MB/s on this box),
network-free — the honest ceiling story for w1.

Runbook fixes queued for a fix(bench) PR: provision.sh jq array bug
(`.[0].id`), poll-for-evaporation (a create can be accepted then reaped —
sv_BoQ45AvrraMYA), run.sh credentials via stdin, README notes. Then PR 2.

## 2026-08-09 15:31 — Phase 6 complete: all three PRs merged
Master now carries the full phase: #32 (harness, 92e7d52), #37 (runbook
hardening, f98a380), #38 (measured defaults, 1070793) — three PRs against
the plan's two; the extra fix PR carries what the live session taught the
scripts, kept separate so the chore stays a chore. CI green on all three;
every commit signed under the verified identity; `git ls-files .journal`
empty on master.

PR #38's substance: DefaultPartSize/DefaultWorkers keep 512MiB/4 with
godocs rewritten from "provisional" to measured (linking the new
docs/reference/benchmarks.md page, now in the mkdocs nav); design.md's
Defaults table cites the matrix and Open Questions now reads "None
remain" with the adaptive-concurrency NO backed by the zero-throttle
data; README status caught up (it still claimed the conformance job was
"next").

PLAN.md phase-6 boxes all checked with dated evidence (b2d5ee8),
including the amended nightly criterion (manual-only by decision).
Teardown verified twice: `lsh servers list` empty, zero bigoci-bench
packages on GHCR. Total measured spend: ~2h × two boxes ≈ $4.

Phase 7 (progress reporting, error-contract audit, docs set, v0.1.0) is
all that remains.
