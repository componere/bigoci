package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingPushIteration returns an iteration whose cancelled context makes a
// push fail without reaching a registry.
func failingPushIteration(t *testing.T, scenarios []string) (*iteration, fixture, *[]row) {
	t.Helper()

	target := Target{Name: "zot", Endpoint: "127.0.0.1:1", PlainHTTP: true, RepoPrefix: "bench"}
	client, counter, err := newTargetClient(target)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "fixture")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))
	emitted := []row{}
	it := &iteration{
		spec:      &Spec{RunID: "unit-run", Scenarios: scenarios},
		cell:      cell{target: target, partSize: 1 << 20, workers: 1, fileSize: 4, id: "zot-p1MiB-w1-f4B"},
		attemptID: "attempt",
		client:    client,
		counter:   counter,
		skip:      map[string]bool{},
		emit: func(r row) error {
			emitted = append(emitted, r)

			return nil
		},
		log: func(string, ...any) {},
	}

	return it, fixture{path: path}, &emitted
}

// TestSelectedPushFailureIsRecordedAndTheMatrixMayContinue preserves the
// paid-run behavior for a scenario that has an error row to account for it.
func TestSelectedPushFailureIsRecordedAndTheMatrixMayContinue(t *testing.T) {
	t.Parallel()

	it, src, emitted := failingPushIteration(t, []string{scenarioColdPush})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ref, err := it.push(ctx, src, scenarioColdPush)
	require.NoError(t, err)
	assert.Empty(t, ref)
	require.Len(t, *emitted, 1)
	assert.NotEmpty(t, (*emitted)[0].Error)
}

// TestUnrecordedPushFailureStopsTheRun prevents a cold-pull-only spec from
// exiting successfully with no rows when its prerequisite cannot be pushed.
func TestUnrecordedPushFailureStopsTheRun(t *testing.T) {
	t.Parallel()

	it, src, emitted := failingPushIteration(t, []string{scenarioColdPull})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ref, err := it.push(ctx, src, scenarioColdPush)
	require.Error(t, err)
	require.ErrorContains(t, err, "unrecorded cold-push prerequisite")
	assert.Empty(t, ref)
	assert.Empty(t, *emitted)
}

// TestSkippedColdPushFailureStopsTheResume covers the case where the earlier
// success row is retained but a fresh downstream prerequisite cannot be made.
func TestSkippedColdPushFailureStopsTheResume(t *testing.T) {
	t.Parallel()

	it, src, emitted := failingPushIteration(t, []string{scenarioColdPush, scenarioColdPull})
	it.skip[it.rowKey(scenarioColdPush)] = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ref, err := it.push(ctx, src, scenarioColdPush)
	require.Error(t, err)
	require.ErrorContains(t, err, "unrecorded cold-push prerequisite")
	assert.Empty(t, ref)
	assert.Empty(t, *emitted)
}

// TestClearPullFilesRemovesPublishedAndPartialState proves a timed cold pull
// cannot inherit either path from a failed or interrupted predecessor.
func TestClearPullFilesRemovesPublishedAndPartialState(t *testing.T) {
	t.Parallel()

	dest := filepath.Join(t.TempDir(), "artifact")
	require.NoError(t, os.WriteFile(dest, []byte("published"), 0o600))
	require.NoError(t, os.WriteFile(dest+pullPartialSuffix, []byte("partial"), 0o600))

	require.NoError(t, clearPullFiles(dest))
	assert.NoFileExists(t, dest)
	assert.NoFileExists(t, dest+pullPartialSuffix)
	require.NoError(t, clearPullFiles(dest), "cleanup must be idempotent")
}
