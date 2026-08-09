package main

import (
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

	keys, err := completedKeys(path)
	require.NoError(t, err)

	assert.True(t, keys["zot-p4MiB-w1-f16MiB|cold-push|0"], "a success is skipped on resume")
	assert.False(t, keys["zot-p4MiB-w1-f16MiB|cold-pull|0"], "a failure is measured again on resume")
}

func TestCompletedKeysTreatsAMissingFileAsAFreshRun(t *testing.T) {
	t.Parallel()

	keys, err := completedKeys(filepath.Join(t.TempDir(), "absent.jsonl"))
	require.NoError(t, err)
	assert.Empty(t, keys)
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
