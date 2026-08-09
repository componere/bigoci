---
id: 008
title: Fresh work session
started: 2026-08-09
---

## 2026-08-09 16:01 — Kickoff
Goal for the session: Start a fresh journal session and await the user's substantive request.
Current state of the world: The default branch is clean at `1070793` (`chore: set measured defaults (#38)`), and the project technical notes plus the three latest closed-session summaries have been loaded.
Plan: Await the user's request, work iteratively from the current repository state, and record meaningful checkpoints here.

## 2026-08-09 16:18 — Independent `bench/` audit
Goal: Audit the benchmark harness, scenarios, result pipeline, and published conclusions at clean `master` commit `3b82229`, formalizing only issues proved by code, saved data, or controlled reproduction.
Evidence: `go test -race ./...` and coverage passed in `bench/`; all 333 archived rows were parsed and compared with their journal copies and regenerated summaries; focused temporary zot runs reproduced cold-label contamination, cross-run resume mixing, and the unreported prerequisite failure.
Confirmed findings: the GHCR matrix never realized eight workers because its files contain only two or four parts; failed-transfer resume can record partial work as a full cold transfer; result identity omits the run/cohort and can silently skip or mix runs; ordinary append reruns and duplicate worker-axis values can record warm replays as cold; an unselected prerequisite cold-push failure exits successfully with no rows; published GHCR/default claims misstate aggregate throughput as per-connection throughput and contradict several recorded values.
Historical boundary: the saved 333-row cohort has unique keys, zero failures, zero missing-blob-free cold pushes, correct throughput arithmetic, and byte-reproducible summaries, so the resume/append defects did not contaminate it. The stage-4 concurrency coverage and its interpretation are affected. The large-file local data still broadly support 512 MiB / four workers; the audit does not establish that the defaults themselves are wrong.
Rejected as unproved: fixed matrix order, cache and thermal effects beyond the documented hot-pull caveat, the two-sample GHCR population by itself, population-versus-sample standard deviation, dirty-build provenance, and the duplicated size parser were not elevated because no outcome impact was demonstrated.
Next: Report the read-only findings and wait for direction; no repository changes were made.

## 2026-08-09 16:41 — Corrected harness proved locally
Authorization: The user approved the fixes, a paid Latitude rerun, GHCR cleanup, and exact server teardown.
Implementation: Created `feat/bench-audit-fixes` from fetched `origin/master` at `54c5730`. The harness now refuses non-resume appends, validates a single run ID and unique successful keys, gives each process attempt fresh fixture and repository identity, replays missing downstream prerequisites in that fresh namespace, removes both destination and `.bigoci-partial` before a timed cold pull, fails loudly when an unrecorded prerequisite push fails, rejects duplicate scenario/worker axes, and reports configured versus maximum-active workers. Stage 4 is now the 24-row 4 GiB, 256/512 MiB, 4/8-worker, three-iteration matrix; all cells can activate their configured workers.
Proof: `moon run bench:check` passed. Two independent smoke runs against fresh local zot produced 12 rows apiece with different attempt IDs and a 404 miss for every cold-push part; a non-resume append was refused and `-resume` appended zero rows. The temporary container and test files were removed. All four archived schema-1 result files remain summarizable.
Latitude boundary: Read-only live reconnaissance found zero current servers and high Dallas stock for the same original `m4-metal-small` client (6 cores, 64 GiB RAM, 2x960 GB NVMe, $0.81/hour). No resource has been provisioned yet.
Next: Commit the clean harness slice, provision exactly one Dallas client, run corrected GHCR stage 4, collect and validate rows, delete the GHCR throwaway package and exact Latitude server, then update conclusions from the observed data.
