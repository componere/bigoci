# bigoci

A Go library that uploads and downloads large files to and from OCI
registries. Large means 5 GB and up, into the tens of GB. That is the whole
library: no images, no bundles, no registry management.

bigoci stores a file as fixed-size parts pushed as OCI blobs, listed in order
as the layers of a standard OCI image manifest. Parts make transfers
parallel, retryable, and resumable on every registry, and keep files under
per-registry layer size caps.

## Status

Push and pull work end to end: a file splits into parts, uploads in parallel,
comes back verified against the manifest, and rides through transient
failures — a dropped connection, a 429, or a 5xx costs a bounded number of
retries, not the transfer. Transfers resume: an interrupted pull re-run
fetches only the parts the partial file does not already hold, a broken
stream is continued from the byte it reached, and a re-push skips every part
the registry already has. Transfers authenticate with the credentials
`docker login` stores — anonymous stays the zero-config default, and
registries that demand a token for anonymous reads get the full exchange —
and registries that hand blob reads to signed object storage are followed
with a clean request that carries no credential.

The defaults are measured, not guessed: a benchmark matrix run on bare
metal against zot, CNCF Distribution, and GHCR confirmed the 512 MiB part
size and 4 workers, and closed the design's one open question — worker
count does not self-tune (zero throttling observed; `WithWorkers` is the
escape hatch). The numbers live in the
[benchmarks reference](https://componere.github.io/bigoci/reference/benchmarks/).
A manually-triggered conformance job exercises GHCR; the
[design document](https://componere.github.io/bigoci/explanation/design/) and
the [artifact format contract](https://componere.github.io/bigoci/reference/format/)
are settled.

Progress reporting, the API polish pass, and the first release remain
before v0.1.0.

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
