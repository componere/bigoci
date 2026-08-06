---
title: Design
description: Why bigoci works the way it does.
---

# About the design

bigoci is a Go library that uploads and downloads large files to and from OCI
registries. Large means 5 GB and up, into the tens of GB. That is the whole
library. This page explains the design and the reasoning behind it.

## Scope

One capability: move a single large file to a registry (push) and back (pull),
as fast and as reliably as the registry allows.

Non-goals, permanently:

- Container images or generic OCI artifacts. Use [oras](https://oras.land) or
  [go-containerregistry](https://github.com/google/go-containerregistry).
- Multiple files per artifact. One artifact holds one file. Callers who need
  bundles push one artifact per file.
- Compression or encryption. Files at this size are usually already compressed
  (model weights, disk images, archives). Callers who want either apply it
  before push.
- Registry management: tag listing, deletion, garbage collection, replication.

A fixed scope lets the library optimize one data path without compromise.

## Protocol facts that shape the design

Five facts about the [OCI distribution
spec](https://github.com/opencontainers/distribution-spec) and real registries
drive every decision below.

1. **Chunked upload is sequential.** The spec requires chunks in order; a
   registry answers an out-of-order chunk with 416. A single blob can never be
   pushed over parallel connections.
2. **Monolithic push works everywhere; the alternatives do not.** Chunked
   upload is inconsistently implemented (GHCR rejects uploads with more than
   one non-empty chunk). Resumable push arrived in spec v1.1 and adoption is
   thin. The one push primitive every registry handles correctly is a
   single-request blob upload.
3. **Registries cap layer size.** GHCR: 10 GB. ECR: ~50 GiB. ACR: 200 GiB. A
   file stored as one blob stops working at these limits.
4. **Parallel range reads work well on pull.** Major registries redirect blob
   GETs to object storage (S3, GCS, Azure Blob), which serves concurrent
   `Range` requests at high throughput. But `Range` support is not guaranteed
   by the spec, so a design may exploit it, not depend on it.
5. **Registries deduplicate blobs by digest** and support cross-repository
   mounts. Identical content uploads once.

## Decision: store files as ordered parts

A pushed file is split into fixed-size parts. Each part is one OCI blob. The
manifest lists the parts in order as layers. Reconstruction is concatenation:
read the layers in manifest order, write the bytes. There is no separate index
or chunk table. The manifest is the whole format.

This is the load-bearing decision, so here is the full trade:

**What it buys:**

- **Parallel push.** Fact 1 forbids parallelizing one blob. N parts push over
  N connections. This is the only spec-legal route to fast uploads.
- **Retry and resume from the universal primitive.** Each part is a small
  monolithic push (fact 2). A failure costs one part, not the whole transfer.
  bigoci never touches chunked or resumable upload, so registry-specific
  breakage in those paths cannot affect it.
- **Fits under every layer cap.** 512 MiB parts sit far below the 10 GB GHCR
  limit (fact 3).
- **Parallel pull everywhere.** Parts are independent blob GETs. No `Range`
  dependence (fact 4). Each part verifies against its own digest during
  download, so a corrupt part re-fetches alone.
- **Free incremental behavior.** The split is deterministic: same bytes, same
  parts, same digests. Re-pushing a file the registry already has reduces to
  existence checks (fact 5). Pushing a file whose tail changed uploads only
  the changed parts.

**What it costs:**

- **The registry-visible digests are part digests, not the file digest.**
  Integrity is unaffected: the manifest carries the whole-file digest, and
  bigoci verifies content on every pull (see
  [Verification](#verification)). What is lost is addressability. You cannot
  ask a registry "do you have blob `sha256:<file digest>`", and generic tools
  such as `oras pull` return parts, not the file.
- **bigoci owns a format.** The mitigation is to keep it nearly nothing: the
  split rule fits in one sentence, reconstruction is concatenation, and the
  media types carry a version for migration. No established convention exists
  to adopt instead. The model-distribution ecosystem (ModelPack, Docker model
  packaging) stores one file as one layer, and the parallel-transfer work
  around it (SOCI, Dragonfly, containerd parallel pull) is pull-side only.

A file at or below the part size produces one part, and that part's digest is
the file digest. Callers who value addressability over speed can raise the
part size to get single-blob behavior on registries whose caps allow it. The
tradeoff is a per-push knob, not a fork in the format.

## Decision: own the transport

bigoci implements the distribution-spec endpoints it needs directly over
`net/http`. It does not build on an existing OCI SDK for the data path.

The reason: the value of bigoci lives in the transport. Parallel part
transfers, per-part retry with backoff, redirect handling, and strict
streaming (no buffering a part in memory) all require owning the HTTP layer.
No existing SDK provides them:

- **oras-go v2** pushes blobs monolithically in one stream; chunked push has
  been an open issue since 2022.
- **go-containerregistry** models everything as images and layers; raw-file
  blob transfer fights its abstractions.
- **regclient** has the closest blob API but is v0 (breaking changes allowed),
  and none of the three parallelizes a single file's transfer.

The needed protocol surface is small and stable:

| Operation | Endpoint |
|---|---|
| Check blob exists | `HEAD /v2/<name>/blobs/<digest>` |
| Read blob | `GET /v2/<name>/blobs/<digest>` |
| Open upload session | `POST /v2/<name>/blobs/uploads/` |
| Complete upload | `PUT <session>?digest=<digest>` |
| Cross-repo mount | `POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<repo>` |
| Read manifest | `GET /v2/<name>/manifests/<ref>` |
| Write manifest | `PUT /v2/<name>/manifests/<ref>` |

Writing this ourselves follows L1: a small piece we control beats importing a
large SDK to use a sliver of it, in the one place we would also have to fight
it.

## Decision: reuse the auth ecosystem behind a port

Authentication is where from-scratch would be waste. The interop point that
matters is Docker's config file and credential-helper protocol. Hooking into
it means `docker login`, cloud credential helpers (ECR, ACR, Artifact
Registry), and CI registry setups work with zero per-provider code in bigoci.

The core defines one port: given a registry and a scope, return an
authenticated `http.RoundTripper`. The default adapter wraps
go-containerregistry's `authn` keychain and `transport` packages, which
resolve Docker config, run the token exchange, and cache tokens. They are the
most widely deployed implementation of this flow (crane, ko, cosign). An
alternative adapter can wrap oras-go v2's `credentials` package if the
dependency weight ever matters; the port makes the swap invisible to the
core.

## The artifact format

The manifest is a standard OCI image manifest (spec v1.1) so every registry
accepts it.

- `artifactType`: `application/vnd.bigoci.file.v1`
- `config`: the OCI empty descriptor (`application/vnd.oci.empty.v1+json`,
  2 bytes, `{}`)
- `layers`: the parts, in file order, media type
  `application/vnd.bigoci.file.part.v1`
- Manifest annotations:
    - `io.bigoci.file.digest`: sha256 of the complete file
    - `io.bigoci.file.size`: file size in bytes
    - `io.bigoci.part.size`: the part size used by the split
    - `org.opencontainers.image.title`: the file name
- Layer annotations: none. A part's position is its index in `layers`; its
  digest and size are in its descriptor.

Parts are raw byte ranges of the file. No compression, no tar, no framing.
The `.v1` in the media types is the format version; a future format change
means new media types, and readers keep accepting old ones.

Pull accepts only bigoci manifests (matched by `artifactType`). Pulling
arbitrary OCI artifacts is out of scope.

## Push path

1. Stat the file; compute the split plan from file size and part size. The
   plan is pure arithmetic: part `i` covers bytes `[i*P, min((i+1)*P, size))`.
2. Workers take parts from the plan. Each worker:
    1. `HEAD` the part digest; skip if present (dedup, and free push resume).
    2. Try a cross-repo mount when a source repo is configured.
    3. Otherwise open a session and `PUT` the part in one request, streaming
       from the file via `io.NewSectionReader`. Nothing is buffered.
3. Part digests and the whole-file digest are computed in the same pass, once
   per file, streaming. Digests are cached keyed by (path, size, mtime) so a
   re-push of an unchanged file skips the hashing pass.
4. When all parts exist, `PUT` the manifest. The manifest goes last: a
   manifest never references a blob the registry does not have, so a killed
   push never leaves a broken artifact, only unreferenced blobs the registry
   garbage-collects.

Failed part uploads retry with exponential backoff and jitter (E3). Retrying
a part re-streams it from disk; the file is the transfer buffer, which is why
push requires a file and not a stream (P2). A push interrupted and rerun
resumes for free: completed parts answer the `HEAD` in step 2.1.

## Pull path

1. Resolve the reference, fetch the manifest, check `artifactType`, read the
   annotations.
2. Create `<dest>.bigoci-partial`, sized to the full file with `Truncate`.
3. Workers `GET` parts concurrently, each writing to its own byte range via
   `WriteAt` and hashing as it streams. A part whose hash does not match its
   descriptor digest is re-fetched. A transfer error retries from the failed
   byte offset with a `Range` request when the registry honors it, and
   re-fetches the whole part when it does not (fact 4).
4. A sidecar file `<dest>.bigoci-partial.json` records completed part digests.
   Rerunning an interrupted pull re-hashes the completed ranges (sha256 runs
   at multiple GB/s per core) and downloads only what is missing.
5. When all parts verify, remove the sidecar and rename the partial file onto
   `<dest>`. The destination only ever exists as a complete, verified file.

### Verification

Every pulled byte is hashed exactly once, against the part digest in the
manifest. This chain is sufficient: the caller names a manifest digest (or a
tag that resolves to one), the manifest is fetched and its content verified
against that digest, and the manifest names every part digest. Trusting the
manifest digest is the same trust model as pulling a container image.

The whole-file digest annotation exists for humans and third-party tools: it
lets anyone confirm what file an artifact carries without bigoci. Verifying
it during pull would require a second sequential read of the assembled file,
so it is off by default and available as an option for callers who want a
second, bigoci-independent check.

## Defaults

| Setting | Default | Reasoning |
|---|---|---|
| Part size | 512 MiB | Small enough that a 5 GB file splits into 10 parts (enough to feed 4–8 connections) and a lost part costs seconds to retry; large enough that per-part overhead (3 requests) is noise: a 50 GB file makes ~300 requests, trivial next to the transfer time. 20× under the lowest registry layer cap. |
| Concurrency | 4 workers | One HTTPS stream to a cloud registry sustains roughly 50–100 MB/s; 4 streams saturate a 2–3 Gbit/s path. Configurable for bigger pipes. |
| Retries per part | 4, exponential backoff with jitter | A transient failure should never surface to the caller (E3); four attempts spans ~30 s of outage. |
| Digest algorithm | sha256 | The OCI default; universally supported. |

Part size and concurrency are per-push/per-pull options. The part size knob
is the format's only parameter, and it is recorded in the manifest, so pull
never needs to guess it.

## Architecture

Hexagonal (A1). The core is pure logic and I/O lives behind ports.

**Core (no I/O):** the split planner, manifest encoding and decoding, the
transfer orchestrator (worker scheduling, retry policy, progress accounting),
and verification bookkeeping. All of it unit-tests without a network or disk.

**Ports (interfaces defined by the core):**

- `Registry`: the seven distribution-spec operations above.
- `Auth`: registry + scope → authenticated `http.RoundTripper`.
- `Source` / `Sink`: random-access file read (`io.ReaderAt`) and write
  (`io.WriterAt` + truncate + atomic rename).
- `State`: pull-resume sidecar persistence.

**Adapters (one purpose each, A2):** the `net/http` distribution client, the
go-containerregistry `authn` wrapper, the OS filesystem, and the JSON sidecar
store. Every adapter gets a mockery-generated mock in a `mocks/` subpackage
(T2, T3).

One deliberate sharp edge lives in the transport adapter: when a blob GET
redirects to presigned object storage, the redirect must be followed
*without* the `Authorization` header, or S3-style endpoints reject the
request. The transport disables automatic redirects for blob GETs and
re-issues the request itself with a clean client.

**Public API (A3, thin):**

```go
client, err := bigoci.New(opts ...bigoci.Option)

desc, err := client.Push(ctx, ref, bigoci.FromFile(path), opts ...bigoci.PushOption)
err = client.Pull(ctx, ref, bigoci.ToFile(path), opts ...bigoci.PullOption)
```

Progress reporting is an option accepting a callback; domain terms get types
(`Reference`, `Digest`, `PartSize`) per I1. Sentinel errors cover the cases
callers branch on (E1): `ErrNotFound`, `ErrNotBigOCI` (manifest is not ours),
`ErrDigestMismatch`, `ErrPartTooLarge` (part size exceeds a registry cap).

## Testing

The three layers from T1, plus measurement:

- **Unit:** split-plan arithmetic, manifest round-trips, retry/backoff
  decisions, resume bookkeeping. Pure functions, table-driven.
- **Integration:** orchestrator against mocked ports. Failure injection lives
  here: dropped connections mid-part, out-of-order completions, digest
  mismatches, mount rejections.
- **End-to-end:** testcontainers running CNCF Distribution and zot. Push and
  pull real multi-gigabyte files; kill transfers mid-flight and resume them.
  These tests gate every feature: a feature is done when it works against a
  real registry, not when its unit tests pass.
- **Benchmarks:** a harness that measures throughput against a local registry
  under varied part size and concurrency, so the defaults above stay grounded
  in data as the implementation evolves.

Cloud registries (GHCR, ECR, ACR, Artifact Registry, Docker Hub, Harbor) get
a manually triggered conformance job rather than per-commit CI, since they
need credentials and cost money to exercise at size.

## Open questions

Questions with a data-backed answer were answered above. These two remain
genuinely open:

1. **Should concurrency self-tune?** A fixed worker count wastes fast pipes
   and can oversubscribe slow ones, but adaptive concurrency (ramp while
   throughput grows, back off on 429/503) risks fighting registry rate
   limiters in ways that are hard to test honestly. Needs the benchmark
   harness and real-registry data before deciding. Until then: fixed default,
   caller-configurable.
2. **Content-defined chunking for mutating files.** Fixed-size parts make
   any insertion or deletion re-upload every later part. CDC (rolling-hash
   boundaries) would make dedup survive edits, at the cost of a genuinely
   more complex format and losing the "split rule fits in one sentence"
   property. Worth revisiting only if real users demonstrate an
   edit-heavy workflow; the media-type version gives it a migration path.
