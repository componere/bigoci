---
id: 006
title: Phase 5 — auth and real registries
started: 2026-08-08
---

## 2026-08-08 18:52 — Kickoff
Goal for the session: execute phase 5 of `.journal/002/PLAN.md` — auth and
real registries.
Current state of the world: phases 1–4 are complete and gated (sessions
002–005; PRs #15–#27; master at d69afc0, clean). Reserved seams for this
phase are intact: exit code 6 / `ErrUnauthorized`, and the phase-5
instrument constraint (auth set at request-build time in `internal/oci`
with the caller's transport outermost; the presigned-redirect clean client
derived from the caller's client — otherwise the CLI's `-debug` goes blind
and the no-auth-leak gate passes vacuously). Verified in session 005:
net/http strips only auth/cookie headers across cross-host redirects, so
`Range` survives a presigned redirect to object storage.
Plan: read the phase-5 section of PLAN.md, then design and implement per
the session's established cadence (design panel, staged PRs, manual gates
with journaled evidence).
