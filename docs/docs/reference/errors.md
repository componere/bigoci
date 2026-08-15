---
title: Errors
description: The five bigoci sentinels, what each reports, what a failed transfer leaves behind, and the reference CLI's exit codes.
---

# Errors reference

Package `bigoci` exports five sentinel errors. They are the failures a caller
branches on; everything else comes back as a structural error naming the
operation and rule that failed without repeating peer-selected values.

| Sentinel | Direction | Cause |
|---|---|---|
| [`ErrNotFound`](#errnotfound) | push, pull | the registry does not hold what the transfer named |
| [`ErrUnauthorized`](#errunauthorized) | push, pull | the registry refused the transfer rather than answering it |
| [`ErrNotBigociArtifact`](#errnotbigociartifact) | pull | the reference resolves to something that is not a bigoci artifact |
| [`ErrDigestMismatch`](#errdigestmismatch) | pull | pulled bytes hash differently than the manifest says |
| [`ErrPartTooLarge`](#errparttoolarge) | push | the registry refused a part as larger than it accepts |

For the signatures these errors come out of, see [API](api.md). For handling
them in a working program, see
[Push and pull a file](../how-to/push-and-pull.md#handle-the-errors-that-matter).

## The errors.Is contract

`Client.Push` and `Client.Pull` run every error they return through one
classifier. It attaches the sentinel in front of the error that was already
built, so both the sentinel and everything under it stay in the chain:

```
not found: pull 127.0.0.1:5050/team/model:absent to /data/x.bin: fetch the
manifest: GET /v2/team/model/manifests/absent: registry returned 404 Not Found
```

Four rules follow.

- **Match with `errors.Is`, over the whole chain.** The sentinel is a wrapper,
  never the returned error itself, so `err == bigoci.ErrNotFound` is always
  false.
- **Never match on message text.** The text under the sentinel names the
  operation and structural failure, and none of that is a contract.
- **At most one sentinel is attached.** The classifier returns on its first
  match, so an error never carries two.
- **An error that matches nothing public comes back unchanged**, rather than
  being dressed up as a case it is not. See
  [Errors with no sentinel](#errors-with-no-sentinel).

Peer-controlled text is not safe diagnosis. A registry can copy the reusable
`Authorization` header it just received into a response body, challenge,
manifest field, redirect, or parser failure. Public error strings therefore
omit registry bodies, token realms, locations, manifest-selected values, and
raw transport or JSON parser messages. They retain fixed operations, statuses,
field names, bounded part indexes, and rules.

The underlying error chain remains inspectable. Sentinels, context errors,
and typed transport or body-read causes still work with `errors.Is` and
`errors.As`; only their potentially peer-derived message text is hidden.

```go
switch {
case errors.Is(err, bigoci.ErrUnauthorized):
    // log in, or ask for access
case errors.Is(err, bigoci.ErrNotFound):
    // the reference names nothing
}
```

## ErrNotFound

```go
var ErrNotFound = errors.New("not found")
```

**Reports** that the registry does not hold what a transfer named: the
manifest a reference resolves to, or a part a pull tried to read. It is a 404
from the registry.

**Seen when** a pull names a tag or a digest that was never pushed, and when a
pull's artifact lost a blob to garbage collection between the manifest fetch
and the part read. The second case is the one where a manifest resolves and a
part underneath it does not.

**Not** a missing local file. A source file that is absent, unreadable, or not
a regular file is a plain error with no sentinel; `ErrNotFound` is about what
the registry does not hold.

**Left behind.** Nothing is published. A pull that failed at the manifest has
written nothing, and its zero-length partial file is removed. A pull that
failed at a part read keeps its partial file — it was sized up front, and its
bytes are what a later pull resumes from.

**The fix** is to check the reference. For the garbage-collection case, push
the artifact again: the manifest still exists but the blob it names does not.

## ErrUnauthorized

```go
var ErrUnauthorized = errors.New("unauthorized")
```

**Reports** that the registry refused the transfer rather than answering it.
Two statuses fold into it:

| Status | What the registry is saying |
|---|---|
| 401 | it wants credentials the transfer did not present |
| 403 | the credentials it read do not reach the repository the reference names |

**The proxy caveat.** A proxy or a web application firewall in front of the
registry answers 403 about something else entirely, and that reports here too.
bigoci cannot tell the two apart and does not guess. If logging in and
checking access both come up clean, what sits in front of the registry is the
next place to look.

**Seen when** a push or a pull names a repository the transfer's credential
does not reach — including the empty credential, which is what a client with
no credential option carries. A push needs write access where a pull needs
read, so a credential that pulls fine can still fail a push.

**Not retried.** Presenting the same credential again gives the same answer, so
a refusal costs no waiting and only the requests the protocol itself spends.
One exception is invisible: a token that expires mid-transfer is replaced and
the transfer carries on.

**Left behind.** A push that was refused leaves whichever parts had already
landed, unreferenced, and no manifest — so no artifact. A pull that was refused
at the manifest publishes nothing and its zero-length partial file is removed.

**The fix** is to log in to that registry, which for most setups is
`docker login <registry>`, and to check that the account it logs in as may
read the repository — or write it, for a push.
[Authenticate to a registry](../how-to/authenticate.md#read-a-refusal) covers
handing bigoci the credential and reading which of the three causes applies.

## ErrNotBigociArtifact

```go
var ErrNotBigociArtifact = errors.New("not a bigoci artifact")
```

**Reports** that the reference resolves to something else: a container image,
an artifact of another kind, or a manifest whose `artifactType` is not the one
the bigoci format defines. A manifest whose media type names something other
than an OCI image manifest reports here as well.

**It means "look somewhere else."** Every other manifest error means the
artifact claims to be bigoci and is broken:

- a schema version other than 2
- a config that is not the OCI empty descriptor
- a layer with the wrong media type
- a missing or unparseable annotation
- parts that disagree with the split rule

Those are structural errors with no sentinel. The format contract they are
checked against is in [Format](format.md).

**Seen when** a pull names a reference that points at a container image or any
other artifact. A push never reports it.

**Left behind.** Nothing is published, and the zero-length partial file the
pull created is removed.

**The fix** is to check the reference.

## ErrDigestMismatch

```go
var ErrDigestMismatch = errors.New("digest mismatch")
```

**Reports** that pulled bytes hash differently than the manifest says they
should. The wrapped error names the part.

**Seen when** the registry serves content the artifact does not describe. A
push never reports it.

**Not retried.** A part that arrives whole and hashes wrong is not fetched
again: asking the registry the same question gives the same answer. This is
the one pull failure that is about the content rather than the transport.

**Not the same as a resume finding damage.** A pull that hashes an existing
partial file and finds a part wrong refetches that part; that is the resume
working, not this error.

**Left behind.** The destination is untouched — a pull publishes nothing until
every part verifies, so the destination is absent or still holds its previous
content. The partial file stays, and it holds the bytes that failed the check.

**The fix** is on the registry side. Rerunning the pull rehashes the partial,
refetches the same part, and gets the same bytes. Pulling by digest checks the
whole chain — the manifest against the digest asked for, and every part
against the manifest — which narrows whether the manifest or the blob is
wrong.

## ErrPartTooLarge

```go
var ErrPartTooLarge = errors.New("part too large")
```

**Reports** that the registry refused a part as larger than it accepts. It is
a 413, and it is how a registry's layer cap surfaces. bigoci ships no table of
vendor limits — the caps differ per registry, they move, and a stale table is
worse than none — so the limit is discovered by being told about it, once, by
the registry that enforces it. The wrapped error names the part and the status
the registry answered with.

**The mapping is the status and nothing else**, so a 413 answering a manifest
write would match too. That is admitted rather than guarded against: a bigoci
manifest is roughly 600 KB at the format's own 4096-part cap, far under any
known limit, and sniffing the body to tell the two apart would be the vendor
table again in a different shape.

**Seen when** a push runs at a part size above the registry's layer cap. A
pull never reports it.

**Left behind.** Whichever parts had already landed stay in the repository,
unreferenced, and no manifest was written — so no artifact exists.

**The fix** is to push again with a smaller
[`WithPartSize`](api.md#withpartsize). Two consequences:

- The same file at a different part size is a different artifact with a
  different manifest digest, because the part size is part of what the
  manifest describes. The second push is not another route to the first one's
  result.
- The second push shares no parts with the failed one. Changing the part size
  moves every part boundary, so every part digest changes and nothing that
  landed can be reused.

## Errors with no sentinel

An error that matches no sentinel is a wrapped error. It reads as the operation
and a structural cause:

```
push /tmp/model.bin to 127.0.0.1:5999/team/model:v1: after 4 attempts: check
whether part 0 (sha256:…) exists: HEAD /v2/team/model/blobs/<digest>: transport
failed
```

`after N attempts` appears when the failure outlived a retry budget. What
lands here:

| Failure | Detail |
|---|---|
| A local source file that is missing, unreadable, or not a regular file | reported by the push that opens it, not by `FromFile` |
| A destination directory that does not exist | reported by the pull that creates the partial file |
| A malformed reference | no registry, an uppercase name, or no tag and no digest |
| A part size or worker count that is not positive | reported before anything is opened |
| A file needing more than 4096 parts at the given part size | the message names the smallest part size that fits |
| A manifest that claims to be bigoci and is broken | see [`ErrNotBigociArtifact`](#errnotbigociartifact) for the split |
| A transport failure that outlived its retries | a dropped connection, a 429, a 5xx |
| A source that changes while a push is reading it | same-length mutation, digest mismatch, or a short part; not retried, and no manifest is written |
| A cancelled context or an expired deadline | see below |
| A manifest or blob response whose content encoding is not identity | a compressing proxy or middlebox; not retried. See below |

A registry, proxy, or middlebox that applies a content coding such as gzip to
a manifest or blob response is this last case. bigoci sends
`Accept-Encoding: identity` on those reads and refuses anything else, because
the bytes a pull hashes must be the bytes the registry stored. The error names
the request (`GET /v2/<name>/manifests/<tag>` or
`GET /v2/<name>/blobs/<digest>`) and the identity rule. It does not name the
encoding the far end chose, and it is not retried.

**Left behind.** A pull that failed at the manifest publishes nothing and its
zero-length partial file is removed. A pull that failed at a part keeps its
partial file.

**The fix** is on the registry side: turn off compression on the distribution
API, or on the proxy or middlebox in front of it. Token-endpoint JSON may
still be gzipped; that path is not a content hash.

## Cancellation and deadlines

A cancelled `ctx` stops the workers, interrupts active local resume hashing,
cuts short any wait in progress, and the transfer returns an error.
`errors.Is(err, context.Canceled)` and
`errors.Is(err, context.DeadlineExceeded)` answer over that chain.

**The error is not a reliable signal that you cancelled.** Cancelling a
transfer surfaces as whatever the unwinding produced first — `context.Canceled`,
a closed file, a reset socket, sometimes an error that matches a sentinel. A
caller that needs to know whether its own cancellation stopped the run should
ask its context, not the error.

**The context is what bounds a transfer.** A `Timeout` on an
[`http.Client`](api.md#withhttpclient) bounds one request. An attempt that
authenticates and follows a redirect chain makes several requests, each with a
fresh window, and bigoci then retries the attempt.

**Left behind.** A cancelled push leaves whichever parts had landed,
unreferenced, and writes no manifest. A cancelled pull leaves the destination
absent or holding its previous content, and leaves its partial file in place
for a later resume.

## Exit codes of the reference CLI

The repository carries a reference CLI under `cli/`. **It is never published,
never released, and never versioned**, and it exists so a person can watch the
library work against a real registry. Its exit codes are listed here because
they are the error contract demonstrated from outside Go: the CLI checks
`errors.Is` against each sentinel in table order and exits with the code that
matched.

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | failure, no sentinel matched: transport, local file, malformed reference, expired deadline, non-identity content coding |
| 2 | usage error: an unknown flag, an unparseable value, a negative duration flag, the wrong operand count, a destination that is a directory |
| 3 | `errors.Is(err, bigoci.ErrNotFound)` |
| 4 | `errors.Is(err, bigoci.ErrNotBigociArtifact)` |
| 5 | `errors.Is(err, bigoci.ErrDigestMismatch)` |
| 6 | `errors.Is(err, bigoci.ErrUnauthorized)` |
| 7 | `errors.Is(err, bigoci.ErrPartTooLarge)` |
| 130 | interrupted by SIGINT |
| 143 | terminated by SIGTERM |

Every sentinel the library exports has a row, 3 through 7 with no gaps. The
CLI's own documentation — flags, the request log, and evidence recipes — is
[`cli/README.md`](https://github.com/imgoci/bigoci/blob/master/cli/README.md),
which owns this table.

Three rules govern the table:

- **A recorded signal outranks the error's shape.** Once SIGINT or SIGTERM has
  been delivered, the run exits 130 or 143 whatever the error underneath it
  matched.
- **A usage error never reaches the sentinel table.** It exits 2 and prints
  the offending command's usage block.
- **An expired `-timeout` is exit 1, not 2.** The deadline is named on the
  failure line and the failure underneath it is still reported.

A failure prints two lines on standard error: the library's error verbatim,
then one of `bigoci: matched sentinel <name> (exit <code>)`,
`bigoci: no sentinel matched (exit 1)`, or the signal that stopped the run.
The second line is how a shell script watches the classification work.
