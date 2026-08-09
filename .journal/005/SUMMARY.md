---
id: 005
title: Phase 4 — resume shipped and gated
date: 2026-08-08
status: complete
repos_touched: [componere/bigoci]
related_sessions: [001, 002, 003, 004]
---

## Goal
Execute phase 4 of `.journal/002/PLAN.md`: a killed push or pull, re-run,
transfers only what is missing — push resume via HEAD skip re-validated under
kill, pull resume via re-hashing the `.bigoci-partial` part ranges and
fetching only mismatches, mid-part continuation via `Range` with whole-part
fallback — plus the kill/resume e2e suite and the three manual gates.

## Outcome
Goal met in full. Three PRs merged (#25–#27; master d69afc0), all four
phase-4 success criteria checked in PLAN.md with dated evidence, and the
three manual gates (plus a bonus complete-partial gate) passed against zot
v2.1.20 behind a toxiproxy throttle with evidence journaled (NOTES 15:58).
The session ran under ultracode with every workflow agent on opus (user
directive: no Fable inheritance): a three-lens design panel judged by two
comparative adversaries, one opus implementer per PR, and per-PR review
panels — with every diff line-by-line reviewed by the lead, all panel
findings applied or explicitly declined, and all commits made by the lead
under the GitHub-verified identity.

## Key Decisions
- `Blobs.Get` grew a third return value naming the offset the stream
  actually starts at (contractually `off` or `0`) -> the 200-for-range
  fallback costs no extra request and no attempt, no sentinel needs a home,
  and the oci-never-imports-transfer rule survives. Unanimous across three
  independent designers.
- Mid-part continuation keeps the hasher alive across attempts (wire bytes
  only) rather than re-hashing the disk prefix -> both judges ruled the
  same way: io.copyBuffer delivers each chunk before surfacing its read
  error so hasher==sink==count on every transient failure, write failures
  are untagged and never retried, and mixing disk bytes into the digest
  would have forced a transient carve-out on the terminal ErrDigestMismatch
  rule — the constraint phase 3 fixed.
- Cold-resume verify folds into the worker pipeline (verify is the first
  step of a part's job, outside the retry budget), gated on
  `Size() == FileSize && FileSize > 0` read BEFORE the unconditional
  Truncate -> hashing runs at worker parallelism overlapping fetches, and
  the ordering is now part of the Sink.Size contract.
- Judges' boundary cap adopted: `done == part.Size` restarts the part whole
  -> a failure arriving with the final chunk would otherwise ask for an
  unsatisfiable range; this is also what keeps 416 safely terminal (lead
  ruling: a real 416 proves the registry's blob is shorter than the
  manifest — content mismatch family, never classified transient).
- Wrong-size partial is Truncated and fully fetched, not discarded ->
  Discard is not on the port, Truncate documents shrink-to-fit, and
  discard-then-recreate reopens the O_NOFOLLOW window. PLAN.md's
  "discarded and restarted" wording amended with a dated note.
- Nothing is ever recorded between runs (no sidecar, no marshalled hasher
  state — the latter can leak file content into a world-readable sibling)
  -> the bytes on disk are the whole of the state; accepted cost: a rerun
  hashes the full partial before fetching, documented in the how-to.
- e2e kills are causal, never timed: with Workers:1 the (N+1)th request
  proves parts 0..N-1 are complete (fetchWorker/drain take the next job
  only after the previous unit returned), and messy rows read the intact
  set off the disk after cmd.Wait, never off wire timing.
- PR shape: three PRs, not the plan's two — the port change landed inert
  first (#25, the transfer call site guards start != 0 so the suites
  staying green proves inertness), the behavior flip second (#23/#25
  precedent from session 004).

## Changes
- `internal/transfer/ports.go`, `internal/oci/blobs.go` — Get reports the
  stream's actual start; 200-for-range becomes `(body, 0, nil)`;
  `blobReadStart` replaces `checkBlobRead`; 416 stays untagged terminal
  (PR #25).
- `internal/transfer/pull.go` — `resumable`, Size-before-Truncate,
  `partFetcher.resume`, `verify` (returns `(bool, error)`; a mismatch is
  never an error value), the `done` counter through fetch/attempt/stream,
  cap + start guard + 200 re-plan, hasher reset only at `done == 0`,
  progress recorded only on the read-side branch (PR #26).
- `internal/transfer/` tests — `resume_test.go` + `continue_test.go` (new),
  `stream_internal_test.go` (in-package pin for the write-side recording
  rule), fixture churn (offset-aware blobStore/fetchingBlobs, seedable
  memFile, Size-wired mock sinks), 29-mutation review pass with four
  surviving mutants each given a killing test before merge (PR #26).
- `docs/` + `README.md` + `cli/README.md` — design.md continuation +
  "What a resume proves" (SIGKILL windows, page cache, accepted costs),
  how-to resume section, front page/index status, CLI resume recipes over
  the frozen grammar (xxd-XOR byte flip; no grammar change) (PR #26).
- `e2e_kill_test.go`, `e2e_proxy_test.go`, `e2e_resume_test.go` (+ unix/
  windows shims) — TestMain self-exec child, counting reverse proxy with
  causal triggers and loud backstops, ten kill/partial/continuation rows;
  `TestE2ECorruptedPartsFailThePull` gained its counted resume assertion
  (PR #27).
- `.journal/002/PLAN.md` — phase-4 boxes checked with dated evidence;
  guard-rail wording amended.
- `.journal/005/DESIGN.md` — the governing phase-4 design preserved.

## Open Threads
- Phase 5 (auth + real registries) is next. Reserved seams intact: exit 6 /
  `ErrUnauthorized`, and the PHASE-5 instrument constraint (auth set at
  request-build time in internal/oci with the caller's transport outermost;
  the presigned-redirect clean client derived from the caller's client).
  Verified this session and worth carrying: net/http strips only
  auth/cookie headers across redirects, so `Range` survives a presigned
  redirect to object storage.
- Release PR #11 (0.1.0) still open until the first release is cut
  deliberately; it now carries this session's two feat commits.
- A CDN/proxy that caps open-ended ranges can exhaust a large part's budget
  mid-tail (each capped answer ends cleanly short and costs an attempt) —
  documented as an accepted cost; revisit only if phase 6 measurement says
  otherwise.
- cli/README's variable-count resume example is framed, not captured; the
  deterministic `blob-read=0 manifest-read=1` line is a live capture from
  gate 4.

## References
- PRs: #25 (feat(oci): report the offset a blob read starts at, 2b06ecf),
  #26 (feat(transfer): pull resume from partial files, d789849),
  #27 (test(e2e): kill-and-resume coverage, d69afc0).
- Governing design: `.journal/005/DESIGN.md` (panel wf_2db7765e-8e0).
- Manual-gate evidence: `.journal/005/NOTES.md` 2026-08-08 15:58 entry.
- Plan: `.journal/002/PLAN.md` (phases 1–4 checked; 5–7 remain).
- Prior sessions: `.journal/001..004/SUMMARY.md`.

## Lessons
- `httptest.Server.Close()` is the happens-before barrier for handler-side
  bookkeeping: it blocks until the last handler returns. Records filed
  after `ReverseProxy.ServeHTTP` returns are NOT ordered against the client
  finishing — the client sees the last body byte first, and a measured 8 ms
  handler deschedule flipped assertion rows red until settle barriers went
  in. Anything asserted from proxy records needs the barrier.
- toxiproxy bandwidth toxics are per-connection: four workers move 4x the
  configured rate. Divide by the worker count when throttling a gate.
- The frozen `-debug` grammar's `http>`/`http<` lines carry no `bigoci:`
  prefix and a sequence number before the verb — grep `^http> .*class=`,
  not the verb.
- `GOOS=windows go vet ./...` type-checks test files; `go build` does not —
  a windows cross-build gate cannot see syscall use in `_test.go`. Unix-only
  test syscalls need build-tagged shims for the gate to stay honest.
- mockery's `.NotBefore(call)` pins cross-method ordering (Size before
  Truncate) — claims mutation testing cannot reach through behavior can be
  pinned as mock-declared ordering instead.
- When removing injected probe code with string replacement, the inserted
  and removed strings must match byte-for-byte including newlines — a
  half-removed injection cost one CI round-trip on a whitespace lint.
