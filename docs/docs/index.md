---
title: bigoci
slug: /
description: Go library for uploading and downloading large files to and from OCI registries.
---

# bigoci

bigoci is a Go library that uploads and downloads large files to and from OCI
registries. Large means 5 GB and up, into the tens of GB. That is the whole
library.

Push and pull work end to end, retry transient failures, resume — a pull that
was interrupted picks up where it stopped, and a re-push skips the parts the
registry already holds — and authenticate to registries that ask for a
credential.

- [Push and pull a file](how-to/push-and-pull.md) — move a file to a
  registry and back with the library.
- [Authenticate to a registry](how-to/authenticate.md) — use the credentials
  `docker login` stores, or pass one in, and read a refusal.
- [Design](explanation/design.md) — why bigoci works the way it does: the
  split-part format, the transport, and the architecture.
- [Format](reference/format.md) — the artifact format contract for
  implementers.
- [Benchmarks](reference/benchmarks.md) — the measured throughput behind
  the default part size and worker count.
