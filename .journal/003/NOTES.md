---
id: 003
title: Phase 2 — reference CLI
started: 2026-08-07
---

## 2026-08-07 21:41 — Kickoff
Goal for the session: execute phase 2 of the implementation plan
(`.journal/002/PLAN.md`) — the reference CLI that serves as the
manual-verification vehicle, plus running phase 1's deferred manual proofs.

Current state of the world: master is 0ce9ecc (PR #18). Phase 1 is merged
(PRs #15–#18): push/pull works end to end against zot in testcontainers,
automated gates checked off in PLAN.md. Phase 1's manual proof is deferred
into phase 2's gate. Release PR #11 reads 0.1.0 and stays open deliberately.

Plan (from PLAN.md phase 2):
1. PR `feat(cli): reference CLI for push and pull` — `cli/` with its own
   Go module (`github.com/componere/bigoci/cli`), `replace` to the core;
   `push`/`pull` commands; flags for part size, workers, plain-HTTP;
   `--debug` HTTP request logging; reference-only README.
2. PR `ci: build and test the cli module` — moon/CI wiring; excluded from
   release-please and any publishing path.
3. Run the phase-2 manual success criteria (which double as phase 1's
   manual proof) and check them off in PLAN.md.
