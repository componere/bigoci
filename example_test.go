package bigoci_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/imgoci/bigoci"
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

// The callback stores the snapshot and returns; rendering happens somewhere
// else, on the consumer's own clock. That split is the whole pattern. The
// callback runs on the transfer's goroutines and blocks them for as long as
// it takes, so a channel send, a network call, or a call back into the
// client belongs in the render loop, never here.
//
// Percent comes from [bigoci.Progress.Fraction] (completed bytes), and a
// throughput reading would come from the change in
// [bigoci.Progress.WireBytes] between two renders — the two counters answer
// different questions, and only [bigoci.PhaseDone] means the transfer
// finished.
//
//nolint:testableexamples // Running would need a live registry; the example exists to be compiled, not executed.
func Example_progress() {
	client, err := bigoci.New()
	if err != nil {
		panic(err)
	}

	// The library serializes the callback's calls, so the callback itself
	// needs no lock. The mutex is for the render loop below, which reads
	// from a goroutine of its own.
	var mu sync.Mutex
	var last bigoci.Progress

	// The renderer stops when the transfer call returns, never by waiting
	// for a terminal snapshot: a push that fails before it begins — a file
	// that will not open, a reference that will not parse — delivers no
	// snapshot at all.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}

			mu.Lock()
			p := last
			mu.Unlock()
			// The zero Progress describes no transfer: nothing has been
			// reported yet, so there is nothing to draw.
			if p.Phase == 0 {
				continue
			}

			fmt.Printf("%s %s: %3.0f%% (%d/%d parts, %d retries)\n",
				p.Direction, p.Phase, 100*p.Fraction(), p.CompletedParts, p.TotalParts, p.Retries)
		}
	}()

	_, err = client.Push(context.Background(), "registry.example.com/team/models:v1",
		bigoci.FromFile("/data/model.bin"),
		bigoci.WithProgress(func(p bigoci.Progress) {
			mu.Lock()
			last = p
			mu.Unlock()
		}),
	)
	close(stop)
	<-done
	if err != nil {
		panic(err)
	}

	// Only the phase says whether the transfer finished — every byte counter
	// can read complete while the manifest is still being written.
	mu.Lock()
	fmt.Println("final phase:", last.Phase)
	mu.Unlock()
}
