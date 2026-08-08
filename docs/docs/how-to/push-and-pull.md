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

## Pull a file

```go
err := client.Pull(ctx, "registry.example.com/team/models:v1",
    bigoci.ToFile("/data/model.bin"))
```

Every pulled byte is verified against the manifest before the destination
appears. A pull that fails or is interrupted leaves the destination alone; a
`.bigoci-partial` file next to it holds whatever arrived.

To pin the exact content instead of trusting a tag, pull by digest:

```go
err := client.Pull(ctx, bigoci.Reference("registry.example.com/team/models@"+desc.Digest.String()),
    bigoci.ToFile("/data/model.bin"))
```

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

Some failures are not worth repeating and end the transfer at once: a part the
registry refuses, a pulled part that fails verification, and a destination
that will not take the bytes.

Two things to know if you configure the transport:

- Cancelling the context, or hitting its deadline, stops a wait immediately.
  Your context is what bounds the total time a transfer may take.
- If you install your own retrying `RoundTripper` with
  `bigoci.WithHTTPClient`, the attempts multiply: yours run inside each of
  bigoci's. A `Timeout` on the client bounds one attempt, not the transfer, so
  an attempt that times out is retried with a fresh window.

## Handle the errors that matter

Four failures are worth branching on with `errors.Is`:

- `bigoci.ErrNotFound` — the registry does not hold what the reference names.
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
