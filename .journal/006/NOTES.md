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

## 2026-08-08 19:58 — Design panel done; DESIGN.md synthesized
Panel wf_6f7971f2-982 (5 opus agents, ~1.03M tokens, 44 min). Both judges
ranked [failure] > [protocol] > [seams]. The correctness judge measured GHCR
and Docker Hub live: bad Basic at the token endpoint answers 403 DENIED (bad
creds never reach a body-bearing request); the presigned target ACCEPTS a
leaked bearer (200) — design.md's "S3/GCS/Azure reject" claim is false, so
the no-leak gate must read auth=none off the -debug log, never infer from
success; an unparseable bearer at GHCR answers 403 not 401 (nobody measured
a real expired token → manual gate 5, long push outliving the token, is
BLOCKING); GHCR's anonymous token has no expires_in (spec default 60s).
Synthesis = [failure]'s architecture (no ping; request-build-time auth;
own-the-dance + oras-go credentials store only; proven/unproven/denied 401
state machine; GetBody-gated re-issue rule) + judge-mandated corrections:
[seams]'s four-field Credential port (two-string Lookup collapses identity
tokens to anonymous), ServerAddressFromHostname, [protocol]'s off-registry
status table (401/403/404/410 transient at a presigned target, never
ErrUnauthorized), same-origin carry + 3-hop cap, 403-with-challenge takes
the refresh path, [seams]'s bearer gateway e2e (C9 expiry proven per-commit
end-to-end), counting-transport C1 test, scrub() for the *url.Error
signature leak, injected clock, numeric no-burn assertions, root TestMain
env isolation carried into the re-exec child.
PR shape: four PRs — PR 0 inert (ErrUnauthorized classification + exit 6),
then the plan's three. DESIGN.md written to .journal/006/DESIGN.md.
Panel outputs in session scratchpad phase5/ (designs + verdicts).

## 2026-08-08 20:35 — PR 0 open (#28)
Opus implementer (wf_1fd0a54e-c5c) built the inert classification change in
.wt/feat-oci-unauthorized; two-lens review panel said fix-then-ship. I
reviewed the full diff line-by-line and applied all findings myself: deleted
doc.go's twin "cannot happen" paragraph (and fixed its stale
pulls-don't-resume claim from phase 4), added ErrUnauthorized to the
Push/Pull godoc and how-to sentinel lists ("Four failures" → five), rewrote
the public sentinel godoc as two plain sentences + the WAF admission, made
the fake registry's challenge realm point at its own server (hermetic when
PR 1 starts following challenges), tightened the sentinelExits comment, and
named unparam in refusedTag's godoc. Implementer proved non-vacuity by
removing the classify row (both tests fail exit 1). Local moon root:check
green after clearing a stale golangci-lint cache that referenced the deleted
phase-4 worktree (.wt/test-kill-resume-e2e — lint caches survive worktree
removal; clean before diagnosing).
Commit 7bc8980 (verified, G), PR #28. Carry-forward recorded in the PR body:
any 401/403 the adapter refreshes in PR 1/PR 2 must not be a bare
*StatusError, with NotErrorIs(oci.ErrUnauthorized) tests.

## 2026-08-08 22:40 — PR 0 merged; PR 1 built, reviewed, fixed
PR #28 squash-merged (7c71aea) after green CI. PR 1 built by a three-stage
opus pipeline (wf_e2350b83-c4d: oci machinery + unit suites; internal/auth +
mockery mock + isolation suite; root/CLI wiring + htpasswd-zot e2e + bearer
gateway e2e + docs) then a three-lens review panel (protocol xhigh, security
xhigh, house high): no blockers, 7 majors, all verified with repros. I
applied every finding by hand:
- Multi-line WWW-Authenticate joined per RFC 9110 (challengeHeader; a
  Basic-line-then-Bearer-line registry now picks Bearer; reproduced before).
- New/CLI no longer hard-fail with no $HOME/$DOCKER_CONFIG (scratch
  containers): unlocatable config = anonymous, error reserved for a
  malformed file.
- oras config-decode errors no longer leak the decoded auth entry (usually
  a pasted secret) into messages: lookupError names registry+file only.
- Username-less secrets now ride the bearer exchange as Basic ":token"
  instead of silently downgrading to anonymous.
- Docker Hub key fixed BOTH ways (design's own correction was wrong for the
  reference form users write: reference.Domain gives docker.io, which only
  ServerAddressFromRegistry maps; serverAddress composes hostname-then-
  registry mapping and both spellings are tested).
- token68 challenge siblings void their own challenge, not the header;
  realm rendering redacted (userinfo password stays out of errors);
  plain-HTTP realms restricted to the registry's own host.
- Root TestMain isolates DOCKER_CONFIG only — HOME/USERPROFILE left alone
  (testcontainers resolves rootless sockets + pull creds through HOME; the
  security reviewer caught the redirect would break other machines).
- Docs de-staled (README status, design.md sharp-edges "not yet" qualifier,
  authenticate.md request-count claims, cli/README redirect-visibility
  paragraph), negative controls ordered first, test godocs added,
  scopeFor/mergeScopes tests moved to credentials_test.go.
Full moon root:check green + fresh -count=1 runs of every suite (golangci
cache needed a second clean). Boundary check: zero lines in cli/debug.go,
cli/redact.go, transfer/retry/plan/manifest/file, format.md. Sequencing
window recorded for the PR body: PR 1 puts Authorization on the wire while
Go's auto-follow still governs redirect headers — PR 2 closes it; no
release between them.

## 2026-08-08 23:55 — PR 1 merged; PR 2 in flight
PR #29 squash-merged (master 9944738) after green CI. PR 2 (presigned
redirects) launched as one opus implementer + three-lens review panel
(wf_7e0edfda-e66) in .wt/feat-presigned-redirects, carrying the two
recorded constraints: close the PR-1 auto-follow window (derived clients,
same-origin carry with exact scheme+host+port), and the off-registry table
must never construct a sentinel-matching error (expired presigned signature
is transient, not ErrUnauthorized/ErrNotFound). T7 single-use-signature e2e
and the gateway 302 row land here; design.md sharp-edges rewrite corrects
the measured-false S3/GCS/Azure claim.
