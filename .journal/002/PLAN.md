# bigoci Implementation Plan

Decomposes `docs/docs/explanation/design.md` (+ `docs/docs/reference/format.md`)
into phases. Ordered inward-outward: the core decisions (split format, owned
transport) get proven end-to-end first; reliability, auth, measurement, and
polish layer on afterward. This follows the design doc's "First slice" order.

Ground rules for every phase:

- **Process:** each PR comes from an implementation worktree off `master`
  (`wt switch --create`), integrates via GitHub PR + squash merge, Conventional
  Commit title. Journal writes stay in the journal worktree.
- **Rule compliance (AGENTS.md / CLAUDE.md):** hexagonal boundaries (A1–A4),
  mockery-only mocks for every adapter (T2/T3), three test layers (T1), godoc
  on everything incl. unexported + `doc.go` per package (D1/D4), docs updated
  in the same PR as user-visible behavior (D5/D6), domain types not plain
  strings (I1), sentinel errors only at the caller-branching level (E1),
  streaming not buffering (P2), 1,000-line file cap (R2).
- **Definition of done:** a phase closes only when its success criteria are
  met, including the manual checks. Passing CI is necessary, never sufficient
  ("a feature is done when it works against a real registry").
- **Manual-verification vehicle:** the library ships no CLI (non-goal), so
  phase 2 builds a **reference CLI** in its own Go module (no CLI dependencies
  in the core; never published, no production intent). All manual checks in
  later phases use it plus generic tools (`curl`, `oras`, `sha256sum`,
  `docker`). Phase 1's manual proofs are deliberately deferred into phase 2's
  gate, since the CLI is the vehicle that exercises them.

---

## Phase 1 — Walking skeleton: prove the format and the transport

**Goal:** push one file to a real registry (zot in testcontainers) as a
split-part artifact and pull it back byte-identical. Small fixed parts,
no retries, no resume, anonymous auth. This proves the two load-bearing
decisions (ordered-parts format, owned `net/http` transport) before anything
else is built on them.

**PRs:**

1. `feat(core): split planner and manifest codec` —
   `internal/plan` + `internal/manifest`, pure logic only.
2. `feat(ports): port definitions and adapters for OCI, file I/O` —
   root-package port interfaces, `internal/oci` (Blobs/Manifests over
   `net/http`), `internal/file` (Source/Sink), mockery config + generated
   mocks.
3. `feat(api): transfer orchestrator and public Push/Pull API` —
   `internal/transfer` (workers, no retry logic yet), root `Client` /
   `New` / `FromFile` / `ToFile` / options, zot testcontainers e2e for
   happy-path push + pull.

**Tasks:**

- Split planner: pure arithmetic per the format's split rule, 4096-part cap
  enforced, `PartSize`/`Digest`/`Reference` domain types (I1).
- Manifest codec: encode/decode `application/vnd.bigoci.file.v1` manifests
  exactly per `format.md` — empty config descriptor, ordered part layers, the
  four annotations, deterministic byte-stable encoding.
- `internal/oci`: the six endpoints; monolithic blob PUT streamed with
  explicit `Content-Length` (sharp edge #3); reference parsing via
  `distribution/reference`; digest-only (tagless) references accepted.
- `internal/file`: `io.NewSectionReader`-based Source; Sink writing to
  `<dest>.bigoci-partial` with `Truncate` + atomic `Commit` rename.
- Orchestrator: sequential hash pipelined into N upload workers; per-part
  `HEAD` skip (dedup comes free); empty-config-blob ensure; manifest written
  last. Pull: concurrent part GETs via `WriteAt`, per-part digest verification,
  rename on success.
- e2e: push/pull a 64 MiB random file at 4 MiB parts against zot; corrupt-part
  detection test (flip a byte in a stored blob, expect digest-mismatch error).
- Scaffolding as needed: mockery config, moon test tasks, testcontainers dep.

**Success criteria:**

- [x] Automated gates: unit tests for plan/manifest; integration tests of the
      orchestrator against mockery mocks; zot e2e green in CI.
      *(2026-08-07: PRs #15, #16, #17 merged; e2e confirmed running in CI.)*
- [x] Structure audit: every package has `doc.go` + full godoc; mocks are
      generated only; no file over 1,000 lines; core packages import no I/O.
      *(2026-08-07: verified per-PR by stabilizer agents and my own review;
      largest file 656 lines; transfer imports no adapter or I/O package.)*
- [ ] Manual functional proof of the skeleton is deferred to phase 2, which
      exists to make it possible; phase 1 is not considered *proven* until
      phase 2's manual criteria pass.

---

## Phase 2 — Reference CLI: the manual-verification vehicle

**Goal:** a small reference CLI that demonstrates push and pull, in its own
Go module so no CLI dependency ever touches the core library's module graph.
"Reference" means: never published, never released, no production intent —
it exists to demonstrate the library and to make every later phase's manual
verification real. Landing it now, immediately after the skeleton, means
every subsequent phase has its vehicle before it needs it.

**PRs:**

1. `feat(cli): reference CLI for push and pull` — `cli/` directory with its
   own `go.mod` (module `github.com/componere/bigoci/cli`), `replace`
   directive pointing at the core module in the repo root; `bigoci push
   <file> <ref>` and `bigoci pull <ref> <dest>`; flags for part size, worker
   count, and plain-HTTP (local registries); progress/log output verbose
   enough to observe HEAD skips, retries (later), and request headers
   (needed by phase 5's no-auth-leak check).
2. `ci: build and test the cli module` — moon/CI wiring so the CLI module
   compiles and its (thin) tests run per commit; explicitly excluded from
   release-please and any publishing path.

**Tasks:**

- Module isolation: core `go.mod` stays free of cobra/CLI deps; the CLI
  module requires the core via `replace`; verify with `go mod graph` that
  nothing flows backward.
- Command surface: push, pull, and a `--debug` mode that logs each HTTP
  request (method, URL, selected headers) — this observability is what makes
  later manual gates checkable.
- Keep it thin: flags map 1:1 onto public API options. If the CLI needs
  something the API cannot express, that is API feedback, not cause for CLI
  logic (A3 pressure valve).
- README in `cli/` stating its reference-only status.
- Run phase 1's deferred manual proofs (below).

**Success criteria** (these double as phase 1's manual proof):

- [ ] Manual: `bigoci push` a ~100 MiB file to a locally running zot;
      `curl` the manifest and confirm by eye it matches `format.md`
      (artifactType, empty config, ordered part layers, all four
      annotations, sizes sum to file size).
- [ ] Manual: pull the artifact back into a different directory;
      `sha256sum` of pulled file equals the original **and** equals the
      `io.bigoci.file.digest` annotation.
- [ ] Manual: push the same file twice; second push visibly skips every part
      (debug output shows HEAD-hits, near-instant completion), and the
      manifest digest is identical both times (determinism observed, not
      just unit-tested).
- [ ] Manual: a file smaller than the part size produces a single part whose
      digest equals the file digest (verify with `sha256sum` against the
      layer descriptor).
- [ ] Structure audit: `go mod graph` on the core module shows zero
      CLI-originated dependencies; CLI module builds and runs from a clean
      checkout; nothing publishes it.

---

## Phase 3 — Retries: transient failure never reaches the caller

**Goal:** the full retry policy from the design's Defaults table — 4 attempts,
exponential backoff (1 s base, 30 s cap, full jitter), `Retry-After` honored,
retry on network errors/429/5xx, fail fast on other 4xx — applied per part.

**PRs:**

1. `feat(transfer): per-part retry with backoff` — retry policy in the
   orchestrator, injected sleep function, part re-stream from disk on retry
   (the file is the transfer buffer), failure-injection integration tests.
2. (If it grows) `feat(oci): retry-relevant error classification` — typed
   HTTP error mapping in the adapter so the core can make retry decisions
   without seeing HTTP.

**Tasks:**

- Retry decision logic in the core, clock-free tests via injected sleep (A1).
- Error classification: adapter maps HTTP failures to typed errors; core
  decides retryable vs. terminal. Sentinel errors for the caller-facing cases
  (E1), including "part too large" as the surface for registry caps.
- Failure injection at the mock layer: dropped connection mid-part PUT,
  mid-part GET, 429 with `Retry-After`, 500s, out-of-order worker completion.
- e2e: proxy zot through toxiproxy (or equivalent) in testcontainers; kill
  connections mid-transfer; assert transfers complete.

**Success criteria:**

- [ ] Manual: run a push through a toxiproxy with periodic connection resets;
      watch the CLI ride through failures and finish; pulled file still
      byte-identical.
- [ ] Manual: restart the zot container mid-push; the in-flight attempt fails,
      retries recover once the registry is back (within backoff budget), and
      the artifact completes intact.
- [ ] Manual: point the CLI at a dead port; observe fail-fast with a clear
      terminal error after the bounded attempts — no hang, no infinite retry.
- [ ] Automated gates: table-driven unit tests for the backoff/decision
      matrix; failure-injection integration suite; flaky-network e2e green.

---

## Phase 4 — Resume: interrupted transfers cost only the missing parts

**Goal:** a killed push or pull, re-run, transfers only what is missing.
Push resume via `HEAD` skip (already present) re-validated under kill; pull
resume via re-hashing the existing `.bigoci-partial` part ranges and fetching
only mismatches. Mid-part pull resume via `Range` when honored, part re-fetch
when not.

**PRs:**

1. `feat(transfer): pull resume from partial files` — partial detection,
   part-range re-hash, selective fetch, `Range`-based mid-part continuation
   with whole-part fallback.
2. `test(e2e): kill-and-resume coverage` — if the harness work is big enough
   to stand alone.

**Tasks:**

- Sink resume path: existing partial at expected size → hash every part range
  (unwritten zeros fail naturally), build a fetch plan of failures only.
- Mid-part continuation: on transfer error, resume from failed offset with
  `Range`; detect non-honored `Range` (200 vs 206) and fall back to full part.
- Guard rails: partial at wrong size is discarded and restarted; destination
  file only ever appears complete (rename-last invariant re-asserted).
- e2e: kill the process (not just cancel the context) mid-push and mid-pull,
  rerun, assert completion and integrity; assert only missing parts moved
  (request counting via proxy or registry logs).

**Success criteria:**

- [ ] Manual: Ctrl-C a large push at ~50%; re-run; debug output shows
      completed parts skipped via HEAD and only the remainder uploads; final
      artifact pulls back byte-identical.
- [ ] Manual: Ctrl-C a large pull at ~50%; confirm `<dest>.bigoci-partial`
      exists and `<dest>` does **not**; re-run; observe only unfinished parts
      fetched; `<dest>` appears only at the end, correct by `sha256sum`.
- [ ] Manual: corrupt a byte inside the partial file before resuming; the
      damaged part (and only it) is re-fetched; result still verifies.
- [ ] Automated gates: resume-bookkeeping unit tests; integration tests for
      partial-hash planning; kill/resume e2e green in CI.

---

## Phase 5 — Auth and real registries: leave the lab

**Goal:** authenticated push/pull via the Docker credential ecosystem, plus
the transport work real registries force: presigned-redirect handling. After
this phase bigoci works against GHCR with `docker login`, not just local zot.

**PRs:**

1. `feat(auth): oras-go credentials adapter behind the Auth port` — Auth port
   already defined; `internal/auth` wraps oras-go v2 `auth`/`credentials`
   (Docker config, credential helpers, bearer token exchange, token cache);
   mocks; auth-enabled registry e2e (htpasswd zot / Distribution).
2. `feat(oci): presigned redirect handling` — disable auto-redirect; re-issue
   redirects with a clean (no `Authorization`) client (sharp edge #1); never
   reuse a stored redirect URL on retry — re-request and follow fresh (sharp
   edge #2).
3. `ci: manual cloud-registry conformance job` — manually triggered workflow
   exercising a real cloud registry with repo credentials.

**Tasks:**

- Auth port wiring into `internal/oci` request path; anonymous remains the
  zero-config default.
- Auth-enabled e2e: token-auth (or htpasswd) local registry in
  testcontainers; wrong-credentials path surfaces the unauthorized sentinel.
- Redirect e2e: an object-storage-backed or redirect-capable local setup if
  feasible; otherwise this is exactly what the manual GHCR check must cover.
- Docs (D6): how-to guide for authentication (docker login, credential
  helpers, the ggcr keychain alternative noted in the design).

**Success criteria:**

- [ ] Manual: `docker login ghcr.io`, then push a multi-part file to a
      private GHCR repo with the CLI and pull it back byte-identical —
      credentials read from Docker config, zero bigoci-side config.
- [ ] Manual: pull from GHCR (which redirects blob GETs to object storage)
      succeeds — proving the clean-client redirect re-issue works against a
      real presigned URL, and no `Authorization` header leaks (verify via
      the CLI's `--debug` request-header logging).
- [ ] Manual: with no/bad credentials, the unauthorized sentinel error
      surfaces with an actionable message (`errors.Is` demonstrable via the
      CLI's exit path).
- [ ] Manual: tagless (digest-only) push + pull round-trip against GHCR.
- [ ] Automated gates: auth adapter integration tests; auth-enabled e2e in
      CI; conformance workflow runs green when triggered by hand.

---

## Phase 6 — Measurement: benchmark harness sets the real defaults

**Goal:** the benchmark harness from the design's Testing section, then
evidence-based final defaults (part size, worker count) before v1, and data
for the design's one open question (adaptive worker count).

**PRs:**

1. `feat(bench): throughput harness against local registries` — matrix over
   part sizes × worker counts × file sizes, against zot/Distribution;
   repeatable invocation (moon task); nightly volume job for multi-GB runs
   (per-commit CI stays small).
2. `chore: set measured defaults` + docs update — whatever the data says;
   design doc and format examples updated in the same PR if defaults change
   (D6).

**Tasks:**

- Harness measuring wall-clock throughput push and pull, warm and cold.
- Run the matrix on real hardware (and ideally one real cloud registry with
  a fat pipe); record results in the journal + a benchmarks reference doc.
- Decide: keep or change 512 MiB / 4 workers; write down the adaptive-
  concurrency verdict (or explicitly defer it with the supporting data).

**Success criteria:**

- [ ] Manual: harness run reproduced on at least one real machine outside CI;
      numbers recorded and sanity-checked (e.g. 4 workers ≈ saturating the
      link per the design's 85–90 MB/s-per-connection expectation).
- [ ] Manual: chosen defaults demonstrably beat at least the naive
      alternatives in the recorded matrix (not hand-waved).
- [ ] Decision recorded for the open question (adaptive worker count) with
      data attached, in the design doc's Open Questions section.
- [ ] Automated gates: nightly volume benchmark job green; per-commit CI
      unaffected (runtime budget respected).

---

## Phase 7 — Finishing touches: API polish, docs, v0.1.0

**Goal:** everything a first real user touches: progress reporting, the full
sentinel-error contract, documentation per Diátaxis, and the first release.

**PRs:**

1. `feat(api): progress reporting option` — callback-based progress
   (bytes/parts, push and pull), wired through the orchestrator's existing
   progress accounting; the reference CLI adopts it for its own output.
2. `docs: user documentation set` — tutorial (first push/pull), how-to guides
   (auth, resume, tuning part size/workers), reference (API surface, errors;
   format.md already exists), explanation (design.md already exists). Plain
   Language style (D5). The reference CLI may appear in docs as a
   demonstration, clearly labeled as unsupported reference material.
3. `chore: v0.1.0 release` — API surface review (thin exported set, A3),
   godoc/example pass on the public boundary (D2), then merge the
   release-please PR once it reads `0.1.0`.

**Tasks:**

- Progress callback API: honest accounting under retries and resume (a
  retried part must not double-count).
- Error contract audit: every case the design names (not found, unauthorized,
  digest mismatch, not-a-bigoci-artifact, part-too-large) has a sentinel,
  a test, and reference documentation.
- Public API review against the design's API sketch; kill anything exported
  that need not be (A3); `Example` functions for Push/Pull (D2).
- README updated to reflect the real, working library.
- Verify the published docs site renders (Pages was enabled last session and
  is still unverified).

**Success criteria:**

- [ ] Manual: follow the tutorial verbatim on a clean machine/checkout (fresh
      clone, no repo context) and reach a successful push + pull against a
      registry — the doc, not tribal knowledge, is sufficient.
- [ ] Manual: progress output observed live in the CLI during a real
      multi-part transfer, including sane behavior across an induced retry
      and a resume.
- [ ] Manual: docs site at componere.github.io/bigoci renders correctly
      (nav, all four Diátaxis sections, design + format pages intact).
- [ ] Manual: `go doc` / pkg.go.dev-style review of the public surface reads
      clean; no accidental exports.
- [ ] v0.1.0 tagged via release-please (merge the open release PR at ≥0.1.0);
      `go get github.com/componere/bigoci@v0.1.0` works from a scratch
      module.

---

## Sequencing notes

- Phases are strictly ordered; each depends on the previous one's proven
  behavior. No phase starts before the prior phase's success criteria are
  checked off (recorded in this file / NOTES.md as they complete). The one
  deliberate exception: phase 1's manual proof lives in phase 2's gate,
  because the reference CLI is the instrument that makes it checkable.
- Within a phase, PRs land in the listed order; each PR keeps CI green and
  respects D6 (docs travel with behavior).
- The reference CLI (`cli/`, own Go module) is demonstration and
  verification tooling, not product: never published or released, no
  backward-compatibility promises, and no CLI dependency may enter the core
  module's graph. If the CLI needs capability the public API lacks, extend
  the API (or record why not) — never work around it inside the CLI.
- Registry-behavior discoveries made during any phase (esp. 5) get recorded
  in NOTES.md and promoted to TECH_NOTES.md at session close.
