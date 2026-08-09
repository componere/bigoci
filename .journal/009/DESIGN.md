# Phase 7 PR 1 — progress reporting: the governing design

Synthesized 2026-08-09 from design panel wf_9efec9b7-6ba: three opus designers
(accounting / ergonomics / consumer-cli lenses), two opus judges (failure-bias,
api-bias). Both judges ranked [accounting] first; this document is the
[accounting] base plus the judges' agreed grafts plus two lead rulings
(SkippedParts adopted; CLI rate/eta dropped). Full panel output preserved in
the session scratchpad (design-*.md, judge-*-synthesis.md, judge-*-breaks.json).

## Summary

One new option, `WithProgress(fn ProgressFunc) TransferOption`, hands the
callback absolute snapshots of the whole transfer. The load-bearing decision:
**two byte counters**. `CompletedBytes` is how much of the file is provably in
its final place (registry holds it / written and verified), credited in
whole-part steps exactly once per part. `WireBytes` is how many bytes crossed
the registry boundary to get there. They are equal when nothing goes wrong and
diverge honestly the moment anything does. `HashedBytes` covers local read
passes (push hash pass, pull resume verify). `SkippedParts` counts parts
satisfied without moving their own bytes. `Retries` counts re-entries into
retry budgets. `Phase` — not the counters — says whether the transfer is over.
Every numeric field is non-decreasing; nothing ever regresses; nobody watching
costs nothing.

## Public API (new file progress.go, package bigoci)

```go
type Direction uint8
const (
    DirectionPush Direction = iota + 1   // iota+1: zero Progress is never valid
    DirectionPull
)
func (d Direction) String() string       // "push" | "pull" | "unknown"

type Phase uint8
const (
    PhaseResolving Phase = iota + 1      // pull only: fetching/decoding the manifest
    PhaseTransferring                    // parts moving (push: hash pass + uploads; pull: verify + fetches)
    PhaseFinalizing                      // push: empty config blob + manifest write; pull: Commit
    PhaseDone                            // success; exactly one snapshot, the last
    PhaseFailed                          // failure; exactly one snapshot, the last; the WHY is Push/Pull's error
)
func (p Phase) String() string           // lowercase word | "unknown"

type ProgressFunc func(Progress)

type Progress struct {
    Direction      Direction
    Phase          Phase
    TotalBytes     int64   // push: known on every snapshot; pull: 0 during PhaseResolving, set once at decode
    TotalParts     int     // parts, not blobs — dedupe never shrinks it
    CompletedBytes int64   // provably in final place; whole-part steps; monotone
    CompletedParts int
    SkippedParts   int     // parts that completed without moving their own bytes over the wire
    WireBytes      int64   // bytes that crossed the registry boundary; monotone; unbounded vs TotalBytes
    HashedBytes    int64   // local bytes read+hashed: push hash pass, pull resume verify; <= TotalBytes
    Retries        int     // entries into a retry budget after the first, all five op kinds
}
func (p Progress) Fraction() float64     // CompletedBytes/TotalBytes; 0 when TotalBytes <= 0

func WithProgress(fn ProgressFunc) TransferOption
```

Godoc must carry (D2 prose is the implementer's, these sentences' content is
frozen): percent/bar/remaining belongs on CompletedBytes; throughput belongs on
WireBytes; only PhaseDone means finished; every numeric field non-decreasing
and Phase only advances; snapshots are totally ordered and never overlap; a
slow callback stalls the transfer by design (forbidden shapes: blocking channel
sends, network calls, calling back into the Client); reports raised after the
transfer ended are dropped; the callback may run on worker goroutines, the
push hash goroutine, and net/http's request-body write goroutine.

TransferOption's godoc ("which today is [WithWorkers] alone") is updated to
name both.

## The contract

- Either fn is never called at all, or its first call carries PhaseResolving
  (pull) / PhaseTransferring (push) and its last carries PhaseDone or
  PhaseFailed. Nothing is delivered after Push/Pull returns.
- Zero-call cases are real and documented: failures before the orchestrator
  runs (settings validation, malformed reference, unopenable source,
  uncreatable sink).
- Pull's first snapshot fires after spec validation, before fetchManifest
  (so a retrying manifest fetch is visible: PhaseResolving + climbing
  Retries). Totals arrive exactly once, at the PhaseTransferring transition
  after manifest.Decode, before Sink.Size/Truncate.
- Push's first snapshot fires right after plan.New, with real totals.
- Empty artifact: TotalBytes==0, TotalParts==1, Fraction() 0 on every
  snapshot, CompletedParts reaches 1, PhaseDone. Fraction's godoc names it.
- Skip rule (one sentence, frozen): a part is skipped when this transfer moved
  none of that part's own bytes over the wire, decided across the part's whole
  retry budget, not per attempt.

## Semantics table (all nine edge cases; frozen)

| case | CompletedBytes | Parts | Skipped | WireBytes | HashedBytes | Retries |
|---|---|---|---|---|---|---|
| a. pull continuation, breaks at k of P | +P once at verify | +1 | 0 | +k, then +(P−k) = P | — | +1 |
| b. whole-blob fallback after a break | +P once | +1 | 0 | +k, then +P = P+k | — | +1 for the break, +0 for the fallback |
| c. done==part.Size restart | +P once | +1 | 0 | 2P | — | +1 |
| d. resume verify, match | +P at match | +1 | +1 | 0 | +P | — |
| d′. resume verify, mismatch | +P at fetch-verify | +1 | 0 | +P | +P | — |
| e. push Exists-skip | +P | +1 | +1 | 0 | +P (hash pass) | — |
| f. dedupe follower | +P at settle | +1 at settle | +1 | 0 | +P | — |
| g. push retry, fails at k | +P once | +1 | 0 | P+k | +P | +1 |
| g′. landed-but-lost (attempt 2 Exists=true) | +P | +1 | 0 | +P | +P | +1 |
| h. push hash pass | — | — | — | — | +file | — |
| i. config blob / manifest write / manifest fetch | — | — | — | 0 | — | counted |

Case (i) is visible as Phase (Finalizing/Resolving) + Retries — which is what
distinguishes "stuck writing the manifest" from "hung at 100%". Case (f):
credit is deferred to settle; the follower's worker still skips instantly.
Case (g′) is why the skip rule reads "across the whole budget": attempt 1
moved the part's bytes, so it is not skipped (implementation: `uploaded *bool`
set immediately before Blobs.Put, never reset between attempts).

Invariants a consumer may rely on: every numeric field non-decreasing; Phase
never regresses; totals change at most once, from zero; CompletedBytes <=
TotalBytes and CompletedParts <= TotalParts once known; SkippedParts <=
CompletedParts; HashedBytes <= TotalBytes; WireBytes unbounded relative to
TotalBytes; exactly one terminal snapshot and it is last; CompletedBytes ==
TotalBytes does NOT imply finished — only PhaseDone does.

## Internal wiring

- internal/transfer gets its own snapshot type + phase enum (no Direction —
  the core stays direction-agnostic). client.go adapts core snapshots to
  bigoci.Progress in the per-call closure, stamping Direction there. The
  mapping is an explicit switch, never a numeric cast between independently
  declared enums, pinned by an exhaustive table test.
- One reporter, one mutex, held across the callback: calls never overlap and
  are totally ordered; every snapshot leaves one critical section (atomics
  rejected — they permit torn snapshots).
- THE CLOSED LATCH (the guarantee-maker): reporter.finish(err) delivers the
  terminal snapshot and sets closed under mu; every recording method returns
  immediately when r == nil || r.closed. Root cause: internal/oci/blobs.go
  hands tagSourceReads to http.Request.Body, so a straggling Read on
  net/http's writeLoop can fire after Push returned. Belt-and-braces:
  complete() is idempotent per part index (credited []bool, sized TotalParts,
  allocated only when watching).
- STRUCTURAL FINISH (no named returns, no defer): split Push/Pull into a thin
  outer that keeps validation + plan.New (push) / the PhaseResolving emit
  (pull), builds the reporter, calls the inner function that is today's body,
  then calls report.finish(err) on the single return path.
- Pull byte counting AT THE WRITER: append a counting writer LAST to stream()'s
  io.MultiWriter(hasher, offsetWriter). MultiWriter stops at the first failing
  writer and CopyBuffer counts a chunk only when every writer took it, so the
  counter sees exactly the bytes that advanced *done — including the
  refused-write case stream_internal_test.go pins. Never count at tagReads.
- Push wire bytes count at tagSourceReads.Read (bytes handed to the
  transport); stragglers are absorbed by the monotone unbounded counter and
  cut off by the latch.
- Claim ledger replaces claimSet's bool: value-typed claim{done bool; waiting
  int}; take(dgst) -> (upload bool, complete int): (true,0) fresh, (false,1)
  already settled, else (false,0) with waiting++. settle(dgst) returns
  1+waiting and zeroes waiting. Claimer credits itself plus waiters at settle;
  a claimer that fails credits nobody; followers still skip instantly.
- Retries: attempted(ctx, policy, op) wraps retry.Do counting entries after
  the first — no change to internal/retry. Applied to part upload, part
  fetch, ensureEmptyConfig, writeManifest, fetchManifest.
- Coalescing: byte-threshold progressStep = 4 MiB (clock-free per A1);
  milestones (phase change, part completion, retry, terminal) always deliver
  and reset the accumulator.
- Zero cost when absent: nil *reporter -> nil-receiver early returns; counting
  wrappers and the third writer not installed at all when nil; attempted calls
  retry.Do directly when nil. Gate with a benchmark pair matching the
  pre-change baseline on allocs/op.

## CLI adoption (reference CLI, cli/)

- Flag: `-progress <duration>` on both commands (commonFlags). Unset OR
  explicit 0 = off (matches -timeout's documented rule); negative = usageError,
  exit 2. Unset changes not one byte of output and installs no option.
- Architecture: the library callback ONLY stores the snapshot under a mutex
  and returns (the CLI is the worked example of its own godoc). A ticker
  goroutine renders; ticker channel and clock are injected (production:
  time.NewTicker(d).C + time.Now; tests drive both). The renderer skips
  printing until the first snapshot arrives. stop() is sync.Once, runs the
  instant the transfer returns, waits for the goroutine, prints exactly one
  final line from the terminal snapshot — before probe.writeSummary() and
  before the frozen result line.
- Line grammar (one shape, every field always present, stderr only):
  `bigoci: progress push transferring pct=41 parts=17/40 skipped=0 bytes=8589934592/21474836480 wire=9126805504 hashed=21474836480 retries=1 elapsed=29.3s`
  Exact byte counts (compare exactly against the preflight line); pct is a
  floored integer (100 only when truly all placed); elapsed at tenths. NO
  rate, NO eta (lead ruling): render(Progress, elapsed) stays a pure function
  asserted byte-for-byte; wire= across successive lines carries throughput.
  Through a 30s backoff the line keeps printing with unchanged counters and
  climbing elapsed — that is the observable induced-retry behavior.
- Frozen contracts hold by construction: stdout untouched; `bigoci: progress `
  shares no prefix with any frozen line or the http> grammar; the preflight
  line gains nothing; exit codes untouched. Concurrent stderr: a mutex-guarded
  whole-line lineWriter wraps e.stderr, shared by the tap and the renderer,
  constructed only when progress is on (in-process tests write to a
  bytes.Buffer, which is not concurrency-safe on its own).

## Tests (beyond the per-case table tests in internal/transfer)

- Shared invariant recorder: every progress test also checks every snapshot
  against its predecessor (monotonicity, phase ordering, totals-once,
  terminal-last).
- Dedupe gate: byte-identical parts, the single Put gated on a channel; while
  blocked, assert no snapshot credits the twin; release, assert both credit.
- TestNoSnapshotArrivesAfterPushReturns: Blobs.Put mock returns a terminal
  error while a spawned goroutine keeps Reading the handed reader for another
  100ms; assert zero callbacks after Push returns, under -race.
- TestEmptyArtifactReportsDoneWithZeroTotals: push and pull a zero-byte file;
  TotalBytes==0, TotalParts==1, Fraction()==0 on every snapshot,
  CompletedParts==1 and PhaseDone terminal.
- Root package: malformed reference and missing file each produce zero calls.
- Exhaustive switch table test for the core->public enum mapping.
- CLI: pure render() asserted byte-for-byte for every shape; in-process runs
  through env{} with a driven ticker (zero ticks -> exactly one final
  progress line).
- Benchmark pair proving the nil path matches baseline allocs/op.

## Manual gate (phase 7 criterion 2)

Four scenarios against zot behind toxiproxy: cold push, induced retry
(limit_data recipe, divided by worker count), SIGINT-then-resume, warm
re-push. Verified with a mechanical awk script over the captured -progress
log: exits non-zero on any counter that falls or any bytes overrunning its
total. Honest note: the whole-blob fallback is not inducible against zot; it
is covered by the existing ignoreRange fixture in unit tests, not faked in
the gate.

## Rejected alternatives (from the panel, with the judges' reasons)

- Event-enum API (ProgressEvent with 8 constants): exports pull.go's attempt
  strategy as a frozen v1 contract; Part field freezes manifest layer position
  multi-file artifacts cannot keep.
- Single byte counter: cannot be simultaneously monotone and honest under the
  whole-blob fallback; splitting Completed/Wire is what removes every
  special case.
- Level-style BytesDone fed from the transport goroutine: optimistic by
  construction (counts unacknowledged bytes), violates its own bound under
  the straggle race.
- Crediting dedupe followers at claim time: reads 97% complete for the whole
  duration of a highly-deduped file whose one real upload is still running.
- Callback-driven CLI printing: silent through the exact backoff window the
  manual gate watches.
- -progress 0 = firehose: inverts the CLI's own -timeout zero-means-unset
  convention.
- Skipped dropped entirely (judge 0): loses the half-skipped push; adopted
  instead with the single-sentence skip rule (lead ruling).
- CLI rate/eta (judge 0 wanted instantaneous rate): every rate any design
  rendered was provably misleading somewhere; purity wins for reference
  tooling (lead ruling).
