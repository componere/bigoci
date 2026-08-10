---
id: 009
title: Phase 7 — progress, docs set, and v0.1.0 released
date: 2026-08-09
status: complete
repos_touched: [componere/bigoci]
related_sessions: [001, 002, 003, 004, 005, 006, 007]
---

## Goal
Execute phase 7 of `.journal/002/PLAN.md` — everything a first real user
touches: the progress reporting option, the full Diátaxis documentation set,
the API surface review, and the first release — closing all five manual gates
and shipping v0.1.0.

## Outcome
Goal met in full. Four PRs merged (#44 progress, #45 docs set, #47 surface
pass, #11 release; master 7d027c2), **v0.1.0 tagged and published**, all five
phase-7 PLAN.md boxes checked with dated evidence, and all seven phases of the
plan now complete. Also absorbed: the Dependabot oras-go bump set (#33–#36,
clearing all 15 open alerts), and four concurrent-session PRs (#40–#43, #46)
that landed mid-phase — including #40's correction of DefaultWorkers from 4 to
8, swept through every doc at rebase. Ran under ultracode with every workflow
agent's model overridden per task (opus for design/review/judging, sonnet for
scans; Fable excluded per Josh's directive); the lead reviewed every diff line
by line and made every commit under the GitHub-verified identity.

## Key Decisions
- Progress carries TWO byte counters — CompletedBytes (provably in place,
  whole-part steps, monotone, bounded) and WireBytes (crossed the boundary,
  unbounded) -> one number cannot be both monotone and honest under the
  whole-blob fallback / retry re-sends; both judges ranked the
  accounting-first design over ergonomics-first and CLI-first.
- SkippedParts adopted over judge 0's objection (lead ruling): the single
  rule "no own bytes over the wire, judged across the whole retry budget"
  makes it one honest fact, and it is the only way to read a half-skipped
  push. Zero-byte-part semantics pinned post-implementation: skipped means
  the part's own Put/Get never ran.
- No rate, no eta in the CLI (lead ruling): every rate any panel design
  rendered was provably misleading somewhere; render(Progress, elapsed) stays
  a pure byte-for-byte-testable function, and wire= deltas carry throughput.
- Dedupe credit deferred to settle: the claim ledger (take/settle with a
  waiter count) credits a follower only when the claiming upload lands, so a
  mostly-duplicate file never reads nearly-complete while its one real
  upload runs.
- The closed latch is the guarantee-maker: tagSourceReads is the http.Request
  body, read on net/http's writeLoop, so a straggling read can fire after
  Push returns; finish() latches under the same lock the callback runs
  under. Judges 0 and 1 independently demanded it; the review round then
  proved the latch untested (deletable with a green suite) until a
  reporter-level mutation-killing table went in.
- CLI renders via a ticker goroutine with injected ticker channel + clock,
  the callback storing only — a callback-driven printer goes silent through
  the exact backoff window the manual gate watches; -progress follows
  -timeout's zero-means-off convention.
- Docs set written by four parallel writers on disjoint files with two
  adversarial reviewers (conformance + technical accuracy), on top of a
  nine-agent violation scan of the existing docs; the lead triaged all 24+37
  findings by hand and rejected the over-flattening ones (house vocabulary
  like "Honest caveats" restored).
- Release flow honored as designed: release-please's draft:true means publish
  is a decision; phase 7 was the decision, and the tag preceded the publish,
  which is all `go get` needs.

## Changes
- `internal/transfer/` — progress.go (reporter: mutex-serialized snapshots,
  closed latch, credited[] idempotence, 4 MiB coalescing, attempted() retry
  counting, wire/hash counting writers), push.go (structural outer/inner
  split, claim ledger take/settle, uploaded-flag skip rule, wire at
  tagSourceReads), pull.go (PhaseResolving before fetchManifest, measured()
  at decode, counting writer appended LAST to stream's MultiWriter — composed
  at rebase with #42's contextReader on the read side), doc.go (PR #44).
- Root — progress.go (Direction/Phase at iota+1, ten-field Progress,
  Fraction, WithProgress), client.go (adapter closure stamping Direction via
  explicit enum switch), options.go, doc.go ("# This phase" auth-stale
  section replaced with # Reliability) (PR #44).
- `cli/` — progress.go (lineWriter, watcher, pure render, injected
  ticker/clock), -progress flag with negative-usage-error, README progress
  sections; frozen grammar untouched (PR #44).
- Tests — per-edge-case accounting tables (all 11 design rows), shared
  invariant recorder, channel-gated dedupe test, deterministic straggle test
  (5 MiB part crossing the coalescing threshold), reporter-level latch table
  (8 rows, each independently mutation-killing), deterministic tick
  rendezvous (holdFirstRequest + signalingWriter, bounded waits), zero-call
  tests, enum-mapping table, construct-level zero-alloc benchmarks (PR #44).
- `docs/` — tutorial/get-started.md (verified verbatim three times, twice at
  authoring + once post-release from the published page), reference/api.md,
  reference/errors.md, index.md grouped by Diátaxis type, mkdocs nav; fixes
  across every existing page incl. format.md manifest size 800KB->600KB
  (computed: 623,192 bytes), sparse-partial disk guidance, design.md
  two-counter explanation + stale-fact removals; README install/usage per
  readme-writer (PR #45).
- Godoc/example pass — retry budget corrected to attempts framing (was
  overstated by one attempt in both transfer godocs), Reference's fourth rule
  (sha256-only), redirect-copy phrasing, Example_progress (deadlock-free:
  stops on the transfer's return), retry-policy comment naming both docs
  pages (PR #47).
- Release — PR #11 merged, tag v0.1.0, draft published; scratch-module
  `go get @v0.1.0` proven (`true <nil> 8 536870912`).
- `.journal/002/PLAN.md` — all five phase-7 boxes checked with dated
  evidence; `.journal/009/DESIGN.md` — the governing progress design with
  both judges' syntheses and the lead's two rulings.

## Open Threads
- Issue #48: the kill-resume e2e row's vacuity guard fired once on CI ("8 is
  not less than 8") — the 8-worker default (#40) shrank the kill window; the
  row needs causal kill scheduling or a bigger fixture if it recurs.
- The tick/terminal render nuance: a tick in the ~100ms window between the
  transfer's return and stop() renders the terminal snapshot beside the
  final line (identical counters). Ruled within contract; documented here in
  case a future gate greps for exactly one terminal line.
- pkg.go.dev will index v0.1.0 on first fetch; nobody has eyeballed its
  rendering of the new progress godoc (the go/doc resolver was verified
  locally, so links should render).
- The plan is fully executed; there is no phase 8. Future work starts from a
  fresh design conversation.

## References
- PRs: #44 (feat(api): progress reporting option, 8a2ba0d), #45 (docs: user
  documentation set, 01979b6), #47 (chore: v0.1.0 API surface and godoc
  pass, 72f6525), #11 (chore(master): release 0.1.0, 7d027c2); Dependabot
  #33–#36; concurrent sessions' #40–#43, #46.
- Release: https://github.com/componere/bigoci/releases/tag/v0.1.0
- Governing design: `.journal/009/DESIGN.md` (panel wf_9efec9b7-6ba; judges'
  breaks and syntheses preserved in the session scratchpad).
- Gate evidence: `.journal/009/NOTES.md` entries 2026-08-09 22:50 and 23:40.
- Plan: `.journal/002/PLAN.md` — phases 1–7 all checked.
- Issue filed: #48 (flaky kill-resume at 8 workers).

## Lessons
- A test that cannot fail is worse than no test: overlay-mutation
  verification (delete the guard, expect red) found the latch and the tick
  renderer both untested behind green assertions. Per-finding adversarial
  verification earned its cost twice more (three of six review findings
  refuted, three confirmed with reproductions).
- testify's mock.Called records assert.CallerInfo() and allocates per stack
  frame — a mock-driven benchmark measures call-stack depth, not your code
  (two empty frames cost more than the whole feature). Prove zero-cost with
  construct-level benchmarks and -gcflags=-m escape analysis instead.
- Streamed LSP diagnostics are unreliable when multiple worktrees share one
  gopls view: they lag, cite the wrong tree, and contradict a passing build.
  Never diagnose from them; run the build.
- Concurrent sessions change ground truth mid-phase: the bench audit moved
  DefaultWorkers 4->8 while the docs were being written. At every rebase,
  re-verify measured facts against the code rather than resolving conflicts
  textually.
- The zero-call contract bites example authors: a progress example that
  waits for a terminal snapshot deadlocks on a pre-transfer failure, because
  the API's own promise is that such failures deliver nothing. The
  fresh-eyes surface review caught the lead's own example doing this.
- `gh pr checks --watch` races check registration right after a push and can
  exit success on stale/absent checks; sleep before watching, and never
  chain an unconditional merge onto it.
- A documented toxiproxy recipe at toxicity 0.3 can legitimately produce
  zero failures across a whole push (connection pooling shrinks the draw
  count); gates that need an observed retry must either raise toxicity or
  loop until the event is actually observed, never assume it happened.
