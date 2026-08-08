---
id: 003
title: Phase 2 — reference CLI shipped and gates proven
date: 2026-08-08
status: complete
repos_touched: [componere/bigoci]
related_sessions: [001, 002]
---

## Goal
Execute phase 2 of the implementation plan (`.journal/002/PLAN.md`): build the
reference CLI as the manual-verification vehicle in its own Go module, wire it
into CI, and run the five phase-2 manual gates — which double as phase 1's
deferred manual proof.

## Outcome
Goal met in full. PRs #19 and #20 merged (master 796d477); all five manual
gates passed against zot v2.1.20 with evidence journaled; PLAN.md now shows
phases 1 AND 2 fully checked. The session ran under ultracode: a three-lens
design panel, an Opus implementer, independent acceptance + live-zot smoke
agents, and a three-reviewer panel (36 findings), with every diff line
reviewed by the lead and correctness fixes applied by hand.

## Key Decisions
- stdlib `flag`, no cobra -> two of three design lenses chose it independently;
  L1 plus a trivially auditable module graph (the phase-2 structure gate)
  decided it. cobra's own proposal conceded break-even at two commands.
- Unset flags pass NO library option (fs.Visit detection) -> library defaults
  rule; phase 6 re-measuring them changes the CLI with zero edits; `-title ""`
  stays distinguishable from omitted.
- The -debug grammar (`http>`/`http<`/`http!` + fixed-shape summary) is a
  frozen contract -> journal recipes grep it; renaming a field is breaking.
- Redaction is default-deny and value-verified: scheme-only Authorization,
  query values elided, `digest=` passes only a verified sha256 value, no body
  ever logged -> closed a real log-forgery vector and a presigned-token leak
  found in review.
- Manual signal handler (Reset first after delivery), not signal.NotifyContext
  -> NotifyContext swallows the second Ctrl-C; one designer got this wrong and
  one caught it; review then caught the implementation putting Reset after a
  blockable write.
- Exit codes 3/4/5 for the sentinels with 6/7 reserved (ErrUnauthorized,
  ErrPartTooLarge) -> phase-5's errors.Is gate reads off `$?`; a recorded
  signal outranks the error's shape (130/143).
- Per-project `--disable gomoddirectives` for cli lint -> keeps the local-
  replace guard armed for the core module where it matters.
- Squash-collapsed the PR branch to one commit under joshuagilman@gmail.com ->
  the ruleset requires verified signatures and the implementer agent's commit
  was attributed to an email GitHub could not verify.

## Changes
- `cli/` — the reference CLI, own module with local replace (PR #19): push and
  pull, part-size/title/workers/plain-http/timeout/debug flags, strict
  stdout/stderr contract, sentinel exit codes, pure-observer debug tap with
  default-deny redaction, in-process fake-registry end-to-end test, 700-line
  README whose recipes are the manual gates.
- `.moon/workspace.yml`, `cli/moon.yml`, `moon.yml` — cli project registered;
  format/lint/build/test/check per commit; cli:check wired into root:check;
  `--allow-parallel-runners` on both lint tasks (PR #20).
- `.github/workflows/ci.yml` — Go/golangci cache keys hash `cli/go.sum` (PR #20).
- `release-please-config.json` — `exclude-paths: ["cli"]` so CLI commits never
  version the library (PR #20).
- `.journal/002/PLAN.md` — phase 1's deferred-proof box and phase 2's five
  gate boxes checked with dated annotations.

## Open Threads
- Phase 3 (retries) is next; the CLI's -debug/-timeout and dead-port fail-fast
  are its ready instrument.
- PHASE-5 CONSTRAINT (all three designers flagged it): auth must be set at
  request-build time in internal/oci with the caller's transport outermost,
  and the presigned-redirect "clean client" must derive from the caller's
  client — otherwise the CLI's -debug goes blind and the no-auth-leak gate
  passes vacuously. Also in TECH_NOTES.md.
- API feedback recorded for phase 7's review: Pull returns only error (the
  file-digest gate needs curl+jq; consider returning the descriptor);
  ToFile accepts a directory (CLI guards it); malformed reference has no
  sentinel (exits 1, not 2); name the future sentinels exactly
  ErrUnauthorized/ErrPartTooLarge (exit codes 6/7 reserved on them).
- Release PR #11 (now 0.1.0) stays open until the first release is cut.
- Go stamps vcs.revision from the parent checkout when building in a git
  worktree — CLI provenance line can name the wrong commit; README documents
  the go version -m cross-check.

## References
- PRs: #19 (feat(cli): reference CLI for push and pull, a658d00),
  #20 (ci: build and test the cli module, 796d477).
- PLAN.md: `.journal/002/PLAN.md` (phases 1–2 checked; 3–7 remain).
- Gate evidence: session 003 NOTES.md 2026-08-08 09:05 entry.
- Prior sessions: `.journal/001/SUMMARY.md`, `.journal/002/SUMMARY.md`.

## Lessons
- Agent-made commits can inherit a git identity the forge cannot verify
  (harness context email vs. the GitHub-associated one): signed-but-
  unverifiable commits block merges under a verified-signature ruleset.
  Commit agent work yourself or audit `%ae` before pushing.
- golangci-lint refuses concurrent instances; any orchestrator that
  parallelizes lint across modules (moon does) needs --allow-parallel-runners.
  It passed locally by scheduling luck — treat single-shot local success of
  parallel pipelines as unproven.
- A debug log that re-emits peer-controlled bytes is an injection surface:
  percent-decoded query names could forge whole log lines, and a value
  pass-through keyed on a parameter NAME leaks anything a host names
  "digest". Escape on render; verify values, never trust names.
- signal.NotifyContext (and any handler that does work before Reset)
  swallows the second Ctrl-C; Reset must be the first statement after the
  receive, before anything that can block.
- GitHub's squash-merge commit is created server-side, so the ruleset's
  signature check applies to the PR's branch commits — a squash does not
  launder an unverifiable commit.
