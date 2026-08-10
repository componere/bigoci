---
title: Push and pull a file
description: Move a large file to an OCI registry and back with the bigoci Go library.
---

# Push and pull a file

This guide moves one file to a registry and back. It assumes you have a Go
project and a registry you can write to.

## Install

```sh
go get github.com/componere/bigoci
```

## Push a file

Build a client, then push. The reference names the registry, the repository,
and a tag or digest — the same grammar container tools use, with the registry
required.

```go
client, err := bigoci.New()
if err != nil {
    return err
}

desc, err := client.Push(ctx, "registry.example.com/team/models:v1",
    bigoci.FromFile("/data/model.bin"))
if err != nil {
    return err
}
fmt.Println("pushed as", desc.Digest)
```

The returned descriptor names the manifest by digest. Pushing the same file
again at the same part size reproduces the same digest, and parts the
registry already holds are not uploaded twice.

For a local registry that speaks plain HTTP (a zot or Distribution container,
for example), build the client with `bigoci.WithPlainHTTP()`.

For a registry that asks for a credential, see [Authenticate to a
registry](authenticate.md).

## Pull a file

```go
err := client.Pull(ctx, "registry.example.com/team/models:v1",
    bigoci.ToFile("/data/model.bin"))
```

Every pulled byte is verified against the manifest before the destination
appears. A pull that fails or is interrupted leaves the destination alone; a
`.bigoci-partial` file next to it holds whatever arrived, and the next pull
carries on from there — see [Resume an interrupted
pull](#resume-an-interrupted-pull).

To pin the exact content instead of trusting a tag, pull by digest:

```go
err := client.Pull(ctx, bigoci.Reference("registry.example.com/team/models@"+desc.Digest.String()),
    bigoci.ToFile("/data/model.bin"))
```

## Resume an interrupted pull

Pull the same reference to the same destination again. There is nothing to
turn on and no state to pass along:

```go
err := client.Pull(ctx, "registry.example.com/team/models:v1",
    bigoci.ToFile("/data/model.bin"))
```

bigoci finds the `.bigoci-partial` file next to the destination, hashes each
part of it, and asks the registry only for the parts that do not match the
manifest. A rerun after a pull you stopped halfway downloads the second half.
A rerun after a pull that had finished everything but the rename downloads
nothing at all.

On Unix, bigoci resumes only from the same regular file it observed before
opening. The file must be owned by the effective user and grant no permissions
to its group or other users. A partial that fails the check is left untouched
and the pull stops before contacting the registry. Other platforms reject a
statically planted symbolic link or non-regular file, but do not claim the Unix
ownership or race-free pathname guarantee.

Three things to know:

- Interrupting a pull is safe at any moment, including with `SIGKILL`. There
  is nothing to corrupt and nothing to reconcile.
- Moving or deleting the partial file is how you start over. Nothing else
  remembers the earlier run.
- A partial file left by a *different* artifact is not reused. If its length
  does not match the manifest, bigoci resizes it and fetches everything.

Plan for the disk: the partial file is created at the artifact's full length,
so reserve that much. It is sparse, so it occupies only what has arrived.
Every rerun also hashes the whole partial before it fetches anything;
cancelling the pull or reaching its deadline interrupts that hash pass rather
than finishing it. [What a resume
proves](../explanation/design.md#what-a-resume-proves) covers why resume is
built this way and what the hash pass costs.

## Choose a part size and worker count

Files move as fixed-size parts, in parallel. Two options control that:

```go
desc, err := client.Push(ctx, ref, bigoci.FromFile(path),
    bigoci.WithPartSize(256<<20), // 256 MiB parts; the default is 512 MiB
    bigoci.WithWorkers(4),        // parallel transfers; the default is 8
)
```

The defaults suit the measured paths. Override the worker count when your path
has different constraints. The part size is recorded in the manifest, so a
pull never needs to be told it.

## Watch a transfer

Hand `bigoci.WithProgress` a callback. It applies to both directions, like
`WithWorkers`:

```go
var mu sync.Mutex
var last bigoci.Progress

desc, err := client.Push(ctx, ref, bigoci.FromFile(path),
    bigoci.WithProgress(func(p bigoci.Progress) {
        mu.Lock()
        last = p
        mu.Unlock()
    }),
)
```

Store the snapshot and render it from somewhere else, as above. The callback
runs on the transfer's own goroutines and blocks them for as long as it takes:
a channel send, a network call, or a call back into the client stalls or
deadlocks the transfer. The [API reference](../reference/api.md) lists what
each snapshot carries and the full callback contract.

## What bigoci retries

A registry that hiccups does not fail your transfer. Each part gets up to four
attempts, with a short wait between them that grows and is jittered. Dropped
connections, 429s, and 5xx answers are retried, and a part whose stream breaks
is asked for again from the byte it reached.

Some failures are not worth repeating and end the transfer at once: a part the
registry refuses, a pulled part that fails verification, and a local file that
will not take the bytes or will not read back. The [retry
policy](../explanation/design.md#retry-policy) covers what the numbers buy,
how `Retry-After` is honored, and what a proxy that caps byte ranges costs.

Two things to know if you configure the transport:

- Cancelling the context, or hitting its deadline, stops a wait immediately.
  Your context is what bounds the total time a transfer may take.
- If you install your own retrying `RoundTripper` with
  `bigoci.WithHTTPClient`, the attempts multiply: yours run inside each of
  bigoci's. A `Timeout` on the client bounds one attempt, not the transfer, so
  an attempt that times out is retried with a fresh window.

## Handle the errors that matter

Five failures are worth branching on with `errors.Is`:

- `bigoci.ErrNotFound` — the registry does not hold what the reference names.
- `bigoci.ErrUnauthorized` — the registry refused the transfer. Log in to it
  (for most setups, `docker login <registry>`) and check that the account may
  read the repository — or write it, for a push. [Authenticate to a
  registry](authenticate.md) covers how to hand bigoci the credential and what
  each refusal means.
- `bigoci.ErrNotBigociArtifact` — the reference resolves to something else,
  such as a container image.
- `bigoci.ErrDigestMismatch` — pulled bytes failed verification; nothing was
  published.
- `bigoci.ErrPartTooLarge` — the registry refused a part as larger than it
  accepts. Push again with a smaller `bigoci.WithPartSize`. The same file at a
  different part size is a different artifact, with a different manifest
  digest.

Everything else comes back as a descriptive error naming the operation that
failed.
