---
id: 005
title: Phase 4 — resume
started: 2026-08-08
---

## 2026-08-08 13:52 — Kickoff
Goal for the session: execute phase 4 (resume) of `.journal/002/PLAN.md`.
Current state of the world: phases 1–3 are complete and gated (master fdbfc03,
PRs #15–#24). The phase-4 seams are already in place: Sink.ReadAt/Size/Discard,
Blobs.Get offset + Content-Range verification, and the partial-file lifecycle.
The retry machinery (internal/retry, per-part retry.Do sites) and the reference
CLI instrument are shipped.
Plan: read PLAN.md phase 4 task list, confirm scope with the user, then design
and implement resume following the established design-panel / implement /
review pattern.

## 2026-08-08 14:05 — Recon complete; design panel launched
User set ultracode and directed: workflow agents must never inherit Fable —
override models per task (opus/sonnet); Fable (the lead) reviews and commits.

Recon of the shipped seams (all already on master):
- oci.Blobs.Get(ctx, dgst, offset) already sends `Range: bytes=N-` for
  offset>0, verifies Content-Range start on 206 (blobs.go checkRangeStart),
  and returns an ERROR on 200-for-range — port doc explicitly delegates the
  whole-part fallback to the orchestrator (ports.go Get godoc).
- file.Sink has the full partial lifecycle: CreateSink deliberately does NOT
  truncate an existing partial; ReadAt is documented for resume hashing;
  Close leaves the partial; Discard removes it; Commit is sync+close+rename+
  dirsync. client.go discardEmptyPartial removes only zero-size partials.
- transfer.Pull currently: fetchManifest → decode → Truncate(FileSize) →
  fetchParts (worker pool, partFetcher with per-worker buf+hasher, whole-part
  refetch per retry attempt) → Commit. Push HEAD-skip resume already works
  (Exists inside the retry attempt = idempotent attempts).
Phase 4 is therefore concentrated in internal/transfer/pull.go + e2e.

Open design questions handed to the panel (Q1–Q10): 200-fallback signaling
across the port (sentinel vs Get returning actual stream start vs adapter-
internal), mid-part continuation state (keep hasher across attempts vs
re-hash prefix from disk; io.CopyBuffer delivers each chunk before surfacing
read errors so hasher==sink on transient failures), resume-verify placement
(pre-pass vs folded into worker jobs), wrong-size partial (truncate vs
discard), budget semantics for continuations, e2e kill harness across the
module boundary (helper-process self-exec vs cli exec; counting proxy;
causal kill trigger; SIGKILL vs SIGINT), push = validation only?, D6 scope,
hash-pass parallelism, Truncate placement.

Workflow wf_2db7765e-8e0: 3 opus/xhigh designers (invariants lens, seams
lens, failure-modes lens) → 2 opus/xhigh comparative adversarial judges
(correctness angle, provability angle). Synthesis into DESIGN.md is mine.
