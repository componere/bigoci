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

## 2026-08-07 14:30 — Plan revised: reference CLI replaces dev driver
User feedback on the plan: instead of a throwaway `internal/dev` driver,
build a proper **reference CLI** — reference meaning never published, no
production intent, exists purely to demonstrate push/pull. It lives in its
own Go module (`cli/`, `replace` directive at the core) so CLI dependencies
never enter the core module's graph, and gets its own phase before it is
needed.

PLAN.md rewritten accordingly: new Phase 2 "Reference CLI" inserted after
the walking skeleton (the CLI needs the public API, so it cannot precede
phase 1); phases renumbered to 7 total. Phase 1's manual proofs are
deliberately deferred into phase 2's gate since the CLI is the instrument
that exercises them — this is the plan's one sequencing exception. The CLI
carries a `--debug` request-logging mode because later manual gates (HEAD
skips in phase 4, the no-auth-header-leak check in phase 5) depend on that
observability.

## 2026-08-07 14:40 — Phase 1 kickoff (PR 1 of 3)
Plan approved by user. Starting PR 1: `feat(core): split planner and
manifest codec`. Repo recon done: Go 1.26.4 (mise-pinned, GOTOOLCHAIN=local),
moon tasks format/lint/build/test/check, strict golangci config (notable:
mnd, funlen, cyclop≤30, dupl, gochecknoglobals, godoclint, godot; depguard
only denies deprecated pkgs; tests get relaxed rules). go.mod has zero deps.

Working pattern per user: implementation legwork by Opus/Sonnet workflow
agents (never Fable-inherited), with my own line-by-line review before
anything is committed — agents' understanding of AGENTS.md rules is not
trusted, it is verified.

Key PR-1 design calls (mine, to be enforced in review):
- deps: opencontainers/image-spec (manifest struct, DescriptorEmptyJSON),
  opencontainers/go-digest, stretchr/testify. All mature/canonical (L1).
  Pre-added to go.mod by me before agents run so parallel agents never race
  on go.mod.
- internal/manifest imports internal/plan for decode-side split-rule
  validation: the format's split rule *is* the planner, one source of truth.
- canonical encoding = compact json.Marshal of the image-spec struct
  (fixed field order, sorted annotation keys) → determinism; decode accepts
  any valid JSON formatting. Round-trip byte-stability test required.
- empty file = one zero-length part (format math holds: part 0 covers
  [0,0)); document it.
Worktree: `feat/core-plan-manifest` off fetched master.

## 2026-08-07 15:19 — PR 1 merged (#15, master 7c40d85)
Workflow ran 2 Opus implementers + Opus stabilizer + 3-lens review panel
(contract/rules/tests). Tree was green as delivered; the panel + my own
line-by-line review surfaced real defects I then fixed myself:
- HIGH (confirmed via `go list -deps`): internal/manifest never linked
  crypto/sha256, so go-digest Validate() would reject EVERY valid manifest
  in a binary that does not link the hash some other way. Tests
  structurally cannot catch it (testing framework links sha256). Fix:
  blank import in non-test code. **This trap recurs for any future package
  that validates digests (internal/oci, internal/transfer) — remember it.**
- json.Marshal HTML-escapes &<> → canonical encoding now uses
  SetEscapeHTML(false); format.md Determinism section now specifies the
  cross-implementation canonical encoding; golden test pins an escapable
  title.
- Non-UTF-8 titles rejected (were silently corrupted by the encoder).
- Part digests pinned to sha256 like the file digest (format.md updated).
- Config `data` member verified against the config digest when present.
- Absent body mediaType accepted (image-spec: member is optional).
- I1: plan.PartSize domain type; transposition now a compile error.
- Split rule single-gated through plan.New; too-many-parts wraps
  plan.ErrTooManyParts consistently on encode and decode.
- plan.New guard order fixed + misnamed test case now asserts messages.
Coverage: plan 100%, manifest ~98%. Local quirk (stabilizer): `mise x --
moon run` collides with proto's go on GOROOT; use `mise x -- env -u GOROOT
moon run ...` or run the go/golangci commands directly.
Next: PR 2 (ports + oci/file adapters + mockery).
