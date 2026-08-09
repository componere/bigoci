---
title: Design
description: Why bigoci works the way it does.
---

# About the design

bigoci is a Go library that uploads and downloads large files to and from OCI
registries. Large means 5 GB and up, into the tens of GB. That is the whole
library.

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
- A database of per-registry capabilities or limits. bigoci reacts to what a
  registry answers and never ships a table of vendor behaviors that would go
  stale.
- Content-defined chunking. Fixed-size parts make an insertion re-upload every
  later part; rolling-hash boundaries would fix that at the cost of a far more
  complex format. Revisit only if an edit-heavy workflow appears in practice.
  The format version gives it a migration path.

A fixed scope lets the library tune one data path.

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
4. **Parallel range reads work on pull.** Major registries redirect blob GETs
   to object storage (S3, GCS, Azure Blob), which serves concurrent `Range`
   requests. The spec does not guarantee `Range` support, so the design may
   exploit it, not depend on it.
5. **Registries deduplicate blobs by digest.** A blob the registry already
   holds needs no upload.

## Decision: store files as ordered parts

A pushed file is split into fixed-size parts. Each part is one OCI blob. The
manifest lists the parts in order as layers. Reconstruction is concatenation:
read the layers in manifest order, write the bytes. There is no separate index
or part table. The manifest is the whole format.

This is the load-bearing decision.

**What it buys:**

- **Parallel push.** Fact 1 forbids parallelizing one blob. N parts push over
  N connections. This is the only spec-legal route to fast uploads.
- **Retry and resume from the universal primitive.** Each part is a small
  monolithic push (fact 2). A failure costs one part, not the whole transfer.
  bigoci never touches chunked or resumable upload, so registry-specific
  breakage in those paths cannot affect it.
- **Fits under every layer cap.** 512 MiB parts sit roughly 19× under the
  lowest limit (fact 3).
- **Parallel pull everywhere.** Parts are independent blob GETs. No `Range`
  dependence (fact 4). Each part verifies against its own digest during
  download, so a corrupt part re-fetches alone.
- **Free incremental behavior.** The split is deterministic: same bytes, same
  parts, same digests. Re-pushing a file the registry already holds reduces to
  existence checks (fact 5). Pushing a file whose tail changed uploads only
  the changed parts.

**What it costs:**

- **The registry-visible digests are part digests, not the file digest.**
  Integrity is unaffected: the manifest carries the whole-file digest, and
  bigoci verifies content on every pull (see
  [Verification](#verification)). What is lost is addressability. You cannot
  ask a registry whether it holds blob `sha256:<file digest>`. Generic tools
  such as `oras pull` return parts, not the file.
- **bigoci owns a format.** The mitigation is to keep it nearly nothing: the
  split rule fits in one sentence, reconstruction is concatenation, and the
  media types carry a version for migration. No established convention exists
  to adopt instead. The model-distribution ecosystem (ModelPack, Docker model
  packaging) stores one file as one layer. The parallel-transfer work around
  it (SOCI, Dragonfly, containerd parallel pull) is pull-side only.

A file at or below the part size produces one part, and that part's digest is
the file digest. Callers who value addressability over speed can raise the
part size to get single-blob behavior on registries whose caps allow it.

The format contract — media types, annotations, manifest layout, limits —
lives in the [format reference](../reference/format.md).

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
- **regclient** comes closest: a blob-level API with chunked-upload fallback
  and retries. But it is v0 (breaking changes allowed) and a full registry
  client, of which bigoci would use a sliver.

None of the three parallelizes a single file's transfer.

The needed protocol surface is six endpoints:

| Operation | Endpoint |
|---|---|
| Check blob exists | `HEAD /v2/<name>/blobs/<digest>` |
| Read blob | `GET /v2/<name>/blobs/<digest>` |
| Open upload session | `POST /v2/<name>/blobs/uploads/` |
| Complete upload | `PUT <session>?digest=<digest>` |
| Read manifest | `GET /v2/<name>/manifests/<ref>` |
| Write manifest | `PUT /v2/<name>/manifests/<ref>` |

A small piece bigoci controls beats importing a large SDK to use a sliver of
it, in the one place bigoci would also have to fight it.

References are parsed with
[`github.com/distribution/reference`](https://github.com/distribution/reference),
the canonical implementation of the `registry/repo[:tag][@digest]` grammar.
Parsing is fiddly and interop-critical, and not where bigoci adds value.

A reference may be tagless. Push accepts a digest-only reference and writes
the manifest by digest, so an artifact can exist untagged; pull accepts one
and verifies against it. This lets callers compose bigoci artifacts into
larger structures — a standard OCI image index that references several
artifacts under one tag, for example — without bigoci knowing about the
composition.

## Decision: borrow the credential store, own the token dance

Authentication splits into two halves, and they have opposite answers.

**Reading credentials is interop, so it is borrowed.** The point that matters
is Docker's configuration file and the credential-helper protocol. Hooking
into it means `docker login`, cloud credential helpers (ECR, ACR, Artifact
Registry), and CI registry setups work with no per-provider code in bigoci.
oras-go v2's `credentials` package does exactly that — the `auths` keys, the
base64 `auth` field, the helper protocol, Docker Hub's odd server address —
and it costs one new module whose own requirements bigoci already has. That
package is the whole of what `internal/auth` wraps.

**Performing the exchange is transport, so bigoci owns it.** oras-go's
`auth.Client` refreshes a credential by *resending the request that failed*,
and its token cache has no notion of expiry precisely because the resend is
its refresh strategy. bigoci's largest request is a blob `PUT` streaming a
section of a file, whose body cannot be produced twice — the standard library
says so, by leaving `GetBody` nil — so a client that recovers by resending
cannot be the one sending it. Authentication is therefore treated as a
**pre-condition of a request, not a recovery from one**: what a request must
carry is worked out before it is built, the header is stamped as it is built,
and everything goes out through the caller's own client, token exchanges
included, so a caller watching that client is never blind to a request bigoci
made.

The port is one method wide: given a registry, return the credential to
present. It deliberately does not return a `RoundTripper`. A transport-shaped
port would replace the caller's client (or hide requests from it), would stamp
`Authorization` on every request that passed through it — including one
re-issued to signed object storage, where the credential does not belong — and
could not express either of the two things the dance actually needs to say:
"resolve this before a body that can only be sent once goes out", and "the
credential you presented was refused".

go-containerregistry's `authn` keychain stays the documented alternative for
callers who want in-process cloud keychains, through `WithHTTPClient`, with the
leak warning the [authentication how-to](../how-to/authenticate.md) spells out.

## Push path

1. bigoci stats the file and computes the split plan from file size and part
   size. The plan is pure arithmetic: part `i` covers bytes
   `[i*P, min((i+1)*P, size))`.
2. One sequential read hashes the file and emits each part digest as the read
   crosses a part boundary. Hashing pipelines into uploading: a part's upload
   starts as soon as its digest is known, while the read continues. The
   whole-file digest falls out of the same read, ready before the manifest
   needs it. With hardware SHA-256 (most current CPUs; absent mainly on
   pre-Ice Lake Intel Xeons) hashing runs at 1.8–3.4 GB/s per core; without
   it, ~0.5 GB/s, which still outruns most upload links. The pipeline hides
   the hash cost either way.
3. Workers drain the plan. For each part: `HEAD` the part digest and skip it
   when present (dedup, and free resume of an interrupted push). Otherwise
   open an upload session and `PUT` the part in one request, streamed from
   the file with `io.NewSectionReader`. Nothing buffers in memory.
4. When all parts exist, bigoci makes sure the 2-byte empty config blob
   exists (a manifest that references a blob the registry does not hold is
   rejected), then writes the manifest. The manifest goes last, so a killed
   push leaves no broken artifact, only unreferenced blobs the registry
   garbage-collects.

Failed part uploads retry per the policy in [Defaults](#defaults). Retrying a
part re-streams it from disk; the file is the transfer buffer, which is why
push takes a file path and not a stream. A rerun of an interrupted push
re-hashes the file and skips every part the `HEAD` in step 3 finds. A digest
cache could skip the re-hash; it is deferred until profiling shows the need.

## Pull path

1. bigoci resolves the reference, fetches the manifest, checks the
   `artifactType`, and reads the annotations.
2. It opens `<dest>.bigoci-partial` and measures what is already there, then
   sizes the file to the full length with `Truncate`. The measurement has to
   come first, because the truncate is what destroys the evidence. A partial
   already at the right length makes the pull a resume: bigoci hashes every
   part range (bytes never written are zeros and fail their check) and fetches
   only the parts that do not match their digest. A partial of any other
   length belongs to some other artifact, so it is cut or grown to fit and
   every part is fetched.
3. Workers `GET` parts concurrently. Each streams into its own byte range via
   `WriteAt`, hashing as it writes. A part whose hash does not match its
   descriptor digest fails the pull: the registry served content the manifest
   does not describe, and asking for it again returns the same bytes. A part
   whose transfer breaks part way through is asked for again from the byte it
   reached, with a `Range` request, and the hash of the part carries on across
   the attempts — so a stream that died near the end of a part costs the bytes
   that never arrived and no more. Serving a range is optional in the spec, so
   a registry may answer with the whole blob instead; the blob port reports
   that the stream starts at byte zero, and the same attempt consumes it as a
   fetch from the beginning, writing over what it already held. That costs no
   extra request and no extra attempt.
4. When every part verifies, bigoci renames the partial file onto `<dest>`.
   The destination only ever exists as a complete, verified file.

### What a resume proves

Resume rests on hashing the bytes on disk, never on a memory of what an
earlier run did. There is no progress file, no journal, and no saved hash
state. That is what makes it safe to interrupt a pull at any instant.

Take the worst interruption, a `SIGKILL`, and walk the windows it can land in.
Before the truncate: a zero-length partial, so everything is fetched. Between
the truncate and the first write: a full-length file of zeros, so every range
fails its check and everything is fetched. In the middle of a write: a write
that completed cannot tear, and one that did not leaves that range failing its
check. After the last write but before the rename: every range verifies, and
the rerun commits without a single blob request. Between the rename and the
directory flush: the destination is complete and the partial is gone, so a
rerun starts over — wasteful, correct.

Two facts underneath that are worth naming. A killed process does not lose its
dirty pages: the kernel still owns them and writes them out, which is why the
partial file needs no per-part flush and the sink flushes once, at commit. A
machine that loses power is different — the partial may then hold ranges that
are half old and half new — and the resume catches those too, because it
hashes the ranges rather than counting them.

The cost is a full hash pass over whatever is on disk before the first byte
moves, at the pull's own worker count, overlapping the fetches of the parts
that failed. A pull that died at the first part therefore pays a read of a
file that holds nothing. Any way of avoiding that is a record of what an
earlier run did, which is exactly what resume is built not to need.

### Verification

Every pulled byte is hashed once per attempt, against the part digest in the
manifest. This chain is sufficient. The caller names a manifest digest, or a
tag that resolves to one. bigoci fetches the manifest and verifies it against
that digest. The manifest names every part digest. Trusting the manifest
digest is the same trust model as pulling a container image.

The whole-file digest annotation exists for humans and third-party tools: it
lets anyone confirm what file an artifact carries without bigoci. Verifying
it during pull would require a second sequential read of the assembled file,
so it is off by default. Callers who want the independent check can turn it
on.

## Authentication

A registry states its requirement by refusing a request, so bigoci waits to be
asked. The first request of a transfer goes out carrying nothing; a registry
that never challenges is never authenticated against, costs no extra request,
and produces exactly the traffic it did before bigoci learned any of this. That
inertness is a gate, not a hope: the end-to-end suites assert the request counts
of an unchallenged transfer to the number.

A challenge is answered before the next request is built. bigoci resolves the
credential for the host it dialed — never for the name the challenge offers in
its `service` parameter, which is the registry choosing which secret leaves the
machine — asks the token endpoint the challenge named, and stamps the result on
requests as they are built. Token exchanges ride the caller's own client, like
every other request.

**Scope is a function of the method:** `pull` for `GET` and `HEAD`,
`pull,push` for everything else. That holds a repository's token cache to two
entries and keeps a pull from ever asking for write access — an anonymous
request for `pull,push` is refused outright at some registries, which would
break every anonymous pull. A challenge that names its own scope widens what
the exchange asks for; it does not change which entry the answer is filed
under.

### What a refusal is worth

The rule underneath the table: **a refusal is worth acting on only when acting
on it changes what the next request will present.** Otherwise it is terminal.
A credential that was itself a refresh and has never carried a successful
request has had its chance.

| Situation | What happens | Attempts spent |
|---|---|---|
| Wrong credential (the token endpoint refuses it) | terminal `ErrUnauthorized`, naming the access that was refused | 0 |
| Anonymous against a private repository | the anonymous token is refused in turn; terminal | 0 |
| Token expired, request replayable | refreshed and re-issued inside the same call | 0 |
| Token expired, request is a blob `PUT` | refreshed, and the part comes back worth repeating: the orchestrator re-streams it from disk | 1 of 4 |
| A 403 carrying a challenge | the same one refresh a 401 gets | ≤1 |
| A 403 carrying no challenge | terminal: a permission answer, or a firewall in front of the registry | 0 |
| A challenge bigoci cannot read or cannot answer | terminal, quoting what arrived | 0 |

Two consequences worth stating. A blob `PUT` that meets an expired token costs
the part one of its four attempts even though nothing was wrong — the
alternative is resending a body that has already been read off the disk, which
is the one thing the transport must never do. And a 403 from a proxy or a web
application firewall reports as unauthorized, which is admitted the same way a
413 answering a manifest write is admitted: sniffing bodies to tell them apart
would be a table of vendor behaviors in another shape.

Failures of the token endpoint itself classify through the ordinary table: a
429 or a 5xx there is transient with the same `Retry-After` floor a blob
request gets, and a 200 carrying no token is terminal and deliberately *not*
`ErrUnauthorized` — that is the registry misbehaving, not your credentials.

### Expiry is read off the monotonic clock

A token is used while `now - acquired < lifetime - margin`, where `lifetime` is
the `expires_in` the endpoint stated (60 seconds when it states none, which is
the spec's own default and what GHCR does) and the margin is 30 seconds, or
half of a shorter lifetime. The registry's `issued_at` is ignored: it is the
registry's wall clock, and it is the only place clock skew could enter.
`time.Since` reads Go's monotonic clock, so an NTP step cannot make a live
token look dead, and a registry that disagrees anyway answers 401 and gets one
refresh. Skew costs an extra exchange, never a transfer.

The margin rests on one assumption, stated because it is load-bearing: a
registry authorizes a request when it reads the headers, not when it finishes
reading the body. At a 512 MiB part size a single `PUT` outlives a 60-second
token on any real link, so nothing else could make the arithmetic work. The
manual conformance gate against a real registry is what de-risks it.

### What is not supported

An `identitytoken` in the configuration — the OAuth2 refresh token some logins
store in place of a password — is refused out loud rather than silently
downgraded to anonymous. Exchanging one is a named follow-up. A
`registrytoken` is presented verbatim, with no exchange.

## Defaults

Part size and worker count are starting points, not measured optima. The
benchmark harness (see [Testing](#testing)) sets the final defaults before
v1.

| Setting | Default | Reasoning |
|---|---|---|
| Part size | 512 MiB | Small enough that a 5 GB file splits into 10 parts and a lost part costs seconds to retry. Large enough that per-part overhead (3 requests) is noise: a 50 GB file makes roughly 300 requests. Roughly 19× under the lowest registry layer cap (GHCR, 10 GB). |
| Workers | 4 | One worker holds one HTTPS connection. AWS measures 85–90 MB/s per S3 connection; four saturate a 2–3 Gbit/s path. Configurable for bigger pipes. |
| Retry policy | 4 attempts; exponential backoff, 1 s base, 30 s cap, full jitter; honors `Retry-After` when sent | A transient failure should never surface to the caller. Network errors, 429, and 5xx retry; other 4xx fail fast. |
| Digest algorithm | sha256 | The OCI default; universally supported. |

Part size and worker count are per-push and per-pull options. Part size is
recorded in the manifest, so pull never guesses it.

### Retry policy

The table gives the numbers. Four things it cannot say:

- **The unit of work is not always a request.** A part's existence check and
  its upload are attempted together, so an upload whose bytes landed and whose
  answer was lost costs the next attempt one `HEAD` instead of the whole part.
  A pull's unit is a part's `GET`, the copy into place, and the digest check.
  A part continued after a break shares that one budget: the attempts are the
  part's, not each stream's, so a link that drops every few hundred bytes runs
  out of them rather than retrying forever. Hashing a part out of an existing
  partial file sits outside the budget entirely — it makes no request, and a
  local disk that will not read is terminal. The empty config blob and each
  manifest call get budgets of their own.
- **`Retry-After` is a floor, not a replacement.** A registry that names a
  wait is waited for at least that long, and never less than the growing
  jittered backoff would have taken anyway — a hint must not send every
  rate-limited worker back at the same instant. The 30 s cap bounds every
  wait, one a registry asked for included, so a header naming an hour parks a
  transfer for half a minute and no longer. Total time is bounded by the
  caller's context, which is the only bound that knows what the caller is
  willing to spend.
- **What is terminal.** Every 4xx but 429, a part that arrives whole and
  hashes wrong, a destination that will not take the bytes, a source that will
  not read, and a context that ended. So is any failure no layer recognized:
  bigoci does not guess that a failure nobody classified might be temporary,
  because repeating it turns an immediate answer into a slow one without
  making it better.
- **A broken part is continued, not re-done, where the registry allows it.** A
  stream that died after 100 MiB is asked for again from byte 100 MiB, and the
  part's hash carries on across the attempts. Serving a byte range is optional
  in the spec, so a registry may send the whole blob instead — that answer is
  consumed as a fetch from zero inside the same attempt, over the bytes
  already in place, at the cost of no extra request. The consequence to accept:
  against a registry that will not serve ranges, a part on a flaky link can
  spend its whole budget having moved more bytes than a clean fetch would
  have. An intermediary that caps how much of an open-ended range it returns
  has the same arithmetic — every capped answer ends cleanly short and costs
  an attempt, so a very large part behind one can run out of budget mid-tail.

## Architecture

Hexagonal: the core is pure logic, and I/O lives behind ports.

**Core (no I/O):** the split planner, manifest encoding and decoding, the
transfer orchestrator (worker scheduling, retry decisions, progress
accounting), and verification bookkeeping. All of it unit-tests without a
network or disk. The orchestrator sleeps through an injected sleep function,
so backoff tests run without a clock.

**Ports**, shaped by the streaming contract:

```go
// Blobs is the distribution-spec blob surface of one repository.
type Blobs interface {
    Exists(ctx context.Context, dgst Digest) (bool, error)
    // Get also reports the byte the returned stream starts at: either the
    // offset asked for, or 0 when the registry ignored the range and is
    // sending the whole blob. Reporting it instead of refusing that answer is
    // what lets the caller fall back to a fetch from the beginning without a
    // second request.
    Get(ctx context.Context, dgst Digest, offset int64) (io.ReadCloser, int64, error)
    Put(ctx context.Context, dgst Digest, size int64, r io.Reader) error
}

// Manifests is the distribution-spec manifest surface of one repository,
// bound at construction to one reference (a tag or a digest).
type Manifests interface {
    Get(ctx context.Context) ([]byte, Descriptor, error)
    Put(ctx context.Context, mediaType string, body []byte) (Digest, error)
}

// Credentials resolves what to present to one registry. A registry the
// resolver knows nothing about is the zero Credential and a nil error:
// anonymous is an answer, not a failure. It is declared by internal/oci,
// which is the only package that knows a credential exists.
type Credentials interface {
    Credential(ctx context.Context, registry Registry) (Credential, error)
}

// Credential mirrors what a Docker configuration file stores. Four fields
// rather than two: a native credential store returns an identity token in
// place of a password, and a pair of strings would collapse that to "no
// credential" and downgrade the transfer to anonymous.
type Credential struct {
    Username      string
    Password      string
    IdentityToken string // refused loudly; bigoci cannot exchange one
    RegistryToken string // presented verbatim, no exchange
}

// Source and Sink are the file ends of a transfer.
type Source interface {
    io.ReaderAt
    Size() int64
}

// Sink also reads: resume hashes the existing partial file's part ranges.
type Sink interface {
    io.ReaderAt
    io.WriterAt
    Size() (int64, error)
    Truncate(size int64) error
    Commit() error // atomic rename onto the destination
}
```

`Manifests` carries no reference parameter because the adapter is bound to
one reference when it is built. A transfer touches exactly one manifest, and
binding it at construction keeps reference grammar out of the core: nothing
behind the ports parses, validates, or renders
`registry/repository:tag@digest`.

**Adapters, one purpose each:** the `net/http` distribution client
(implements `Blobs` and `Manifests`), the oras-go credentials wrapper
(`Credentials`), and the OS filesystem (`Source` and `Sink`). Every adapter
gets a mockery-generated mock in a `mocks/` subpackage.

**Package layout:**

```
bigoci/              public API: Client, options, sentinel errors
├── internal/plan      split arithmetic
├── internal/manifest  format encode and decode
├── internal/transfer  orchestrator: workers, retries, progress
├── internal/retry     what is worth another attempt, and how long to wait
├── internal/oci       net/http distribution client (Blobs, Manifests), and
│                      the token dance behind the Credentials port
├── internal/auth      credential sources: the Docker configuration and its
│                      helpers (oras-go), and one credential passed in
└── internal/file      OS filesystem adapter (Source, Sink)
```

Only the root package is importable; the thin API below is the whole exported
surface.

**Transport sharp edges**, owned by `internal/oci`:

- Automatic redirect following is off for every request. The adapter derives
  two clients from the caller's by copying the struct — `http.Client` is four
  exported fields and no hidden state, so the caller's own client is never
  touched — and follows a redirect itself, up to three hops, for a `GET` or a
  `HEAD` and nothing else. Every hop is a fresh request carrying two headers,
  `Range` and `Accept` — plus the credential in the one same-origin case the
  next bullet names, and nothing else. The second client also drops the
  cookie jar.
- A blob `GET` that redirects to presigned object storage does not forward the
  `Authorization` header. This is a confidentiality requirement, not a
  compatibility one: the header is not rejected by the storage those
  registries use. GHCR's storage and Docker Hub's CloudFront both answer 200
  to a request that carries the registry's bearer token as well as the
  signature, which is measured behavior, not an assumption — **so a working
  pull is not evidence that nothing leaked.** The evidence is the request log:
  every off-registry line reads `auth=none`. The credential rides a hop only
  when the location is the registry itself — same scheme, same host, same
  port — which is stricter than the standard library's rule, and deliberately:
  Go forwards to any domain-or-subdomain target, so a CDN on a subdomain of
  the registry would be handed the token.
- Presigned redirect URLs expire. No redirect URL is ever stored: a location
  lives inside the one call that received it, and a later attempt re-requests
  the blob from the registry and follows the fresh redirect. Beyond the
  registry, `401`, `403`, `404`, and `410` are all worth another attempt for
  that reason, and none of them reports as `ErrUnauthorized` or `ErrNotFound`
  — an expired signature is neither a credential problem nor a missing
  artifact.
- No error carries a signed URL. Every failure is reported against the
  registry method and path the request started as, and where naming the far
  end helps, only its host appears.
- A `PUT` streamed from an `io.SectionReader` must set `Content-Length`
  explicitly. Go otherwise sends chunked transfer encoding, which some
  registries and proxies reject.

**Public API:**

```go
client, err := bigoci.New(opts ...bigoci.Option)

desc, err := client.Push(ctx, ref, bigoci.FromFile(path), opts ...bigoci.PushOption)
err = client.Pull(ctx, ref, bigoci.ToFile(path), opts ...bigoci.PullOption)
```

Progress reporting is an option accepting a callback. Domain terms get types:
`Reference`, `Digest`, `PartSize`. Sentinel errors cover the cases callers
branch on: not found, unauthorized, digest mismatch, a manifest that is not a
bigoci artifact, and a part `PUT` the registry rejected as too large — which
is how registry caps surface, since bigoci carries no table of vendor limits.

## Testing

Three layers, plus measurement:

- **Unit:** split-plan arithmetic, manifest round-trips, retry decisions,
  resume bookkeeping. Pure functions, table-driven.
- **Integration:** the orchestrator against mocked ports. Failure injection
  lives here: dropped connections mid-part, out-of-order completions, digest
  mismatches.
- **End-to-end:** testcontainers running CNCF Distribution and zot. Because
  part size is an option, small inputs exercise every large-file code path at
  full fidelity: per-commit runs push and pull 64 MiB files at a 4 MiB part
  size, killing and resuming transfers mid-flight. Multi-gigabyte volume runs
  in a nightly job, not per commit (CI runners have ~14 GB of disk). A
  feature is done when it works against a real registry, not when its unit
  tests pass.
- **Benchmarks:** a harness measuring throughput against a local registry
  under different part sizes and worker counts. It exists to set the defaults
  above and to keep them honest as the implementation evolves.

Cloud registries (GHCR, ECR, ACR, Artifact Registry, Docker Hub, Harbor) get
a manually triggered conformance job, since they need credentials and cost
money to exercise at size.

## First slice

The walking skeleton, in order: push and pull one file against zot in
testcontainers, with small fixed parts, no retries, no resume, anonymous
auth. Then retries, then resume, then auth — each shipped behind the
end-to-end gate above. The benchmark harness, which sets the real defaults,
is what remains.

## Open questions

One question is genuinely open.

**Should worker count self-tune?** A fixed count wastes fast pipes and can
oversubscribe slow ones. Adaptive concurrency (ramp while throughput grows,
back off on 429 and 503) risks fighting registry rate limiters in ways that
are hard to test honestly. This needs benchmark-harness data and
real-registry behavior before a decision. Until then: fixed default,
caller-configurable.
