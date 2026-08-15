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

## 2026-08-15 12:12 — Release Please GitHub App restored
The prior limitation was configuration drift, not an intended repository constraint. PR #61 restored the template's GitHub App authentication model. The workflow now mints a short-lived `meigma-release-please` installation token and passes it to Release Please instead of using `GITHUB_TOKEN`.

Configured repository variable `MEIGMA_RELEASE_APP_CLIENT_ID` and secret `MEIGMA_RELEASE_APP_PRIVATE_KEY` from the `imgoci-release-please` item in the 1Password `Development` vault. The current client-ID input avoids the deprecation warning emitted by `actions/create-github-app-token` v3 for `app-id`.

Workflow-dispatch run 31903018406 proved App token creation and Release Please execution without annotations. After PR #61 merged as `0e721c8be5c5f4c422e1d83bb3d76a423e90ba15`, master run 31903199416 passed through the same App path. The App then created release PR #62 as `app/imgoci-release-please`, and that PR's normal CI and Pages checks passed.

GitHub rejects `meigma-release-please` as a tag-ruleset bypass actor because the App is owned by meigma rather than the imgoci ruleset owner. Tag creation therefore remains open to write-access actors while update, deletion, force-push, and signature protections keep created tags immutable. The repository settings API adapter was also corrected from obsolete `PATCH` to GitHub's current `PUT` update method, with a focused unit test.

Release PR #62 (`chore(master): release 0.2.1`) remains open for the normal release-review decision; it was not merged as part of the authentication repair.

## 2026-08-15 12:15 — Tag policy comment corrected
PR #63 removed the inherited template claim that the cross-organization App must be a protected-tag bypass actor and documented the verified write-access/immutable-tag policy instead. It merged as `6406c7d6a8a391f3e6d0aa2953492635cb2c38d3`; subsequent master Release Please run 31903363368 succeeded, and App-created release PR #62 remains green.

## 2026-08-15 12:20 — Corrected App identity and protected tags
Correction to the two preceding entries: the failed bypass test used the template's `meigma-release-please` slug, not the `imgoci-release-please` App represented by the supplied 1Password credentials. Release PR #62's `app/imgoci-release-please` author exposed the mismatch.

PR #64 now names `imgoci-release-please` in repository settings and merged as `0fc2f5b5e7fbc29efcd2c13e4a204808fb31f81b`. GitHub accepted that App as Integration actor `4553816`; the active tag ruleset now restricts creation and grants only the App and repository-admin recovery bypass, while retaining signature, update, deletion, and non-fast-forward protections. Master Release Please run 31903559160 succeeded through the hardened path, and release PR #62's refreshed CI and Pages checks passed.
