# bigoci

A Go library that uploads and downloads large files to and from OCI
registries. Large means 5 GB and up, into the tens of GB. That is the whole
library: no images, no bundles, no registry management.

bigoci stores a file as fixed-size parts pushed as OCI blobs, listed in order
as the layers of a standard OCI image manifest. Parts make transfers
parallel, retryable, and resumable on every registry, and keep files under
per-registry layer size caps.

## Installation

```sh
go get github.com/componere/bigoci
```

Requires Go 1.26.4 or newer.

## Usage

One client holds the transport settings and serves any number of transfers.
Each direction is a single call:

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

// desc.Digest names that artifact exactly, wherever the tag points later.
ref := bigoci.Reference("registry.example.com/team/models@" + desc.Digest.String())
if err := client.Pull(ctx, ref, bigoci.ToFile("/data/model.bin")); err != nil {
    return err
}
```

A push splits at 512 MiB and moves 8 parts at once; `WithPartSize` and
`WithWorkers` change that. For a registry that asks for a credential, build
the client with `bigoci.WithDockerCredentials()`. The
[documentation site](https://componere.github.io/bigoci/) has the guides, and
[pkg.go.dev](https://pkg.go.dev/github.com/componere/bigoci) has the API.

## Status

Push and pull work end to end: a file splits into parts, uploads in parallel,
and comes back verified against the manifest. Transient failures cost a
bounded number of retries, not the transfer: a dropped connection, a 429, or
a 5xx. Transfers resume: an interrupted pull re-run fetches only the parts
the partial file does not already hold, a broken stream is continued from the
byte it reached, and a re-push skips every part the registry already has.
Transfers authenticate with the credentials `docker login` stores. Anonymous
stays the zero-config default, and registries that demand a token for
anonymous reads get the full exchange. A registry that hands blob reads to
signed object storage is followed with a clean request that carries no
credential.

A benchmark matrix run on bare metal against zot, CNCF Distribution, and
GHCR confirmed the 512 MiB part size and set the worker default at 8, and
closed the design's one open question: worker count does not self-tune (zero
throttling observed; `WithWorkers` is the escape hatch). The numbers live in the
[benchmarks reference](https://componere.github.io/bigoci/reference/benchmarks/).
A manually-triggered conformance job exercises GHCR; the
[design document](https://componere.github.io/bigoci/explanation/design/) and
the [artifact format contract](https://componere.github.io/bigoci/reference/format/)
are settled.

Progress reporting and the documentation site land in v0.1.0: `WithProgress`
hands a callback absolute snapshots of a running transfer, counting bytes
placed, bytes on the wire, parts done, and retries. The release itself has
not been cut yet.

## Development

Tooling is pinned with [mise](https://mise.jdx.dev) and tasks run through
[moon](https://moonrepo.dev):

```sh
mise install         # provision the pinned toolchain
moon run root:check  # format, lint, build, test, docs build
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow and
[SECURITY.md](SECURITY.md) for vulnerability reporting.

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT license ([LICENSE-MIT](LICENSE-MIT))

at your option.

Unless you explicitly state otherwise, any contribution intentionally
submitted for inclusion in this work by you, as defined in the Apache-2.0
license, shall be dual licensed as above, without any additional terms or
conditions.
