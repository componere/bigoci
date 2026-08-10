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

Three things follow from hashing rather than bookkeeping:

- It is safe to interrupt a pull at any moment, including with `SIGKILL`. A
  range that was never written reads back as zeros and fails its check, so
  there is nothing to corrupt and nothing to reconcile.
- Moving or deleting the partial file is how you start over. Nothing else
  remembers the earlier run.
- A partial file left by a *different* artifact is not reused. If its length
  does not match the manifest, bigoci resizes it and fetches everything.

The cost is worth knowing. A pull that fails leaves a partial file the full
size of the artifact, whatever it managed to download, so the disk space is
committed from the first attempt. And every rerun hashes the whole partial
before it fetches anything — the hashing runs across your workers and overlaps
the downloads of the missing parts, but a pull that died at the very start
still pays a read of a file that holds nothing useful.
Cancelling the pull or reaching its deadline interrupts this hash pass between
256 KiB reads; it does not finish hashing the rest of a large partial first.

## Choose a part size and worker count

Files move as fixed-size parts, in parallel. Two options control that:

```go
desc, err := client.Push(ctx, ref, bigoci.FromFile(path),
    bigoci.WithPartSize(256<<20), // 256 MiB parts; the default is 512 MiB
    bigoci.WithWorkers(8),        // parallel transfers; the default is 4
)
```

The defaults suit most links. Raise the worker count on a fast pipe. The part
size is recorded in the manifest, so a pull never needs to be told it.

## What bigoci retries

A registry that hiccups does not fail your transfer. Each part gets up to four
attempts, with a short wait between them that grows and is jittered, so
workers that failed together do not come back together. Dropped connections,
429s, and 5xx answers are retried; so is a part whose body ends early. When a
registry sends `Retry-After`, bigoci waits at least that long, up to 30
seconds.

A part whose stream breaks is asked for again from the byte it reached, so a
retry near the end of a large part costs the tail and not the whole part. If
the registry will not serve a byte range it sends the whole part instead, and
bigoci writes it again from the start. A proxy or CDN that caps how much of a
range it returns costs one retry per capped answer, so a very large part
behind one can use up its retries before the tail arrives.

Some failures are not worth repeating and end the transfer at once: a part the
registry refuses, a pulled part that fails verification, and a local file that
will not take the bytes or will not read back.

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
