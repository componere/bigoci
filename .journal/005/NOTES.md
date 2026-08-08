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

## 2026-08-08 14:45 — Design synthesized (DESIGN.md in this folder)
Panel outcome: both judges ranked [seams] first and ruled identically on the
one contested question. Seven of ten questions unanimous across designers:
Blobs.Get grows a third return value (actual stream start ∈ {off, 0} — the
200-fallback becomes free, no sentinel, import graph untouched); verify folds
into the worker pipeline gated on size==FileSize && FileSize>0; Truncate
stays unconditional after Size (order load-bearing); wrong-size partial =
Truncate + fetch-all (PLAN.md "discarded" wording to be amended with dated
note); one unchanged budget per part, 200-fallback costs no attempt; push
needs zero code; Workers/copyBufferSize reused.
Q2 ruled for the LIVE HASHER across attempts (against [invariants]' prefix
re-hash): io.copyBuffer delivers each chunk before surfacing its read error,
multiwriter feeds hasher first, write-side failures are untagged and never
retried — and decisively, re-hashing mixes disk bytes into the digest, which
forces a transient carve-out on the terminal ErrDigestMismatch rule.
Judges' mandatory corrections adopted: cap done→0 at part.Size boundary
(unsatisfiable-range 416 bug in [seams]); orchestrator terminal-guards
start ∉ {0, done}; progress recorded only on nil-or-read-error branch;
hasher reset only when resume point is 0, after the re-plan.
[failure-modes] had a fatal flaw (accepting 0<start<offset corrupts the
digest verdict) but its e2e harness is grafted wholesale: TestMain self-exec
child, counting proxy (pass-through/strip-Range/cut-mid-part-once),
Workers:1 causal kill trigger on the (N+1)th request, on-disk intact-set
assertions ([seams] graft) for messy rows, SIGKILL window table.
My lead rulings: 416 stays untagged terminal (cap removes the self-inflicted
path; a real 416 proves the registry's blob is shorter than the manifest —
same family as content mismatch); sparse-partial rerun hash cost accepted +
documented; refuse-Range pinned at integration level, not e2e.
PR shape: 3 PRs — feat(oci) inert port change (call site guards start!=0,
provable inertness), feat(transfer) resume + all docs, test(e2e) kill
coverage. Next: implement PR 1.
