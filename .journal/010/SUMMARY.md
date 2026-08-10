---
id: 010
title: Independent security audit and remediation
date: 2026-08-09
status: complete
repos_touched: [componere/bigoci]
related_sessions: [006, 009]
---

## Goal
Independently audit `bigoci` at `3b82229` and report only defects proven exploitable through the current public library or shipped CLI with a consequence appropriate to the package. Resolve every confirmed root issue with consequence-level regressions and land the fixes through reviewed GitHub PRs.

## Outcome
The goal was met in full. The audit proved six root security boundaries: off-origin upload authentication crossing, registry-selected private authority and ambient-cookie use, manifest allocation amplification, attacker-owned pull partials, uncancellable sparse-resume hashing, and CLI credential/capability/control output. Six reviewed PRs (#41, #42, #43, #46, #49, #51) squash-merged, leaving clean `master` at `0a80682` with no reproduced exploit remaining.

The final integrated matrix passed the BOCI-01 through BOCI-05 consequence tests and eleven BOCI-06 tests three times under the race detector. Original probes confirmed zero private-realm calls, immediate cancellation of a 64 GiB resume check, a reduction from roughly 864.7 MiB to 9.7 MiB end-to-end allocation for the compressed manifest bomb, and structural CLI output with no forged record or terminal control. Hosted CI, CodeQL, Pages, and live GHCR conformance also passed.

## Key Decisions
- Admit findings only after a public-API or real-CLI exploit demonstrates a concrete consequence; reject scanner-only, unreachable, or caller-local candidates.
- Group related exploit variants by the boundary that must hold, producing six narrow remediations instead of one patch per symptom.
- Enforce registry-selected cross-host authority at the actual direct peer before HTTP bytes leave; require the explicit `WithUnverifiedExternalTransport` escape hatch when an opaque caller transport makes that proof impossible.
- Decode manifests through a bounded wire representation that materializes only fields bigoci consumes; counting only top-level layers was insufficient against nested descriptor and annotation amplification.
- Make peer-controlled CLI data structurally unrepresentable rather than heuristically redacting known secrets; authenticated peers repeatedly reflected reusable credentials through valid paths, headers, numeric counters, errors, and ranges.
- Keep release publication separate from remediation integration; PR #50 remains the explicit v0.1.1 publication decision.

## Changes
- `internal/oci/` — isolated off-origin upload requests from repository auth and ambient cookies; added the default-deny external destination boundary, Jar-free token path, structural public errors, and caller-owned escape hatch.
- `internal/manifest/` — replaced broad OCI manifest materialization with bounded wire decoding and allocation regressions for large layers, duplicate members, descriptor URLs, and annotations.
- `internal/file/` — rejects foreign-owned or permissive existing partials before network access and hardens the relationship between the validated path and opened file.
- `internal/transfer/` — makes resume verification context-aware so cancellation interrupts hashing large or sparse partials.
- `cli/` — structurally redacts challenges, peer headers, token and distribution identifiers, Location-derived capabilities, transport errors, pull byte counts, summaries, and ranges; escapes terminal controls and invalid text while preserving classification and exit behavior.
- `.github/workflows/conformance.yml`, CLI documentation, design/reference documentation, and consequence-level tests — updated to exercise and explain the new boundaries.

## Open Threads
- [PR #50](https://github.com/componere/bigoci/pull/50) is open and mergeable to publish v0.1.1 with the post-v0.1.0 registry-authority and CLI-output fixes. It was intentionally not merged as part of remediation closeout.

## References
- [PR #41 — isolate off-origin upload sessions](https://github.com/componere/bigoci/pull/41)
- [PR #42 — make resume verification cancellable](https://github.com/componere/bigoci/pull/42)
- [PR #43 — reject unsafe pull partials](https://github.com/componere/bigoci/pull/43)
- [PR #46 — bound manifest decoding allocations](https://github.com/componere/bigoci/pull/46)
- [PR #49 — constrain registry-selected authority](https://github.com/componere/bigoci/pull/49)
- [PR #51 — redact peer-controlled CLI output](https://github.com/componere/bigoci/pull/51)
- [Live GHCR conformance run](https://github.com/componere/bigoci/actions/runs/31353198494)
- Full private evidence: `/Users/josh/.codex/visualizations/2026/08/09/019fe8ce-613c-7b00-b7f4-722730668899/bigoci-security-audit.md`
- Prior release context: `.journal/009/SUMMARY.md`

## Lessons
- Adversarial review must attack the guarantee, not the current payload shape: several green prototypes were bypassed through duplicate JSON members, nested descriptor fields, DNS resolution, malformed redirects, response reflection, and numeric progress fields.
- A resource limit must run before materialization and cover every consumed nested collection; bounding one visible array does not bound allocation.
- If an authenticated peer can choose a high-domain printable value, it can copy a reusable credential into that value. Credential-safe diagnostics therefore require structural markers, not a growing list of secret-looking patterns.
- URL validation alone cannot prove destination safety when DNS, proxies, custom dialers, or registered protocols control the actual peer; enforcement belongs at the concrete outbound connection or with the explicitly responsible caller.
