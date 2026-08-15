---
id: 012
title: Plan IMGOCI upstream requests
started: 2026-08-15
---

## 2026-08-15 09:37 — Kickoff
Goal for the session: Process `IMGOCI_UPSTREAM_REQUESTS.md` with a planning agent and produce a complete implementation plan satisfying the full request.
Current state of the world: The personal journal is initialized on `journal/jmgilman`; sessions 001–011 are complete; the upstream request document is available in the main checkout and has not yet been analyzed in this session.
Plan: Have a planning agent inspect the request and relevant repository code, review its proposal, save the final plan in this session folder, and checkpoint the journal.

## 2026-08-15 09:59 — Implementation plan complete
The planning agent traced all five upstream requests through the current decoder, OCI adapters, redirect path, push orchestrator, public API, tests, docs, benchmark harness, and release workflow.
Reviewed and tightened the proposal: response coding is checked before status/body handling across repeated header fields; manifest and blob functional failures are exercised separately; token compression is proven through a complete authenticated transfer; `Push` and `PushByDigest` share one private transfer body; upload verification reuses the existing source-error terminal path; and no performance threshold is invented.
Saved the final six-phase implementation and release plan as `PLAN.md`. Next: implement phases 1–5, run integrated verification, and release `v0.2.0`.

## 2026-08-15 10:03 — Implementation orchestration started
The approved plan is now being implemented on five isolated Worktrunk branches from the fetched `origin/master`: `feat/manifest-casefold`, `feat/identity-reads`, `feat/push-by-digest`, `docs/external-transport-contract`, and `feat/upload-wire-verify`.
Each phase will receive independent Programmer implementation, orchestrator-run verification, a focused Conventional Commit, and a squash-merged GitHub PR. Integrated verification and the `v0.2.0` Release Please flow follow only after all phase PRs are green and merged.

## 2026-08-15 11:56 — Upstream requests released
All five requested changes shipped through independently reviewed, green PRs:

- PR #55 accepts decoder media types case-insensitively while preserving canonical encoder bytes.
- PR #58 requires and validates identity content coding on manifest/blob reads, including redirects, while preserving compressed token-realm authentication.
- PR #57 adds repository-only `Client.PushByDigest` publication without changing `Push` or `Pull`.
- PR #56 publishes the external-transport wrapper contract and its structural contract test.
- PR #59 re-hashes every upload attempt against the hash-pass digest and terminates source mutation without publishing a manifest.

The final upload benchmark pooled two 512 MiB populations with 103 samples per path. Median cold-push throughput changed from 1129.71 MB/s to 1113.96 MB/s (-1.39%); warm-push changed from 1337.30 MB/s to 1335.23 MB/s (-0.16%). No CPU increase was observed: the final paired large run used 8.82 seconds of user+system CPU before and 8.26 seconds after. The always-on integrity check was therefore retained.

Integrated verification passed `mise exec -- moon run root:check`. Regenerating mocks with pinned `mockery` produced no diff, and all five implementation branches contained no tracked `.journal` files. The temporary upstream request document was deleted after its requirements were represented in code, tests, documentation, benchmark evidence, and release notes.

PR #60 released `v0.2.0` at commit `38fdcb2ed737a25cd597e7d593c64daa0032f0b8`. The public GitHub release and remote tag resolve to that commit, and `go list -m github.com/imgoci/bigoci@v0.2.0` resolves successfully. Release notes prominently warn that registry front ends and middleboxes must preserve stored manifest/blob bytes.

Release automation limitation: the default `GITHUB_TOKEN` is not permitted to create pull requests in this repository. Release Please updated its branch but failed when opening the PR, so PR #60 and the draft/public release transition were completed manually. A future session should either enable Actions-created PRs or move Release Please to an appropriately scoped GitHub App token.
