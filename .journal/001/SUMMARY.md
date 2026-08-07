---
id: 001
title: bigoci design and repo bootstrap
date: 2026-08-07
status: complete
repos_touched: [componere/bigoci]
related_sessions: []
---

## Goal
Take the freshly template-cloned repo from zero to "ready to build": port a
missed upstream template commit, research and write the initial design
document for bigoci (a surgical Go library that uploads/downloads 5 GB to
tens-of-GB files to/from OCI registries), convert the template scaffolding
into the real library repo, and get CI/docs/release automation working.

## Outcome
Goal met. The repo is fully bootstrapped: design and format docs are merged
and published, the template is converted (module `github.com/componere/bigoci`,
library-only shape), repository settings are applied, dependencies are
current with zero open Dependabot alerts, and release automation works
end-to-end. No library code exists yet — that is deliberate; implementation
starts with the design doc's "first slice".

## Key Decisions
- Own the distribution-spec transport (six endpoints over `net/http`) instead
  of building on oras-go/go-containerregistry/regclient -> the library's value
  (parallel transfers, per-part retry, streaming, redirect handling) lives in
  the HTTP layer none of them provide; oras-go cannot chunk push at all.
- Split-blob format: files stored as fixed-size parts (default 512 MiB,
  provisional) as ordered manifest layers; reconstruction = concatenation ->
  only spec-legal route to parallel push (chunked upload is sequential),
  required by GHCR's 10 GB layer cap, and gives per-part retry via universally
  supported monolithic PUTs. Whole-file digest carried as a manifest
  annotation; per-part digest verification is the integrity model.
- Reuse auth behind a RoundTripper-shaped port; default adapter wraps oras-go
  v2 `auth`/`credentials` (smaller module graph), ggcr `authn` documented as
  the in-process cloud keychain alternative.
- Manifests are deterministic (no timestamps, stable encoding) and references
  may be tagless -> keeps signatures/SBOM referrers valid across re-pushes and
  lets external structures (e.g. an OCI image index) compose artifacts without
  bigoci knowing.
- Dual license Apache-2.0 + MIT, "at your option" (user decision).
- Release Please on plain `GITHUB_TOKEN` (no GitHub App in componere org);
  organic pre-1.0 versioning: `bump-patch-for-minor-pre-major=false` so feat
  commits bump minor (first feat carries the release PR to 0.1.0), manifest
  baseline 0.0.1 because release-please reserves 0.0.0 for its bootstrap path
  (proposes 1.0.0, ignoring pre-major flags — googleapis/release-please#2087).
- Tag ruleset leaves creation open (write access only in practice) because
  GitHub's API refuses the `github-actions` integration as a ruleset bypass
  actor; tags are immutable once created.

## Changes
- `docs/docs/explanation/design.md` — the full design document (PR #8):
  scope/non-goals, protocol facts, the three core decisions, push/pull paths,
  provisional defaults, hexagonal architecture with port signatures and
  package layout, testing strategy, first slice, one open question (adaptive
  concurrency). Refined by three parallel review passes (fact-check,
  constraints, plain language) plus a consistency pass.
- `docs/docs/reference/format.md` — the artifact format contract (PR #8):
  split rule, 4096-part cap, manifest/annotation spec, determinism guarantee,
  versioned media types.
- Whole-repo template conversion (PR #9): deleted CLI/container/binary-release
  machinery, renamed module, seeded root `doc.go`, rebranded moon/mise/docs,
  added LICENSE-APACHE + LICENSE-MIT, rewrote README/SECURITY/CONTRIBUTING.
- `AGENTS.md` — categorized Go best-practice rules ported from template-go
  upstream (PR #7).
- Release automation fixes (PRs #12, #13, #14): release-as experiment, then
  organic versioning config, then 0.0.1 manifest baseline.
- `docs/uv.lock` — pymdown-extensions 11.0.1, resolving both Dependabot
  security alerts (PR #10).
- Applied `.github/repository-settings.toml` via the configure script (repo
  settings, branch/tag rulesets); enabled org+repo "Actions can create PRs"
  (lives outside the manifest, in the Actions permissions API).

## Open Threads
- PR #11 (`chore(master): release 0.0.2`) stays open on purpose: it
  self-updates per merge; merge it only when cutting the first real release.
  Expectation: feat PRs push it to 0.1.0 before that happens.
- Implementation has not started. Next session should begin with the design
  doc's "First slice": push/pull one file against zot in testcontainers with
  small fixed parts, no retries/resume/auth, then layer in retries, resume,
  auth, and the benchmark harness (which must validate the provisional
  512 MiB / 4-worker defaults before v1).
- Design doc's one open question (should worker count self-tune?) awaits
  benchmark-harness data.
- GitHub Pages deploys on merge to master; user enabled Pages during this
  session — verify the published site renders after the next docs merge.

## References
- Design document: `docs/docs/explanation/design.md` (published at
  https://componere.github.io/bigoci/explanation/design/) — read this first;
  it is the buildable spec.
- Format contract: `docs/docs/reference/format.md` (published at
  https://componere.github.io/bigoci/reference/format/).
- PRs: #7 (agent rules), #8 (design docs), #9 (template conversion),
  #10 (security fix), #12/#13/#14 (release config), #11 (open release PR).
- Upstream template commit ported: meigma/template-go@2dc7b01.
- release-please 0.0.0 bootstrap trap: googleapis/release-please#2087.

## Lessons
- Registry protocol variance is the enemy: chunked upload, resumable push,
  and Range support are all inconsistently implemented. The design leans
  exclusively on monolithic PUT + blob GET, the two operations that work
  everywhere; treat any future feature that depends on optional registry
  behavior with suspicion.
- The `gh` OAuth token needs the `workflow` scope to merge PRs touching
  `.github/workflows/` and `admin:org` for org Actions settings — both were
  refreshed interactively this session.
- Release-please pre-1.0 semantics are a trap twice over:
  `bump-patch-for-minor-pre-major=true` silently prevents ever reaching 0.1.0
  organically, and a 0.0.0 manifest baseline triggers the 1.0.0 bootstrap
  default.
