package bigoci_test

import (
	"context"
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
