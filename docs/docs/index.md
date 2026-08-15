---
title: bigoci
slug: /
description: Go library for uploading and downloading large files to and from OCI registries.
---

# bigoci

bigoci is a Go library that uploads and downloads large files to and from OCI
registries. Large means 5 GB and up, into the tens of GB. That is the whole
library.

Push and pull move a file end to end and retry transient failures. An
interrupted pull resumes where it stopped, and a re-push skips the parts the
registry already holds. bigoci authenticates to registries that ask for a
credential, and reports progress to a callback while a transfer runs.

**Tutorial** — learn by doing:

- [Get started](tutorial/get-started.md) — your first push and pull, against
  a registry you run locally.

**How-to guides** — get a task done:

- [Push and pull a file](how-to/push-and-pull.md) — move a file to a
  registry and back with the library.
- [Authenticate to a registry](how-to/authenticate.md) — use the credentials
  `docker login` stores, or pass one in, and read a refusal.

**Reference** — look up the facts:

- [API](reference/api.md) — every exported type, function, and option, and
  what each one does.
- [Errors](reference/errors.md) — the failures bigoci reports, what raises
  each, and what to do about it.
- [Format](reference/format.md) — the artifact format contract for
  implementers.
- [Registry compatibility](reference/registry-compatibility.md) — dated push
  and pull results against hosted registries.
- [Benchmarks](reference/benchmarks.md) — the measured throughput behind
  the default part size and worker count.

**Explanation** — understand the design:

- [Design](explanation/design.md) — why bigoci works the way it does: the
  split-part format, the transport, and the architecture.
