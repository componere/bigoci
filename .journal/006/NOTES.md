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

## 2026-08-08 19:05 — Scout complete; tensions for the design panel
Read design.md (auth decision, port sketch, transport sharp edges), the oci
adapter (repository.go/blobs.go/classify.go), root client/options/errors,
transfer ports, CLI run.go/debug.go, go.mod, moon/mockery config, and the
005 DESIGN.md shape. Session runs under ultracode; user directive renewed:
workflow agents must never inherit Fable — override to opus/sonnet per task.
Findings the panel must resolve:
- No Auth port exists in code yet ("already defined" in PLAN means defined
  in design.md's sketch: RoundTripper(ctx, registry, scope)). That sketch is
  in tension with the settled instrument constraint (Authorization at
  request-build time in internal/oci, caller's transport outermost).
- A body-bearing PUT cannot ride a 401-challenge resend (spent reader; the
  port forbids adapter retries), so the bearer dance must resolve before
  body-bearing requests. Token expiry mid-transfer vs 401-terminal needs a
  ruling (bad-creds 401 → ErrUnauthorized; stale-token 401 → ?).
- oras-go v2 may not export a bare token-fetch primitive (auth.Client.Do
  does the dance internally); panel must ground the real API surface.
- Exit 6 / ErrUnauthorized activation is reserved and allowed; -debug
  grammar is frozen (token exchange = class=other; no grammar change).
- Caller's http.Client must not be mutated (redirect handling derives a
  clean client; NewRepository currently uses the caller's client as-is).
- Tests must never read the developer's real Docker config (DOCKER_CONFIG /
  injected store isolation).
Next: three-lens design panel (protocol/interop, seams, failure-modes) plus
two comparative adversarial judges, all opus/xhigh; then I synthesize
.journal/006/DESIGN.md.
