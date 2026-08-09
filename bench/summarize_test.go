package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// speedRow returns a successful cold-push row at the given matrix point
// and speed.
func speedRow(registry string, partSize int64, workers int, speed float64) row {
	return row{
		Schema:   rowSchema,
		RunID:    "unit-run",
		CellID:   cellID(registry, partSize, workers, 16<<20),
		Registry: registry,
		Scenario: scenarioColdPush,
		PartSize: partSize,
		Workers:  workers,
		FileSize: 16 << 20,
		Parts:    (16 << 20) / partSize,
		WallMS:   100,
		MBPerS:   speed,
	}
}

func TestSummarizeRendersMedianGrids(t *testing.T) {
	t.Parallel()

	rows := []row{
		speedRow("zot", 4<<20, 1, 100),
		speedRow("zot", 4<<20, 1, 300),
		speedRow("zot", 4<<20, 1, 200),
		speedRow("zot", 8<<20, 1, 400),
	}

	var out strings.Builder
	summarize(rows, &out)
	text := out.String()

	assert.Contains(t, text, "## zot — cold-push — 16MiB file")
	assert.Contains(t, text, "| 4MiB | 200.0 |", "the median of 100, 300, 200 is 200")
	assert.Contains(t, text, "| 8MiB | 400.0 |")
	assert.Contains(t, text, "## All populations")
}

func TestSummarizeMarksFailuresAndAbsences(t *testing.T) {
	t.Parallel()

	allFailed := speedRow("zot", 4<<20, 1, 0)
	allFailed.Error = "boom"
	someFailed := speedRow("zot", 8<<20, 1, 250)
	failedTwin := someFailed
	failedTwin.Error = "boom"
	failedTwin.MBPerS = 0
	visited := speedRow("zot", 8<<20, 4, 500)

	var out strings.Builder
	summarize([]row{allFailed, someFailed, failedTwin, visited}, &out)
	text := out.String()

	assert.Contains(t, text, "| 4MiB | FAIL | — |", "an all-failed cell reads FAIL; an unvisited point reads a dash")
	assert.Contains(t, text, "| 8MiB | 250.0* | 500.0 |", "a partly failed cell is starred")
}

func TestSummarizeCountsThrottles(t *testing.T) {
	t.Parallel()

	throttled := speedRow("ghcr", 4<<20, 8, 90)
	throttled.HTTPStatus = map[string]int{"429": 3, "401": 1}

	var out strings.Builder
	summarize([]row{throttled}, &out)

	require.Contains(t, out.String(), "| 1 | 90.0 | 90.0 | 0.0 | 90.0 | 90.0 | 0 | 3 |",
		"the stats table counts 429s and ignores the auth protocol's own 401s")
}

func TestStatistics(t *testing.T) {
	t.Parallel()

	assert.InDelta(t, 200.0, median([]float64{100, 300, 200}), 0.001)
	assert.InDelta(t, 250.0, median([]float64{100, 300, 200, 400}), 0.001, "an even population averages the middle two")
	assert.InDelta(t, 200.0, mean([]float64{100, 300}), 0.001)
	assert.InDelta(t, 100.0, stddev([]float64{100, 300}), 0.001)
	assert.Zero(t, median(nil))
	assert.Zero(t, stddev([]float64{5}))
}
