package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleRow returns a success row the tests can vary.
func sampleRow(scenario string, iteration int) row {
	return row{
		Schema:    rowSchema,
		RunID:     "unit-run",
		CohortID:  "unit-cohort",
		AttemptID: "attempt",
		Timestamp: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		CellID:    "zot-p4MiB-w1-f16MiB",
		Registry:  "zot",
		Scenario:  scenario,
		PartSize:  4 << 20,
		Workers:   1,
		FileSize:  16 << 20,
		Parts:     4,
		Iteration: iteration,
		WallMS:    100,
		MBPerS:    167.8,
		Commit:    "abcdef123456",
	}
}

func TestRowsRoundTripThroughTheFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "results.jsonl")

	writer, err := newRowWriter(path)
	require.NoError(t, err)
	require.NoError(t, writer.write(sampleRow(scenarioColdPush, 0)))
	require.NoError(t, writer.write(sampleRow(scenarioColdPull, 0)))
	require.NoError(t, writer.close())

	rows, err := readRows(path)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, sampleRow(scenarioColdPush, 0), rows[0])
	assert.Equal(t, scenarioColdPull, rows[1].Scenario)
}

func TestRowWriterAppendsAcrossReopens(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "results.jsonl")

	first, err := newRowWriter(path)
	require.NoError(t, err)
	require.NoError(t, first.write(sampleRow(scenarioColdPush, 0)))
	require.NoError(t, first.close())

	second, err := newRowWriter(path)
	require.NoError(t, err)
	require.NoError(t, second.write(sampleRow(scenarioColdPush, 1)))
	require.NoError(t, second.close())

	rows, err := readRows(path)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "a resumed run must append, never truncate")
}

func TestCompletedKeysSkipsOnlySuccesses(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "results.jsonl")

	writer, err := newRowWriter(path)
	require.NoError(t, err)
	require.NoError(t, writer.write(sampleRow(scenarioColdPush, 0)))
	failed := sampleRow(scenarioColdPull, 0)
	failed.Error = "connection reset"
	failed.MBPerS = 0
	require.NoError(t, writer.write(failed))
	require.NoError(t, writer.close())

	keys, err := completedKeys(path, "unit-run", "unit-cohort")
	require.NoError(t, err)

	assert.True(t, keys["zot-p4MiB-w1-f16MiB|cold-push|0"], "a success is skipped on resume")
	assert.False(t, keys["zot-p4MiB-w1-f16MiB|cold-pull|0"], "a failure is measured again on resume")
}

func TestCompletedKeysTreatsAMissingFileAsAFreshRun(t *testing.T) {
	t.Parallel()

	keys, err := completedKeys(filepath.Join(t.TempDir(), "absent.jsonl"), "unit-run", "unit-cohort")
	require.NoError(t, err)
	assert.Empty(t, keys)
}

// TestPrepareOutputProtectsNonResumeRuns checks the append boundary without
// weakening interrupt-safe resume behavior.
func TestPrepareOutputProtectsNonResumeRuns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	absent := filepath.Join(dir, "absent.jsonl")
	empty := filepath.Join(dir, "empty.jsonl")
	nonempty := filepath.Join(dir, "nonempty.jsonl")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	require.NoError(t, os.WriteFile(nonempty, []byte("occupied"), 0o600))

	for _, path := range []string{absent, empty} {
		keys, err := prepareOutput(path, false, "unit-run", "unit-cohort")
		require.NoError(t, err)
		assert.Empty(t, keys)
	}

	_, err := prepareOutput(nonempty, false, "unit-run", "unit-cohort")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-resume")
}

// TestCompletedKeysRejectsAnotherRun proves a retained output cannot skip a
// newly named measurement session.
func TestCompletedKeysRejectsAnotherRun(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "results.jsonl")
	writer, err := newRowWriter(path)
	require.NoError(t, err)
	require.NoError(t, writer.write(sampleRow(scenarioColdPush, 0)))
	require.NoError(t, writer.close())

	_, err = completedKeys(path, "new-run", "unit-cohort")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unit-run")
	assert.Contains(t, err.Error(), "new-run")
}

// TestCompletedKeysRejectsAnotherCohort proves a retained output cannot mix
// another effective spec or harness build into a partial run.
func TestCompletedKeysRejectsAnotherCohort(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "results.jsonl")
	writer, err := newRowWriter(path)
	require.NoError(t, err)
	require.NoError(t, writer.write(sampleRow(scenarioColdPush, 0)))
	require.NoError(t, writer.close())

	_, err = completedKeys(path, "unit-run", "other-cohort")
	require.ErrorContains(t, err, "cohort_id")
}

// TestValidateResultRowsRejectsMixedOrDuplicateSuccesses checks both ways a
// summary could otherwise combine unrelated measurements.
func TestValidateResultRowsRejectsMixedOrDuplicateSuccesses(t *testing.T) {
	t.Parallel()

	mixed := []row{sampleRow(scenarioColdPush, 0), sampleRow(scenarioColdPull, 0)}
	mixed[1].RunID = "other-run"
	require.ErrorContains(t, validateResultRows(mixed), "multiple run_ids")

	mixedCommit := []row{sampleRow(scenarioColdPush, 0), sampleRow(scenarioColdPull, 0)}
	mixedCommit[1].Commit = "fedcba654321"
	require.ErrorContains(t, validateResultRows(mixedCommit), "multiple harness commits")

	mixedCohort := []row{sampleRow(scenarioColdPush, 0), sampleRow(scenarioColdPull, 0)}
	mixedCohort[1].CohortID = "other-cohort"
	require.ErrorContains(t, validateResultRows(mixedCohort), "multiple cohort_ids")

	duplicate := []row{sampleRow(scenarioColdPush, 0), sampleRow(scenarioColdPush, 0)}
	require.ErrorContains(t, validateResultRows(duplicate), "duplicate successful")

	failedThenPassed := []row{sampleRow(scenarioColdPush, 0), sampleRow(scenarioColdPush, 0)}
	failedThenPassed[0].Error = "connection reset"
	failedThenPassed[0].MBPerS = 0
	require.NoError(t, validateResultRows(failedThenPassed))
}

// TestValidateResultRowsAcceptsLegacyRows keeps archived schema-one and
// schema-two result files readable after cohort identity was added.
func TestValidateResultRowsAcceptsLegacyRows(t *testing.T) {
	t.Parallel()

	for _, schema := range []int{1, 2} {
		legacy := sampleRow(scenarioColdPush, 0)
		legacy.Schema = schema
		legacy.CohortID = ""
		if schema == 1 {
			legacy.AttemptID = ""
		}
		require.NoError(t, validateResultRows([]row{legacy}))
	}
}

// TestCompletedKeysRejectsLegacyRows keeps old files summarizable without
// pretending their missing spec fingerprint is safe for resume.
func TestCompletedKeysRejectsLegacyRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	legacy := sampleRow(scenarioColdPush, 0)
	legacy.Schema = 2
	legacy.CohortID = ""
	writer, err := newRowWriter(path)
	require.NoError(t, err)
	require.NoError(t, writer.write(legacy))
	require.NoError(t, writer.close())

	_, err = completedKeys(path, "unit-run", "unit-cohort")
	require.ErrorContains(t, err, "predates cohort identity")
}

// TestCohortIDChangesWithHarnessAndEffectiveSpec checks both inputs that make
// resumed rows one comparable population.
func TestCohortIDChangesWithHarnessAndEffectiveSpec(t *testing.T) {
	t.Parallel()

	spec := &Spec{RunID: "unit-run", Targets: []Target{{Name: "zot", Endpoint: "one"}}}
	first, err := newCohortID(spec, "111111111111")
	require.NoError(t, err)

	otherHarness, err := newCohortID(spec, "222222222222")
	require.NoError(t, err)
	assert.NotEqual(t, first, otherHarness)

	spec.Targets[0].Endpoint = "two"
	otherSpec, err := newCohortID(spec, "111111111111")
	require.NoError(t, err)
	assert.NotEqual(t, first, otherSpec)
}

// TestNewAttemptIDIsPathSafe checks the namespace can be used directly in an
// OCI repository path without normalizing away entropy.
func TestNewAttemptIDIsPathSafe(t *testing.T) {
	t.Parallel()

	id, err := newAttemptID()
	require.NoError(t, err)
	assert.Len(t, id, 16)
	assert.True(t, validPathSegment(id))
}

// TestBuildCommitPrefersTheInjectedRevision guards the paid-run provenance
// override against the VCS revision Go can take from the main worktree.
func TestBuildCommitPrefersTheInjectedRevision(t *testing.T) {
	previous := injectedCommit
	injectedCommit = "abcdef123456"
	t.Cleanup(func() { injectedCommit = previous })

	assert.Equal(t, "abcdef123456", buildCommit())
}

func TestReadRowsRefusesANewerSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "results.jsonl")

	writer, err := newRowWriter(path)
	require.NoError(t, err)
	future := sampleRow(scenarioColdPush, 0)
	future.Schema = rowSchema + 1
	require.NoError(t, writer.write(future))
	require.NoError(t, writer.close())

	_, err = readRows(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema")
}
