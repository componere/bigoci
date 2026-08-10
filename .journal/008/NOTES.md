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

## 2026-08-09 17:01 — Corrected GHCR cohort complete and cleaned
Run: Provisioned exactly one Dallas `m4-metal-small` client, `sv_bBmw0K2Mra9VR` at `162.43.191.111`, using hourly billing. The harness ran as `latitude-sv_bbmw0k2mra9vr`, attempt `b055068e917eb00a`, at exact injected commit `ab79d0f90f06`. Two launch preflights stopped before remote execution: the first exposed uppercase Latitude IDs in the lowercase run-ID grammar; the second exposed a recycled-IP SSH host-key collision. Both were fixed and committed before any GHCR rows were produced.
Validated cohort: 24 unique schema-2 rows, three repeats for every 256/512 MiB x 4/8-worker x push/pull population; 16 or 8 parts made every configured worker active. All rows succeeded, every cold push recorded at least one 404 per part, aggregate throughput arithmetic reproduced, and no 429/503 occurred. The local JSONL exactly matched the remote SHA-256 `5b62f083cfc1c8056c0119e1f3a14fd14710856fa6e50f3224d965b9c94b9ce3`.
Medians: 256 MiB push was 111.2 MB/s at w4 and 112.3 at w8; pull was 163.2 and 273.4. 512 MiB push was 109.0 and 109.5; pull was 161.6 and 262.1. At both part sizes, eight workers changed push by at most about 1% and raised pull by 62-68%. At a fixed worker count, 512 MiB stayed within 1-4% of 256 MiB.
Cleanup: Destroyed exact server ID `sv_bBmw0K2Mra9VR`; `servers get` returned `not_found` and the ID was absent from project inventory. Deleted the four exact GHCR packages created under `bigoci-bench/latitude-sv_bbmw0k2mra9vr/b055068e917eb00a/`; the post-delete `bigoci-bench` package query was empty. Raw results and the generated summary are preserved under `.journal/008/results/`.
Conclusion to implement: Retain the 512 MiB part size, increase the default worker count from four to eight because the corrected GHCR pull benefit is large and the zot/Distribution 512 MiB rows show no material regression, remove every per-connection interpretation, and keep adaptation out of v1 while zero measured throttle responses remain the observed trigger data.
