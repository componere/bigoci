---
id: 002
title: Phase 1 — walking skeleton implemented
date: 2026-08-07
status: complete
repos_touched: [componere/bigoci]
related_sessions: [001]
---

## Goal
Turn the session-001 design into working code: write a phased implementation
plan (PLAN.md in this folder), get it approved, and execute phase 1 — the
walking skeleton that pushes and pulls one file against zot in testcontainers
with small fixed parts, no retries/resume/auth.

## Outcome
Goal met, plus a user-requested precision audit on top. Four PRs merged
(#15–#18); master is 0ce9ecc. bigoci pushes and pulls end to end against a
real registry: the per-commit e2e suite moves a 64 MiB file at 4 MiB parts,
asserts the stored manifest clause-by-clause over raw HTTP, proves
determinism/dedup, pulls tagless by digest, and catches injected corruption
with the destination untouched. Phase 1's two automated success criteria are
checked off in PLAN.md; its manual proof is deliberately deferred into phase
2's gate (the reference CLI is the instrument). Phase 2 not started — next
session begins there.

## Key Decisions
- Seven-phase plan (PLAN.md), inward-outward: skeleton → reference CLI →
  retries → resume → auth/real registries → benchmarks → polish. Every phase
  gates on manual functional verification, not just CI.
- Reference CLI (user call): its own Go module under `cli/`, never published,
  replaces the throwaway dev-driver idea; needs the public API, so it follows
  the skeleton, and phase 1's manual proofs live in its gate.
- Delegation pattern (user call): Opus/Sonnet workflow agents do
  implementation legwork; Fable reviews every diff line-by-line and applies
  correctness fixes itself. Every PR's panel found real defects worth fixing
  — the pattern earned its keep.
- Manifests port binds its reference at construction → reference grammar
  never enters the core; design.md updated in-PR (D6).
- Canonical encoding: compact JSON, sorted annotation keys, NO HTML escaping;
  format.md now specifies it precisely so third-party writers produce
  byte-identical manifests. All v1 digests pinned to sha256.
- Split rule single-gated through plan.New everywhere (codec and orchestrator
  validators delegate; no second copy of the rule exists).
- oci.StatusError (typed status, errors.As/Is) is the deliberate seam for
  phase-3 retry classification; 404s match ErrNotFound via Is with no second
  wrapping layer.
- Windows: kept the !unix build branch and pinned it with a GOOS=windows
  cross-build in moon check/CI rather than declaring the library unix-only —
  that scope call belongs to the user.

## Changes
- `internal/plan` — split arithmetic (PartSize domain type, overflow-safe,
  iter.Seq); 100% coverage (PR #15).
- `internal/manifest` — canonical codec; sha256 blank-import fix; EmptyConfig
  helper (PRs #15, #17).
- `internal/transfer` — ports + orchestrator: hash-pipelined parallel push
  with in-flight claim set, verify-every-byte pull with read/write error
  tagging, manifest-last invariant (PRs #16, #17).
- `internal/oci` — net/http distribution client: explicit Content-Length,
  relative-Location resolve, Content-Range verification, StatusError,
  sha256-only digest refs (PR #16).
- `internal/file` — Source/Sink adapters: partial file, atomic publish with
  dir fsync, symlink refusal, Discard (PRs #16, #18).
- Root package — Client/Push/Pull, options, three public sentinels mapped in
  one classify helper, compile-pinned Example (PR #17).
- `e2e_test.go` — zot testcontainers suite incl. byte-flipping proxy
  corruption test; zot pinned v2.1.20 (PR #17).
- `docs/` — format.md canonical-encoding + sha256 clauses; design.md port
  sketch reconciled; how-to/push-and-pull.md added (PRs #15–#17).
- Tooling — mockery 3.7.2 mise-pinned, mocks + staleness asserts, moon
  `mocks`/`build-windows` tasks, test task runs -race (PRs #16, #18).
- Precision audit cuts, net -69 lines (PR #18): Pull returns error-only, dead
  accessor/guards removed, not-found chain deduplicated, .moon template
  leftovers deleted.

## Open Threads
- Phase 2 (reference CLI, own module under `cli/`) is next; it also carries
  phase 1's deferred manual proofs (PLAN.md phase-2 checklist).
- Release PR #11 now reads 0.1.0 and stays open until the first release is
  deliberately cut.
- Docs site renders (Pages deploy green) but the phase-7 checklist still owns
  the full verification.
- Declined audit finding recorded for posterity: public WithHTTPClient nil
  guard stays (documented nil-is-ignored contract; auditors disagreed).

## References
- PRs: #15 (plan+manifest), #16 (ports+adapters), #17 (orchestrator+API+e2e),
  #18 (precision audit). Open: #11 (release-please, 0.1.0).
- PLAN.md (this folder) — the approved seven-phase plan with per-phase gates.
- Design: https://componere.github.io/bigoci/explanation/design/ ; format:
  https://componere.github.io/bigoci/reference/format/ ; how-to:
  docs/docs/how-to/push-and-pull.md.
- Prior session: `.journal/001/SUMMARY.md`.

## Lessons
- go-digest registers no hash itself: without a `crypto/sha256` blank import
  in NON-TEST code, digest validation rejects everything in production while
  tests pass (the testing framework links the hash). Recurs for any new
  digest-touching package.
- Registry variance lesson #2: zot v2.1.9 loses zero-length blobs via dedupe
  hardlinking. Pin registry images by exact version in e2e; distrust
  point-release registries near edge cases (empty blobs).
- `encoding/json.Marshal` HTML-escapes `&<>` — deadly for cross-
  implementation manifest determinism; SetEscapeHTML(false) and pin bytes
  with golden tests including escapable characters.
- `mise x --cd <dir>` changes the child process's working directory — two
  agents corrupted go.mod because of it; forbid the flag in agent prompts.
- `io.CopyBuffer` collapses reader and writer errors into one value; tag
  reader errors if downstream classification must tell a network failure
  from a disk failure.
- Corrupting bytes via a reverse proxy in front of the registry beats
  mutating container storage: deterministic, works in CI, exercises the real
  verification path.
