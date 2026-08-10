---
id: 008
title: Benchmark audit and corrected GHCR rerun
date: 2026-08-09
status: complete
repos_touched: [bigoci]
related_sessions: [007]
---

## Goal
Independently audit `bench/` for demonstrated measurement, performance, duplication, and interpretation problems without elevating conjecture. Repair confirmed defects, rerun only the measurements invalidated by them, update conclusions from observed data, and clean up all paid and registry resources.

## Outcome
The goal was met. The audit reproduced invalid cold/resume boundaries, incomplete result identity, misleading effective-worker labels, a silent prerequisite failure, and unsupported GHCR conclusions. PR [#40](https://github.com/componere/bigoci/pull/40) fixed those defects, a corrected 24-row GHCR cohort exercised every configured worker, and the resulting data changed the default from four to eight workers while retaining 512 MiB parts. GitHub squash-merged the work as `3b351c6`; CI, Pages, and CodeQL passed on the approved head. The Latitude server and all GHCR packages created for the rerun were deleted and verified absent.

## Key Decisions
- Formalize only reproduced defects -> the archived same-site data remained valid, while the structurally capped original GHCR slice was explicitly superseded instead of dismissing the whole session.
- Rerun only GHCR on one matched Dallas client -> the worker-cap defect affected the public-registry slice; repeating the already valid same-site matrices would add cost without answering a new question.
- Keep `DefaultPartSize` at 512 MiB and raise `DefaultWorkers` to 8 -> at 512 MiB, four to eight workers changed zot push/pull by +1.238%/-0.089%, Distribution by +0.134%/-0.035%, and GHCR by +0.424%/+62.173%.
- Keep self-tuning out of v1 -> the fixed eight-worker default captures the observed GHCR pull gain, while all 333 published transfers recorded zero 429 or 503 responses and therefore supply no measured throttle signal for a control loop.
- Bind resume to an immutable cohort -> schema-3 fingerprints the effective post-override spec, row schema, and harness identity; schema-1/2 files remain summarizable but cannot be resumed without proof of equivalence.

## Changes
- `bench/` - preserves cold boundaries with fresh attempt namespaces and pull-partial cleanup; rejects unsafe append, duplicate axes/successes, mixed runs/builds/cohorts, and silent prerequisite failures; reports configured and maximum-active workers.
- `bench/specs/stage4-ghcr.json` - replaces the capped 1 GiB slice with a 4 GiB, 256/512 MiB, 4/8-worker, three-iteration matrix.
- `bench/latitude/` - stamps the exact revision, isolates SSH host trust, quotes remote arguments, propagates remote failures after collecting partial rows, and gives each spec/client pair a distinct run identity.
- `options.go`, `README.md`, `cli/`, and `docs/` - make eight workers the measured default, retain 512 MiB parts, remove per-connection interpretations, and publish the corrected aggregate results and caveats.
- `.journal/008/results/` - preserves the raw corrected JSONL and generated Markdown summary; the raw SHA-256 is `5b62f083cfc1c8056c0119e1f3a14fd14710856fa6e50f3224d965b9c94b9ce3`.

## Open Threads
- Revisit worker adaptation only after an observed registry or path supplies a concrete throttle or scaling trigger; the audit does not claim counts above eight can never help.
- Local `root:lint` still reports three pre-existing G704 warnings in unchanged OCI request code; this session did not broaden into that unrelated security work.

## Lessons
- A configured worker count is not evidence of concurrency: deciding matrices need at least as many parts as workers, and reports should expose `min(workers, parts)`.
- A cold label is a state contract, not just a scenario name: retries need fresh remote repositories, fixtures, and partial destinations, while result resume must bind the effective spec and exact harness build.
- Saved aggregate throughput cannot support per-connection conclusions unless the harness measures connections separately.

## References
- [PR #40 — fix(bench): preserve measurement validity and correct defaults](https://github.com/componere/bigoci/pull/40)
- [Squash commit `3b351c6`](https://github.com/componere/bigoci/commit/3b351c65d6d5047b48702b14a2d1c371f6bb19d7)
- `.journal/008/results/stage4-ghcr-audit.jsonl`
- `.journal/008/results/stage4-ghcr-audit-summary.md`
- `.journal/007/SUMMARY.md`
- `docs/docs/reference/benchmarks.md`
