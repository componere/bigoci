---
id: 011
title: New work session
started: 2026-08-13
---

## 2026-08-13 10:14 — Kickoff
Goal for the session: Create and bind a new journal session; the substantive task has not been stated yet.
Current state of the world: The original seven-phase implementation roadmap and independent security remediation are complete, and the repository is ready for a fresh request.
Plan: Wait for the substantive request, then scope, implement, and verify it using the repository's established contracts.

## 2026-08-13 10:30 — go-oci-blob integration research
Goal: Plan replacement of bigoci's bespoke monolithic blob upload with github.com/imgoci/go-oci-blob.
Findings: bigoci's artifact orchestration must remain: internal/transfer still owns splitting, hashing, dedupe, the shared Exists/Put retry budget, manifest publication, and the two-counter progress contract. The replaceable seam is internal/oci.Blobs.Put plus its upload-session helpers. go-oci-blob v1.0.0 supplies the POST/PUT upload, exact-size enforcement, body ownership, session cleanup, and off-origin transport split; the inspected local master is 7453b75-dirty, while the relevant public/upload source matches v1.0.0 except for a lint comment.
Compatibility gaps: a direct swap would nest retries, lose exact WireBytes because go-oci-blob stages up to 1 MiB, follow 307/308 write redirects bigoci currently refuses, expose less structured/sanitized registry errors, bypass bigoci's bearer state unless adapted, and omit bigoci's private-peer/userinfo/downgrade policy unless its guarded storage transport remains in the path.
Decision: use a two-repository, contract-first migration. First add the narrow embedding hooks to go-oci-blob (safe structured registry errors, transport-consumption progress, and configurable write-redirect refusal), release them, then adapt bigoci's existing auth and guarded external transports around a retry-disabled blob client. Preserve every public, retry, progress, debug, and security contract; delete the old open/complete upload implementation only after the full push matrix passes. Start with a throwaway spike and stop if the bridge copies protocol logic instead of deleting it.

## 2026-08-13 10:34 — Upstream scope narrowed
Decision: make the integration clean by changing go-oci-blob first; full control of both repositories removes any reason to build a response-parsing transport shim in bigoci.
Minimal upstream surface: keep RetryPolicy{} as the one-attempt mode; add an authoritative retry inspection API that preserves Retry-After and exported unauthorized/too-large sentinels; add a transport-consumption wire-byte callback; and add an option that rejects write redirects while retaining the current default. Harden upload Location and registry-error rendering so raw peer-selected capabilities never appear, independent of bigoci.
Boundary: authentication and private-actual-peer policy remain bigoci adapters through WithTransport and WithStorageTransport. Artifact splitting, Exists-before-Put idempotence, outer retry scheduling, progress aggregation, and manifest publication remain in bigoci.

## 2026-08-13 10:37 — Upstream agent handoff
Created temporary root file `GO_OCI_BLOB_UPSTREAM_REQUIREMENTS.md` for an implementation agent. It specifies the integration boundary, retry inspection and sentinels, transport-consumption progress, strict write redirects, capability-safe errors and locations, non-goals, regression matrix, documentation, and release criteria. The file is intentionally uncommitted and will be deleted after the upstream work.

## 2026-08-13 13:56 — v1.1.0 verification
Verified published `github.com/imgoci/go-oci-blob` v1.1.0 at signed commit `220500bf`; release is public/non-prerelease and its CI, CodeQL, Pages, and release workflows passed. A fresh tag checkout passed `go test -race ./...`, `go build ./...`, and `mise x -- env -u GOROOT moon run root:check`, including e2e.
The required embedding surface is present and correctly covered: one-attempt `RetryPolicy{}`, `Retryable` plus `StatusCode`, registry-only `ErrUnauthorized`/`ErrTooLarge`, `WithWireProgress`, `WithWriteRedirects(false)`, structural Location/errors, preserved opaque queries, caller-owned registry/storage transports, and embedding documentation. A throwaway bigoci test proved two outer attempts produce exactly two POSTs/two PUTs, preserve a two-second Retry-After floor, and count both failed and successful attempt bytes.
Two discrepancies prevent an unconditional approval. First, v1.1.0 declares Go 1.26.5 while bigoci pins Go 1.26.4 with `GOTOOLCHAIN=local`; the real pinned-toolchain spike fails until bigoci updates its Go/mise lock. Second, `Retryable` returns a negative duration for a past HTTP-date Retry-After although its contract says zero when no usable floor exists; a public regression probe reproduced `-1h...`. Bigoci's retry loop ignores non-positive values, so this does not break scheduling, but it is an upstream API-contract defect suitable for v1.1.1.

## 2026-08-13 14:33 — Past Retry-After fix opened
Dispatched an isolated agent in go-oci-blob. PR #35 (`fix: clamp elapsed Retry-After dates to zero`) is open and non-draft at commit `af45bd3`; it changes only `retry.go` and `retryable_test.go`. The regression fails against v1.1.0 with a negative duration and passes with `max(at.Sub(now), 0)`. Reviewed the diff: it preserves retryability and changes only the unusable past-date delay. CI, CodeQL for Go and Actions, and Pages all passed; GitHub reports the PR mergeable and CLEAN.

## 2026-08-13 14:40 — v1.1.1 released
Squash-merged fix PR #35 as verified master commit `5eeca6d`. Release Please created PR #36 with only the 1.1.1 manifest bump and changelog entry; its CI, CodeQL, and Pages checks passed, then it was squash-merged as `3290a98`. Release Please created the protected tag and draft release. After all workflows on the release commit passed (Release Please, CI including e2e, both CodeQL runs, and Pages), published the draft. `v1.1.1` is public at `https://github.com/imgoci/go-oci-blob/releases/tag/v1.1.1`; `go list -m -json github.com/imgoci/go-oci-blob@v1.1.1` resolves the tag to `3290a98fe9db7d2be2dc92cacdb9c64ea978af2e`.

## 2026-08-13 22:07 — Integration plan revised for v1.1.1
The architecture and bigoci migration boundary do not change. Upstream implementation/release and the throwaway integration spike are now completed gates rather than future phases. Bigoci implementation should start by raising its pinned Go toolchain and user-facing prerequisite from 1.26.4 to at least go-oci-blob v1.1.1's 1.26.5 floor, then pin v1.1.1 and implement the two transport adapters around a retry-disabled client. No downstream clamp or workaround for elapsed Retry-After dates is needed; use `Retryable` directly. Preserve the planned auth, private-peer, strict-redirect, progress, outer-retry, artifact-splitting, and manifest-publication boundaries.

## 2026-08-14 08:37 — go-oci-blob cutover implemented
Implemented the integration on `feat/go-oci-blob` as commit `c210f20` (`refactor(oci): delegate blob uploads to go-oci-blob`). Raised every Go module and the mise lock to Go 1.26.5, pinned `github.com/imgoci/go-oci-blob` v1.1.1, and updated the README and tutorial prerequisite.
The cutover deletes bigoci's upload-session protocol code. `internal/oci.Blobs.Put` now delegates one monolithic attempt to a retry-disabled blob client. Narrow registry and storage `RoundTripper` adapters retain bigoci's bearer challenge state, caller HTTP transport and timeout behavior, ambient-cookie stripping, private-peer/DNS-rebinding guard, sanitized transport errors, and write-redirect refusal. `internal/transfer` still owns fresh readers, retries, dedupe, manifest publication, and progress aggregation; the Blobs port now passes a synchronous wire-consumption callback so failed and successful attempt bytes remain counted.
Verification passed: focused `internal/oci` and `internal/transfer` tests; root and CLI push integration tests; the complete `moon run root:check` matrix including formatting, lint, unit/race tests, Windows build, nested modules, and docs; and a live zot v2.1.20 cold/warm push smoke proving repeated pushes reproduce the artifact. The final diff removes more legacy upload logic than it adds and leaves no compatibility shim.

## 2026-08-14 08:45 — Integration PR opened
Pushed `feat/go-oci-blob` at `c210f20` and opened non-draft PR #53, `refactor(oci): delegate blob uploads to go-oci-blob`: https://github.com/imgoci/bigoci/pull/53. GitHub reports the head mergeable against `master`; CI and Pages checks registered and are queued.
