---
title: bigoci
slug: /
description: Go library for uploading and downloading large files to and from OCI registries.
---

# bigoci

bigoci is a Go library that uploads and downloads large files to and from OCI
registries. Large means 5 GB and up, into the tens of GB. That is the whole
library.

Push and pull work end to end. Retries, resume, and authentication are next.

- [Push and pull a file](how-to/push-and-pull.md) — move a file to a
  registry and back with the library.
- [Design](explanation/design.md) — why bigoci works the way it does: the
  split-part format, the transport, and the architecture.
- [Format](reference/format.md) — the artifact format contract for
  implementers.
