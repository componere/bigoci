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
