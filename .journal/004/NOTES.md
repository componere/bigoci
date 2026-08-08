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
