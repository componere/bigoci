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
