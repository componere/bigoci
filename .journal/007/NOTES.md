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
