---
id: 011
title: go-oci-blob released and integrated
date: 2026-08-14
status: complete
repos_touched: [imgoci/go-oci-blob, imgoci/bigoci]
related_sessions: [009, 010]
---

## Goal
Replace bigoci's bespoke monolithic blob uploader with a reusable `go-oci-blob` client without weakening authentication, destination security, retry, progress, or error contracts. Release the required upstream embedding surface before integrating it into bigoci.

## Outcome
The goal was met. `go-oci-blob` v1.1.1 is released with one-attempt retry inspection, wire-consumption progress, strict write-redirect control, structured registry errors, and hardened upload locations. bigoci PR #53 now delegates blob uploads to that client through narrow authentication and guarded-storage transports, and deletes its upload-session protocol implementation.

## Key Decisions
- Change the upstream library before bigoci -> avoids a downstream response-parsing shim and keeps protocol ownership in one package.
- Disable upstream retries inside bigoci -> preserves the orchestrator as the only retry scheduler and retains fresh-reader ownership.
- Adapt registry and storage transports separately -> preserves bigoci's bearer state on the registry origin while retaining its cookie-free private-peer and DNS-rebinding guard off origin.
- Report bytes at transport consumption -> preserves `WireBytes` across failed and successful attempts without counting disk reads or buffering parts.
- Refuse write redirects in the embedded client -> preserves the one-body-per-attempt contract while leaving the upstream default unchanged for other users.

## Changes
- `imgoci/go-oci-blob` v1.1.0/v1.1.1 - added and corrected the embedding contracts required by bigoci, including clamping elapsed HTTP-date `Retry-After` values to zero.
- `bigoci/internal/oci/blobs.go` - delegates monolithic `Put` attempts to `go-oci-blob` v1.1.1 and maps retry metadata back into bigoci's outer retry contract.
- `bigoci/internal/oci/blob_transport.go` and `repository.go` - adapt existing authentication, caller HTTP behavior, timeout handling, error sanitization, and guarded off-origin transport.
- `bigoci/internal/transfer` - passes a synchronous wire-consumption callback through the `Blobs` port while retaining splitting, hashing, dedupe, retries, progress aggregation, and manifest publication.
- `bigoci/go.mod`, nested modules, `mise.toml`, and `mise.lock` - raise the pinned Go floor to 1.26.5 and pin `go-oci-blob` v1.1.1.
- `bigoci/README.md` and `docs/docs/tutorial/get-started.md` - publish the Go 1.26.5 prerequisite.

## Open Threads
- No migration work remains. Future bigoci releases continue through the existing Release Please workflow.

## Lessons
- A small upstream embedding API deleted more downstream protocol code than it added; the two-repository contract-first spike exposed the required hooks before either repository committed to the cutover.
- A library retry-inspection contract must clamp unusable time floors itself. Depending on a consuming scheduler to ignore negative values leaves the public API internally inconsistent.

## References
- [bigoci PR #53](https://github.com/imgoci/bigoci/pull/53)
- [go-oci-blob v1.1.1](https://github.com/imgoci/go-oci-blob/releases/tag/v1.1.1)
- [go-oci-blob PR #35](https://github.com/imgoci/go-oci-blob/pull/35)
- [go-oci-blob release PR #36](https://github.com/imgoci/go-oci-blob/pull/36)
- `.journal/009/SUMMARY.md`
- `.journal/010/SUMMARY.md`
