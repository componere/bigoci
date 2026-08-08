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

## 2026-08-07 16:16 — PR 2 merged (#16, master a1e423d)
3 Opus implementers (ports+mockery / oci adapter / file adapter) + Opus
stabilizer + 3-lens panel. My design calls going in: Manifests port binds
its reference at construction (core never sees reference grammar) —
design.md updated in-PR (D6); Auth port deferred to phase 5; mockery 3.7.2
mise-pinned by me pre-spawn; deps (distribution/reference) pre-added by me.

Panel findings I verified and fixed myself:
- 206 responses now verify Content-Range starts at the requested offset
  (an off-position range would silently corrupt the assembled file).
- Digest references restricted to sha256 (v1 format pin, consistent with
  PR 1's decision).
- Sink: refuses a planted symlink at the predictable partial path
  (Lstat + O_NOFOLLOW via build-tagged const), and Commit fsyncs the
  parent dir after rename (publish durability).
- Dropped file.ErrCommitted sentinel (E1: nobody branches on it).
- Added oci.StatusError (typed, errors.As-able HTTP status) — the seam
  phase 3 retry classification builds on.
- ctx-cancellation tests extended to all five endpoint methods; post-commit
  ReadAt/Truncate covered; mocks got compile-time staleness asserts; moon
  test task now runs -race; OpenSource rejects non-regular files.
Declined (recorded): I1 typedef `oci.Reference string` — the parsed
reference.Named one line in IS the domain type; public Reference type
arrives with the API in PR 3.
Implementer disclosures both PRs: `mise x --cd` sets the child cwd —
two agents accidentally ran go mod tidy in the worktree because of it.
Watch that flag in prompts.
Next: PR 3 — orchestrator + public API + zot e2e (testcontainers).

## 2026-08-07 17:28 — PR 3 merged (#17, master 829a861) — PHASE 1 COMPLETE
2 Opus implementers (orchestrator / public API) → Opus e2e author → Opus
stabilizer → 3-lens panel. e2e passed against real zot first try — no
implementation bug surfaced. Notable: the e2e agent found a REAL zot defect
(v2.1.9 loses zero-length blobs via its dedupe hardlinking; logged
`failed to hard link`) and pinned v2.1.20 — registry-variance lesson #2
for the design's thesis.

Panel findings I verified and fixed myself:
- Identical parts raced Exists→Put and uploaded the same blob once per
  worker; added an in-flight claim set (skip-not-wait is sound: a failed
  claimant fails the whole push).
- Pull's io.CopyBuffer collapsed read/write errors → registry hang-ups
  were blamed on the disk; added a tagging reader so phase 3 can classify.
- Hash pass now rejects a source that shrank mid-push (was: silent
  wrong-manifest risk when the short digest's blob already existed).
- io.ReadFull for the long-blob probe ((0,nil) reads no longer skip it).
- min(workers, parts) goroutines; empty-partial cleanup on failed lookup
  (a not-found pull no longer litters); classify() chain tests; Example()
  compile-pinned; how-to doc (docs/how-to/push-and-pull.md) added per D6.
CI: e2e verified actually running on GH Actions (9.3s root pkg incl. zot).

Phase 1 success criteria: both automated gates checked off in PLAN.md.
Manual proof deferred to phase 2 (reference CLI) per plan. Phase 2 not
started — awaiting user go-ahead.

## 2026-08-07 20:04 — Precision audit merged (#18, master 0ce9ecc)
User asked for a dedicated exactly-what-we-need audit before phase 2.
Three read-only auditors (scope/Opus, redundancy/Opus, mechanical/Sonnet)
swept master. Verdict: tree is tight — no dead subsystems, deps all
load-bearing, deadcode/staticcheck clean, zero TODOs. 20 findings; I
accepted 18 (net -69 lines), the notable ones:
- transfer.Pull → error-only (descriptor had no consumer; design's
  public API is error-only).
- Split-rule validation single-gated through plan.New everywhere
  (extends the PR-1 decision); shared validateRegistryPorts helper.
- Not-found chain said "not found" 3x → StatusError.Is(ErrNotFound),
  no second wrapping layer. Phase 3 classification builds on this.
- Sink.Discard() owns partial removal; Client composes clientSettings.
- Unreachable guards cut (negative offset/size, dead rangeStart
  disjunct); Plan.PartSize() accessor cut; template leftovers in
  .moon/ deleted.
- GOOS=windows cross-build added to moon check/CI so the !unix branch
  cannot rot (chose CI-pin over declaring the library unix-only — a
  scope call the user hasn't made).
- plan.ErrTooManyParts kept + documented as held for the public error
  contract (phase 7).
DECLINED (recorded): cutting the public WithHTTPClient nil guard —
auditors disagreed with each other; the guard implements the documented
nil-is-ignored contract at the public boundary (observable with
nil-after-real option ordering). Both layers keep their guard.
scaffold/ at repo root is session-protocol machinery (journal templates
+ skills), not template debt — correctly unflagged.

## 2026-08-07 21:34 — Close
User cut the session here; phase 2 (reference CLI) is saved for next time.
Handoff state: all session PRs merged and squashed — #15 (plan+manifest),
#16 (ports+adapters), #17 (orchestrator+API+e2e), #18 (precision audit) —
master at 0ce9ecc, worktrees cleaned, no journal contamination, working
tree clean. Release PR #11 reads 0.1.0 and stays open on purpose. SUMMARY.md
written; TECH_NOTES.md updated with the sha256-linkage trap, canonical-
encoding contract, registry-variance case files, deliberate phase seams,
and mise/moon quirks. Next session: start at PLAN.md phase 2 — the `cli/`
reference module — whose gate also carries phase 1's deferred manual proofs.
