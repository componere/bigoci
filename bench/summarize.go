package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
)

// runSummarize executes the summarize subcommand: read a JSONL results
// file and render it as Markdown.
func runSummarize(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bench summarize", flag.ContinueOnError)
	flags.SetOutput(stderr)
	inPath := flags.String("in", "", "path of the JSONL results file to read (required)")

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if *inPath == "" {
		fmt.Fprintln(stderr, "bench summarize: -in is required")

		return exitUsage
	}

	rows, err := readRows(*inPath)
	if err != nil {
		fmt.Fprintf(stderr, "bench summarize: %v\n", err)

		return exitFailure
	}
	if len(rows) == 0 {
		fmt.Fprintf(stderr, "bench summarize: %s holds no rows\n", *inPath)

		return exitFailure
	}
	if err := validateResultRows(rows); err != nil {
		fmt.Fprintf(stderr, "bench summarize: %v\n", err)

		return exitFailure
	}

	summarize(rows, stdout)

	return exitOK
}

// group is one summarized population: every measurement that shares a
// registry, scenario, and full transfer shape.
type group struct {
	// registry, scenario, partSize, workers, fileSize, and parts identify
	// the population; maxActive is derived from workers and parts.
	registry  string
	scenario  string
	partSize  int64
	workers   int
	maxActive int
	fileSize  int64
	parts     int64
	// speeds holds the successful measurements' throughput, in MB/s.
	speeds []float64
	// failures counts the error rows.
	failures int
	// throttles counts 429 and 503 answers across the population's rows.
	throttles int
}

// summarize renders rows: one median-throughput grid per registry, file
// size, and scenario, then one long-form statistics table over everything.
func summarize(rows []row, w io.Writer) {
	groups := groupRows(rows)

	for _, g := range gridOrder(groups) {
		writeGrid(w, g, groups)
	}
	writeStats(w, groups)
}

// gridKey names one grid: a registry, file size, and scenario that appear
// together in the results.
type gridKey struct {
	// registry and scenario mirror the group fields; fileSize is the
	// transfer size the grid's cells share.
	registry string
	scenario string
	fileSize int64
}

// groupRows folds rows into their populations.
func groupRows(rows []row) []*group {
	index := map[string]*group{}
	var groups []*group
	for _, r := range rows {
		key := r.Registry + "|" + r.Scenario + "|" + strconv.FormatInt(r.PartSize, 10) +
			"|" + strconv.Itoa(r.Workers) + "|" + strconv.FormatInt(r.FileSize, 10)
		g, ok := index[key]
		if !ok {
			g = &group{
				registry:  r.Registry,
				scenario:  r.Scenario,
				partSize:  r.PartSize,
				workers:   r.Workers,
				maxActive: maxActiveWorkers(r.Workers, r.Parts),
				fileSize:  r.FileSize,
				parts:     r.Parts,
			}
			index[key] = g
			groups = append(groups, g)
		}

		if r.Error != "" {
			g.failures++
		} else {
			g.speeds = append(g.speeds, r.MBPerS)
		}
		g.throttles += r.HTTPStatus["429"] + r.HTTPStatus["503"]
	}

	return groups
}

// gridOrder returns the grids to render, in first-appearance order of
// registry, then ascending file size, then pipeline order of scenario.
func gridOrder(groups []*group) []gridKey {
	var keys []gridKey
	for _, g := range groups {
		key := gridKey{registry: g.registry, scenario: g.scenario, fileSize: g.fileSize}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}

	registries := make(map[string]int)
	for _, g := range groups {
		if _, ok := registries[g.registry]; !ok {
			registries[g.registry] = len(registries)
		}
	}

	slices.SortStableFunc(keys, func(a, b gridKey) int {
		if c := registries[a.registry] - registries[b.registry]; c != 0 {
			return c
		}
		if a.fileSize != b.fileSize {
			return int(a.fileSize - b.fileSize)
		}

		return scenarioRank(a.scenario) - scenarioRank(b.scenario)
	})

	return keys
}

// Scenario ranks, in the order an iteration runs its phases.
const (
	// rankColdPush sorts first: the phase every iteration starts with.
	rankColdPush = iota
	// rankWarmPush sorts between the pushes and the pull.
	rankWarmPush
	// rankColdPull sorts last.
	rankColdPull
)

// scenarioRank orders scenarios the way an iteration runs them.
func scenarioRank(name string) int {
	switch name {
	case scenarioColdPush:
		return rankColdPush
	case scenarioWarmPush:
		return rankWarmPush
	default:
		return rankColdPull
	}
}

// writeGrid renders one median-throughput grid: part sizes down, worker
// counts across.
func writeGrid(w io.Writer, key gridKey, groups []*group) {
	var cells []*group
	for _, g := range groups {
		if (gridKey{registry: g.registry, scenario: g.scenario, fileSize: g.fileSize}) == key {
			cells = append(cells, g)
		}
	}

	partSizes := axisValues(cells, func(g *group) int64 { return g.partSize })
	workers := axisValues(cells, func(g *group) int { return g.workers })

	fmt.Fprintf(w, "## %s — %s — %s file\n\n", key.registry, key.scenario, formatSize(key.fileSize))
	fmt.Fprintf(
		w,
		"Median aggregate MB/s. Columns are configured workers; capped cells show their maximum active workers.\n\n",
	)

	var header, rule strings.Builder
	header.WriteString("| part \\ workers |")
	rule.WriteString("|---|")
	for _, count := range workers {
		header.WriteString(" " + strconv.Itoa(count) + " |")
		rule.WriteString("---|")
	}
	fmt.Fprintln(w, header.String())
	fmt.Fprintln(w, rule.String())

	for _, partSize := range partSizes {
		var line strings.Builder
		line.WriteString("| " + formatSize(partSize) + " |")
		for _, count := range workers {
			line.WriteString(" " + gridCell(cells, partSize, count) + " |")
		}
		fmt.Fprintln(w, line.String())
	}
	fmt.Fprintln(w)
}

// gridCell renders one grid cell: the population's median throughput, a
// failure marker when every measurement failed, or a dash when the matrix
// never visited the point.
func gridCell(cells []*group, partSize int64, workers int) string {
	for _, g := range cells {
		if g.partSize != partSize || g.workers != workers {
			continue
		}
		if len(g.speeds) == 0 {
			return cappedCell("FAIL", g)
		}
		text := strconv.FormatFloat(median(g.speeds), 'f', 1, 64)
		if g.failures > 0 {
			text += "*"
		}

		return cappedCell(text, g)
	}

	return "—"
}

// cappedCell annotates a grid cell when the requested worker count exceeds
// the number of parts available to schedule.
func cappedCell(text string, g *group) string {
	if g.maxActive < g.workers {
		return text + " (" + strconv.Itoa(g.maxActive) + " max)"
	}

	return text
}

// writeStats renders the long-form table over every population.
func writeStats(w io.Writer, groups []*group) {
	fmt.Fprintln(w, "## All populations")
	fmt.Fprintln(w)
	fmt.Fprintln(
		w,
		"| registry | scenario | part | configured | max active | file | parts | n | median | mean | stddev | min | max | fail | 429/503 |",
	)
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")

	for _, g := range groups {
		fmt.Fprintf(w, "| %s | %s | %s | %d | %d | %s | %d | %d | %s | %s | %s | %s | %s | %d | %d |\n",
			g.registry, g.scenario, formatSize(g.partSize), g.workers, g.maxActive, formatSize(g.fileSize), g.parts,
			len(g.speeds), mbs(median(g.speeds)), mbs(mean(g.speeds)), mbs(stddev(g.speeds)),
			mbs(slices.Min(orZero(g.speeds))), mbs(slices.Max(orZero(g.speeds))), g.failures, g.throttles,
		)
	}
	fmt.Fprintln(w)
}

// axisValues collects the distinct values one axis takes across cells, in
// ascending order.
func axisValues[V int | int64](cells []*group, value func(*group) V) []V {
	var values []V
	for _, g := range cells {
		if !slices.Contains(values, value(g)) {
			values = append(values, value(g))
		}
	}
	slices.Sort(values)

	return values
}

// orZero substitutes a single zero for an empty population, so min and max
// have something to reduce.
func orZero(speeds []float64) []float64 {
	if len(speeds) == 0 {
		return []float64{0}
	}

	return speeds
}

// mbs renders a throughput with one decimal.
func mbs(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// half is the divisor the median and even-population averaging share.
const half = 2

// median returns the middle of the population, zero when it is empty.
func median(speeds []float64) float64 {
	if len(speeds) == 0 {
		return 0
	}
	sorted := slices.Clone(speeds)
	slices.Sort(sorted)
	mid := len(sorted) / half
	if len(sorted)%half == 0 {
		return (sorted[mid-1] + sorted[mid]) / half
	}

	return sorted[mid]
}

// mean returns the population's average, zero when it is empty.
func mean(speeds []float64) float64 {
	if len(speeds) == 0 {
		return 0
	}
	total := 0.0
	for _, v := range speeds {
		total += v
	}

	return total / float64(len(speeds))
}

// minSpread is the smallest population a spread statistic means anything
// for.
const minSpread = 2

// stddev returns the population standard deviation, zero when fewer than
// two measurements exist.
func stddev(speeds []float64) float64 {
	if len(speeds) < minSpread {
		return 0
	}
	avg := mean(speeds)
	sum := 0.0
	for _, v := range speeds {
		sum += (v - avg) * (v - avg)
	}

	return math.Sqrt(sum / float64(len(speeds)))
}
