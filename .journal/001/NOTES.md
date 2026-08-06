---
id: 001
title: First session in freshly set-up workspace
started: 2026-08-05
---

## 2026-08-05 22:26 — Kickoff
Goal for the session: not yet stated — the developer opened the session immediately after running session-setup and has not made a request yet.
Current state of the world: fresh workspace on `master` at 8ea58b5 (clean). Journal branch `journal/jmgilman` was just created from `origin/master`, scaffolded (`INDEX.md`, `SKILLS.md`, `TECH_NOTES.md`), and pushed. No prior sessions exist.
Plan: wait for the developer's actual request, then record it here and proceed.

## 2026-08-05 22:28 — Goal recorded
Goal: port the latest template-go commit (2dc7b01, "docs(agents): add categorized Go best practices rules (#43)") into this repo on a new branch. It postdates the template clone.
Findings: the commit (1) appends a categorized Go Best Practices section to AGENTS.md, (2) removes `.claude/` from .gitignore, (3) tracks a `.claude/skills -> ../.agents/skills` symlink. Divergence: bigoci currently tracks `.claude` itself as a symlink to `.agents`, so applying part (3) means replacing that symlink with a real `.claude/` dir containing the `skills` symlink, matching the template layout.
Plan: create implementation worktree `docs/agents-go-best-practices` from origin/master, apply the AGENTS.md/.gitignore hunks from the template commit, restructure `.claude`, commit and push.

## 2026-08-05 22:32 — Template commit ported
Done: created worktree/branch `docs/agents-go-best-practices` from origin/master; applied AGENTS.md and .gitignore hunks straight from template commit 2dc7b01 via `git show -- | git apply`; replaced the tracked `.claude -> .agents` symlink with a real `.claude/` dir containing `skills -> ../.agents/skills` to match the template. Verified AGENTS.md byte-identical to template and symlink target identical. Committed as 4ced85a and pushed; `git ls-files .journal` clean on the branch.
Next: open a PR when the developer wants to integrate (squash merge; PR title = commit subject).

## 2026-08-05 23:11 — PR merged and cleaned up
Done: opened PR #7 and squash-merged it (merge commit 16938b6). Fast-forwarded local master to origin/master; `wt remove` cleaned up the `docs/agents-go-best-practices` worktree and branch. The Go Best Practices rules (cited as A1/D4/etc.) are now live in AGENTS.md on master.

## 2026-08-05 23:15 — New goal: initial design document
Goal: produce the initial design document for `bigoci` — a surgical Go library with one purpose: efficiently upload/download large files (>5GB, up to tens of GB) to/from OCI registries. No other use cases, ever.
Open questions to research first: (1) build on an existing OCI SDK (oras-go v2, go-containerregistry, regclient) vs narrow from-scratch implementation over the distribution spec; (2) auth strategy — reuse existing ecosystem (docker config, credential helpers, cloud-provider helpers) vs custom.
Next: research the current library landscape and registry protocol realities (chunked upload support, Range GET, S3 redirects), then deliver an assessment.

## 2026-08-05 23:24 — Research findings: SDK and auth landscape
Protocol facts that shape the design:
- OCI chunked upload is strictly sequential (out-of-order chunk => 416), so a single blob can never be pushed in parallel. Parallel upload requires splitting a file into multiple blobs — a manifest/format decision, not transport.
- Distribution spec v1.1 added resumable push (GET on session returns received range) and blob Range GET is in conformance tests; cloud registries redirect blob GETs to presigned object-storage URLs (S3/GCS/Azure) that support parallel Range reads well. Parallel pull of one blob is very feasible.
- Registry chunked-push support is inconsistent (GHCR historically broken for >1 chunk; monolithic streaming PUT is the universal path). Layer size limits vary: GHCR 10GB, ECR ~50GiB, ACR 200GiB.
Library facts:
- oras-go v2: monolithic push only; chunked push (issue #338) open since 2022, milestone "Future". Clean auth: registry/remote/auth + credentials (docker config, native stores, helpers).
- go-containerregistry: active (releases mid-2026), image-centric model; authn keychain is the de-facto auth standard with in-process cloud helper adapters (ECR/ACR/GCR) and reusable token-dance transport RoundTripper.
- regclient: closest in spirit (blob-level API, monolithic-with-chunked-fallback, retries, progress callbacks) but v0 API, effectively single-maintainer, no parallel range pull.
Assessment direction: own the transport (narrow distribution-spec v1.1 implementation — the value of bigoci lives exactly in the transport layer none of the SDKs deliver, per L1); reuse auth behind a small port, default adapter wrapping ggcr authn keychain + transport (oras-go v2 credentials is the lighter alternative).

## 2026-08-06 09:05 — Split-blob trade analysis
User raised the two split-blob costs: (1) OCI digest no longer matches the file digest, (2) owning a custom split/knit convention. Research: no prior art exists for manifest-level file splitting — modelpack/Docker model format use one-file-one-layer uncompressed; SOCI/Dragonfly/containerd parallel-pull work is all Range/P2P pull-side. Gains identified: parallel push (only spec-legal route; chunks are sequential), per-part retry/resume using only universally-supported monolithic PUTs (sidesteps flaky chunked/v1.1-resumable support), GHCR 10GB layer cap (single-blob fails the stated tens-of-GB scope there), parallel pull without Range dependence + per-part digest verification, deterministic split gives free dedup/skip on re-push. Mitigations: whole-file digest carried as manifest annotation + verified during knit (end-to-end integrity preserved; only registry-level addressability lost); format = fixed-size parts ordered as manifest layers, reconstruction = concatenation, versioned media type — manifest IS the knit plan, no side index. Part-count-1 degenerate case makes single-blob a knob, not a fork. Verdict: gains decisively offset costs; risk moves from runtime protocol variance to a controlled tiny format.

## 2026-08-06 09:40 — Design doc drafted, review passes in flight
Drafted docs/docs/explanation/design.md on branch docs/initial-design (worktree .wt/docs-initial-design, draft commit 61522bf) plus mkdocs nav entry. Doc decides: split-blob format (fixed 512MiB parts as ordered layers, manifest-is-the-format, part-count-1 degenerate case), own transport (7 distribution endpoints), auth port defaulting to ggcr authn/transport, hash-pass-before-upload (monolithic PUT needs digest upfront; digest cache keyed path/size/mtime), pull via WriteAt + sidecar resume + atomic rename, per-part verification as the integrity model (whole-file check optional), defaults table with reasoning, hexagonal core/ports/adapters, 3-layer testing + benchmark harness. Open questions kept to two: adaptive concurrency, CDC chunking. Three Opus review agents dispatched in parallel: technical fact-check (web-grounded), constraints compliance (AGENTS.md/TECH_NOTES/mission), plain-language + Diátaxis + streamlining. Next: merge findings, revise, final check, PR.
