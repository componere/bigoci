---
id: 004
title: Phase 3 — retries
started: 2026-08-08
---

## 2026-08-08 09:39 — Kickoff
Goal for the session: execute phase 3 of `.journal/002/PLAN.md` — the full
retry policy from the design's Defaults table (4 attempts, exponential backoff
with 1 s base / 30 s cap / full jitter, `Retry-After` honored, retry on network
errors/429/5xx, fail fast on other 4xx), applied per part.

Current state of the world: phases 1–2 are complete and proven. Master is at
796d477 with a clean tree. The core library pushes/pulls end to end against zot
(e2e suite incl. corruption proxy), and the reference CLI (`cli/`, PRs #19–#20)
passed all five phase-2 manual gates — its `-debug`/`-timeout` output and
sentinel exit codes are the ready instrument for phase 3's manual gates.
Deliberate seams already in place for this phase: `oci.StatusError` for retry
classification and transfer's read-vs-write error tagging.

Plan (from PLAN.md phase 3): retry decision logic in the core with injected
sleep (clock-free tests); adapter maps HTTP failures to typed errors so the
core decides retryable vs. terminal without seeing HTTP; failure injection at
the mock layer (dropped connections mid-part, 429 + Retry-After, 500s,
out-of-order completion); e2e through toxiproxy killing connections
mid-transfer. Gates: three manual (toxiproxy ride-through, zot restart
mid-push, dead-port fail-fast) plus automated backoff/decision matrix and
flaky-network e2e.

## 2026-08-08 09:50 — Situated; design panel running
Session runs under ultracode; user directive: workflow agents must NOT run on
Fable — override to opus/sonnet/haiku per task.

Read the full core surface: transfer (ports/push/pull/transfer), oci
(repository/blobs/manifests incl. StatusError + Is(ErrNotFound)), public
errors/client/options, e2e harness (zot fixture, corrupting proxy), cli/run.go
(sentinel exit table; 6/7 reserved rows documented in sentinelExits comment).
Observations that matter for phase 3:
- transfer's Push/Pull godocs say "does not retry" — those lines and ports.go's
  "orchestrator owns the retry policy" contract need reconciling edits (D6/D1).
- Pull's readError tagging is already in place for read-vs-write classification;
  digest mismatch stays terminal.
- oci.StatusError carries Method/Path/Status/Detail; needs Retry-After capture
  for 429/503 without breaking Is(ErrNotFound) or the CLI's error rendering.
- e2e uses testcontainers.Run + a corrupting httputil.ReverseProxy pattern —
  toxiproxy fits the same shape (client -> proxy -> zot).

Launched a three-lens design panel (workflow wf_0cd5dd2b-ce7, all opus:
concurrency@xhigh, boundaries@high, verification@high). Each returns a full
design proposal + key decisions + risks + PR split; I synthesize the final
design myself before spawning the implementer.

## 2026-08-08 10:21 — Design panel results; boundaries lens re-running
Panel finished (601k tokens). Concurrency and verification lenses returned
complete, high-quality designs (scratchpad design-*.md). The boundaries agent
burned its budget on research and submitted literal placeholders
("DEFERRED_TO_TOOL_INPUT") — re-running it as wf_938926b3-846 with an explicit
anti-deferral instruction and a minLength on the schema.

Where the two finished lenses AGREE (locked in for synthesis):
- Push retry unit = Exists+Put as one attempt sharing one per-part budget
  (idempotency win: a PUT that landed but lost its response is found by the
  next attempt's HEAD). Claim stays outside the loop. Section reader and the
  empty-config bytes.Reader are constructed INSIDE the attempt.
- Pull retry unit = Get+stream+verify whole-part; hasher.Reset + fresh
  OffsetWriter per attempt; every retry calls Get(dgst, 0) — no Range in
  phase 3. Manifest Get/Put and empty config get their own budgets.
- Terminal: unclassified errors (default-stop), digest mismatch, long body,
  sink write/truncate/commit, source reads, ctx cancellation (checked FIRST —
  a cancelled request looks like a transport error underneath).
- Transient: transport failures (unconditionally — no per-errno table),
  429/503 overloaded (carry Retry-After), 408/5xx faulted, mid-stream read
  errors (short body). Two adapter choke points: Repository.do + statusError.
- Two real bug-catches to fold in: (a) the one-extra-byte tail probe in
  pull.stream must read through tagReads or a connection dying right after
  the last byte is a terminal false alarm; (b) manifest body reads (io.ReadAll
  in Manifests.Get) must classify as transport failures too.
- 413 only → oci-level match via StatusError.Is → public ErrPartTooLarge →
  CLI exit 7 activated; codes table comment narrows to 6. No vendor tables.
- No public retry knobs (design doc API sketch names none; ctx +
  WithHTTPClient are the seams). Internal policy fully injectable via specs.
- Injected Sleep func(ctx,d) error selecting on ctx.Done (a terminal peer
  failure wakes sleeping workers instantly); injected jitter fn; rand/v2.
- Windows double in an overflow-proof guarded loop: 1s/2s/4s defaults, no
  sleep after last attempt, wrap only on exhaustion ("after N attempts:"),
  attempts never reset on kind change.
- e2e: new test file, zot+toxiproxy on one docker network, Shopify
  toxiproxy/v2/client (stdlib-only), toxic removed on first OBSERVED failure
  via a counting gate transport (never a wall-clock schedule), limit_data +
  reset_peer; no new moon task, no build tag. Dead-port automated shadow test
  at root (20s ceiling, NotErrorIs DeadlineExceeded).
- Digest mismatch stays terminal — contradicts one design.md prose line ("a
  corrupt part re-fetches alone") which gets reconciled in the doc edit. Real
  behavior decision to surface at review.

Where they DISAGREE (boundaries lens breaks the tie, then I decide):
1. Vocabulary/layout: internal/fault (Kind enum: Broken/Overloaded/Faulted/
   Rejected + Fault() interface; oci imports fault) + internal/retry package
   — vs — Transient()/Delayed() interfaces declared in transfer, oci
   implements structurally (no new import edge), retry loop unexported inside
   transfer.
2. Retry-After: floor (max(jitter, min(hint, Cap)), one 30s cap) — vs —
   override (hint replaces jitter, separate 60s clamp).
3. PR order: oci-classification-first (user-visible ErrPartTooLarge lands
   alone, then transfer flips behavior) — vs — transfer-inert-first, oci
   flips. Both 3-PR splits keep e2e+deps last.

## 2026-08-08 10:37 — Design finalized (DESIGN-phase3.md in scratchpad)
Boundaries rerun landed a decisive design and CHANGED my provisional call on
the seam. Final synthesis (scratchpad/DESIGN-phase3.md is the governing doc):

- Seam: ONE new package internal/retry holding both the vocabulary — a
  transparent tag `retry.Transient(err, after)` + `retry.IsTransient(err)`,
  untagged = terminal — and the policy (Policy/Default/Do, Sleep+Rand func
  types, zero-value = default). Killer arguments: duck-typed interfaces
  can't classify *url.Error values bigoci doesn't own (a wrapper is needed
  anyway); a Kind enum's "core decides" is ceremony (the per-status table
  lives in the adapter's kindOf regardless) at the cost of a second package
  and methods on three error types. oci imports retry (adapter → core leaf,
  legal); oci still never imports transfer; root never imports retry.
- blobBody wrapper in oci.Blobs.Get tags mid-stream read failures — and
  structurally fixes the tail-probe false-terminal AND composes with
  readError untouched. readManifest's ReadAll gets the same tag.
- My amendments vs the boundaries proposal: context-first guard kept in
  Do (defense in depth); Retry-After = FLOOR clamped by the one 30s Cap in
  the CORE (not override-as-sent + 2min adapter constant); errors.Join
  replaced by single-line %w wrap (Join is multi-line → would break the
  CLI's one-line failure contract); single-attempt failures unwrapped.
- Push unit = Exists+Put (idempotency win), claim outside loop; pull unit =
  whole part, fresh OffsetWriter+hasher per attempt; manifest ops + empty
  config own budgets; digest mismatch/long body/disk/unclassified terminal.
- PRs: (1) feat(oci): classify registry failures for retry [retry pkg +
  oci tagging, INERT], (2) feat(transfer): per-part retry with backoff
  [behavior flip + ErrPartTooLarge + CLI exit 7 + ALL docs], (3) test(e2e):
  toxiproxy ride-through suite [only PR adding deps].
- Verified myself: Retry-After already on CLI redactor allow-list; Shopify
  toxiproxy client stdlib-only (2 go.sum lines); design.md pull-path prose
  contradiction confirmed (doc edit + review flag).
Next: implementation worktree from fetched master; Opus implementer builds
PR1; I review line-by-line, commit myself (verified-signature rule), then
review panel.
