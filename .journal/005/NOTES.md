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

## 2026-08-08 15:58 — Phase-4 manual gates: all four PASSED
Instrument: CLI built from master d789849 (provenance cross-checked with
go version -m: vcs.revision=d789849, vcs.modified=false — built in the main
checkout, not a worktree, dodging the wrong-commit stamp quirk). Registry:
fresh zot v2.1.20 (gate-zot, host 5050) behind toxiproxy 2.12.0 (host 5051)
with 25 MB/s-per-connection bandwidth toxics both ways — loopback moves
2 GiB in ~2 s, far too fast to interrupt at 50%, and the toxic rate is PER
CONNECTION (4 workers ≈ 4×), which the first attempt missed. Interrupts
sent causally: poll the -debug log for the Nth request line, then kill -INT
(equivalent to Ctrl-C for a single process). Fixture: 2 GiB urandom at
128 MiB parts = 16 parts; fresh content per push scenario (zot dedupes
globally — the first fixture's parts were already in storage from a
throttle-calibration push and would have made gate 1 vacuous).
Grammar gotcha: http>/http< debug lines carry NO "bigoci:" prefix and a
sequence number before the verb — grep '^http> .*class=blob-write', not
'http> PUT'.

Gate 1 (push, .journal/002/PLAN.md:244): run 1 SIGINT after 8/16 part PUTs
opened → exit 130, 4 parts landed (four 201s in g1r1.log). Run 2 summary:
requests=42 blob-check=17 (5 hit, 12 miss) blob-write=12 upload-open=12
manifest-write=1 — the 4 landed parts + shared empty config HEAD-skipped,
only the 12 missing parts uploaded. Pull-back sha256 b48f4733…f92f ==
source. PASS.
Gate 2 (pull, PLAN:247): run 1 SIGINT after 8/16 part GETs opened → exit
130, dest.bin.bigoci-partial 2 GiB mode 0600 present, dest.bin ABSENT.
Run 2: requests=13 blob-read=12 manifest-read=1 → 4 parts came off disk;
partial gone, dest appeared only at the end, sha256 == source. PASS.
Gate 3 (corrupt byte, PLAN:250): complete partial, xxd-XOR flip at offset
300000000 (0x28→0xd7; offset/134217728 = part 2). Resume: requests=2 —
1 manifest read + 1 blob read of sha256:7b241f07…, verified equal to the
manifest's layers[2] digest via raw curl. sha256 restored. PASS.
Gate 4 (bonus from DESIGN): complete partial under the partial name →
requests=1 blob-read=0 manifest-read=1, commits, sha256 correct — the
cli/README "blob-read=0 manifest-read=1" recipe number is now a live
capture, and the recipe's requests=3/blob-read=2 framed example shape
matched run 2's arithmetic (requests = blob-read + manifest-read). PASS.
Evidence files in session scratchpad gates/: g1r1.log g1r2.log/out
g2r1.log g2r2.log g3.log g4.log model2.sha.
PLAN.md boxes get checked after PR 3 merges (the automated-gate box needs
kill/resume e2e green in CI).

## 2026-08-08 17:50 — PR 3 merged (#27, master d69afc0); phase 4 COMPLETE
test(e2e): kill-and-resume coverage. Opus implementer + 2 xhigh reviewers
(flake-hunter, provability; wf_b0ba0609-547), ~50 suite runs between them
incl. CPU-hog perturbation and GOMAXPROCS=1, plus harness mutations (timer
triggers, neutered guards, shared content — all fail loudly; the stamping
mutation proved zot dedupe would starve the push triggers without per-row
bytes). Harness highlights: TestMain self-exec child (Setpgid group, kill
aimed at -pid, WaitStatus.Signaled asserted so a child that finished before
the signal fails loudly), counting reverse proxy with causal triggers —
Workers:1 makes the (N+1)th request a PROOF about parts 0..N-1 because
fetchWorker/drain take the next job only after the previous unit returned —
and messy rows read the intact set off the DISK after cmd.Wait, never off
wire timing. The implementer independently derived (workers+1)×partSize as
the messy trigger (lower bound becomes a theorem) and replaced a racy
WaitGroup barrier with httptest.Server.Close as the settle primitive.
Panel findings fixed by lead before merge: (1) MAJOR record-read race —
records were filed after ReverseProxy.ServeHTTP returns, but the client
sees the last body byte before that; an 8ms handler deschedule flipped rows
red (measured), and the complete-partial row's blob-read=0 assertion could
even pass vacuously; fixed with settle barriers before every record read,
verified green with 50ms injected into record(). (2) assertCut now asserts
the cut BIT (flag set where errCutShort returns) not that it was handed
out. (3) Go runtime knobs (GOMAXPROCS/GODEBUG/GOTRACEBACK/GORACE)
forwarded into the child so scheduling bugs stay reproducible. (4)
GOOS=windows go vet regressed (SysProcAttr.Setpgid, syscall.Kill) and
root:build-windows can't see test files — the two syscalls now live behind
//go:build unix|windows shims; both platforms vet clean. (5)
messyKillBytes godoc claimed "by construction" for a bound only guarded
loudly; reworded. (6) corrupted-parts row gained a NotEmpty(intact)
non-vacuity guard. One CI round-trip: my delay-injection cleanup left a
stray blank line (whitespace lint) — the injected string carried a newline
the removal string didn't.
Learned: httptest.Server.Close IS the happens-before barrier for
handler-side bookkeeping (it blocks until the last handler returns);
anything asserted from proxy records needs it — "record after ServeHTTP"
orders nothing against the client finishing. And toxiproxy bandwidth
toxics are per-connection (already journaled at the gates).
Phase-4 closeout: all four PLAN.md phase-4 boxes checked with dated
evidence; the wrong-size-partial guard-rail wording amended per DESIGN Q4
(Truncate, not discard; dated note in place). Phases 1-4 now complete;
5 (auth) — 7 remain. Reserved seams intact: exit 6/ErrUnauthorized and the
PHASE-5 instrument constraint (auth at request-build time, caller's
transport outermost, clean redirect client derived from caller's client).
