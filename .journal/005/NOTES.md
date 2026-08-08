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

## 2026-08-08 15:35 — PR 1 merged (#25, master 2b06ecf)
feat(oci): report the offset a blob read starts at. Opus implementer +
3-lens review panel (wf_f57be27f-35c): all clean at fatal/major/minor, with
8-mutation kill checks, independent byte-identical mock regen, and a 9-case
contract probe ("could not construct an input where the adapter reports a
start the port forbids"). Lead applied the panel's polish: guard message
blames the blob port not the registry (an operator would otherwise chase a
registry bug that lives in an adapter), port godoc gains the failed-call-
reports-zero clause + negative-off precondition + "served a range" wording,
blobReadStart godoc explains the redundant-but-clarifying 404 arm, explicit
wantTransient:false on the 416 row, new ranged-404 row pinning ErrNotFound.
Commit made by lead under joshuagilman@gmail.com (verified+signed).
Learned: (1) golangci-lint caches results pointing into deleted worktrees
and then aborts its generated-file filter — mock-file lint noise after a
worktree removal means "golangci-lint cache clean", not mock edits. (2) The
IDE LSP diagnoses this multi-worktree repo against the WRONG worktree's
packages (claimed old mock arity while build+tests pass in the worktree) —
trust the compiler in the worktree, verify with grep before dismissing.
PR-2 handoff (from panel, verified): widen the pull.go guard to {0, done}
and update its one "asks for zero" clause; invert the two assert.Zero
offset guards (fixtures_test.go:322, retry_fixtures_test.go:372);
wholeBlob (fixtures_test.go:331) is the seam for nonzero starts; four
inline MockBlobs doubles in hardening_test/pull_test accept any offset and
need a sweep when the guards invert; a 206 honoring the start may legally
truncate the range (CDNs cap open-ended ranges) so stream's declared-length
check is the only thing catching a short 206; blobBody mid-read transient
tagging is proven only for the whole-blob shape — PR 2 needs a ranged-body
break row; resumeOffset=5 fixture in blobs_test is the ready honored-range
case. The DESIGN.md cap rule (done==part.Size → restart at 0) is what keeps
the orchestrator from ever sending an unsatisfiable range, so 416-terminal
stays safe for the complete-partial case (cold verify never fetches).

## 2026-08-08 16:55 — PR 2 merged (#26, master d789849)
feat(transfer): pull resume from partial files — the behavior flip. Opus
implementer + 3-lens panel (wf_31660ca4-150). State-machine reviewer walked
all 14 failure-table rows with in-package probes (hasher==sink==count after
every transient shape, incl. composite break→200-fallback→break). Tests
reviewer ran 29 mutations: 24 killed outright, 4 real survivors each got a
killing test from me before merge — (1) offsets assertion in
TestPullAssemblesTheFileThroughARetry pins done-unchanged-on-Get-error, (2)
TestPullContinuesTheRefetchOfAResumedPart crosses resume×continuation and
pins verify-outside-retry.Do (moving it inside would false-ErrDigestMismatch
the likeliest field scenario: killed pull rerun on the same flaky link), (3)
TestPullRefusesAnOverlongBlobOnAContinuedAttempt pins the remaining-bytes
limit, (4) stream_internal_test.go pins write-side no-progress-recording
(only observable in-package). Also added TestPullRefusesAPartialThatReadsShort
(verify short-read row), tightened blobStore.serve to refuse at-end offsets,
brokenPrefix now takes content (was coupled to a prefix-stable PRNG
coincidence). Docs fixes from panel: front-page README still denied resume
(D6 miss in DESIGN docs list — README.md was absent from it), client.go
"before fetching anything" contradiction, range-capping-intermediary cost
sentence in design.md+how-to, cli/README byte-flip made a real flip (xxd
XOR; printf 'x' is a no-op 1/256), variable summary block reframed, blob-read
"count of missing parts" qualified with no-retries.
e2e: ZERO assertion changes needed; flaky suite passes 5/5 (continuation
strictly helps limit_data rows converge). Implementer deviations all
accepted (wholeBlob folded into serve(dgst, off) — Go forbids f(a, g())
multi-value; brokenPrefix real-bytes flip was semantically forced by
continuation; two test files on the which-parts vs inside-one-part seam).
Declined: ctx param on verify (doc'd the one-part cancellation bound
instead); design.md "First slice" prose left as plan record.
Lesson: mockery .NotBefore(call) pins cross-method ordering (Size before
Truncate) — the TOCTOU-ish claims that mutation testing can't reach via
behavior get pinned as mock-declared ordering instead.
Next: PR 3 test(e2e) kill-and-resume.
