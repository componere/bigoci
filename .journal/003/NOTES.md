---
id: 003
title: Phase 2 — reference CLI
started: 2026-08-07
---

## 2026-08-07 21:41 — Kickoff
Goal for the session: execute phase 2 of the implementation plan
(`.journal/002/PLAN.md`) — the reference CLI that serves as the
manual-verification vehicle, plus running phase 1's deferred manual proofs.

Current state of the world: master is 0ce9ecc (PR #18). Phase 1 is merged
(PRs #15–#18): push/pull works end to end against zot in testcontainers,
automated gates checked off in PLAN.md. Phase 1's manual proof is deferred
into phase 2's gate. Release PR #11 reads 0.1.0 and stays open deliberately.

Plan (from PLAN.md phase 2):
1. PR `feat(cli): reference CLI for push and pull` — `cli/` with its own
   Go module (`github.com/componere/bigoci/cli`), `replace` to the core;
   `push`/`pull` commands; flags for part size, workers, plain-HTTP;
   `--debug` HTTP request logging; reference-only README.
2. PR `ci: build and test the cli module` — moon/CI wiring; excluded from
   release-please and any publishing path.
3. Run the phase-2 manual success criteria (which double as phase 1's
   manual proof) and check them off in PLAN.md.

## 2026-08-07 21:52 — Phase 2 underway (ultracode mode)
User confirmed phase 2 (earlier "phase 3" was a misstatement). Ultracode is on:
workflow orchestration for substantive tasks, agent models overridden per task
(Opus/Sonnet/Haiku only — Fable excluded per user directive). Delegation
pattern from session 002 stays: agents do legwork, I review every diff line.

Done so far: verified phase 2 untouched on master (0ce9ecc, no cli/, no PRs);
created implementation worktree `.wt/feat-reference-cli` (branch
feat/reference-cli); surveyed public API (client.go, options.go, errors.go,
file.go), moon.yml, workspace.yml, release-please config, CI workflow.
Launched 3-lens design panel (stdlib-minimal / cobra-conventional /
verification-first, all Opus): I synthesize the spec when it returns.

## 2026-08-07 22:20 — Design synthesized, implementation launched
Design panel returned three strong proposals (~150 KB, archived in the
session scratchpad). Two of three (stdlib-minimal, verification-first)
independently chose stdlib `flag`; cobra's own case conceded break-even at
two commands. Decision: stdlib flag — L1, trivially auditable module graph
(the phase-2 gate), reference-only surface.

Synthesized spec (scratchpad/SPEC.md) — key calls:
- Unset flags pass NO option (fs.Visit detection); library defaults rule;
  `-title ""` ≠ omitted. Help text reads Default* constants at runtime.
- Part-size grammar: binary units only (B/K/KiB/M/MiB/G/GiB), SI units
  rejected with a teaching message, no TiB, overflow-checked.
- stdout = data only (push: one digest line; pull: nothing). stderr:
  `bigoci:` lines + `http>`/`http<`/`http!` debug grammar (frozen contract).
- Exit codes 0/1/2, sentinels 3/4/5, 6/7 reserved (ErrUnauthorized,
  ErrPartTooLarge), 130/143 signals. Unconditional "matched sentinel" line.
- Signals: manual Notify + Reset after first delivery — NOT
  signal.NotifyContext (it swallows the second Ctrl-C; wedged transfer
  would be unkillable). One design got this wrong, one caught it.
- -debug only installs the tap (WithHTTPClient); without it the library's
  default client path is what runs. Tap: pure observer, two lines per
  request (send + headers), default-deny header allow-list, auth=scheme-only
  (no fingerprint), query values elided except digest, no body ever logged,
  class-based summary counters.
- Verified mechanics: `bin/` gitignore covers cli/bin; golangci-lint has
  per-invocation -D so cli lint disables gomoddirectives while the core
  keeps the guard; release-please v5 supports exclude-paths (PR 2).

API feedback captured for later phases (record at close in TECH_NOTES):
1. PHASE-5 INSTRUMENT CONSTRAINT: caller's transport must stay outermost
   (auth header set at request-build in internal/oci, not an inner
   RoundTripper) and the presigned-redirect "clean client" must be derived
   from the caller's client — otherwise the CLI's -debug goes blind and the
   no-auth-leak gate passes vacuously. All three designs flagged this.
2. Pull returns only error → the file-digest-annotation gate needs curl+jq;
   candidate pre-v0.1.0: Pull returns descriptor (phase-7 API review).
3. Reserve sentinel names ErrUnauthorized / ErrPartTooLarge (exit 6/7).
4. ToFile accepts a directory (confusing partial+rename failure); CLI
   guards it as a usage error.
5. Malformed reference has no sentinel → lands on exit 1 not 2.

Launched phase2-cli-implement workflow: Opus implementer (commit on branch),
then parallel acceptance re-check (Sonnet), live zot smoke test (Sonnet,
Docker), and three Opus reviewers (correctness/rules/spec-fidelity). My
line-by-line review follows when it returns.
