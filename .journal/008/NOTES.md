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
