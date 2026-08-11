package transfer_test

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/transfer"
)

// This file holds what every progress suite shares: the recorder the
// snapshots are collected in, and the invariant check it runs on each one as
// it arrives. The per-case accounting lives next door, in
// progress_push_test.go and progress_pull_test.go.

// snapshots collects the snapshots of one transfer and checks each against
// the one before it as it arrives.
//
// Checking on arrival rather than afterwards is the point: every progress
// test gets the whole invariant suite for free, and a violation is reported
// against the snapshot that broke it rather than against a slice.
//
// The mutex is belt to the orchestrator's braces. The reporter already
// serializes its callbacks, so nothing here can be called twice at once; the
// lock is what lets a test read the record while the transfer is still
// running, which the dedupe gate does.
type snapshots struct {
	// mu guards seen.
	mu sync.Mutex
	// t reports a violated invariant. Only assert is used, never require:
	// these run on the transfer's own goroutines.
	t *testing.T
	// seen is every snapshot delivered, in order.
	seen []transfer.Snapshot
}

// newSnapshots returns an empty recorder.
func newSnapshots(t *testing.T) *snapshots {
	t.Helper()

	return &snapshots{t: t}
}

// record is the [transfer.Report] a test installs on a spec.
func (s *snapshots) record(snap transfer.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.seen) > 0 {
		assertAdvance(s.t, s.seen[len(s.seen)-1], snap)
	}
	s.seen = append(s.seen, snap)

	assertBounds(s.t, snap)
}

// all returns every snapshot recorded so far, in order.
func (s *snapshots) all() []transfer.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.seen)
}

// last returns the most recent snapshot, and the zero value when none has
// arrived.
func (s *snapshots) last() transfer.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.seen) == 0 {
		return transfer.Snapshot{}
	}

	return s.seen[len(s.seen)-1]
}

// assertAdvance checks one snapshot against its predecessor: nothing falls,
// the phase never regresses, and totals settle once and then hold.
func assertAdvance(t *testing.T, before, after transfer.Snapshot) {
	t.Helper()

	assert.GreaterOrEqual(t, after.Phase, before.Phase, "a phase must never regress")
	assert.GreaterOrEqual(t, after.CompletedBytes, before.CompletedBytes, "completed bytes fell")
	assert.GreaterOrEqual(t, after.CompletedParts, before.CompletedParts, "completed parts fell")
	assert.GreaterOrEqual(t, after.SkippedParts, before.SkippedParts, "skipped parts fell")
	assert.GreaterOrEqual(t, after.WireBytes, before.WireBytes, "wire bytes fell")
	assert.GreaterOrEqual(t, after.HashedBytes, before.HashedBytes, "hashed bytes fell")
	assert.GreaterOrEqual(t, after.Retries, before.Retries, "retries fell")

	// Totals change at most once, from zero: a push knows them from its first
	// snapshot, and a pull learns them when it decodes the manifest.
	if before.TotalBytes != 0 {
		assert.Equal(t, before.TotalBytes, after.TotalBytes, "total bytes changed after it was known")
	}
	if before.TotalParts != 0 {
		assert.Equal(t, before.TotalParts, after.TotalParts, "total parts changed after it was known")
	}

	assert.NotEqual(t, transfer.PhaseDone, before.Phase, "a snapshot arrived after the terminal one")
	assert.NotEqual(t, transfer.PhaseFailed, before.Phase, "a snapshot arrived after the terminal one")
}

// assertBounds checks the promises a single snapshot makes on its own.
//
// WireBytes is deliberately absent: it is the one counter with no ceiling,
// because a part that was sent twice really did cost twice.
func assertBounds(t *testing.T, snap transfer.Snapshot) {
	t.Helper()

	assert.LessOrEqual(t, snap.SkippedParts, snap.CompletedParts, "more parts were skipped than completed")

	if snap.TotalParts == 0 {
		return
	}

	assert.LessOrEqual(t, snap.CompletedParts, snap.TotalParts, "more parts completed than the transfer has")
	assert.LessOrEqual(t, snap.CompletedBytes, snap.TotalBytes, "more bytes completed than the file holds")
	assert.LessOrEqual(t, snap.HashedBytes, snap.TotalBytes, "more bytes hashed than the file holds")
}

// assertReported checks the shape of a whole run: some snapshots arrived,
// the first one opened in wantFirst, and the last one is the terminal phase
// the transfer's outcome calls for.
func assertReported(t *testing.T, s *snapshots, wantFirst, wantLast transfer.Phase) {
	t.Helper()

	all := s.all()
	require.NotEmpty(t, all, "a transfer that ran must report at least the terminal snapshot")

	assert.Equal(t, wantFirst, all[0].Phase, "the first snapshot opened in the wrong phase")
	assert.Equal(t, wantLast, all[len(all)-1].Phase, "the last snapshot is not the terminal one")

	for i, snap := range all[:len(all)-1] {
		assert.NotEqual(t, transfer.PhaseDone, snap.Phase, "snapshot %d is terminal but is not last", i)
		assert.NotEqual(t, transfer.PhaseFailed, snap.Phase, "snapshot %d is terminal but is not last", i)
	}
}
