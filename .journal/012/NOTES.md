---
id: 012
title: Plan IMGOCI upstream requests
started: 2026-08-15
---

## 2026-08-15 09:37 — Kickoff
Goal for the session: Process `IMGOCI_UPSTREAM_REQUESTS.md` with a planning agent and produce a complete implementation plan satisfying the full request.
Current state of the world: The personal journal is initialized on `journal/jmgilman`; sessions 001–011 are complete; the upstream request document is available in the main checkout and has not yet been analyzed in this session.
Plan: Have a planning agent inspect the request and relevant repository code, review its proposal, save the final plan in this session folder, and checkpoint the journal.

## 2026-08-15 09:59 — Implementation plan complete
The planning agent traced all five upstream requests through the current decoder, OCI adapters, redirect path, push orchestrator, public API, tests, docs, benchmark harness, and release workflow.
Reviewed and tightened the proposal: response coding is checked before status/body handling across repeated header fields; manifest and blob functional failures are exercised separately; token compression is proven through a complete authenticated transfer; `Push` and `PushByDigest` share one private transfer body; upload verification reuses the existing source-error terminal path; and no performance threshold is invented.
Saved the final six-phase implementation and release plan as `PLAN.md`. Next: implement phases 1–5, run integrated verification, and release `v0.2.0`.

## 2026-08-15 10:03 — Implementation orchestration started
The approved plan is now being implemented on five isolated Worktrunk branches from the fetched `origin/master`: `feat/manifest-casefold`, `feat/identity-reads`, `feat/push-by-digest`, `docs/external-transport-contract`, and `feat/upload-wire-verify`.
Each phase will receive independent Programmer implementation, orchestrator-run verification, a focused Conventional Commit, and a squash-merged GitHub PR. Integrated verification and the `v0.2.0` Release Please flow follow only after all phase PRs are green and merged.
