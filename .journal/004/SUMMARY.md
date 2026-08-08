---
id: 004
title: Phase 3 — retries shipped and gated
date: 2026-08-08
status: complete
repos_touched: [componere/bigoci]
related_sessions: [001, 002, 003]
---

## Goal
Execute phase 3 of `.journal/002/PLAN.md`: the full retry policy from the
design's Defaults table — 4 attempts, exponential backoff (1 s base, 30 s
cap, full jitter), `Retry-After` honored, retry on network errors/429/5xx,
fail fast on other 4xx — applied per part, with the adapter classifying and
the core deciding, plus the failure-injection suites, the toxiproxy e2e, and
the three manual gates.

## Outcome
Goal met in full. Four PRs merged (#21–#24; master fdbfc03), all four
phase-3 success criteria checked in PLAN.md with dated evidence, and the
three manual gates passed with the evidence journaled (NOTES 13:09 entry).
The session ran under ultracode with all workflow agents on opus/sonnet
(user directive: no Fable inheritance): a three-lens design panel, one Opus
implementer per PR, adversarial review panels, and a dedicated flake-hunter
for the e2e — with every diff reviewed line-by-line by the lead and all
fixes committed under the GitHub-verified identity.

## Key Decisions
- Classification seam: ONE new leaf package `internal/retry` holding both a
  transparent tag (`retry.Transient(err, after)` / `IsTransient`; untagged =
  terminal) and the policy/loop -> duck-typed interfaces cannot classify
  `*url.Error` values bigoci does not own, and a fact-enum's "core decides"
  is ceremony at the cost of a second package; oci imports retry (adapter →
  core leaf), oci still never imports transfer, root never imports retry.
- Cancellation is read off ctx, never off the error's shape -> Go renders
  dial/header/client timeouts as errors matching context.DeadlineExceeded
  while the caller's ctx is alive (review-panel blocker, reproduced before
  fixing); consequence: a caller-supplied http.Client Timeout bounds one
  attempt and the timed-out attempt is retried with a fresh window.
- Retry-After is a floor under the jittered backoff, clamped by the one 30 s
  cap in the core -> never shortens the escalation (herd-safe), no second
  constant, hostile headers cannot park a transfer; adapter reports raw.
- Push retry unit = Exists+Put in one attempt sharing one per-part budget ->
  a PUT that landed but lost its response costs the next attempt one HEAD
  (observed live in gate 1: `blob-check (1 hit)`); claim taken once outside
  the loop; readers constructed inside the attempt.
- Digest mismatch stays terminal -> content-addressed storage serving wrong
  bytes serves them again; design.md's contrary prose edited (deliberate
  behavior decision, flagged in the PR body).
- The disk is never retried on either path -> the uploader tags its section
  reader and unwraps its own marker after a failed Put, because a source
  failing mid-upload surfaces through the adapter looking like a broken
  connection (review-panel find, verified by reviewer with an overlay test).
- No public retry options -> the design doc's API sketch names none, ctx
  bounds total time, WithHTTPClient owns the transport; knobs are forever.
- e2e damage scheduling is causal, never wall-clock -> a gate transport
  repairs the link when counters prove a retry is under way; a fixed 1.2 s
  outage window would flake ~1-in-7 (three jittered sleeps sum < 1.2 s with
  3.6% per unit × 5 units).
- PR order: oci classification inert first, transfer flip second (one
  bisectable behavior commit carrying all D6 docs), e2e + its dependency
  last.

## Changes
- `internal/retry/` — new package: transparent transient tag, Policy/Default/
  Do with injected Sleep/Rand, full unit matrix (PR #21, hardened by #21's
  review fixes).
- `internal/oci/` — classification at the choke points: do()/statusError
  tagging, StatusError.RetryAfter + 413↔ErrTooLarge, blobBody wrapper,
  manifest-read tagging, classify.go (PR #21).
- `internal/transfer/` — uploader restructure, six retry.Do sites, per-
  attempt readers/hasher/offset-writer, source-read tagging on push,
  short-part core tag, ports.go contract rewrite, ~30-row failure-injection
  suite + budget-isolation assertions (PR #23).
- Root — ErrPartTooLarge sentinel + classify row, Push/Pull/options godocs,
  real-clock dead-port test; `cli/` — exit code 7 activated + tests; docs —
  design.md retry subsection + reconciled pull-path prose, how-to "What
  bigoci retries", README/index status, cli/README dead-port recipe rewrite
  (measured counts) + "watching a retry happen" recipe (PR #23).
- `e2e_flaky_test.go` — zot behind toxiproxy, four damage rows, causal gate
  transport with bounded dial, per-digest budget bounds, per-row content
  stamping; dep `Shopify/toxiproxy/v2` client (stdlib-only, 3 lines total in
  go.mod+go.sum) (PR #24).
- `cli/debug_test.go` — per-test transports (de-flake, PR #22).
- `.journal/002/PLAN.md` — phase-3 boxes checked with dated evidence.
- `.journal/004/DESIGN.md` — the governing phase-3 design preserved from the
  session scratchpad.

## Open Threads
- Phase 4 (resume) is next; its seams are in place: Sink.ReadAt/Size/Discard,
  Blobs.Get offset + Content-Range verification, and the partial-file
  lifecycle. PLAN.md phase 4 has the task list.
- Reserved: exit 6 / ErrUnauthorized for phase 5. The PHASE-5 instrument
  constraint in TECH_NOTES still stands.
- Release PR #11 (0.1.0) stays open until the first release is cut
  deliberately; it carries this session's two feat commits.
- The `-timeout`/client-Timeout interaction is documented in WithHTTPClient's
  godoc; phase 5's auth transport must preserve it.
- Design doc's open question (worker self-tuning on 429/503) still awaits
  phase 6 benchmarks; the tag carries `after` but no overload kind — extend
  the vocabulary if phase 6 needs the distinction.

## References
- PRs: #21 (feat(oci): classify registry failures for retry, cb02877),
  #22 (test(cli): tap de-flake, 34e56ce), #23 (feat(transfer): per-part
  retry with backoff, 4dca285), #24 (test(e2e): broken-network suite,
  fdbfc03).
- Governing design: `.journal/004/DESIGN.md` (panel outputs summarized in
  NOTES 10:21/10:37 entries).
- Manual-gate evidence: `.journal/004/NOTES.md` 2026-08-08 13:09 entry.
- Plan: `.journal/002/PLAN.md` (phases 1–3 checked; 4–7 remain).
- Prior sessions: `.journal/001..003/SUMMARY.md`.

## Lessons
- Never gate retry logic on errors.Is(err, context.DeadlineExceeded): net
  and net/http deliberately render dial/header/client timeouts as matches
  for it while the caller's context is alive, so the guard eats the retry
  budget for exactly the failures retries exist for. Only ctx.Err() knows
  whether the transfer is over.
- A source failing mid-upload reaches the orchestrator through client.Do
  looking exactly like a broken connection; keeping "never retry the disk"
  true requires the orchestrator to tag its own reader and unwrap the marker
  on the way back.
- httptest.Server.Close closes http.DefaultTransport's idle connections;
  parallel tests doing real HTTP through the default transport break each
  other. Per-test transports.
- zot dedupes blobs globally and answers HEAD across repositories — any
  multi-scenario e2e reusing content silently stops uploading anything.
  Vary bytes per scenario.
- Damage semantics decide what evidence a fault-injection row can claim:
  reset-at-connect kills the cheap probe request, not the expensive body,
  so "an upload retried through the damage" is only provable under a toxic
  the probes survive (limit_data). A reviewer's fix can be right about the
  problem and wrong about the mechanism — the proposed upload-failure gate
  would have deadlocked for the same reason the finding was true.
- errgroup cancellation mid-backoff means only the first-exhausted unit
  shows the full attempt count; any gate or doc counting retries per worker
  must expect fewer on the cancelled peers (measured 4/3/3/3).
- Full jitter makes fixed fault windows flaky by construction (P(three
  sleeps < 1.2 s) ≈ 3.6% per unit); schedule fault removal causally, off
  observed damage.
