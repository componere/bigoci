package bigoci_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci"
)

// TestProgressIsNeverCalledForAFailureBeforeTheTransfer pins the other half
// of the contract: a callback is either called properly or not at all.
//
// Both rows fail before there is a transfer to report on — one on a reference
// that is not one, the other on a file that is not there — and neither has a
// sensible opening snapshot to offer. Delivering a lone terminal snapshot for
// them would mean a consumer could receive [bigoci.PhaseFailed] with no
// totals, no direction it could trust, and nothing it had ever drawn.
func TestProgressIsNeverCalledForAFailureBeforeTheTransfer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, client *bigoci.Client, watch bigoci.ProgressFunc) error
	}{
		{
			name: "a reference that will not parse",
			run: func(t *testing.T, client *bigoci.Client, watch bigoci.ProgressFunc) error {
				t.Helper()

				_, err := client.Push(
					t.Context(), "not a reference", bigoci.FromFile(existingFile(t)), bigoci.WithProgress(watch),
				)

				return err
			},
		},
		{
			name: "a file that is not there",
			run: func(t *testing.T, client *bigoci.Client, watch bigoci.ProgressFunc) error {
				t.Helper()

				_, err := client.Push(
					t.Context(),
					"registry.example.com/team/model:v1",
					bigoci.FromFile(filepath.Join(t.TempDir(), "absent.bin")),
					bigoci.WithProgress(watch),
				)

				return err
			},
		},
		{
			name: "a worker count no transfer can run with",
			run: func(t *testing.T, client *bigoci.Client, watch bigoci.ProgressFunc) error {
				t.Helper()

				return client.Pull(
					t.Context(),
					"registry.example.com/team/model:v1",
					bigoci.ToFile(filepath.Join(t.TempDir(), "out.bin")),
					bigoci.WithWorkers(0),
					bigoci.WithProgress(watch),
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, err := bigoci.New()
			require.NoError(t, err)

			var (
				mu    sync.Mutex
				calls []bigoci.Progress
			)
			watch := func(p bigoci.Progress) {
				mu.Lock()
				defer mu.Unlock()

				calls = append(calls, p)
			}

			require.Error(t, tt.run(t, client, watch))

			mu.Lock()
			defer mu.Unlock()
			assert.Empty(t, calls, "a failure before the transfer began must report nothing at all")
		})
	}
}

// TestProgressFraction checks the number a bar is drawn from, including the
// two cases where zero is the honest answer rather than a guard against
// dividing.
func TestProgressFraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    bigoci.Progress
		want float64
	}{
		{name: "nothing yet", p: bigoci.Progress{TotalBytes: 400}, want: 0},
		{name: "part way", p: bigoci.Progress{TotalBytes: 400, CompletedBytes: 100}, want: 0.25},
		{name: "every byte placed", p: bigoci.Progress{TotalBytes: 400, CompletedBytes: 400}, want: 1},
		{
			name: "a pull that has not read the manifest",
			p:    bigoci.Progress{Phase: bigoci.PhaseResolving},
			want: 0,
		},
		{
			name: "an empty artifact, finished",
			p:    bigoci.Progress{Phase: bigoci.PhaseDone, TotalParts: 1, CompletedParts: 1},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.InDelta(t, tt.want, tt.p.Fraction(), 1e-9)
		})
	}
}

// TestProgressEnumStrings checks the words the enums render as, which is what
// a log line prints, and the answer for a value this package never produced.
func TestProgressEnumStrings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "push", bigoci.DirectionPush.String())
	assert.Equal(t, "pull", bigoci.DirectionPull.String())
	assert.Equal(t, "unknown", bigoci.Direction(0).String())

	assert.Equal(t, "resolving", bigoci.PhaseResolving.String())
	assert.Equal(t, "transferring", bigoci.PhaseTransferring.String())
	assert.Equal(t, "finalizing", bigoci.PhaseFinalizing.String())
	assert.Equal(t, "done", bigoci.PhaseDone.String())
	assert.Equal(t, "failed", bigoci.PhaseFailed.String())
	assert.Equal(t, "unknown", bigoci.Phase(0).String())
}

// existingFile writes a file a push can really open, so a row about a
// malformed reference fails on the reference and not on the file.
func existingFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "model.bin")
	require.NoError(t, os.WriteFile(path, []byte("bigoci"), 0o600))

	return path
}
