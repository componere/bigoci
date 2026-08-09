---
id: 007
title: Phase 6 — benchmark harness and measured defaults
date: 2026-08-09
status: complete
repos_touched: [componere/bigoci]
related_sessions: [001, 002, 003, 004, 005, 006]
---

## Goal
Execute phase 6 of `.journal/002/PLAN.md`: build the benchmark harness from
the design's Testing section, run the part-size × worker-count × file-size
matrix on real hardware, set evidence-based final defaults (512 MiB / 4
workers were provisional), and record a data-backed verdict on the design's
one open question — adaptive worker count on 429/503.

## Outcome
Goal met in full. Four PRs merged (#32, #37, #38, #39; master 3b82229),
every phase-6 PLAN.md box checked with dated evidence (including the
nightly-CI criterion amended to manual-only by Josh's decision), and the
measurement session run on two Latitude bare-metal boxes in Dallas for
~2 hours ≈ $4: 333 recorded transfers, zero failures, zero 429/503.
Defaults confirmed at 512 MiB / 4 workers — now measured, not guessed —
and the adaptive-concurrency question closed as "no, not for v1" with the
data in design.md. Teardown verified: empty server list, zero leftover
GHCR packages. Unlike sessions 003–006 this ran without ultracode: the
lead built the harness directly, with Explore/Plan agents used only for
design.

## Key Decisions
- Harness as a third module `bench/` mirroring `cli/` (stdlib flag, local
  replace, never released) -> clean separation Josh required, and the
  existing module pattern already solves lint/moon/release integration.
- Matrix lives in checked-in JSON spec files, scenarios as phases within an
  iteration (cold-push always executes; timing is opt-in per spec) ->
  changing the matrix is a JSON edit, never code; a new scenario is one
  pipeline case.
- Every iteration generates unique seeded ChaCha8 bytes (seed =
  f(run_id, cell_id, iteration)) -> zot's global dedupe can never turn a
  cold push into a silent no-op; warm-push measures the HEAD-skip path
  deliberately, cold measures real uploads.
- `-resume` skips (cell, scenario, iteration) rows already recorded, and an
  all-recorded iteration skips fixture generation -> a crash 40 minutes
  into a paid run resumes instead of restarting; proven live on the smoke
  matrix.
- A status-counting RoundTripper (WithHTTPClient) records non-2xx/3xx per
  timed phase -> the adaptive-concurrency verdict became counts, not vibes:
  zero 429/503 in all 333 rows, including 8 workers against GHCR.
- Two boxes same site + iperf3 link measurement before any stage -> every
  throughput number has a denominator (9.42 Gbit/s ≈ 1178 MB/s); the
  fastest cell (1070) stayed under it, sanity intact.
- Registry-stock failure handled by sibling plan, not site move -> DAL ran
  out of m4-metal-small between the two creates; the registry box became
  f4-metal-small in the same site because the client is the measured side.
- Defaults kept at 512 MiB / 4 workers -> within 2% of best at 16 GiB on
  zot, best on Distribution at w4, tied within noise on GHCR; 64 MiB costs
  11% at scale; w8 buys 1–3%. GHCR measured 78–99 MB/s per push — the
  design's borrowed 85–90 MB/s figure observed directly.
- No nightly benchmark job (Josh's decision, PLAN.md amended) -> GH-runner
  numbers are noise; `bench:check` gives CI lint/build/unit-tests only and
  measurement stays operator-run.

## Changes
- `bench/` — new module: spec.go/matrix.go/scenario.go/target.go/
  fixture.go/result.go/summarize.go/run.go + unit tests, five staged spec
  files, latitude/ runbook with four scripts, moon.yml (PR #32).
- Root integration — .moon/workspace.yml `bench` project, root:check dep,
  ci.yml cache keys, dependabot /bench entry, release-please exclude,
  .gitignore for run artifacts (PR #32).
- `bench/latitude/` hardening from live failures — lsh array-shaped JSON,
  create-then-reap detection, credentials over stdin instead of argv
  (PR #37).
- `options.go` — DefaultPartSize/DefaultWorkers godocs rewritten from
  "provisional" to measured, linking the benchmarks page; values unchanged
  (PR #38).
- `docs/docs/reference/benchmarks.md` — new reference page (method, median
  grids for zot/Distribution/GHCR, decisions, caveats incl. the ~1125 MB/s
  client hash floor that warm-push measures); mkdocs nav entry; design.md
  Defaults table cites the data and Open Questions now reads "None
  remain"; README status brought current (PR #38).
- `bench/specs/stage2.json` — committed as actually run, keeping the
  reproducibility claim honest (PR #39).
- `.journal/002/PLAN.md` — phase-6 boxes checked with dated evidence;
  nightly criterion amended. `.journal/007/` — DESIGN.md (approved plan),
  results/ (all four stages' JSONL + rendered summaries + link.txt).

## Open Threads
- Phase 7 is all that remains: progress reporting, error-contract audit,
  the Diátaxis docs set, API surface review, v0.1.0 via release PR #11
  (still open by design).
- The docs site rendering at componere.github.io/bigoci is still a phase-7
  manual criterion — benchmarks.md deployed green but was not eyeballed in
  a browser this session.
- Dependabot opened its first /bench (and cli/actions) update PRs right
  after #32 landed — triage them whenever dependencies are next touched.
- The 10 Dependabot vulnerabilities GitHub reports on the default branch
  (4 high) predate this session and remain unexamined.
- The retry tag still carries `after` with no overload kind — deliberately
  left unextended now that phase 6 decided against self-tuning; revisit
  only if a registry is ever observed throttling multi-part transfers.

## References
- PRs: #32 (feat(bench): throughput harness, 92e7d52), #37 (fix(bench):
  runbook hardening, f98a380), #38 (chore: set measured defaults,
  1070793), #39 (chore(bench): stage-2 spec as run, 3b82229).
- Governing design: `.journal/007/DESIGN.md` (the approved plan).
- Evidence: `.journal/007/results/` (stage1–4 JSONL, summaries, link.txt);
  NOTES 15:24 and 15:31 entries.
- Plan: `.journal/002/PLAN.md` (phases 1–6 checked; 7 remains).
- Published: https://componere.github.io/bigoci/reference/benchmarks/
- Prior sessions: `.journal/001..006/SUMMARY.md`.

## Lessons
- A warm re-push's wall time is the client's single-pass read+sha256 floor
  (~1125 MB/s on a 6-core box), network-free — flat across every part
  size and worker count. Any benchmark that re-pushes existing bytes is
  measuring the hasher, and any single-worker transfer is bounded by it.
- Short transfers lie about pulls: 2 GiB pulls plateaued at ~650 MB/s
  while 16 GiB pulls sustained ~900+ on the same path — per-transfer
  overheads dominate small files. Size the deciding matrix at the target
  workload, not the cheap one.
- `lsh` renders creates/gets as single-element JSON arrays, and a Latitude
  create can be accepted, report status "on" with an IP, and then 404
  minutes later. Poll for not_found as a first-class outcome, and never
  assume the second create succeeds because the first did — same-site
  stock can vanish between them (sibling plan in the same site beats
  moving sites when only the non-measured box is affected).
- Never interpolate credentials into an ssh command string — they sit in
  local and remote process listings for the whole run; pipe them over
  stdin into remote `read`.
- The golangci-lint stale-cache phantom (TECH_NOTES) bit twice more,
  citing worktrees removed hours earlier; `golangci-lint cache clean` is
  now the reflex before diagnosing any lint failure naming `.wt/` paths.
- macOS parks AirPlay on port 5000 answering 403 to everything — a
  registry smoke test against localhost:5000 can talk to the wrong server
  entirely. The zot image's default config also refuses anonymous; both
  now documented in the bench READMEs.