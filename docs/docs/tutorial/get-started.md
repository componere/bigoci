---
title: Get started
description: Push a file to a registry on your own machine with the bigoci Go library, pull it back, and check the two copies match.
---

# Get started

We will push a 256 MiB file to a registry running on this machine, pull it back
to a second path, and check that the two copies are byte for byte the same.
Everything runs locally, so none of it needs a registry account.

You need Go 1.26.4 or newer, Docker, and a POSIX shell — macOS or Linux. Every
command below is meant to be run in order, from the same directory.

## Start a registry

Make a directory to work in:

```sh
mkdir bigoci-tutorial
cd bigoci-tutorial
```

Write a configuration file for the registry:

```sh
cat > zot-config.json <<'EOF'
{
  "storage": {"rootDirectory": "/var/lib/registry"},
  "http": {"address": "0.0.0.0", "port": "5000"},
  "log": {"level": "error"}
}
EOF
```

This is the configuration bigoci's own end-to-end tests run against: the
smallest one that serves the registry API.

Start zot, the registry:

```sh
docker run -d --name bigoci-zot -p 5001:5000 \
  -v "$PWD/zot-config.json:/etc/zot/config.json:ro" \
  ghcr.io/project-zot/zot:v2.1.20
```

The first run downloads the image, then prints the container id, which is
different every time. The registry serves port 5000 inside the container; we
publish it on **5001** because macOS gives port 5000 to its AirPlay receiver.

Wait for it to answer. zot takes a second or two to start, so ask until it does:

```sh
until curl -sf -o /dev/null http://localhost:5001/v2/; do sleep 1; done
echo "registry ready"
```

```text
registry ready
```

`/v2/` is the base endpoint of the registry API. An answer from it means the
registry is speaking the protocol, not merely listening. If the loop is still
running after a few seconds, stop it with `Ctrl-C` and read
`docker logs bigoci-zot`.

## Make a file to move

```sh
head -c 268435456 /dev/urandom > model.bin
wc -c < model.bin
```

```text
268435456
```

macOS pads the count with leading spaces; the number is what matters.

256 MiB is small for bigoci, which is built for files of 5 GB and up. It is
enough to split into several parts, and it copies in seconds.

## Set up the Go module

```sh
go mod init bigoci-tutorial
go get github.com/componere/bigoci
```

```text
go: creating new go.mod: module bigoci-tutorial
go: added github.com/componere/bigoci v0.1.0
go: added github.com/distribution/reference v0.6.0
go: added github.com/opencontainers/go-digest v1.0.0
go: added github.com/opencontainers/image-spec v1.1.1
go: added golang.org/x/sync v0.22.0
go: added oras.land/oras-go/v2 v2.6.2
```

The `go: downloading` lines that appear between these are omitted, and the
versions move as dependencies are updated.

## Write the program

Create `main.go` next to `model.bin`, with this content:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/componere/bigoci"
)

// repo is the registry and the repository on it that we push to and pull from.
const repo = "localhost:5001/tutorial/model"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// WithPlainHTTP talks http:// instead of https://. Local registries only.
	client, err := bigoci.New(bigoci.WithPlainHTTP())
	if err != nil {
		return err
	}

	desc, err := client.Push(ctx, repo+":v1",
		bigoci.FromFile("model.bin"),
		bigoci.WithPartSize(64<<20), // 64 MiB parts, so 256 MiB is four of them
	)
	if err != nil {
		return err
	}
	fmt.Println("pushed", desc.Digest)

	// The digest names exactly what the push wrote, whatever the tag does later.
	ref := bigoci.Reference(repo + "@" + desc.Digest.String())
	if err := client.Pull(ctx, ref, bigoci.ToFile("model-pulled.bin")); err != nil {
		return err
	}
	fmt.Println("pulled model-pulled.bin")

	return nil
}
```

One client serves both directions and holds no state from either, so a program
that moves many files builds it once.

## Push and pull

```sh
go run .
```

```text
pushed sha256:1a518141bdc272fea807a141400dda47865ddbaa6b447f672970551e26eaadef
pulled model-pulled.bin
```

Your digest differs from this one. It describes the bytes of the file, and
`/dev/urandom` gave you different bytes.

The push split `model.bin` into four 64 MiB parts, uploaded them all in
parallel, and wrote a manifest listing them in order. The pull
read that manifest, fetched the parts in parallel, checked each one against the
digest the manifest gives it, and renamed the finished file into place only once
every part passed. [Design](../explanation/design.md) covers why it works that
way.

## Check the two copies match

```sh
cmp model.bin model-pulled.bin && echo identical
```

```text
identical
```

`cmp` prints nothing and exits 0 when two files are byte for byte the same.

The registry now holds the artifact under the tag we pushed it to:

```sh
curl -s http://localhost:5001/v2/tutorial/model/tags/list
```

```text
{"name":"tutorial/model","tags":["v1"]}
```

## Clean up

```sh
docker rm -f bigoci-zot
cd ..
rm -rf bigoci-tutorial
```

## Where to go next

- [Push and pull a file](../how-to/push-and-pull.md) — part size, worker count,
  resuming an interrupted pull, and the errors worth branching on.
- [API reference](../reference/api.md) — every exported name, including
  `WithProgress`, which reports how far a transfer has got.
- [Authenticate to a registry](../how-to/authenticate.md) — for a registry that
  asks for a credential.
- [Format](../reference/format.md) — the manifest and the parts we just wrote.
