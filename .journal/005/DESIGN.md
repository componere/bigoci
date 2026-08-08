# Phase 4 — Resume: governing design

Synthesized 2026-08-08 from a three-lens design panel (invariants, seams,
failure-modes; all opus/xhigh) and two comparative adversarial judges
(correctness, provability; workflow `wf_2db7765e-8e0`, outputs preserved in the
session scratchpad). Both judges ranked [seams] first and ruled identically on
the one contested question. This document is the synthesis: [seams]'s port
contract and orchestrator shape, the judges' four mandatory corrections, and
the named grafts from the other two designs. It supersedes nothing in
`docs/docs/explanation/design.md` except the "arrives with resume" deferrals it
now fulfills.

## Stance

Resume is a hash of the bytes on disk, never a memory of what an earlier run
did. The core learns exactly one new fact it cannot infer — whether a registry
honored a `Range` — and buys it with one new return value on `Blobs.Get`, not
a sentinel, not an import edge, not a sidecar file. Cold resume folds into the
existing worker pipeline; mid-part continuation is a byte counter and the
worker's own hasher kept alive across retry attempts. No new public API, no
CLI grammar change, no format change, no push code change.

## The ten decisions (settled)

Q1 — **200-fallback: the port signature.**
`Get(ctx, dgst, off) (io.ReadCloser, int64, error)`; the second value is the
offset the returned stream actually starts at, contractually **either `off` or
`0`** and nothing else. A 200 answering a ranged request stops being an error:
the adapter returns the body with `start=0` and the orchestrator consumes it
as a from-zero fetch **inside the same attempt** — zero wasted round trips,
import graph untouched (`oci` still imports only the `retry` leaf), and the
adapter's 200-for-range error branch (`internal/oci/blobs.go:254-262`)
collapses to `return 0, nil`. A 206 starting anywhere other than `off` stays
an adapter error (`checkRangeStart` unchanged). Rejected: sentinel (no legal
home; wastes a round trip; spends an error on a success), adapter-internal
discard (moves the prefix over the wire invisibly), a second `GetFrom` method.

Q2 — **Mid-part continuation: live hasher across attempts** (both judges,
overruling [invariants]' prefix re-hash). Verified against go1.26.4:
`io.copyBuffer` writes and counts each chunk before inspecting the read error
that came with it (`io/io.go:428-452`); `multiWriter.Write` feeds the hasher
first (`io/multi.go:83-95`, order set at `pull.go:292`); `hash.Hash.Write`
never fails; so after any **read-side** failure, hasher state == sink bytes ==
the running count. The one divergent case is a **write-side** failure, which
is returned untagged and therefore never retried (`pull.go:301`,
`retry.Do` at `internal/retry/retry.go:62-65`, pinned by
`TestPullDoesNotRetryADestinationItCannotWrite`). Decisive argument against
re-hashing the disk prefix: it mixes disk bytes into a digest that must speak
only about wire bytes, which forces a transient carve-out on the terminal
`ErrDigestMismatch` rule — the constraint phase 3 fixed. The live hasher keeps
the mismatch verdict unconditional. Structural enforcement: progress is
recorded **only** on the nil-or-read-error branch of the copy.

Q3 — **Cold-resume verify folds into the worker pipeline.** Each job's first
step, when the pull is a resume, hashes its own range from the sink through
the worker's existing buffer and hasher; a match completes the job with zero
network and **no error value ever constructed**; a mismatch falls through to
the fetch path. Verification therefore runs at `Workers` parallelism and
overlaps the fetches of broken parts. Gate: `resume := existing ==
artifact.FileSize && artifact.FileSize > 0`, computed from `Sink.Size()`
**before** `Truncate`. The `> 0` clause keeps a zero-byte artifact fetching
its single empty part exactly as today (load-bearing for
`internal/transfer/pull_test.go:40` and the empty-file e2e row).

Q4 — **Wrong-size partial: `Truncate(FileSize)` and fetch everything.** No
`Discard`, no re-create: `Discard` is not on the port, `Truncate` already
documents shrink-to-fit for exactly this case (`internal/file/sink.go:129-133`),
and discard-then-recreate reopens the `O_NOFOLLOW` window
(`internal/file/sink.go:60-76`). Same-size-different-artifact: every range
fails verify, every part is fetched; the extra read pass is accepted — the
alternative is recorded state, i.e. the sidecar this design forbids.
**PLAN.md's phase-4 wording ("discarded and restarted") is amended by this
decision** — observable behavior is identical; annotate PLAN.md with a dated
note at gate-closing time.

Q5 — **Budgets unchanged: one budget per part, 4 attempts default.** A
continued attempt is an ordinary attempt. The 200-fallback consumes **zero**
attempts and zero extra requests (it is handled inside the attempt that
discovered it). Progress never resets the budget — a budget that resets on
progress is unbounded. The resume verify sits outside `retry.Do` and has no
budget: disk failures are terminal.

Q6 — **e2e kill harness: `TestMain` self-exec child + counting reverse proxy
+ causal triggers.** Details under Testing. SIGKILL for automated kill rows
(harshest state, no unwinding window), one SIGINT row for the graceful path,
SIGINT stays the manual gates' signal (the CLI's exit-130 contract).

Q7 — **Push: no code change.** `Exists` already sits inside the retry attempt
(`internal/transfer/push.go:283-305`); a re-run re-hashes and HEAD-skips.
Phase 4 adds kill-and-rerun evidence only. A killed push's abandoned upload
session is garbage the registry collects; assert nothing about it.

Q8 — **Docs: `format.md` untouched** (resume changes no byte a registry
stores; if it had to change, resume would have leaked into the wire contract —
that would be the bug). The `-debug` grammar gains nothing: `range=` and
`crange=` are already allow-listed (`cli/redact.go:68-92`), blob paths print
digests, and the summary line already carries `blob-read=`/`manifest-read=`/
`blob-check=N (H hit, M miss)`. Full change list under Docs impact.

Q9 — **No separate verify parallelism; reuse `copyBufferSize`.** Parallelism
is `Workers` because the verify is the first step of a job. The buffer is the
worker's own, idle at that moment; `io.SectionReader` has no `WriteTo` and
sha256's digest no `ReadFrom`, so `CopyBuffer` genuinely streams through it.

Q10 — **`Size()` first, then `Truncate(FileSize)` unconditionally.** Order is
load-bearing (`Truncate` destroys the evidence); the no-op truncate keeps
"workers write into a fully-sized file" a fact this pull established rather
than an assumption, and keeps the existing `AssertCalled(Truncate, fileSize)`
shapes. `Size()` failure is terminal.

## Algorithms

### Cold resume

```
Pull: validate → fetchManifest (own budget; unconditional — it names the
digests, part size, and file size; nothing can be verified without it)
→ decode → existing := Sink.Size() [terminal on error]
→ resume := existing == FileSize && FileSize > 0
→ Sink.Truncate(FileSize) [unconditional]
→ fetchParts(…, resume) → Commit
```

Per job, per worker, when `resume`: hash `[part.Offset, part.Offset+part.Size)`
from the sink. Match → job done, no network. Mismatch → fetch with progress 0.
Sink read failure or short read → terminal. Holes read as zeros
(`internal/file/sink.go:84-95`) and cannot match a real digest.

**Complete partial:** manifest fetch + N hash passes + `Commit`, zero blob
GETs. CLI evidence: `blob-read=0 manifest-read=1`.

### Mid-part continuation

Per part, `done int64` starts 0 and lives in `fetch`'s frame across the whole
`retry.Do` run; the worker's hasher accumulates wire bytes across attempts.

Per attempt:

1. **Cap (judges' correction a):** `if done == part.Size { done = 0 }` — a
   read failure or probe failure that surfaced with the final chunk leaves
   nothing fetchable; asking for `bytes=part.Size-` would be unsatisfiable
   (416). Restart the part whole.
2. `content, start, err := blobs.Get(ctx, dgst, done)`. On error: `done`
   unchanged (no byte moved; the prefix and hasher are intact); the tag
   decides whether a next attempt runs and it continues from `done`.
3. **Guard (correction b):** `start ∉ {0, done}` → terminal error. The port
   promises only two answers; a core that positions writes by an
   adapter-supplied number bounds it anyway.
4. `start == 0 && done > 0` → range ignored: `done = 0`; consume the same
   body as a from-zero fetch. **No error, no attempt consumed.**
5. **Hasher reset only when `done == 0`, after the re-plan (corrections c+d).**
6. Stream `part.Size - done` bytes through
   `MultiWriter(hasher, NewOffsetWriter(sink, part.Offset+done))`; record
   `done += written` **only** on the nil-or-read-error branch; the write-side
   branch returns untagged first and records nothing.
7. Clean-EOF short body → orchestrator's transient tag, message phrased
   against the part's total (`done`), next attempt continues from `done`.
8. `done == part.Size` → one-extra-byte probe (longer-than-declared stays
   terminal; a probe read failure is transient and the cap restarts the part),
   then the digest check — **terminal `ErrDigestMismatch`, unconditionally**;
   the hasher holds wire bytes only.

### Per-attempt failure table (normative)

| # | Outcome | Detected at | Tag | `done` after | Next attempt asks | Budget |
|---|---|---|---|---|---|---|
| 1 | transient read failure mid-body (incl. presigned URL expiring mid-stream) | adapter tag via `tagReads` | transient | `done+written` | `Range` from `done` | −1 |
| 2 | body ends cleanly short of declared size | orchestrator (`written != part.Size-done`) | transient | `done+written` (cap rule if boundary) | `Range` from `done` | −1 |
| 3 | 200 answering a ranged Get | `start == 0` | **not a failure** | reset 0 | same attempt, byte 0, body in hand | −0 |
| 4 | 206 at the wrong byte / no `Content-Range` | adapter `checkRangeStart` | untagged | — | — | terminal |
| 5 | `start ∉ {0, done}` reaches the core | orchestrator guard | untagged | — | — | terminal |
| 6 | write failure / `ErrShortWrite` | not a `*readError` | untagged | **not recorded** | — | terminal |
| 7 | digest mismatch, part fully arrived | after streaming | untagged `ErrDigestMismatch` | — | — | terminal |
| 8 | blob longer than declared | end probe returns a byte | untagged | — | — | terminal |
| 9 | end probe read error | end probe | transient (adapter tag) | `part.Size` → cap → 0 | whole part | −1 |
| 10 | 416 answering a ranged Get | adapter `statusError`, untagged | untagged | — | — | terminal |
| 11 | `Get` fails to open (dial, 404, 429, 5xx) | adapter classification | per tag | unchanged | `Range` from `done` | −1 |
| 12 | resume-verify mismatch | `verify`, before `retry.Do` | **no error value exists** | starts 0 | first attempt, byte 0 | −0 |
| 13 | resume-verify read failure / short read | `verify` | untagged | — | — | terminal |
| 14 | ctx cancelled | `retry.Do` | outranks every tag | — | — | ends |

**Lead ruling on row 10 (416):** stays untagged terminal, with no adapter
classification change. Our ranges always start inside the declared part size,
so a 416 proves the registry holds a shorter blob than the manifest describes
— the same family as content the manifest does not describe. The row-1/2 cap
rule removes the only self-inflicted path to a 416. The [failure-modes]
belt-and-braces transient tag is rejected: it alternates the surfaced error
across the budget and can bury the truthful truncation message.

Rows 7 and 12 are kept apart **by construction**: `verify` returns
`(bool, error)` and cannot express a mismatch as an error; `attempt` returns
`error` and cannot express one as benign. A row-12 mismatch whose refetch then
hits row 7 is a genuine `ErrDigestMismatch` — that time the registry really
served bad bytes.

### SIGKILL timing windows (all safe; graft from [failure-modes])

Before `Truncate`: 0-byte partial → fetch everything. Between `Truncate` and
first write: full-size zeros → every range fails verify. Mid-`WriteAt`: a
completed `pwrite` cannot tear; the affected range fails verify. After last
write, before `Commit` (or between `Sync` and `Rename`): complete partial →
zero blob GETs, verify + commit. Between `Rename` and `syncDir`: destination
complete, partial gone → fresh pull re-fetches; wasteful, correct. **Page
cache:** SIGKILL kills the process, not its dirty pages — resume after a
process kill is sound with no per-part fsync (why the sink syncs only at
`Commit`). After a machine crash the partial may hold torn ranges — the verify
catches them because it hashes rather than counts.

## Port & contract changes

Exactly one method signature changes; no port gains a method; `Sink`,
`Source`, `Manifests` untouched; `partJob` untouched (resume state lives on
`partFetcher`, not the job — the type is shared with push).

`internal/transfer/ports.go` — `Blobs.Get` becomes:

```go
Get(ctx context.Context, dgst digest.Digest, off int64) (io.ReadCloser, int64, error)
```

Godoc obligations (adapt [seams]'s draft): the reported offset is either `off`
(range honored) or `0` (range ignored; whole blob from its first byte); no
other value is ever reported — an implementation handed a range starting
elsewhere fails the call. Reporting rather than refusing is what makes the
fallback free. Fresh-request-per-call, caller-owns-reader, and
classified-body-failures paragraphs survive as they stand.

`internal/oci/blobs.go` — `Get` returns the third value; `checkBlobRead`
becomes `blobReadStart(resp, off) (int64, error)`; the 200-for-range branch
returns `(0, nil)`; `checkRangeStart`/`rangeStart` unchanged; godoc at
`blobs.go:76-81` rewritten from promise to description. Sink godoc for `Size`
gains "a pull reads it once, before it truncates" (the ordering is contract,
not comment). Compile-time port assertions in `internal/oci/port_test.go` and
`internal/file/port_test.go` unchanged. `.mockery.yml` unchanged; only
`internal/oci/mocks/blobs.go` regenerates.

## Orchestrator changes (`internal/transfer/pull.go`)

- `Pull`: add `Size()`-before-`Truncate` and the `resumable` computation.
- `resumable(existing int64, artifact manifest.Artifact) bool` — new,
  unexported, pure, one expression; godoc carries the `FileSize > 0`
  reasoning.
- `fetchParts`/`fetchWorker`/`newPartFetcher`: thread one `resume bool`.
- `partFetcher`: one new field `resume bool`.
- `fetch`: verify-first when resuming; then `var done int64` + `retry.Do`
  closure over `attempt(ctx, job, &done)`.
- `verify`: new, ~six lines, `CopyBuffer(hasher, SectionReader(sink, off,
  size), buf)` after `hasher.Reset()`; short-read check; returns
  `(matched bool, err error)`.
- `attempt`: takes `done *int64`; cap rule; unpacks `start`; guard; re-plan;
  conditional hasher reset.
- `stream`: takes `done *int64`; limits to `part.Size-*done`; writes at
  `part.Offset+*done`; records progress only on nil-or-read-error branch;
  phrases the short-body transient against the part total.
- `tagReads`/`readError`: unchanged. `push.go`, `transfer.go`: unchanged.

## Testing plan

### Unit — `internal/oci`

`TestBlobsGet` grows a `wantStart` column; the "registry ignores the range" row
flips from `wantErr: true` to `(whole body, start 0)`; wrong-start and
missing-`Content-Range` 206 rows stay errors; a 416 row pins untagged/terminal
(`retry.IsTransient` false).

### Integration — `internal/transfer` (mockery mocks + existing fixtures)

Fixture churn (inventory is normative — graft from [invariants]):
`memFile` gains `readAt` + seedable constructor
(`fixtures_test.go:336-393`); `mockSink` wires `Size`/`ReadAt`
(`fixtures_test.go:400-408`); every bare `MockSink` gains a `Size`
expectation (`retry_pull_test.go:269-272`, `:303-305`, `pull_test.go:210-243`
rows, `hardening_test.go`); both "never asks for a byte range" guards invert
(`fixtures_test.go:322`, `retry_fixtures_test.go:372`); `fetchingBlobs`
records per-`Get` offsets and its script gains an `ignoreRange` answer.
`TestPullDoesNotRetryADestinationItCannotSize` (`retry_pull_test.go:288-318`)
is actually a Truncate test — rename it `…CannotTruncate`; the new `Size` test
takes the freed name.

New tests (names indicative):
1. `TestPullResumesFromAPartialFile` — table: complete partial → zero Gets,
   one Commit; prefix present → exactly the missing digests; one corrupted
   middle part → exactly that digest; all zeros → everything; too-long/too-
   short partial → Truncate to FileSize + everything; zero-byte artifact with
   zero-byte partial → still fetches its one part.
2. `TestPullContinuesAPartMidStream` — body dies at 40%; second `Get`'s
   offset is exactly the bytes written; only the remainder served; bytes
   exact. Second row: two consecutive continuations.
3. `TestPullRestartsAPartWhenTheRegistryIgnoresTheRange` — mock answers
   `(whole body, 0)`; exactly one `Get` for the attempt; only the original
   break's backoff recorded (no attempt consumed); bytes exact.
4. `TestPullKeepsAResumeMismatchOutOfTheDigestSentinel` — corrupted partial
   resumes to success; no `ErrDigestMismatch` anywhere.
5. `TestPullDoesNotRetryAPartialItCannotRead` — `ReadAt` fails: zero Gets,
   zero sleeps, terminal.
6. `TestPullDoesNotRetryADestinationItCannotMeasure` — `Size` fails: no
   Truncate, no Get.
7. `TestPullMeasuresTheDestinationBeforeItSizesIt` — ordered expectations
   (`Size` `.NotBefore` `Truncate`).
8. `TestPullBudgetsAContinuedPartLikeAnyOther` — three mid-part breaks then a
   fourth failure → "after 4 attempts", three waits, no budget extension.
9. `TestPullReportsARegistryBlobShorterThanTheManifest` — the 416 row and the
   converging-short-body row.
10. Boundary-cap row — read error delivered with the final chunk: next
    attempt asks offset 0, not `part.Size` (pins correction a).

### e2e — `e2e_resume_test.go` (+ kill harness)

**Helper:** `TestMain` in `package bigoci_test` (verified: none exists);
`BIGOCI_E2E_HELPER=push|pull` + ref/path/part-size/workers env vars → run one
transfer, `os.Exit`, never `m.Run()`. Child builds its own `http.Client`
(per-process transport). Parent: `os.Executable()` re-exec, explicit
`cmd.Env`, streams buffered and logged on failure, `cmd.Cancel`/`WaitDelay`
reap a wedged child, `cmd.Wait()` before any filesystem assertion. Kill rows
assert the child was **signalled**, and the only timers (kill backstop,
`WaitDelay`) fail the row loudly when they fire.

**Counting proxy:** shape of `newCorruptingProxy` (`e2e_test.go:625-660`);
records `(method, class, digest, status)`; counting readers, never
`io.ReadAll`; completion recorded after `ServeHTTP` returns; on trigger it
kills and **blocks all later blob requests on a closed channel** so nothing
completes while the signal lands. Modes: pass-through / strip-Range /
cut-mid-part-once (close downstream after K bytes of part J, once).

**Causal triggers (never a timer):** pull-exact kills on the (N+1)th distinct
blob GET with `Workers: 1` — `fetchWorker` takes the next job only after
`fetch` returns, so the request for part N+1 proves parts 0..N−1 are verified
on disk; push-exact mirrors with the (N+1)th `POST …/blobs/uploads/`
(`drain`). Messy rows (`Workers: 4`) kill on ≥ half the bytes downstream and
compute **`intact` from the on-disk partial after `cmd.Wait()`** (split +
hash, the [seams] graft): assertions are exact against `parts \ intact`, never
against wire timing. Guards: `1 ≤ len(intact) < parts`, `<dest>` absent,
partial exactly `FileSize` bytes.

**Rows:** pull exact-kill (SIGKILL, W1: rerun GET set == `parts[N:]`,
`manifest-read == 1`, sha256 equal, partial gone); pull messy-kill (SIGKILL,
W4: rerun GET set == `parts \ intact` exactly, per-digest ≤ 4); pull graceful
(SIGINT, W4, child installs a two-line handler); pull corrupted-partial
(seeded: good pull, rename dest → partial, flip one byte in part k → exactly
one blob read, part k); pull complete-partial (rename back → zero blob reads,
one manifest read); pull wrong-size partial (plant wrong length → every part
fetched); pull mid-part continuation (cut-mid-part-once, pass-through: log
what zot answered; only when 206, assert the continued GET moved strictly
fewer bytes — **never assert a registry into an optional capability**); pull
range-stripped (cut-mid-part-once + strip-Range: proxy saw ≥1 `Range`,
answered 200, pull succeeded, sha256 equal); push exact-kill (SIGKILL, W1:
rerun HEADs every part, uploads exactly `parts \ landed` where `landed` is
HEADed directly against zot after the kill; pull-back byte-identical); push
messy-kill (SIGKILL, W4: uploads a strict non-empty subset). The
refuse-Range(416) behavior is pinned at integration level, not e2e (it is
terminal by design).

**Traps:** per-row stamped content (`newRowFile` shape) against zot's global
dedupe; kill rows use 1 MiB parts of an 8 MiB fixture so a part is comfortably
larger than a socket buffer. `TestE2ECorruptedPartsFailThePull`
(`e2e_test.go:251-283`) silently becomes a resume test — add a counted
assertion that its second pull re-fetches only the corrupted parts. Check
`e2e_flaky_test.go` expectations still hold once continuation lands (toxiproxy
limit_data now exercises ranged continuation naturally).

### Manual gates (CLI instrument; all greppable with the frozen grammar)

1. Ctrl-C a large push at ~50%, re-run with `-debug`: `blob-check` hits for
   landed parts, uploads equal the remainder; pull back, `shasum` matches.
2. Ctrl-C a large pull at ~50%: exit 130, partial at full size, `<dest>`
   absent; re-run: `blob-read=<remainder>`; `<dest>` appears only at the end;
   `sha256sum` matches.
3. Flip a byte inside the partial: exactly one digest under `class=blob-read`;
   result verifies.
4. Free bonus: rename `<dest>` back to the partial name, re-run →
   `blob-read=0 manifest-read=1` (reconstructs the killed-between-verify-and-
   Commit state, which cannot otherwise be scheduled).

## PR breakdown (three; the plan's two split along its own seam)

1. **`feat(oci): report the offset a blob read starts at`** — port signature +
   godoc, adapter change, mock regen, `TestBlobsGet` rows, mechanical call-site
   arity updates. The single transfer call site passes 0 and **terminal-guards
   `start != 0`**, so inertness is provable: no library behavior changes, and
   the existing suites staying green is the proof. (Session-004 precedent:
   inert classification PR #21 before the behavior flip #23.)
2. **`feat(transfer): pull resume from partial files`** — `resumable`,
   Size-before-Truncate, `partFetcher.resume`, `verify`, the `done` counter
   through `fetch`/`attempt`/`stream`, cap + guard + re-plan; all integration
   tests and fixture churn; every docs change below.
3. **`test(e2e): kill-and-resume coverage`** — TestMain child, counting proxy,
   the rows above, strengthened `TestE2ECorruptedPartsFailThePull`. No
   production code.

## Docs impact (PR 2 unless noted)

- `docs/docs/explanation/design.md`: pull-path step 3 (continuation +
  200-fallback story), retry bullet "re-done whole … arrives with resume",
  `Blobs.Get` sketch (three values), unit-of-work clause (continued part
  shares its budget), step-2 clause order (Size before Truncate), plus a short
  "what a resume proves" note (SIGKILL windows / page cache / machine crash).
- `docs/docs/index.md:13`: "Resume and authentication are next" → resume
  landed.
- `internal/transfer/doc.go:17-18`: "Resume and progress accounting are not
  here yet" — rewrite (the [invariants] catch the others missed).
- `docs/docs/how-to/push-and-pull.md`: "Resume an interrupted pull" section +
  one sentence on the accepted costs (a failed pull leaves a full-size
  partial; every rerun re-hashes the whole partial before fetching — at worker
  parallelism, overlapping the fetches).
- `client.go` Pull godoc ("this phase always fetches every part" is false
  now); `internal/transfer/pull.go` top godoc likewise;
  `internal/transfer/ports.go` `Blobs.Get` (PR 1) and `Sink.Size` ordering
  clause; `internal/oci/blobs.go` godoc (PR 1).
- `cli/README.md`: forward pointer → real recipe "Resuming an interrupted
  pull" (partial exists / dest absent; `blob-read=<missing>`; digest grep;
  `blob-read=0` twin of the warm-push gate); drop "no resume" from limits.
  **No `-debug` grammar change.**
- `format.md`: untouched, deliberately.

## Accepted costs & standing decisions

- A pull that fails at part 0 leaves a full-size sparse partial (true on
  master already); every rerun pays a full hash pass that finds nothing
  before the first byte moves. Accepted: any mitigation is recorded state.
  Documented in the how-to.
- Same-size-different-artifact: full verify pass, then full fetch. Accepted.
- A Range-hostile registry on a flaky link can exhaust a part's budget having
  moved more bytes than a clean run. Accepted; noted in design.md.
- Rejected forever (do not re-litigate): any sidecar/progress journal
  (`ports.go:178-181` designs it away); persisting marshalled hasher state
  beside the partial (`hash.Hash` marshalled state "may contain portions of
  the input in its original form" — a content leak into a world-readable
  sibling); public resume/verify knobs; a "registry ignores Range" memo
  (saves zero bytes and zero requests).
- Model policy for this session: all workflow/implementer agents run on
  opus/sonnet explicitly; nothing inherits Fable.
