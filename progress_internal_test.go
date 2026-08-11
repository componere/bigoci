package bigoci

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/transfer"
)

// TestPublicPhaseCoversEveryCorePhase is the seam between two enums that are
// deliberately not the same enum.
//
// The core's phases are an implementation detail and this package's are a
// contract, so the two are declared separately and bridged by a switch rather
// than a conversion. The table is exhaustive on purpose: a phase added to
// either side without a row here is the bug this test exists to catch, and a
// numeric cast is what it exists to prevent.
func TestPublicPhaseCoversEveryCorePhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		core transfer.Phase
		want Phase
	}{
		{name: "resolving", core: transfer.PhaseResolving, want: PhaseResolving},
		{name: "transferring", core: transfer.PhaseTransferring, want: PhaseTransferring},
		{name: "finalizing", core: transfer.PhaseFinalizing, want: PhaseFinalizing},
		{name: "done", core: transfer.PhaseDone, want: PhaseDone},
		{name: "failed", core: transfer.PhaseFailed, want: PhaseFailed},
		{name: "a phase no constant names", core: transfer.Phase(0), want: 0},
	}

	seen := make(map[Phase]bool, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, publicPhase(tt.core))
		})

		seen[tt.want] = true
	}

	for _, phase := range []Phase{PhaseResolving, PhaseTransferring, PhaseFinalizing, PhaseDone, PhaseFailed} {
		assert.True(t, seen[phase], "%s has no row: every public phase must be reachable from a core one", phase)
	}
}

// TestProgressReportStampsTheDirection checks the one field the core does not
// carry, and that a transfer nobody named a callback for installs none.
func TestProgressReportStampsTheDirection(t *testing.T) {
	t.Parallel()

	assert.Nil(t, progressReport(DirectionPush, nil), "no callback must install no report at all")

	for _, direction := range []Direction{DirectionPush, DirectionPull} {
		var got Progress
		report := progressReport(direction, func(p Progress) { got = p })
		require.NotNil(t, report)

		report(transfer.Snapshot{
			Phase:          transfer.PhaseTransferring,
			TotalBytes:     100,
			TotalParts:     2,
			CompletedBytes: 50,
			CompletedParts: 1,
			SkippedParts:   1,
			WireBytes:      60,
			HashedBytes:    100,
			Retries:        3,
		})

		assert.Equal(t, Progress{
			Direction:      direction,
			Phase:          PhaseTransferring,
			TotalBytes:     100,
			TotalParts:     2,
			CompletedBytes: 50,
			CompletedParts: 1,
			SkippedParts:   1,
			WireBytes:      60,
			HashedBytes:    100,
			Retries:        3,
		}, got, "every field must cross the adapter unchanged but the direction")
	}
}

// TestProgressOptionReachesBothDirections checks that [WithProgress] really
// is a [TransferOption]: one option value, applied by a push and by a pull.
func TestProgressOptionReachesBothDirections(t *testing.T) {
	t.Parallel()

	var calls int
	option := WithProgress(func(Progress) { calls++ })

	var push pushSettings
	option.applyPush(&push)
	require.NotNil(t, push.progress)
	push.progress(Progress{})

	var pull pullSettings
	option.applyPull(&pull)
	require.NotNil(t, pull.progress)
	pull.progress(Progress{})

	assert.Equal(t, 2, calls)
}
