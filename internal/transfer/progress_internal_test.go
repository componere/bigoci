package transfer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file probes the reporter from inside the package, because the rule it
// pins cannot be reached from the Push and Pull boundary with any certainty.
// The latch exists for a race — a straggling read on the transport's own
// goroutine — and a test that has to provoke that race can only ever show the
// latch holding for the cases it managed to provoke. Calling the recording
// methods after the terminal snapshot directly shows it holding for all of
// them.

// errClosedTransfer is the failure the latch tests end their transfer with.
var errClosedTransfer = errors.New("the transfer ended")

// deliveries collects what a reporter handed its callback, for a test that
// runs it on one goroutine and needs no lock of its own.
type deliveries struct {
	// seen is every snapshot delivered, in order.
	seen []Snapshot
}

// record is the [Report] the latch tests install.
func (d *deliveries) record(snap Snapshot) {
	d.seen = append(d.seen, snap)
}

// TestReporterDropsEveryRecordingAfterTheTerminalSnapshot is the mutation
// killer for the closed latch: one row per recording method, each calling
// exactly one of them on a reporter that has already finished.
//
// Each row is chosen so that the call would deliver a snapshot if its guard
// were gone — a part index nothing has credited, a byte count over the
// coalescing threshold — because a guard whose removal changes nothing is a
// guard no test is really holding.
func TestReporterDropsEveryRecordingAfterTheTerminalSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// record is the one call the row makes after the transfer ended.
		record func(r *reporter)
	}{
		{name: "measured", record: func(r *reporter) { r.measured(2000, 8) }},
		{name: "finalizing", record: func(r *reporter) { r.finalizing() }},
		{name: "complete", record: func(r *reporter) { r.complete(1, 250, false) }},
		{name: "completeTwins", record: func(r *reporter) { r.completeTwins(2, 250) }},
		{name: "wire", record: func(r *reporter) { r.wire(progressStep) }},
		{name: "hashed", record: func(r *reporter) { r.hashed(progressStep) }},
		{name: "retried", record: func(r *reporter) { r.retried() }},
		{name: "a second finish", record: func(r *reporter) { r.finish(nil) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := &deliveries{}
			r := newReporter(got.record)

			r.begin(PhaseTransferring, 1000, 4)
			r.finish(errClosedTransfer)

			require.Len(t, got.seen, 2, "the opening and the terminal snapshot")
			terminal := got.seen[1]
			require.Equal(t, PhaseFailed, terminal.Phase)

			tt.record(r)

			assert.Len(t, got.seen, 2, "a recording after the terminal snapshot was delivered")
			assert.Equal(t, terminal, got.seen[len(got.seen)-1], "the terminal snapshot is no longer the last")
		})
	}
}

// TestReporterDropsEveryRecordingAfterASuccessfulFinish is the same latch on
// the other terminal phase, because a push that succeeds hands its reader
// back exactly as late as one that fails.
func TestReporterDropsEveryRecordingAfterASuccessfulFinish(t *testing.T) {
	t.Parallel()

	got := &deliveries{}
	r := newReporter(got.record)

	r.begin(PhaseTransferring, 1000, 4)
	r.finish(nil)

	require.Len(t, got.seen, 2)
	terminal := got.seen[1]
	require.Equal(t, PhaseDone, terminal.Phase)

	r.measured(2000, 8)
	r.finalizing()
	r.complete(1, 250, false)
	r.completeTwins(2, 250)
	r.wire(progressStep)
	r.hashed(progressStep)
	r.retried()
	r.finish(errClosedTransfer)

	assert.Len(t, got.seen, 2, "nothing may follow the terminal snapshot")
	assert.Equal(t, terminal, got.seen[1], "the terminal snapshot must survive unchanged")
}

// TestNilReporterRecordsNothingAndPanicsAtNothing is the other half of the
// same guard. A transfer nobody is watching carries a nil *reporter through
// every one of these calls, so each has to be safe on one — the alternative
// is a nil check at each of the twenty places a recording is made.
func TestNilReporterRecordsNothingAndPanicsAtNothing(t *testing.T) {
	t.Parallel()

	var r *reporter

	assert.NotPanics(t, func() {
		r.begin(PhaseTransferring, 1000, 4)
		r.measured(2000, 8)
		r.finalizing()
		r.complete(0, 250, true)
		r.completeTwins(2, 250)
		r.wire(progressStep)
		r.hashed(progressStep)
		r.retried()
		r.finish(nil)
	})

	assert.Nil(t, newReporter(nil), "a nil callback is a nil reporter, not a reporter that discards")
}
