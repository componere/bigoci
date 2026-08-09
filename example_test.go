package bigoci_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/componere/bigoci"
)

// The example compiles with the package, so the usage it shows can never
// drift from the real API, but it cannot run under go test: it needs a live
// registry to talk to.
//
//nolint:testableexamples // Running would need a live registry; the example exists to be compiled, not executed.
func Example() {
	client, err := bigoci.New()
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	desc, err := client.Push(ctx, "registry.example.com/team/models:v1",
		bigoci.FromFile("/data/model.bin"),
		bigoci.WithPartSize(256<<20),
		bigoci.WithWorkers(8),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println("pushed as", desc.Digest)

	if err := client.Pull(ctx, "registry.example.com/team/models:v1",
		bigoci.ToFile("/data/model.bin"),
	); err != nil {
		panic(err)
	}
}

// A registry that asks for a credential needs one option and nothing else: the
// transfer calls are the same, and so is everything that happens against a
// registry that asks for nothing.
//
// This example uses what `docker login` stored. A caller who already holds the
// secret — a CI job reading it out of the environment — names
// [bigoci.WithCredentials] instead.
//
//nolint:testableexamples // Running would need a live registry; the example exists to be compiled, not executed.
func Example_authentication() {
	client, err := bigoci.New(bigoci.WithDockerCredentials())
	if err != nil {
		// A configuration file that cannot be read fails here, before any
		// transfer starts. A file that is not there is not an error.
		panic(err)
	}

	err = client.Pull(context.Background(), "ghcr.io/team/models:v1", bigoci.ToFile("/data/model.bin"))
	if errors.Is(err, bigoci.ErrUnauthorized) {
		fmt.Println("log in to ghcr.io, and check that the account may read the repository")

		return
	}
	if err != nil {
		panic(err)
	}
}
