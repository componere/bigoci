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
retries, not the transfer. Registry access is anonymous, and a pull always
fetches every part.

Resume and authentication are next, in that order. The
[design document](https://componere.github.io/bigoci/explanation/design/) and
the [artifact format contract](https://componere.github.io/bigoci/reference/format/)
are settled.

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
