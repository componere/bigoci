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

## 2026-08-08 10:21 — Design panel results; boundaries lens re-running
Panel finished (601k tokens). Concurrency and verification lenses returned
complete, high-quality designs (scratchpad design-*.md). The boundaries agent
burned its budget on research and submitted literal placeholders
("DEFERRED_TO_TOOL_INPUT") — re-running it as wf_938926b3-846 with an explicit
anti-deferral instruction and a minLength on the schema.

Where the two finished lenses AGREE (locked in for synthesis):
- Push retry unit = Exists+Put as one attempt sharing one per-part budget
  (idempotency win: a PUT that landed but lost its response is found by the
  next attempt's HEAD). Claim stays outside the loop. Section reader and the
  empty-config bytes.Reader are constructed INSIDE the attempt.
- Pull retry unit = Get+stream+verify whole-part; hasher.Reset + fresh
  OffsetWriter per attempt; every retry calls Get(dgst, 0) — no Range in
  phase 3. Manifest Get/Put and empty config get their own budgets.
- Terminal: unclassified errors (default-stop), digest mismatch, long body,
  sink write/truncate/commit, source reads, ctx cancellation (checked FIRST —
  a cancelled request looks like a transport error underneath).
- Transient: transport failures (unconditionally — no per-errno table),
  429/503 overloaded (carry Retry-After), 408/5xx faulted, mid-stream read
  errors (short body). Two adapter choke points: Repository.do + statusError.
- Two real bug-catches to fold in: (a) the one-extra-byte tail probe in
  pull.stream must read through tagReads or a connection dying right after
  the last byte is a terminal false alarm; (b) manifest body reads (io.ReadAll
  in Manifests.Get) must classify as transport failures too.
- 413 only → oci-level match via StatusError.Is → public ErrPartTooLarge →
  CLI exit 7 activated; codes table comment narrows to 6. No vendor tables.
- No public retry knobs (design doc API sketch names none; ctx +
  WithHTTPClient are the seams). Internal policy fully injectable via specs.
- Injected Sleep func(ctx,d) error selecting on ctx.Done (a terminal peer
  failure wakes sleeping workers instantly); injected jitter fn; rand/v2.
- Windows double in an overflow-proof guarded loop: 1s/2s/4s defaults, no
  sleep after last attempt, wrap only on exhaustion ("after N attempts:"),
  attempts never reset on kind change.
- e2e: new test file, zot+toxiproxy on one docker network, Shopify
  toxiproxy/v2/client (stdlib-only), toxic removed on first OBSERVED failure
  via a counting gate transport (never a wall-clock schedule), limit_data +
  reset_peer; no new moon task, no build tag. Dead-port automated shadow test
  at root (20s ceiling, NotErrorIs DeadlineExceeded).
- Digest mismatch stays terminal — contradicts one design.md prose line ("a
  corrupt part re-fetches alone") which gets reconciled in the doc edit. Real
  behavior decision to surface at review.

Where they DISAGREE (boundaries lens breaks the tie, then I decide):
1. Vocabulary/layout: internal/fault (Kind enum: Broken/Overloaded/Faulted/
   Rejected + Fault() interface; oci imports fault) + internal/retry package
   — vs — Transient()/Delayed() interfaces declared in transfer, oci
   implements structurally (no new import edge), retry loop unexported inside
   transfer.
2. Retry-After: floor (max(jitter, min(hint, Cap)), one 30s cap) — vs —
   override (hint replaces jitter, separate 60s clamp).
3. PR order: oci-classification-first (user-visible ErrPartTooLarge lands
   alone, then transfer flips behavior) — vs — transfer-inert-first, oci
   flips. Both 3-PR splits keep e2e+deps last.

## 2026-08-08 10:37 — Design finalized (DESIGN-phase3.md in scratchpad)
Boundaries rerun landed a decisive design and CHANGED my provisional call on
the seam. Final synthesis (scratchpad/DESIGN-phase3.md is the governing doc):

- Seam: ONE new package internal/retry holding both the vocabulary — a
  transparent tag `retry.Transient(err, after)` + `retry.IsTransient(err)`,
  untagged = terminal — and the policy (Policy/Default/Do, Sleep+Rand func
  types, zero-value = default). Killer arguments: duck-typed interfaces
  can't classify *url.Error values bigoci doesn't own (a wrapper is needed
  anyway); a Kind enum's "core decides" is ceremony (the per-status table
  lives in the adapter's kindOf regardless) at the cost of a second package
  and methods on three error types. oci imports retry (adapter → core leaf,
  legal); oci still never imports transfer; root never imports retry.
- blobBody wrapper in oci.Blobs.Get tags mid-stream read failures — and
  structurally fixes the tail-probe false-terminal AND composes with
  readError untouched. readManifest's ReadAll gets the same tag.
- My amendments vs the boundaries proposal: context-first guard kept in
  Do (defense in depth); Retry-After = FLOOR clamped by the one 30s Cap in
  the CORE (not override-as-sent + 2min adapter constant); errors.Join
  replaced by single-line %w wrap (Join is multi-line → would break the
  CLI's one-line failure contract); single-attempt failures unwrapped.
- Push unit = Exists+Put (idempotency win), claim outside loop; pull unit =
  whole part, fresh OffsetWriter+hasher per attempt; manifest ops + empty
  config own budgets; digest mismatch/long body/disk/unclassified terminal.
- PRs: (1) feat(oci): classify registry failures for retry [retry pkg +
  oci tagging, INERT], (2) feat(transfer): per-part retry with backoff
  [behavior flip + ErrPartTooLarge + CLI exit 7 + ALL docs], (3) test(e2e):
  toxiproxy ride-through suite [only PR adding deps].
- Verified myself: Retry-After already on CLI redactor allow-list; Shopify
  toxiproxy client stdlib-only (2 go.sum lines); design.md pull-path prose
  contradiction confirmed (doc edit + review flag).
Next: implementation worktree from fetched master; Opus implementer builds
PR1; I review line-by-line, commit myself (verified-signature rule), then
review panel.

## 2026-08-08 11:08 — PR1 implemented, lead-reviewed, committed; panel running
Worktree .wt/feat-oci-retry-classify (branch feat/oci-retry-classify from
fetched master 796d477). Opus implementer (xhigh, wf_989e3f47-32e) built PR1
in one pass: internal/retry (doc/fault/policy/retry + 3 test files, 1124
lines, stdlib-only) + oci classification (classify.go, StatusError.RetryAfter,
ErrTooLarge + Is row, tagging at do/statusError/blobBody/readManifest,
doc.go, ~500 diff lines of tests). All five check commands passed for the
implementer including full root:check with real-zot e2e; existing assertions
untouched.

Implementer deviations, all accepted after my line-by-line review: (1)
delta-seconds overflow guard in retryAfter (MaxInt64/time.Second — a
readability bound, not a policy clamp; overflow would go negative); (2)
halfway-to-Cap guard in backoff's doubling loop (Cap near MaxInt64 would
overflow the last doubling); (3) whitebox classify_test.go for pure funcs +
HTTP rows in existing external files; (4) resetConnection/dropConnection
split (flush-before-hijack commits a 200 on the PUT row — found by a failing
test); (5) floor proof via recorded jitter ceilings instead of rng-not-called.

My line-by-line verdict: matches the governing design incl. all amendments
(context-first guard; floor max(jitter, min(after, Cap)); single-line
interrupted wrap; exhaustion-only wrapping — note terminal-mid-budget also
returns unwrapped, consistent with 2 of 3 lenses and CLI rendering). Test
quality high (ceilings assertion = positive floor proof; adapter-does-not-
retry request counting; golden 503 message through the tag).

Implementer's open question worth carrying to PR2: http.Client.Timeout
(caller-supplied via WithHTTPClient) surfaces as req.Context().Err() ≠ nil →
UNtagged → terminal → no retries on slow parts. Accepted as designed (the
deadline is the caller's instruction); PR2 must add the WithHTTPClient godoc
sentence about it.

Verified myself: build + race tests + lint/format green; committed as
fd98e93 "feat(oci): classify registry failures for retry" under
joshuagilman@gmail.com, signature G. Stale-LSP scare ("policy_placeholder"
package) checked and dismissed — files all declare package retry.

Review panel wf_4823b998-7b2 running: correctness/contracts/tests lenses
(opus xhigh) + acceptance full-gate rerun (sonnet). I verify findings myself
before applying any fix.

## 2026-08-08 11:26 — Panel caught a real blocker; fixed; PR #21 open
Panel results (9 findings; acceptance full-gate PASS):

THE BLOCKER (found independently by correctness AND contracts lenses; I
reproduced it myself on Go 1.26.4 before touching anything): net and
net/http deliberately render dial/header/client timeouts as errors matching
context.DeadlineExceeded (net.errTimeout and http's timeoutError both define
Is(target)==context.DeadlineExceeded). Do's errors.Is context guard therefore
treated the most common transient failure class — a hung or blackholed
registry — as a cancellation and returned on attempt 1 with the budget
unspent. Fix: the guard reads ctx itself (`if ctx.Err() != nil { return err }`);
only the loop's own context says whether the transfer is over. My repro also
showed req.Context().Err() stays nil under http.Client.Timeout (net/http
forks the request before setRequestCancel), which BOTH kills the implementer's
earlier open-question claim and resolves it favorably: caller-supplied client
timeouts are tagged transient and retried with a fresh window per attempt.
PR2 must document that in WithHTTPClient's godoc. DESIGN-phase3.md §1
amendment 1 rewritten accordingly.

LESSON (promote to TECH_NOTES at close): never guard retry logic with
errors.Is(err, context.DeadlineExceeded) — Go's transport timeouts match it
by design while the caller's context is alive; gate on ctx.Err() instead.

Other findings, all fixed: loop-top ctx check now wraps the failure in hand
via interrupted() (was dropping the op error and contradicting the godoc);
oci/doc.go carve-out for cancelled requests; TestDefaultSleep hardened
(elapsed >= wait; new cut-short-mid-wait test — the old test passed a
mutation that never waited); new mixed-kind escalation row (hint mid-run
neither resets the count nor the escalation); date-window slack widened to
5s for CI stalls; no-header→after==0 pinned; WithHTTPClient timeout row now
asserts the timeout stays retryable; two new semantics pins (timeout-shaped
tagged error retried; Canceled-resembling tagged error with live ctx
retried).

Commits (both signed G, joshuagilman@gmail.com): fd98e93 feat(oci) classify,
bf81069 fix(retry) ctx-not-shape guard + test hardening. root:check green
after fixes. PR #21 open (3-PR plan documented in body); CI monitor armed.

## 2026-08-08 11:33 — PR #21 and #22 merged; PR2 implementer running
PR #21 first CI run FAILED on cli:test TestTapSummary — "http:
CloseIdleConnections called" on a package the PR never touched. Diagnosis
(verified in code, not assumed): httptest.Server.Close calls
http.DefaultTransport.CloseIdleConnections() as a courtesy, and the five
debug-tap tests passing nil to newTap all shared DefaultTransport under
t.Parallel(), so any sibling test's deferred srv.Close() could kill a tap
test's request mid-flight. Pre-existing flake since PR #19, surfaced by CI
timing. Rerun passed (flake confirmed). Fix shipped as its own PR #22
(test(cli): isolate the debug tap tests from the shared default transport —
per-test http.Transport with Cleanup; nil-fallback still covered by the
run-driven -debug tests); 10× -race green.

LESSON (promote to TECH_NOTES at close): httptest.Server.Close reaches into
http.DefaultTransport — parallel Go tests that do real HTTP through the
default transport race with every other test's server Close; give taps/
clients per-test transports.

Merged (squash): PR #21 → cb02877, PR #22 → 34e56ce. Master local ff'd.
Old worktree removed; PR2 worktree .wt/feat-transfer-retry created and reset
to fetched master (wt --base master had used the stale local master — watch
for that). PR2 implementer running (wf_e8cfa721-0f5, opus xhigh): transfer
wiring + uploader refactor + ErrPartTooLarge + CLI exit 7 + full
failure-injection suite + all D6 docs, per governing DESIGN §9.2. PR3
(toxiproxy e2e) remains after that, then the three manual gates.

## 2026-08-08 12:15 — PR2 implemented, lead-reviewed, committed; panel running
Implementer (36min, 418k tokens) delivered the full PR2 scope green with ten
recorded deviations, all sound (best: the config-blob fresh-reader row must
script the PUT not the Exists — only a draining Put catches a hoisted
reader; the mixed-kind row needs a 1500ms hint because a 1s hint is
indistinguishable from the 1s window under floor semantics; found the
existing wrong-length pull row would silently gain ~4.4s of REAL sleeping —
now runs an explicit one-attempt policy).

My line-by-line review: push.go/pull.go/ports.go/errors.go/client.go/
options.go/cli/run.go all faithful to the governing design, house voice
kept, all inner messages byte-stable. Test suites strong: overwrite proof
uses a garbage prefix + whole-file byte equality; served==closed accounting;
wake-on-peer-failure test blocks the terminal Put until a sibling is
provably inside a backoff. Docs: dead-port recipe rewritten with the real
"after 4 attempts:" line; retry recipe promoted from forward-pointers with
both uniq -d greps; design.md pull-path prose reconciled + Retry policy
subsection.

My decisions on the implementer's open questions: split the 859-line
fixtures file NOW (R2 says before the cap; phase 4 adds more) →
retry_fixtures_test.go (408+474 lines); moved the two new CLI tests to
registry_test.go (house layout beats my instruction); kept noRetry() on the
wrong-length row (suite stays clock-free), the design.md altitude, the
~0.5s real-clock CLI test, and no root-level 413-on-manifest row.

Committed e985d53 feat(transfer): per-part retry with backoff (signed G).
Review panel wf_a9bedb85-580 running (3 opus xhigh lenses + sonnet
acceptance incl. the real dead-port and E2E runs).

## 2026-08-08 12:44 — PR2 panel: 9 findings, all fixed; PR #23 open
Panel (582k tokens) returned 9 findings, acceptance PASS. The big one
(correctness+contracts, verified by the reviewer with an overlay test): a
Source read failure DURING an upload surfaces from inside client.Do, so oci
tags it transient and a dead disk cost the whole budget per part — exactly
what ports.go/push.go/design.md promise cannot happen. Fix: uploader tags
its section reader (tagSourceReads/sourceError, mirror of pull's readError)
and unwraps its own marker after a failed Put, rebuilding the error untagged
("read part N of the source at offset..."). The mock Put now mimics the
adapter (tags body-read failures) so the new test proves the tag DISCARD,
and counts the died-mid-body attempt.

Also measured by the panel and fixed: the dead-port recipe's "exactly four
http! lines per digest, sixteen total" overclaim — only the FIRST-exhausted
digest reaches 4; peers are cancelled mid-backoff (measured totals 8-13).
Recipe + governing DESIGN gate-3 evidence amended. Blockers: cli/doc.go and
root doc.go still said exit 7 reserved / "nothing is retried" — both narrowed;
repo README Status too. Test gaps closed: budget-isolation assertions in
both suites (a per-transfer budget passed every row — reviewer verified
red-on-mutant), exit-7 row in the CLI sentinel table, twin-digest guard in
the wake test. gocognit forced the isolation loop into a shared helper
(assertOnlyTargetRetried).

LESSON (for TECH_NOTES): the errgroup-cancels-peers-mid-backoff effect means
"attempts per digest" is only exact for the first-exhausted digest — any
gate or doc that counts retries per worker must account for cancellation.

Commits: e985d53 + 7f9dda4 (both G). PR #23 open, CI monitored. PR3 worktree
.wt/test-flaky-e2e branched from feat/transfer-retry (e2e needs the retry
behavior; will rebase onto master after #23 merges).

## 2026-08-08 12:46 — PR #23 merged; retries are live on master
PR #23 squash-merged → master 4dca285 (CI green first try this time — the
tap de-flake held). Phase-3 code is fully shipped: classification (PR #21) +
orchestrator wiring/ErrPartTooLarge/exit 7/docs (PR #23). feat/transfer-retry
worktree removed. Remaining: PR3 toxiproxy e2e (implementer wf_adf2a3f6-36a
running in .wt/test-flaky-e2e — must rebase onto master before PR since it
branched from the now-squashed PR2 branch), then the three manual gates with
journaled evidence, then PLAN.md checkboxes + session close. Deliberately
waiting for the implementer to finish before heavy docker use so its e2e
timing runs stay clean.

## 2026-08-08 13:09 — ALL THREE PHASE-3 MANUAL GATES PASSED
Instrument: CLI built from master 4dca285 (go version -m: vcs.revision=
4dca285…, vcs.modified=false — provenance cross-check per TECH_NOTES). Fresh
zot v2.1.20 + toxiproxy 2.12.0 containers on a private docker network
(published on 127.0.0.1:15000 direct / :18666 proxied / :18474 control).

GATE 1 — ride-through (README recipe verbatim: limit_data upstream,
bytes=2000000, toxicity 0.3, 64 MiB at 16 MiB parts, -debug):
- First attempt vs an ALREADY-PUSHED repo produced failed=0 in 0.12s — two
  gate lessons: connection pooling means toxicity 0.3 has a ~10% fully-clean
  chance per run (rerun with fresh content is the procedure, now noted), and
  a repeat push HEAD-skips everything so each attempt needs new bytes.
- Fresh-content attempt 1: exit=0 in 1.08s, http! = 2. Evidence (7 items):
  (1) exit 0; (2) stdout digest sha256:331de8a4…; (3) 2 http! lines (one
  "write: connection reset by peer", one "EOF", both mid-PUT with
  clen=16777216); (4) uniq -d found TWO duplicated blob-write digests
  (a6a549…, c8934a…); (5) a6a549…'s lines: PUT seq 0012 fails at +0.054s →
  HEAD seq 0016 at +0.841s (0.787s jittered backoff) → fresh session PUT seq
  0018 at +0.848s — new seq, new session URL, same digest, exactly the
  README's promised shape; (6) summary "requests=20 failed=2 blob-check=7
  (1 hit, 6 miss) blob-write=6 upload-open=6 …" — the 1 HIT is the
  idempotency win LIVE: c8934a…'s reset PUT actually landed and the retry's
  HEAD skipped the re-upload; (7) pull via clean port byte-identical
  (b7d46361… == b7d46361…).

GATE 2 — zot restarted mid-push (1 GiB at 128 MiB parts, direct port;
docker restart -t 0 fired at +0.7s, took 0.54s):
- exit=0; total 2.17s vs 1.46s clean baseline — the restart cost one
  backoff, not a storm. http! = 1 (PUT dies "EOF" at +0.990s as zot goes
  down). The failed digest's full story in the log: fresh HEAD after a
  0.805s backoff (largest inter-stamp gap 0.805s << 7s budget), 404, fresh
  upload session, 201. Pull back byte-identical (a8138f0f… ×2).

GATE 3 — dead port (README recipe: push to 127.0.0.1:5999, -debug, NO
-timeout, 64 MiB at 16 MiB parts):
- exit=1 in 2.49s (<10s); second line exactly "bigoci: no sentinel matched
  (exit 1)"; first failure line "…after 4 attempts: check whether part 0
  (sha256:f5fa72…) exists: HEAD …: connect: connection refused"; per-digest
  http! = 4/3/3/3, total 13 ≤ 16 — EXACTLY the amended expectation (only
  the first-exhausted digest reaches 4; peers cancelled mid-backoff); zero
  "context deadline exceeded" anywhere — the library bounded itself.

Gate logs kept in scratchpad/gates/ (gate1-final.log, gate2.log, gate3.log)
until session close. Containers torn down.

## 2026-08-08 13:31 — PR3 flake-hunted, reworked, PR #24 open
PR3 implementer delivered the toxiproxy suite with two first-class catches of
its own: (a) zot v2.1.20 dedupes blobs GLOBALLY and answers HEAD across
repositories — with size-seeded fixtures, three of four rows uploaded NOTHING
and the suite passed while proving nothing; fixed by stamping the row's repo
digest into every part (newRowFile). (b) A fixed 1.2s outage window would
flake ~1-in-7 (P(three jittered sleeps sum < 1.2s) ≈ 3.6% × 5 units); the
outage row enables the proxy causally instead.

Dedicated flake-hunter reviewer (20 stressed runs + read toxiproxy source):
6 findings, all folded in. Key mechanism: reset_peer timeout:0 kills the
FIRST chunk on every fresh connection, so under blanket reset/outage the
failures always land on the HEAD existence checks and the uploads run after
the repair — rows 2-3 could never claim "upload retried through damage"
(uploads == parts in 20/20 runs), and the reviewer's own fix (gate on upload
failures) would DEADLOCK for exactly the same reason. My rework: the
limit_data row (checks survive the 87KB cut, PUTs die mid-body) asserts
uploads > parts STRICTLY; rows 2-3 keep honest claims. Also: bounded gate
dial (1s) so port-forwarders that black-hole a disabled port don't hang into
the backstop; one 30s backstop replaces the machine-sized 1.2s/3s walls;
pull row gates+asserts on a REPEATED read of one blob (total counts can be
inflated by request-pattern changes → silent pass); aggregate budget
ceilings (arithmetically unreachable) became per-digest bounds that can
fail. 3 further green runs post-rework.

LESSONS (promote at close): zot global blob dedupe can hollow out any
multi-row e2e that reuses content — vary bytes per row; and "the damage the
toxic does" depends on WHICH request meets it first — reset-at-connect hits
the cheap probe requests, not the expensive bodies, so evidence claims must
be scoped per toxic.

Commits d20caf7 + df6eaf8 (both G). PR #24 open, CI monitored. After merge:
PLAN.md phase-3 boxes + TECH_NOTES promotions + session close.

## 2026-08-08 13:35 — PHASE 3 COMPLETE
PR #24 merged (CI green first try) → master fdbfc03. All four phase-3
success criteria checked in .journal/002/PLAN.md with dated evidence
annotations. The phase shipped as four PRs: #21 feat(oci) classification +
internal/retry (inert), #22 test(cli) tap de-flake (found by #21's CI),
#23 feat(transfer) the behavior flip + ErrPartTooLarge + exit 7 + all D6
docs, #24 test(e2e) the toxiproxy suite. Three manual gates passed with
journaled evidence (13:09 entry). Release PR #11 will pick up the two feat
commits (stays open per policy until the first release is cut).

Phase 4 (resume) is next per PLAN.md; its seams are already in place
(Sink.ReadAt/Size/Discard, Blobs.Get offset + Content-Range verification).
Session stays open pending user review / close request.

## 2026-08-08 13:46 — Close
Phase 3 shipped whole: PRs #21 (classification + internal/retry, cb02877),
#22 (cli tap de-flake, 34e56ce), #23 (per-part retry + ErrPartTooLarge +
exit 7 + all D6 docs, 4dca285), #24 (toxiproxy e2e, fdbfc03) — all
squash-merged, local master fast-forwarded to fdbfc03, all session worktrees
removed. All four PLAN.md phase-3 criteria checked with dated evidence; the
three manual gates' full evidence is in the 13:09 entry. Governing design
preserved as .journal/004/DESIGN.md. SUMMARY.md written; INDEX row set to
complete; TECH_NOTES gained the retry-classification contract, the
ctx-not-error-shape rule, the never-retry-the-disk mechanics, the per-test
transport rule, and the fault-injection e2e discipline, and its seams line
now points at phase 4. Handoff: phase 4 (resume) starts clean from master
fdbfc03; release PR #11 stays open per policy.
