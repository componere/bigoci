---
title: API
description: Every exported identifier of package bigoci, with its signature, behavior, and constraints.
---

# API reference

Package `bigoci`, import path `github.com/componere/bigoci`. Everything the
package exports is listed on this page.

The godoc at
[pkg.go.dev/github.com/componere/bigoci](https://pkg.go.dev/github.com/componere/bigoci)
is the canonical rendering: it is generated from the source and carries the
full doc comments. This page groups the same surface by what a reader looks
up. Where the two disagree, the godoc is right.

For tasks, see [Push and pull a file](../how-to/push-and-pull.md) and
[Authenticate to a registry](../how-to/authenticate.md). The failure contract
has its own page: [Errors](errors.md). The wire format is in
[Format](format.md).

## Client and transfers

### Client

```go
type Client struct { /* unexported fields */ }
```

Holds transfer-wide settings, its credential resolver, and one lazily prepared
external connection pool — no connection to one registry and no state from one
transfer. One `Client` serves any number of concurrent pushes and pulls against
any number of repositories. The zero value is usable and behaves as one built
with no options.

### New

```go
func New(opts ...Option) (*Client, error)
```

Returns a client configured by `opts`.

`New` reports an error in one case today. `WithDockerCredentials` records the
intent to use the credentials `docker login` stores, and `New` is where they
are read, so a configuration file that exists and cannot be parsed fails here
rather than in the middle of a transfer. A file that is not there — or a
machine that cannot name where one would be, with no home directory and no
`$DOCKER_CONFIG` — is not an error: every registry then resolves to the
anonymous credential.

A client built with no credential option is not a client with authentication
turned off. A registry that asks for a token still gets the full exchange,
made anonymously. It only means bigoci has no user name or secret to offer
when the exchange asks for one.

### Client.Push

```go
func (c *Client) Push(
    ctx context.Context,
    ref Reference,
    src FileSource,
    opts ...PushOption,
) (ocispec.Descriptor, error)
```

Uploads the file `src` names to `ref` and returns the descriptor of the
manifest it wrote. `ocispec` is
`github.com/opencontainers/image-spec/specs-go/v1`.

- **Split.** The file is split into parts of [`DefaultPartSize`](#defaultpartsize)
  bytes, or of whatever [`WithPartSize`](#withpartsize) asks for. One
  sequential read hashes the file and hands each part to the workers as its
  digest completes, so uploading overlaps hashing.
- **Reads.** The file is read once from start to end, and a part is re-read
  only to upload it. No part is buffered in memory at any part size. The file
  must not change while the push runs.
- **Dedupe.** Parts the registry already holds are not uploaded again.
- **Ordering.** The manifest is written last, once every blob it references
  exists. A push that dies leaves no artifact behind, only unreferenced blobs
  the registry collects.
- **Determinism.** The returned digest is a pure function of the file bytes,
  the part size, and the title, so pushing the same file twice reproduces it.
- **Validation.** The part size and the worker count are checked before
  anything is opened. Both must be positive.
- **Retries.** A failure worth repeating — a 429, a 5xx, a dropped connection
  — costs up to four attempts per part. The wait between attempts is drawn
  from a window that doubles from one second to a thirty-second ceiling, and
  never falls short of a `Retry-After` the registry sent, itself bounded at
  thirty seconds. Everything else fails at once. A push that gives up leaves
  whichever parts had already landed in the repository, unreferenced; a later
  push finds and skips them.
- **Cancellation.** A cancelled `ctx` stops the workers, cuts short any wait
  in progress, and returns its error.
- **Sentinels.** [`ErrNotFound`](errors.md#errnotfound),
  [`ErrUnauthorized`](errors.md#errunauthorized), and
  [`ErrPartTooLarge`](errors.md#errparttoolarge).

### Client.Pull

```go
func (c *Client) Pull(
    ctx context.Context,
    ref Reference,
    dest FileDest,
    opts ...PullOption,
) error
```

Downloads the artifact `ref` names into the file `dest` names.

- **Manifest first.** `Pull` fetches the manifest, checks that it is a bigoci
  artifact, and reads the part size and the part digests out of it. Nothing
  about the transfer has to be told twice.
- **Verification.** Workers stream each part straight into its byte range of a
  partial file, hashing as they write. Every part is checked against the
  digest the manifest gives it. A digest reference makes the chain end to end:
  the manifest is verified against the digest asked for, and every part
  against the manifest.
- **Publication.** The destination is published with one atomic rename, once
  every part has passed. It is never observed half written: a pull that fails,
  is cancelled, or is killed leaves the destination absent or holding its
  previous content, and leaves the partial file beside it.
- **Resume.** The partial file's bytes are what the next pull resumes from. When the
  partial file is already the length the manifest declares, `Pull` hashes each
  part out of it and asks the registry only for the parts that do not match. A
  partial file of any other length belongs to some other artifact and is
  refilled from the start. Nothing is recorded between runs.
- **Validation.** The worker count is checked before the partial file is
  created. It must be positive.
- **Retries.** A part whose fetch breaks in a way worth repeating is fetched
  again, up to four attempts in total, on the same schedule a push uses. Those attempts
  carry on from the byte the broken one reached, unless the registry will not
  serve a byte range, in which case it answers with the whole blob and the
  part is written again from its first byte. A part that arrives whole and
  hashes wrong is not retried.
- **Cancellation.** A cancelled `ctx` stops the workers, interrupts active
  local resume hashing, cuts short any wait in progress, and returns its error.
- **Sentinels.** [`ErrNotFound`](errors.md#errnotfound),
  [`ErrUnauthorized`](errors.md#errunauthorized),
  [`ErrNotBigociArtifact`](errors.md#errnotbigociartifact), and
  [`ErrDigestMismatch`](errors.md#errdigestmismatch).

## References and file endpoints

### Reference

```go
type Reference string
```

Names one artifact: a registry, a repository on it, and a tag or a digest,
written `registry/repo[:tag][@digest]`. The grammar is the one container
tooling uses, parsed by `github.com/distribution/reference`. Three rules
follow from it:

| Rule | Consequence |
|---|---|
| The registry is required | `team/model:v1` is not a reference, and no short name is expanded to Docker Hub |
| The name must be canonical | lowercase only |
| A tag or a digest is required | every transfer names one manifest |

A reference that carries both is bound to its digest: the digest names one
manifest exactly, and the tag beside it is a claim about where that tag
pointed. Pulling by digest also makes bigoci check the manifest it fetched
against it.

### FileSource

```go
type FileSource struct { /* unexported fields */ }
```

The file end of a push: the path [`Client.Push`](#clientpush) reads the bytes
from. The zero value names no file.

### FromFile

```go
func FromFile(path string) FileSource
```

Names the local file a push uploads. Nothing is opened here: the file is
opened and measured when `Push` runs, and closed before it returns, so
building a source cannot fail and holding one costs no file handle. A file
that is missing, unreadable, or not a regular file is reported by the push
that tries to read it.

The file must not change while the push runs. Its size is read once, and the
split plan, the part digests, and the manifest all describe what was there at
that moment.

### FileDest

```go
type FileDest struct { /* unexported fields */ }
```

The file end of a pull: the path [`Client.Pull`](#clientpull) writes the
assembled file to. The zero value names no file.

### ToFile

```go
func ToFile(path string) FileDest
```

Names the local file a pull writes. The pull writes into a sibling file named
`path` plus `.bigoci-partial` and renames it onto `path` only once every part
has verified. The directory holding `path` must already exist.

A pull that fails leaves the partial file behind on purpose: those bytes are
what a later resume starts from. An existing destination is replaced only by a
pull that finishes.

## Client options

Client options are applied by [`New`](#new) and hold for every transfer the
client makes.

### Option

```go
type Option func(*clientSettings)
```

Configures a `Client` as `New` builds it. The signature names a type the
package keeps to itself, which seals the set: the only options that exist are
the ones declared here.

### WithHTTPClient

```go
func WithHTTPClient(client *http.Client) Option
```

Sends every registry request with `client` instead of the default one. A nil
client is ignored, so a caller may pass one through unconditionally.

This is the seam for timeouts, proxies, connection pool tuning, and a
credential source bigoci does not have — an authenticating
`net/http.RoundTripper` such as go-containerregistry's keychain.

Three consequences:

- **Retries multiply.** bigoci retries failed transfers itself. A transport
  that also retries makes the attempts a part gets the product of the two
  counts, and the waits between them stack.
- **`Timeout` bounds a request, not a transfer.** One attempt that
  authenticates and follows a redirect chain makes several requests, each with
  a fresh window. The deadline for a whole transfer belongs on the context.
- **A transport sits below bigoci's redirect decision.** bigoci strips
  `Authorization` on its way to a host the registry named, but whatever a
  transport sets, it sets on the request bigoci already cleaned. Large
  registries answer a blob read with a redirect to object storage on a URL
  that is already signed. The fix is a host check in the transport;
  [Authenticate to a registry](../how-to/authenticate.md#if-you-need-a-credential-source-bigoci-does-not-have)
  shows it in full.

bigoci copies this client rather than using it, and never writes to the
original. The copies keep the transport and timeout, set a redirect policy of
their own, and remove the cookie jar from registry-selected token, upload, and
redirect requests.

For a concrete `http.Transport`, cross-host requests use one shared clone and
check the direct connection's peer before HTTP request bytes leave. A proxy,
custom dial hook, custom `TLSNextProto`, or opaque `RoundTripper` hides that
destination and therefore fails closed unless
[`WithUnverifiedExternalTransport`](#withunverifiedexternaltransport)
explicitly delegates the boundary to the caller.

### WithUnverifiedExternalTransport

```go
func WithUnverifiedExternalTransport() Option
```

Authorizes registry-selected cross-host requests through a transport whose
final destination bigoci cannot verify. This is a security escape hatch for a
caller whose own transport or network policy enforces an equivalent boundary.

The option uses the original transport for those requests, preserving proxy,
dial, pooling, and `http.Transport.RegisterProtocol` behavior. The caller owns
the full destination check on that path. Direct private-IP token realms and
upload locations remain refused before the transport runs. See
[Authenticate to a registry](../how-to/authenticate.md#if-you-need-a-credential-source-bigoci-does-not-have)
for the threat boundary and a host-scoped authenticating transport.

### WithPlainHTTP

```go
func WithPlainHTTP() Option
```

Talks `http://` to the registry instead of `https://`. Everything a transfer
sends rides unencrypted under it, credentials and token exchanges included. A
token endpoint a plain-HTTP registry names is refused unless it is the
registry's own host.

For local registries — zot or CNCF Distribution in a container, a test fixture
— and nothing else.

### WithDockerCredentials

```go
func WithDockerCredentials() Option
```

Authenticates with the credentials `docker login` stores: the entries in the
Docker configuration file, and whatever the credential helpers that file names
print for a registry.

The file is `$DOCKER_CONFIG/config.json` where that variable is set, and
`.docker/config.json` under the user's home otherwise. [`New`](#new) reads it,
so a file that cannot be parsed fails there. A file that is not there is not a
failure: that is a machine nobody has logged in on, and every registry
resolves anonymously.

Helpers are asked afresh at every lookup; the file itself is read once, so a
`docker login` run during a transfer does not reach it. Resolving a credential
through a helper means running `docker-credential-<name>` from `PATH`. bigoci
only ever reads — no transfer writes a credential anywhere.

### WithCredentials

```go
func WithCredentials(username, secret string) Option
```

Presents `username` and `secret` to whatever registry a transfer dials.
Nothing is looked up, no file is read, and no program is run. `secret` is a
password or, at most registries today, a personal access token.

Every registry is the deliberate part: the credential goes to whatever host
the reference names, so the caller, who chose both the secret and the
reference, is the one deciding who sees it.
[`WithDockerCredentials`](#withdockercredentials) is the other shape — it
answers only for the hosts a login was stored under.

Naming both credential options leaves the last one named in effect.

## Transfer options

Transfer options are applied to one call. They are sealed by an unexported
method, so the set is the one this package ships.

### PushOption

```go
type PushOption interface { /* unexported method */ }
```

Configures one call to [`Client.Push`](#clientpush).

### PullOption

```go
type PullOption interface { /* unexported method */ }
```

Configures one call to [`Client.Pull`](#clientpull).

### TransferOption

```go
type TransferOption interface {
    PushOption
    PullOption
}
```

Configures either direction. An option is one of these when what it sets means
the same thing to a push and to a pull: [`WithWorkers`](#withworkers) and
[`WithProgress`](#withprogress). Part size and title describe how a file is
stored and belong to the push that decides it.

### WithPartSize

```go
func WithPartSize(size PartSize) PushOption
```

Splits the pushed file into parts of `size` bytes, in place of
[`DefaultPartSize`](#defaultpartsize). The last part is whatever is left over.

`size` must be positive; `Push` checks it before it opens anything. The part
size is recorded in the manifest, so a pull never guesses it, and it is part
of what the manifest digest describes: **the same file at two part sizes is
two artifacts**.

Raising it trades parallelism for fewer, larger requests, and has to stay
under the registry's layer cap — a registry that refuses a part for being
larger than it accepts reports
[`ErrPartTooLarge`](errors.md#errparttoolarge). Lowering it makes a failed
part cheaper to re-push, and puts a ceiling on the file, because the format
allows at most 4096 parts.

### WithTitle

```go
func WithTitle(title string) PushOption
```

Records `title` as the artifact's file name annotation instead of the base
name of the pushed file. The annotation travels in the standard
`org.opencontainers.image.title` key and is informational. An empty title
writes no annotation at all.

The title is part of what the manifest digest describes, the same way the part
size is.

### WithWorkers

```go
func WithWorkers(n int) TransferOption
```

Moves `n` parts at once, in place of [`DefaultWorkers`](#defaultworkers). `n`
must be positive; `Push` and `Pull` check it before they open anything.

Each worker holds one connection to the registry for the length of a part, so
this is the knob that trades connections for throughput. The measurements
behind the default are in [Benchmarks](benchmarks.md).

### WithProgress

```go
func WithProgress(fn ProgressFunc) TransferOption
```

Calls `fn` with absolute snapshots of the whole transfer. See
[Progress reporting](#progress-reporting) for the fields, the callback
contract, and the invariants a consumer may rely on. A transfer with no
`WithProgress` installs no counters.

## Defaults and limits

### PartSize

```go
type PartSize int64
```

The size in bytes of the parts a file is split into: the `P` of the format's
split rule, where part `i` covers bytes `[i*P, min((i+1)*P, size))`. It is a
distinct type so a part size cannot be transposed with a file size or a worker
count.

### DefaultPartSize

```go
const DefaultPartSize PartSize = 512 << 20 // 536870912
```

The part size a push splits at when the caller names none.

512 MiB sits roughly 19 times under the lowest registry layer cap, makes a
5 GB file into ten parts, and keeps the per-part request overhead in the
noise. The value is measured: on a 10 Gbit/s path it came within two percent
of the best cell of a 64 MiB to 1 GiB sweep against zot at 16 GiB, led the
sweep against CNCF Distribution, and tied within noise against GHCR. See
[Benchmarks](benchmarks.md).

### DefaultWorkers

```go
const DefaultWorkers = 8
```

How many parts a push or a pull moves at once when the caller names no worker
count. One worker holds one connection. With 512 MiB parts, moving from four
to eight workers left aggregate push throughput effectively flat against zot,
CNCF Distribution, and GHCR, raised GHCR's aggregate pull median from 161.6 to
262.1 MB/s, and drew no 429 or 503. See [Benchmarks](benchmarks.md).

### Constraints

| Value | Constraint | Reported by |
|---|---|---|
| Part size | must be positive | `Push`, before it opens anything |
| Worker count | must be positive | `Push` and `Pull`, before they open anything |
| Part count | 1 to 4096 | `Push`, once it plans the split; the message names the smallest part size that fits |
| Part size against the registry | under the registry's layer cap | the registry, as [`ErrPartTooLarge`](errors.md#errparttoolarge) |

At the default 512 MiB part size the 4096-part cap allows files up to 2 TiB.
The cap is a format rule, described in [Format](format.md#limits).

## Progress reporting

`WithProgress` hands a callback absolute snapshots of the whole transfer. A
snapshot is a value: it describes the transfer at that moment, not what
changed since the last one.

### ProgressFunc

```go
type ProgressFunc func(Progress)
```

The callback [`WithProgress`](#withprogress) installs.

### Direction

```go
type Direction uint8

const (
    DirectionPush Direction = iota + 1
    DirectionPull
)

func (d Direction) String() string
```

Which call the snapshot describes. The constants start at one, so the zero
`Progress` is never a valid snapshot. `String` returns `"push"`, `"pull"`, or
`"unknown"`.

### Phase

```go
type Phase uint8

const (
    PhaseResolving Phase = iota + 1
    PhaseTransferring
    PhaseFinalizing
    PhaseDone
    PhaseFailed
)

func (p Phase) String() string
```

Where the transfer is. `String` returns the constant's lowercase word, or
`"unknown"`.

| Phase | Meaning |
|---|---|
| `PhaseResolving` | Pull only: fetching and decoding the manifest |
| `PhaseTransferring` | Parts moving. Push: the hash pass and the uploads. Pull: the resume verify and the fetches |
| `PhaseFinalizing` | Push: the empty config blob and the manifest write. Pull: the commit |
| `PhaseDone` | Success. Exactly one snapshot carries it, and it is the last |
| `PhaseFailed` | Failure. Exactly one snapshot carries it, and it is the last. The snapshot does not say why — the error `Push` or `Pull` returned does |

### Progress

```go
type Progress struct {
    Direction      Direction
    Phase          Phase
    TotalBytes     int64
    TotalParts     int
    CompletedBytes int64
    CompletedParts int
    SkippedParts   int
    WireBytes      int64
    HashedBytes    int64
    Retries        int
}
```

| Field | Meaning |
|---|---|
| `Direction` | Push or pull |
| `Phase` | Where the transfer is, per the table above |
| `TotalBytes` | The file's size. A push knows it on every snapshot. A pull reports 0 during `PhaseResolving` and sets it once, at manifest decode |
| `TotalParts` | How many parts the file splits into. Parts, not blobs: dedupe never shrinks it |
| `CompletedBytes` | Bytes provably in their final place — held by the registry for a push, written and verified for a pull. Credited in whole-part steps, exactly once per part |
| `CompletedParts` | How many parts have been credited |
| `SkippedParts` | Parts that completed without moving their own bytes over the wire |
| `WireBytes` | Bytes of the file that crossed the registry boundary to get there. The manifest and the empty config blob are not counted |
| `HashedBytes` | Local bytes read and hashed: a push's hash pass, a pull's resume verify |
| `Retries` | Entries into a retry budget after the first |

`Retries` counts across all five retried operations: a part upload, a part
fetch, the empty config blob write, the manifest write, and the manifest
fetch.

### The two byte counters

`CompletedBytes` and `WireBytes` answer different questions, and they are
equal only when nothing goes wrong.

- **Percent, a progress bar, and bytes remaining belong on
  `CompletedBytes`.** It is bounded by `TotalBytes` and moves in whole-part
  steps, so it never regresses and never overshoots.
- **Throughput belongs on `WireBytes`.** It counts what actually crossed the
  boundary, including bytes a broken attempt moved and a retry moved again,
  so it is unbounded relative to `TotalBytes`.

Why the snapshot splits the two is covered in the
[design document](../explanation/design.md).

### Fraction

```go
func (p Progress) Fraction() float64
```

Returns `CompletedBytes / TotalBytes`, and 0 when `TotalBytes` is not
positive. An empty artifact — `TotalBytes` 0, `TotalParts` 1 — therefore reads
0 on every snapshot, including the terminal one, where `CompletedParts` is 1
and `Phase` is `PhaseDone`.

### The callback contract

**Bracketing.** Either `fn` is never called at all, or its first call carries
`PhaseResolving` for a pull and `PhaseTransferring` for a push, and its last
carries `PhaseDone` or `PhaseFailed`.

**Zero calls.** A failure before the transfer starts delivers nothing:
settings validation, a malformed reference, a source that cannot be opened, a
sink that cannot be created.

**First snapshot.**

| Direction | When the first snapshot fires | Totals |
|---|---|---|
| Push | Right after the split plan is computed | Real on the first snapshot |
| Pull | After settings validation, before the manifest fetch | 0 until the `PhaseTransferring` transition after manifest decode, where they arrive exactly once |

A pull's first snapshot fires before the manifest fetch so a manifest fetch
that is retrying is visible: `PhaseResolving` with a climbing `Retries`.

**Nothing arrives late.** No snapshot is delivered after `Push` or `Pull`
returns. Reports raised after the transfer ended are dropped.

**Calls are serialized.** Snapshots are totally ordered and never overlap. One
call finishes before the next begins.

**The callback blocks the transfer, by design.** A slow callback stalls the
transfer. Forbidden shapes: a blocking channel send, a network call, and
calling back into the `Client`. Store the snapshot and return.

**Callbacks run on whatever goroutine got there.** That includes worker
goroutines, a push's hash goroutine, and `net/http`'s request-body write
goroutine. A callback must be safe to call from any of them.

**Delivery is coalesced.** Byte-only movement is delivered at a 4 MiB
granularity rather than per read. A phase change, a part completion, a retry,
and the terminal snapshot are always delivered.

### Invariants

A consumer may rely on all of these:

- Every numeric field is non-decreasing.
- `Phase` never regresses.
- `TotalBytes` and `TotalParts` change at most once, from zero.
- `CompletedBytes <= TotalBytes` and `CompletedParts <= TotalParts`, once the
  totals are known.
- `SkippedParts <= CompletedParts`.
- `HashedBytes <= TotalBytes`.
- `WireBytes` is unbounded relative to `TotalBytes`.
- Exactly one terminal snapshot exists, and it is the last.
- **`CompletedBytes == TotalBytes` does not mean the transfer finished.** Only
  `PhaseDone` does.

### The skip rule

A part is skipped when this transfer moved none of that part's own bytes over
the wire, decided across the part's whole retry budget, not per attempt.

The budget wording carries a case: a push whose first attempt uploaded the
part and whose second attempt found the registry already holding it is **not**
skipped, because attempt one moved the bytes.

### What moves each counter

`P` is the part's size in bytes. Every row is one part of one transfer.

| Case | `CompletedBytes` | `CompletedParts` | `SkippedParts` | `WireBytes` | `HashedBytes` | `Retries` |
|---|---|---|---|---|---|---|
| Push, uploaded first try | +P | +1 | — | +P | +P | — |
| Push, registry already holds the part | +P | +1 | +1 | — | +P | — |
| Push, second part with identical bytes | +P | +1 | +1 | — | +P | — |
| Push, upload breaks at k bytes then succeeds | +P once | +1 | — | +(P+k) | +P | +1 |
| Push, retry finds the part already landed | +P | +1 | — | +P | +P | +1 |
| Pull, fetched cleanly | +P | +1 | — | +P | — | — |
| Pull, fetch breaks at k bytes and resumes by range | +P once | +1 | — | +P total | — | +1 |
| Pull, fetch breaks and the registry re-sends the whole blob | +P once | +1 | — | +(P+k) | — | +1 |
| Pull resume, part in the partial file hashes correct | +P | +1 | +1 | — | +P | — |
| Pull resume, part in the partial file hashes wrong | +P | +1 | — | +P | +P | — |
| Manifest fetch, config blob write, manifest write | — | — | — | — | — | counted |

Three notes on the rows:

- A push credits a part with identical bytes to another part when the upload
  that covers both settles, not when the duplicate is recognized. A file that
  is mostly duplicates does not read as nearly complete while its one real
  upload is still running.
- A pull whose stream broke after its last byte, before the attempt completed,
  fetches the part again in full: `WireBytes` counts 2P.
- The last row is why `Phase` matters. A manifest write that is retrying shows
  as `PhaseFinalizing` with a climbing `Retries`, which is what tells a
  watcher the difference between stuck and hung at 100%.

## Errors

`ErrNotFound`, `ErrUnauthorized`, `ErrNotBigociArtifact`, `ErrDigestMismatch`,
and `ErrPartTooLarge` are the failures a caller branches on. Both directions
run every error they return through the same check, so `errors.Is` answers for
the whole chain no matter how deep the failure started.

[Errors](errors.md) is the full contract: what each sentinel reports, when a
caller sees it, what the transfer left behind, and what unmatched errors look
like.
