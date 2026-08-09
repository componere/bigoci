---
id: 010
title: Fresh work session
started: 2026-08-09
---

## 2026-08-09 16:16 — Kickoff
Goal for the session: Start a fresh journal session; the substantive work request has not yet been provided.
Current state of the world: `master` is clean at `3b82229`; phases 1–6 are complete, and the latest closed-session context leaves phase 7 as the remaining planned documentation and release work.
Plan: Wait for the user's actual request, inspect its exact target, work iteratively, and keep this session log current at meaningful checkpoints.

## 2026-08-09 16:19 — Security audit scoped
Goal for the session: Independently audit the `bigoci` package for security defects on `master` at `3b82229dd7938278eac6a197e5d1abf822382cef`.
Evidence rule: Report a finding only when an attacker-controlled path is reproducibly exploitable in the current code and produces a concrete consequence consistent with how `bigoci` is used; theoretical or best-practice-only concerns are not findings.
Scope and plan: Map the public-library trust boundaries and shipped CLI exposure, inspect the network/authentication, manifest/transfer/filesystem, error/logging, and dependency surfaces in parallel, run baseline checks, and independently reproduce every candidate before reporting it.

## 2026-08-09 17:18 — Security audit completed
Outcome: Six root findings survived public-API or shipped-CLI exploitation and consequence validation: off-origin upload sessions can cross the registry authentication boundary; Bearer realms permit private-IP SSRF and ambient Cookie Jar authority; a 4.3 KiB gzip manifest drove roughly 560 MiB RSS before the part-count guard; an attacker-owned regular partial in a shared non-sticky directory captured private artifact bytes across UIDs; a malicious sparse partial made resume hashing ignore cancellation for 24.6 seconds at 64 GiB; and CLI debug/error output leaked a redeemable realm query or accepted forged records/control bytes.
Strongest proof: A public zero-byte `Client.Push` replayed a live repository push bearer to the off-origin storage host, sent the Docker PAT to the storage-selected realm, and still returned success. The nonempty/default push path separately proved challenge-state poisoning and PAT exfiltration without direct initial Authorization forwarding.
Filesystem proof: In an isolated Linux container, uid 1002 pre-created a mode-0666 partial, uid 1001 completed the pull successfully, and uid 1002 read `private registry content` from the attacker-owned final file. The tested `1777` sticky-directory configuration was blocked by Linux `fs.protected_regular=2` and is not claimed vulnerable.
Verification: The 23-task `root:check` graph passed after clearing a stale lint cache; root, CLI, and benchmark module integrity checks passed; govulncheck and gosec results were manually reachability-reviewed; focused TLS proofs passed together; and transfer proofs were rerun on Darwin ARM64 and Linux ARM64. Scanner-only ORAS, Go ECH, `os.Root`, variable-path, and integer-conversion candidates were rejected when their vulnerable paths or consequences were not reachable.
Current target note: `origin/master` advanced during the audit to `54c5730d067cb28b02a49ac87122516c5d1d124e`, but `git diff 3b82229..54c5730 -- '*.go'` is empty. Proofs remain pinned to `3b82229`; the affected Go source is unchanged on the new remote head.
Artifact: The full evidence, prerequisites, remediation directions, rejected-candidate list, and verification record are in `/Users/josh/.codex/visualizations/2026/08/09/019fe8ce-613c-7b00-b7f4-722730668899/bigoci-security-audit.md`. No product code was changed.
