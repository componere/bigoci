---
id: 006
title: Phase 5 — auth and real registries shipped and gated
date: 2026-08-09
status: complete
repos_touched: [componere/bigoci]
related_sessions: [001, 002, 003, 004, 005]
---

## Goal
Execute phase 5 of `.journal/002/PLAN.md`: authenticated push/pull via the
Docker credential ecosystem, presigned-redirect handling, and a manually
triggered cloud-registry conformance job — closed by four manual GHCR gates
plus the automated gates, so bigoci works against GHCR with `docker login`,
not just local zot.

## Outcome
Goal met in full. Four PRs merged (#28–#31; master e49ce48), every phase-5
PLAN.md box checked with dated evidence, the conformance workflow green
against real GHCR on its first hand-triggered run, and all five manual gates
passed — run by the lead with Josh's authorization against a throwaway
private package under his account, using his keychain credential (the
credential-helper path proven live), then deleted. The blocking gate — a
1 GiB push spanning five full token-expiry cycles (ten exchanges, ~60 s
cadence) with zero failed parts — retired the phase's riskiest unknown: the
proactive re-mint means an expired token never reaches the registry. The
session ran under ultracode with every workflow agent on opus (user
directive: never inherit Fable): a three-lens design panel whose judges
measured GHCR and Docker Hub live, per-PR implementers and adversarial
review panels, every finding verified by reproduction, every fix applied by
the lead's hand, all commits under the GitHub-verified identity.

## Key Decisions
- No ping: the challenge is learned from the registry's own 401 -> the
  anonymous path is byte-inert (the nine committed `other=0` summary lines
  stayed untouched as each PR's regression gate), and PR 0/1 kept the
  phase-3/4 inert-first precedent.
- Own the bearer dance; borrow only oras-go's credentials store -> verified
  in source: `auth.Client` refreshes by resending (impossible for a spent
  blob-PUT body), exports no token-fetch primitive, and its cache has no
  expiry. Net new modules: one.
- `Credentials` port in `internal/oci` returning a four-field `Credential`
  -> judge-mandated over the two-string shape, which collapses
  helper-stored identity tokens to ("","") and silently downgrades to
  anonymous — the exact failure the design forbids.
- The refusal rule: transient iff handling it changed what the next request
  will present; an unproven refresh's refusal is denied, and denied is
  absorbing -> bad credentials burn zero retry budget (proven numerically),
  token expiry self-heals, and a 403 carrying a challenge takes the same
  path (GHCR answers invalid bearers with 403, measured — nobody ever
  measured a truly expired one, hence the blocking manual gate).
- Authorization set at request-build time under the caller's transport ->
  the -debug tap sees every request including token exchanges
  (class=other); no grammar change; the no-leak gate cannot pass blind.
- Redirects followed manually with auto-follow off everywhere: <=3 hops,
  GET/HEAD only, fresh request + {Range, Accept} allow-list, credential
  only on exact same-origin (stricter than Go's domain-or-subdomain rule);
  off-registry 401/403/404/410 are transient through an unexported error
  matching no sentinel -> an expired signature never reads as "fix your
  credentials" or "missing artifact".
- No error may name a signed URL, extended twice beyond the design by
  review findings: 3xx bodies (servers echo their Location into them)
  never become error detail, and redirectError carries no storage body at
  all (S3's SignatureDoesNotMatch echoes a live session credential).
- Upload sessions get the origin rule -> a review reproduction harvested
  Authorization plus blob bytes via a hostile session Location; now
  validated like a redirect target, credential stripped off-origin.
- Docker Hub's credential key mapped from both spellings -> the design's
  own correction was wrong for the reference form users write
  (reference.Domain yields docker.io, which only ServerAddressFromRegistry
  maps); serverAddress composes both, each pinned by a test.
- CLI credentials always-on with no flag (the no-credential state is
  provable with DOCKER_CONFIG pointing at an empty dir); `New`'s error
  reserved for a malformed config — a machine with no $HOME is the
  anonymous case, not a failure (scratch containers).

## Changes
- `internal/oci/` — credentials.go (port, Credential, scopes), challenge.go
  (RFC 9110 scanner, validateRealm), token.go (Basic/Bearer exchange, GET
  only, never the OAuth2 POST), authstate.go (proven/unproven/denied state
  machine, injected clock, single-flight), redirect.go (follow table,
  same-origin carry, off-registry table, scrub, checkSession),
  repository.go (newRequest stamping, send/answer/replay/accepted, derived
  clients, statusDetail), blobs/manifests threading (PRs #28–#30).
- `internal/auth/` — Store (oras-go DynamicStore wrapper, read-only,
  helper timeout, leak-proof lookupError), Static, mockery mock, isolation
  suite with fake-helper positive control and ExecutableTrace structural
  proof (PR #29).
- Root — ErrUnauthorized + classify row; WithDockerCredentials /
  WithCredentials; New's first reachable error; WithHTTPClient godoc
  corrections (PRs #28–#30).
- `cli/` — exit 6 active; always-on credentials; main_test.go TestMain
  isolation; conformance_test.go (build-tagged, counted no-leak gate with
  vacuity guards) (PRs #28, #29, #31).
- e2e — e2e_auth_test.go (htpasswd zot, negative control first),
  e2e_gateway_test.go (in-process bearer gateway: expiry with K<parts
  proven per commit, 302 row), e2e_redirect_test.go (single-use
  signatures: a continuation gets a fresh 307), root TestMain
  DOCKER_CONFIG-only isolation (PRs #29, #30).
- `.github/workflows/conformance.yml` — workflow_dispatch GHCR job,
  SHA-pinned, least-privilege, artifact logs (PR #31).
- Docs — how-to/authenticate.md (new); design.md auth decision + Ports +
  Authentication section + sharp-edges rewrite correcting the measured-
  false "S3/GCS/Azure reject" claim; README/index status; cli/README
  recipes incl. the counted no-leak grep and the failed>=1-under-challenge
  note (PRs #29–#31).
- `.journal/002/PLAN.md` — all five phase-5 boxes checked with dated
  evidence; `.journal/006/DESIGN.md` — the governing design preserved.

## Open Threads
- Phase 6 (benchmark harness, real defaults) is next; the design's open
  question (worker self-tuning on 429/503) and the retry tag's missing
  overload vocabulary await its measurements.
- Identity-token credentials (ACR) are refused loudly, not supported —
  named follow-up: the OAuth2 refresh_token grant.
- The conformance cleanup cannot delete a package's last version (GitHub
  rule); one version of ghcr.io/componere/bigoci/conformance persists per
  design (best-effort). Possible follow-up: delete the whole package when
  it holds only this run's versions.
- cli/redact.go:80's future-tense comment is stale but C6-frozen; queued
  for a commit where the no-edit invariant is not the gate.
- GHCR's authenticated-token TTL is inferred (~90 s from the measured 60 s
  re-mint cadence under a 30 s margin), not confirmed.
- Release PR #11 (0.1.0) stays open until the first release is cut
  deliberately; it now carries this session's three feat commits.

## References
- PRs: #28 (feat(oci): classify a refused request as unauthorized,
  7c71aea), #29 (feat(auth): oras-go credentials adapter, 9944738),
  #30 (feat(oci): presigned redirect handling, 521753d), #31 (ci: manual
  cloud-registry conformance job, e49ce48).
- Governing design: `.journal/006/DESIGN.md` (panel wf_6f7971f2-982; both
  judges ranked [failure] first; verdicts in the session scratchpad).
- Conformance run: GitHub Actions run 31323351318 (green, all five rows).
- Manual-gate evidence: `.journal/006/NOTES.md` 2026-08-09 09:55 entry.
- Plan: `.journal/002/PLAN.md` (phases 1–5 checked; 6–7 remain).
- Prior sessions: `.journal/001..005/SUMMARY.md`.

## Lessons
- A working pull through presigned storage proves nothing about leaks:
  GHCR's Azure backend and Docker Hub's CloudFront both answer 200 to a
  request that also carries the registry bearer (measured). Every no-leak
  gate must read auth=none off the instrument, presence-required, with a
  control proving the instrument saw authenticated traffic.
- GHCR's status vocabulary is not the spec's: invalid bearers get 403 (401
  only for absent credentials), anonymous tokens carry no expires_in, and
  upload sessions live at /blobs/upload/ — singular — so a frozen
  URL-shape classifier files session PUTs under class=other and
  blob-write=0 on GHCR pushes. Evidence recipes must count /token request
  lines, never class=other.
- Three stdlib facts carried the phase's leak-proofing: url.Error.Error()
  renders the full URL (query included — where signatures live), servers
  echo their Location into 3xx bodies, and GetBody's population rules are
  exactly the replay gate an adapter needs (the request that must never be
  re-sent is the one net/http already refuses to re-send).
- A design's own corrections need the same adversarial verification as the
  design: the judge-mandated ServerAddressFromHostname ruling was wrong
  for the input shape bigoci actually produces, caught only because a
  reviewer compiled a probe instead of trusting the citation.
- golangci-lint's cache survives worktree removal and produces phantom
  findings citing deleted paths (bit twice this session); clean the cache
  before diagnosing, and never pipe the check that gates a commit — the
  pipe's exit status masked a root:check failure once.
- An implementer that ships e2e rows it could not execute (Docker outage)
  and says so plainly is worth more than a green report; the lead re-ran
  the suites fresh before merge and they passed — but only because the
  honesty made the gap visible.
- Per-package TestMain isolation must match what the package talks to: the
  cli suite empties PATH (helpers unexecutable by construction), while the
  root suite isolates DOCKER_CONFIG only, because testcontainers resolves
  rootless sockets and pull credentials through HOME and PATH.
