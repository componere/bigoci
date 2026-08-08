# Phase 3 — Per-part retry: synthesized design (lead: Fable, from a 3-lens Opus panel)

This is the governing design. Where it conflicts with the per-lens proposals
(design-concurrency.md, design-verification.md, design-boundaries.md), THIS
file wins. The lens documents remain valuable for rationale, test tables, and
edge-case discussion — the implementer must read the relevant sections, but
build THIS.

Provenance of the synthesis: the seam mechanism, package layout, spec wiring,
and port-godoc contract come from design-boundaries.md (§1–§9 there); the
concurrency proofs and worker-interaction analysis from design-concurrency.md
(§5 there); the test tables and manual-gate evidence lists from
design-verification.md (§6–§7 there). Deviations from each are listed at the
bottom and are deliberate.

## 0. Shape

ONE new package, `internal/retry` — a leaf importing only
context/errors/fmt/math/rand/v2/time. It holds both halves of the phase:

- The classification vocabulary: `Transient(err error, after time.Duration)
  error` (a transparent tag: Error() renders the inner message unchanged,
  Unwrap keeps every errors.Is/As working; nil in → nil out) and
  `IsTransient(err error) (time.Duration, bool)` (errors.As walk; untagged =
  terminal — the load-bearing default: a failure nobody classified is one
  nobody has thought about). The tag type itself is unexported.
- The policy: `Policy{Attempts int, Base, Cap time.Duration, Sleep Sleep,
  Rand Rand}`, named func types `Sleep func(ctx context.Context,
  d time.Duration) error` and `Rand func(n int64) int64` (exactly
  math/rand/v2's Int64N signature so the default is a method value),
  `Default()`, consts `DefaultAttempts = 4`, `DefaultBase = time.Second`,
  `DefaultCap = 30 * time.Second`, and `Do(ctx, Policy, op) error`.
  Unexported: `normalized`, `backoff`, `sleep` (real interruptible timer).

Import graph (oci → retry is the adapter speaking the core's vocabulary;
oci still never imports transfer; root never imports retry):

    internal/oci ──────► internal/retry ◄────── internal/transfer
    internal/transfer ──► internal/plan, internal/manifest
    bigoci (root) ──────► internal/transfer, internal/oci, internal/file, ...

- transfer's transitive closure gains no net/http (Retry-After parsing stays
  in oci because http.ParseTime lives in net/http).
- retry touches no digests → no sha256 blank import.
- No new mocks, no .mockery.yml change: retry exports no interface anyone
  implements; Sleep/Rand are plain func fields (T2/T3 govern ports/adapters,
  not injected data). Port SIGNATURES don't change → no mock regeneration.

WHY this seam (decision record): duck-typed interfaces lose because nothing
compile-checks the agreement AND transport failures are plain *url.Error
values bigoci does not own — they cannot grow methods, so a wrapper is needed
anyway, and "once you need the wrapper, the tag design is the interface
design with a name". A Kind enum in a separate fault package loses because
the per-status knowledge necessarily lives in the adapter either way (its
kindOf IS the table), the core-side Kind→verdict mapping is ceremony, and it
costs a second package plus Fault methods scattered over three error types.
Transfer-owned vocabulary loses because the adapter would import the
orchestrator (kills oci/port_test.go's "nothing in oci imports transfer"
premise). The tag keeps every classification site greppable:
`grep -rn 'retry\.Transient('` must list exactly the sites below.

## 1. internal/retry — exact semantics

Follow design-boundaries.md §4 code shapes, with these AMENDMENTS:

1. **Context-first guard in Do — READS ctx, NEVER the error's shape**
   (amended post-review; the review panel proved and I reproduced on Go
   1.26.4 that net and net/http deliberately render dial/header/client
   timeouts as errors matching context.DeadlineExceeded, so an errors.Is
   guard eats the retry budget for the most common transient failures):
   after `err = op(ctx)` fails, BEFORE consulting IsTransient:
   `if ctx.Err() != nil { return err }`. Only the loop's own context says
   whether the transfer is over. The loop-top check wraps a failure already
   in hand: `interrupted(ctxErr, attempt-1, err)` → single line
   "%w after %d attempts: %w". (The adapter ALSO avoids tagging requests
   whose own context ended — both guards stay.) CONSEQUENCE FOR PR2: a
   caller-supplied http.Client{Timeout: d} surfaces with the caller's ctx
   alive and req.Context().Err() == nil, so client timeouts are TAGGED and
   RETRIED with a fresh window per attempt — the WithHTTPClient godoc
   sentence in PR2 documents that a transport that also retries multiplies
   the schedule, and that a client Timeout bounds each attempt, not the
   transfer.
2. **Retry-After is a FLOOR, clamped by Cap, in the core**:
   `wait := p.backoff(attempt); if after > 0 { wait = max(wait, min(after, p.Cap)) }`.
   NOT boundaries' "as sent, never trimmed" and NO adapter-side
   maxRetryAfter/usableWait — one constant (Cap) bounds every wait, the
   adapter reports raw facts, a hostile 24h header waits 30s, and a hint
   never SHORTENS the jittered escalation (herd-safe).
3. **Interrupted sleep wraps single-line**:
   `fmt.Errorf("%w after %d attempts: %w", waitErr, attempt, err)` — NOT
   errors.Join, which renders multi-line and would break the CLI's one-line
   failure presentation. Both errors stay reachable via errors.Is.
4. **Single-attempt failures come back untouched**: wrap with
   `fmt.Errorf("after %d attempts: %w", ...)` ONLY when attempts > 1 (a
   terminal failure on attempt 1, or an Attempts:1 policy, reads exactly as
   phase 2 did).
5. **backoff windows double in a guarded loop** (concurrency's shape —
   overflow-proof without shift-range reasoning): start at min(Base, Cap),
   double while below Cap; ceilings for defaults are 1s, 2s, 4s (worst-case
   sleep 7s/unit, expected 3.5s); wait drawn `time.Duration(p.Rand(int64(ceiling)))`
   (full jitter, [0, ceiling)); no sleep after the last attempt; ctx.Err()
   checked at the top of every iteration (deterministic timer-vs-cancel).
   Zero/negative ceiling → 0 wait, still passed through Sleep (one recorded
   entry per backoff in tests).
6. Zero-value Policy = the default policy via normalized() inside Do
   (Attempts <= 0 → 4, etc.). Do returns the LAST error. Attempts never
   reset on a changed failure kind.

## 2. internal/oci — classification at the choke points

Per design-boundaries.md §2–§3:

- `Repository.do`: on client.Do error, build the same fmt.Errorf message
  (byte-identical to today), then: if `req.Context().Err() != nil` return it
  UNtagged; else return `retry.Transient(failure, 0)`. No TransportError
  type — the tag replaces it.
- `statusError(resp)`: StatusError gains field `RetryAfter time.Duration`
  (raw, what the registry asked; zero = asked nothing / unreadable; Error()
  does NOT render it). New `transientStatus(status)`: 429 or 500–599 — and
  nothing else (408 is a 4xx; the design table says other 4xx fail fast).
  Transient statuses return `retry.Transient(statusErr, statusErr.RetryAfter)`;
  everything else returns the bare *StatusError. Detail still read (and
  drained) for transient statuses.
- `retryAfter(resp)` in new file `internal/oci/classify.go`: delta-seconds
  (reject negatives) and HTTP-date via http.ParseTime (past date → 0);
  garbage/absent → 0. NO upper clamp here (core clamps at Cap).
- `blobBody` wrapper (unexported io.ReadCloser) returned by Blobs.Get: Read
  tags every failure except io.EOF with `retry.Transient(err, 0)`; Close
  passes through. This single wrapper structurally covers: mid-stream body
  deaths, the pull's one-extra-byte tail probe (it reads the same wrapped
  reader — no tagReads change needed), and composes with transfer's
  readError untouched (the fault rides inside; readError stays transparent).
- `readManifest`: its io.ReadAll failure becomes
  `retry.Transient(fmt.Errorf("%s %s: read manifest: %w", ...), 0)` —
  message byte-identical (verification's catch: a manifest body dying
  mid-read must be retryable once Pull wraps Manifests.Get).
- `ErrTooLarge = errors.New("too large")`; `StatusError.Is` gains the 413
  clause beside the 404 one (switch form). 413 only — no 400-body sniffing,
  no 507 (that's a 5xx = transient), no vendor tables.
- oci/doc.go paragraph: this adapter classifies (choke points above) and
  never retries.
- Grep-check during implementation: oci tests asserting CONCRETE error
  values on 5xx paths would break (tag now outermost) — boundaries found 4
  candidate sites (blobs_test.go:60,158,285, manifests_test.go:64), all
  believed to use wantErr/errors.As; confirm.

## 3. internal/transfer — wiring (design-boundaries.md §5 + concurrency §4–5)

- `PushSpec.Retry retry.Policy` / `PullSpec.Retry retry.Policy` — zero value
  IS the default policy; spec godocs change to "Every field but Title and
  Retry is required" / "Every field but Retry is required". validate()
  unchanged (a Policy cannot be wrong, only unset). Root client.go struct
  literals UNCHANGED (no Retry line; root never imports retry — the default
  has exactly one home).
- Push: `uploadPart`/`uploadParts` become an `uploader` struct {blobs,
  source, claims, policy} shared by workers (stateless beyond the shared
  claim set). `upload`: claim ONCE outside the loop (a retry is the same
  worker continuing; an exhausted claimed part fails the push; no release
  path — the claim set never needs one), then `retry.Do(ctx, u.policy,
  u.attempt)`. `attempt` = Exists (inside the loop — the idempotency win: a
  PUT that landed but lost its response is discovered by the next attempt's
  HEAD) + SectionReader constructed INSIDE the attempt + Put. Inner error
  messages unchanged; no outer wrap (a failure reads
  "after 4 attempts: upload part 3 (sha256:…): PUT …: registry returned 500 …").
- `ensureEmptyConfig`: same treatment; `bytes.NewReader(content)` INSIDE the
  attempt (a spent reader on attempt 2 uploads zero bytes under a
  Content-Length that promises two — the classic trap).
- `writeManifest`: Manifests.Put wrapped in retry.Do (own budget; identical
  bytes each attempt — idempotent by construction).
- Pull: partFetcher gains policy; today's fetch body renamed `attempt`;
  `fetch` = retry.Do wrapper. hasher.Reset stays at attempt top; OffsetWriter
  constructed inside stream per attempt (fresh position — overwrite-range
  byte-correctness proof is design-concurrency.md §5.4); every attempt calls
  Get(dgst, 0) — whole-part re-fetch, no Range in phase 3, and a fresh Get
  re-resolves redirects so no presigned URL is ever reused.
- Pull's Manifests.Get wrapped in retry.Do. Truncate/Commit/WriteAt/ReadAt
  NEVER retried (local disk is not a transient peer; the hash pass is not
  retried).
- Core's own one legitimate tag: short part (clean early EOF —
  written != part.Size with nil error) becomes
  `retry.Transient(fmt.Errorf("part %d ended after %d bytes, ..."), 0)` —
  the orchestrator diagnosed it from its own byte accounting. The LONG-part
  case stays terminal (extra bytes = content the manifest does not
  describe). Sink-write failures: unchanged path, never tagged → terminal.
  readError/tagReads: UNCHANGED (blobBody's tag flows through).
- Digest mismatch stays TERMINAL (never tagged). Deliberate; design.md prose
  edited to match; flagged in the PR body.
- Cancellation invariants unchanged (channel sized to all parts, closed by
  hash pass, no rendezvous, NO re-queue — a failed part loops in place in
  its owning worker). Worker comments gain "…or as soon as a backoff sleep
  is interrupted". A sleeping worker wakes instantly on a peer's terminal
  failure because Sleep selects on the errgroup ctx (concurrency §5.2 walk).

## 4. Public surface + CLI

- root errors.go: `ErrPartTooLarge = errors.New("part too large")` (godoc per
  design-boundaries.md §6 — includes "a different part size is a different
  artifact"); classify() gains `case errors.Is(err, oci.ErrTooLarge)`;
  classify godoc narrows its deferred-cases paragraph to unauthorized only.
- NO public retry options, NO public retry constants (the design doc's API
  sketch names none; its Defaults table marks only part size and workers as
  options; ctx bounds total time; WithHTTPClient owns the transport; knobs
  are forever — A3/L1; the phase-6 self-tuning question would prejudge).
- client.go: godoc ONLY (Push/Pull retry paragraphs per design-boundaries.md
  §5 diff sketches — drop "Nothing is retried in this phase"; Push's
  branchable list gains ErrPartTooLarge; Pull's does NOT — a pull cannot
  raise it). options.go: WithHTTPClient gains "a transport that also retries
  multiplies the schedule"; WithPartSize gains the ErrPartTooLarge pointer.
- cli/run.go: `exitPartTooLarge = 7` + sentinelExits row
  {bigoci.ErrPartTooLarge, 7, "bigoci.ErrPartTooLarge"}; reserved-codes
  comment narrows to 6 only. NOTHING else changes in the CLI (verified: tap
  gives each round trip a fresh seq; Retry-After already on the redactor's
  response allow-list; summary-line shape untouched).

## 5. Ports godoc contract (D1 — these are contract changes)

Apply design-boundaries.md §8 verbatim: Blobs "may retry" sentence REPLACED
with must-not-retry + the tagging contract ("what an implementation owes is a
verdict... tagged with retry.Transient, carrying whatever wait the far end
asked for; everything else untagged and terminal"); Blobs.Get gains the
tagged-reader sentence; Blobs.Put gains "a spent reader is a bug, not a
retry"; Manifests gains classification + idempotence-of-both-methods;
Source/Sink gain "never classifies; local failures are terminal";
transfer/doc.go retry paragraph; push.go/pull.go orchestrator godocs.

## 6. Tests

Verification lens's tables govern (design-verification.md §6), MECHANICALLY
ADAPTED to this seam:

- `transientStub` → `retry.Transient(errors.New("boom"), 0)`;
  `delayedStub{d}` → `retry.Transient(err, d)`; `terminalStub` → plain
  errors.New. Stub TYPES are unnecessary — the vocabulary is a constructor.
  (Hand-written error VALUES in tests are fine; T2 governs port mocks.)
- Retry-After rows use FLOOR semantics: with Rand→0, wait == min(hint, Cap);
  with Rand→n-1 (ceiling-1), wait == max(ceiling-1, min(hint, Cap)). Drop
  override-only rows ("rng not called") and the (0,true)/(0,false)
  distinction rows (no bool carrier).
- retry unit tests (in internal/retry): backoff ceiling table (attempts 1–6
  and 1000 at defaults; Cap=0; Base>Cap; huge-Base overflow row), the Do
  decision matrix (success first try; transient×2 then success with recorded
  sleeps; terminal attempt 1 → error byte-identical, no wrap; exhausted →
  last error + "after 4 attempts:" + inner sentinel still matches;
  Retry-After floor rows incl. min(24h,Cap); tagged-but-cancelled →
  context-first guard wins, no sleep; ctx dead before first attempt → op
  never called; sleep interrupted → single-line wrap, both errors match;
  Attempts:1 → unwrapped; zero Policy == Default()-normalized field by
  field; Default() pins the design-doc numbers with a comment saying the
  doc must change with it), tag tests (nil→nil; transparency of
  Error()/Is/As through 3 fmt.Errorf layers; IsTransient on
  untagged/tagged/nested; outermost tag wins), default Rand bounds + 4
  goroutines under -race, default sleep (the only real-clock rows,
  milliseconds).
- transfer integration (existing mockery mocks + recording Sleep + scripted
  Rand): design-verification.md §6.B push rows 1–16 and pull rows 1–14, plus
  design-concurrency.md table rows 13 (peer terminal failure wakes a
  sleeping worker — Push returning at all is the proof) and 15 (twin parts:
  owner exhausts its budget → push fails, Put called exactly 4× for that
  digest, never by the other worker). Fixture helpers per verification
  §6.B (testPolicy/countingSource/scriptedBlobs/flakyBody with garbage
  prefix). EXISTING transfer tests need zero edits (untagged mock errors are
  terminal on attempt 1 with the identical error) — add the fixture comment
  saying so.
- oci tests: verification §6.C rows adapted — transientStatus table (429 ✓,
  500/502/503/504/599 ✓; 400/401/403/404/405/408/409/413/422/3xx ✗);
  Retry-After both forms + garbage + past-date (NO clamp rows here);
  golden-string StatusError.Error() for a tagged 503 (message unchanged
  through the tag); Is: 404↔ErrNotFound, 413↔ErrTooLarge, cross-negatives;
  refused connection → IsTransient true, message unchanged; reset mid-PUT →
  tagged; broken blob body mid-read → tagged (hijackAfter fixture); broken
  manifest body → tagged; CANCELLED request → NOT tagged and
  errors.Is(err, context.Canceled); Get statelessness (two calls, two
  requests); adapter-does-not-retry (500-then-201 fake sees exactly one
  PUT).
- root: TestPushAgainstADeadPortFailsWithinTheRetryBudget (bound-then-closed
  listener; 20s ctx ceiling; NotErrorIs DeadlineExceeded — the automated
  "no hang, no infinite retry"; ErrorContains "after 4 attempts");
  errors_internal_test.go row for oci.ErrTooLarge → ErrPartTooLarge.
- CLI: fake-registry 413 hook → exit 7 + exact line
  "bigoci: matched sentinel bigoci.ErrPartTooLarge (exit 7)"; ONE real-clock
  single-part 500-then-success push → exit 0 (the only default-clock retry
  test in the tree; expected ~0.5s).

## 7. e2e (PR3): zot behind toxiproxy

As my earlier draft (unchanged by the boundaries lens):

- New `e2e_flaky_test.go`. Deps: github.com/Shopify/toxiproxy/v2 v2.12.0
  (client subpackage, stdlib-only — verified, 2 go.sum lines) +
  testcontainers network subpackage (already in module). Image
  ghcr.io/shopify/toxiproxy:2.12.0 pinned beside zotImage. NO testcontainers
  toxiproxy module (L1: ten lines for another dependency).
- Fixture: one docker network; newZot gains variadic
  `opts ...testcontainers.ContainerCustomizer` (existing call sites
  unchanged); toxiproxy via plain testcontainers.Run; CreateProxy
  "0.0.0.0:8666" → "zot:5000"; `through` (proxied) + `direct` endpoints.
- Toxic scheduling CAUSAL, never wall-clock: counting gate transport (wraps
  cloned DefaultTransport; counts round-trip FAILURES for push, blob-GET
  REQUESTS for pull — a body death is invisible to RoundTrip) removes the
  toxic via sync.Once once the transfer is provably hurt; 3s backstop timer.
- Rows: (1) push through limit_data upstream (bytes ≈ part/3); (2) push
  through reset_peer (RST path); (3) pull through limit_data downstream
  (gate on requests > parts); (4) proxy.Disable → push → Enable ~1.2s later
  (automated shadow of the zot-restart gate).
- Assertions: no error; gate saw failures (or requests > parts); bounded
  request counts (≤ budget × units); toxic was active before cleared; file
  pulled through DIRECT endpoint byte-identical; no partial left. 1 MiB at
  256 KiB parts; suite < 60s; no new moon task, no build tag.

## 8. Docs (D6) — the full list

Union of the three lenses:

- design.md: pull-path step 3 reconciled (a mismatched part FAILS the pull —
  re-fetching gets the same bytes; Range/mid-part resume marked phase 4);
  "Retry policy" subsection under Defaults (unit of work, floor semantics
  with the 30s cap bounding every wait, the terminal set, whole-part
  re-fetch, unclassified-is-terminal); package-layout block gains
  `internal/retry`.
- index.md: "retries transient failures" sentence per boundaries §6.
- how-to/push-and-pull.md: "What bigoci retries" section (plain language;
  ctx stops waits immediately; a stacked retrying RoundTripper multiplies
  attempts); errors list gains ErrPartTooLarge + smaller-part-size fix
  ("Three failures" → "Four failures").
- cli/README.md: exit-7 row; reserved paragraph → code 6 only; dead-port
  recipe REWRITE (per-digest http! count becomes 4, total up to 16, "after
  4 attempts:", wall clock < 10s — this recipe is a manual gate; stale =
  gate fails for the wrong reason); "watching a retry happen" recipe
  promoted from forward-pointers (uniq -d greps from verification §6.G);
  -timeout sentence (bounds retries and backoff too); Limits bullet drops
  "no retries".
- godocs: client.go Push/Pull, options.go WithHTTPClient/WithPartSize,
  errors.go classify, transfer doc.go/push.go/pull.go/ports.go (§5 above),
  oci doc.go.

## 9. PR plan (each green, each shippable)

1. `feat(oci): classify registry failures for retry`
   internal/retry package COMPLETE with unit tests; oci classify.go +
   StatusError.RetryAfter + Is row + ErrTooLarge + tagging at do/
   statusError/blobBody/readManifest + oci tests + doc.go. INERT: nothing
   calls retry.Do; tags are invisible to every existing errors.Is and every
   rendered message. Zero behavior change (grep the 4 concrete-value test
   sites).
2. `feat(transfer): per-part retry with backoff`
   THE behavior flip, one bisectable commit: spec Retry fields; uploader
   refactor + six retry.Do sites + core short-body tag; root ErrPartTooLarge
   + classify row (the sentinel becomes REACHABLE here, so its docs land
   here — D6); CLI exit 7 + tests; ports.go contract rewrite; the full
   failure-injection suite; dead-port root test; CLI real-clock test; ALL
   docs from §8.
3. `test(e2e): transfers ride through a broken network`
   e2e_flaky_test.go; newZot variadic refactor; gate transport; go.mod/sum
   (the only PR that adds a dependency — reviewable on its own, and a flaky
   fixture never blocks the library change).

## 10. Manual gates (after PR3; evidence → journal)

Exactly design-verification.md §7: gate 1 (toxiproxy ride-through) journals
the seven items verbatim (exit 0; digest; http! count > 0; uniq -d duplicate
digest; the two http> lines showing the backoff gap; summary line with
failed= > 0; byte-identical shasum through the clean port); gate 2 (zot
restart) journals five incl. the largest inter-stamp gap < 7s; gate 3 (dead
port) journals five incl. real elapsed < 10s, and NO "context deadline
exceeded" (run without -timeout so the library bounds itself). AMENDED after
PR2 review measurement: only the FIRST-exhausted digest shows exactly four
http! lines; peers are cancelled mid-backoff and show one to four; totals
run eight to thirteen. Journal the first-exhausted digest's count of 4, not
a per-digest expectation. Concurrency lens's notes apply: toxicity 0.3 for the human-watched
gate; a FIREWALLED (SYN-dropping) port is bounded by the OS connect timeout,
not the retry budget — document, don't fix.

## Deliberate decisions to surface at review (do not silently change)

- Digest mismatch terminal; design.md prose edited to match (was "a corrupt
  part re-fetches alone").
- Retry-After as floor bounded by the one 30s Cap, clamped in the CORE (the
  adapter reports raw). No second constant.
- Verdict-shaped tag (retry.Transient) instead of a fact enum — with the
  context-first guard kept in the loop as defense in depth.
- 413-only mapping for ErrPartTooLarge (a 413 on a manifest PUT would match
  too; unreachable in practice — godoc admits it).
- Transport errors unconditionally transient (a wrong port/hostname costs
  ≤7s of sleeps before the real error surfaces — bounded, accepted; a
  SYN-dropping firewall is bounded by the OS, not us).
- Manifest Get/Put and the empty-config blob retry under their own budgets —
  a small, called-out scope expansion beyond the plan's "per part" sentence.
- Zero-value Policy = default (root does not restate it and does not import
  internal/retry).

## Deviations from each lens (so the implementer doesn't "correct" them back)

- vs boundaries: Retry-After floor+Cap-clamp in core (they had
  override-as-sent + 2min adapter drop); context-first guard added to Do;
  errors.Join replaced with single-line %w wrap; single-attempt failures
  unwrapped; PR count 3 not 2 (sentinel+CLI move to PR2 with the docs; e2e
  separate); named Sleep/Rand func types.
- vs concurrency: no internal/fault package, no Kind enum, no TransportError
  type, no Fault methods; tail probe fixed by blobBody (no tagReads
  re-routing); Retry field zero-value-defaulted, not required+validated;
  their PR1 content redistributed.
- vs verification: no Transient/Delayed interfaces, no terminalError/
  transientError tag types, no judge/verdict, no RetryAfterLimit; retry
  lives in its own package; Retry-After floor not override; their test
  tables carry over with the mechanical adaptations in §6.
