---
id: 002
title: Begin first-slice implementation
started: 2026-08-07
---

## 2026-08-07 13:00 — Kickoff
Goal for the session: begin implementing the design from session 001 — the
design doc's "First slice": push/pull one file against zot in testcontainers
with small fixed parts, no retries/resume/auth yet.

Current state of the world: repo is fully bootstrapped but has no library code.
Design doc (`docs/docs/explanation/design.md`) and format contract
(`docs/docs/reference/format.md`) are merged and published; they are the
buildable spec. Release automation works; release PR #11 stays open on purpose.
Master is clean at 205361c.

Plan: read the design doc's first-slice section, set up an implementation
worktree from master, and build the slice per the doc's implementation order,
with functional tests (zot via testcontainers) before calling it done.

## 2026-08-07 13:16 — Phased implementation plan written
Wrote `PLAN.md` (this folder): six phases decomposing the design, ordered
inward-outward per the design doc's "First slice" sequence.

1. Walking skeleton — core (plan/manifest), ports/adapters, orchestrator,
   public API; push/pull vs zot; 3 PRs.
2. Retries — backoff policy, error classification, failure injection.
3. Resume — pull resume from partials, kill-and-resume e2e.
4. Auth + real registries — oras-go creds adapter, presigned-redirect
   handling, cloud conformance job; manual GHCR verification.
5. Benchmark harness — sets real defaults, answers the adaptive-worker
   open question.
6. Finishing touches — progress reporting, error-contract audit, Diátaxis
   docs, v0.1.0.

Key plan decisions: each phase gates on manual functional verification (not
just CI); a throwaway `internal/dev` driver is added in phase 1 as the
manual-verification vehicle since the library ships no CLI; AGENTS.md rule
compliance (hexagonal, mockery-only mocks, godoc/doc.go, D6 docs-with-PR)
is restated as a per-phase ground rule. Awaiting user review of the plan
before starting phase 1.
