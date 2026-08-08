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

## 2026-08-07 23:40 — Implementation landed; review adjudicated; fixes applied
Implement workflow done (6 agents, ~2.5k lines incl. tests + 525-line README,
commit 7abae4e on feat/reference-cli). Independent acceptance re-check: all
green. Live zot smoke (8 checks incl. manifest conformance, warm-push
HEAD-skip, sentinel exits): ALL PASS. One environment quirk: Go stamps
vcs.revision as the worktree's PARENT commit inside a git worktree — README
caveat, not a code bug.

Review panel returned 36 findings; my line-by-line pass confirmed and I
applied the correctness fixes MYSELF:
1. redact.go: query-name log-line INJECTION (percent-decoded names re-emitted
   raw into RawQuery → a registry-controlled Location could forge whole
   http< lines) + digest= pass-through keyed on the parameter NAME (a host
   calling its signed token "digest" would be logged in full) + unparseable
   queries silently shortened. Fixed: QueryEscape names, isDigest() gates the
   value (sha256:64-lower-hex only), parse failure renders the query as `…`.
2. watchSignals: signal.Reset ran AFTER cancel + a blockable stderr write —
   second Ctrl-C swallowed while stderr stalls (the exact NotifyContext trap
   the handler exists to avoid). Reset is now first after the receive.
3. reportError: 130/143 were gated on context.Canceled surviving the chain;
   now a recorded signal wins over any error shape, with documented lines
   `interrupted by SIGINT (exit 130)` / `terminated by SIGTERM (exit 143)`.
4. `--` terminator now disarms the misplaced-flag guard (was rejected with
   misleading advice); messages teach it.
5. Negative -timeout is now a usage error (was silently unbounded).
6. Summary line prints ALL classes always (fixed shape; warm-push gate reads
   blob-write=0 explicitly instead of noticing an absence).
7. size.go: dropped the TiB special case that hardcoded the internal
   4096-part constant with a rationale 1024G contradicted.
Rejected/deferred: CI-wiring "blocker" (deliberate PR-2 scope per plan);
gomoddirectives config change (PR 2 uses per-project --disable, verified);
auth fingerprint idea (scheme-only is the safer contract).

Delegated to an Opus fix agent (FIXLIST.md): the 3 test updates my changes
require, review-driven test hardening (exact option-slice assertions,
pullOptions test, quoted-value-aware grammar regexes, injection/digest-gate/
unparseable-query redaction tests, signal-beats-error-shape row), a NEW
in-process fake-registry integration test (push/pull through run(), stdout
contract, warm-push dedup, -title wire check), and ~16 README/doc.go
consistency fixes (port 5050, -part-size 64MiB recipes with true numbers,
curl by digest, dead-port wording, provenance caveat, etc.). I review its
diff before committing.
