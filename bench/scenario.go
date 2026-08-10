package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/componere/bigoci"
)

// iteration runs one repeat of one cell: generate a unique fixture, push it
// cold, optionally re-push it warm, optionally pull it back, and emit one
// row per phase the spec selected.
//
// The cold push always executes even when the spec does not time it,
// because the warm push and the pull both need the artifact in the
// registry. Its row is only emitted when the spec asks for it. A failed
// cold push poisons the phases behind it, so the iteration stops there;
// the row loop above carries on with the next iteration.
type iteration struct {
	// spec is the run's spec, for scenario selection and verify policy.
	spec *Spec
	// cell is the matrix point being measured.
	cell cell
	// number is the repeat, starting at zero.
	number int
	// attemptID names this process attempt's fresh fixture and repository
	// namespace.
	attemptID string
	// cohortID binds emitted rows to the effective spec and harness build.
	cohortID string
	// commit is the human-readable harness revision.
	commit string
	// client is the library client for the cell's target.
	client *bigoci.Client
	// counter watches the client's traffic per timed phase.
	counter *statusCounter
	// skip holds the resume set: rows already measured in the output file.
	skip map[string]bool
	// emit records one finished row.
	emit func(row) error
	// log writes one progress line.
	log func(format string, args ...any)
}

// tagOf names the artifact tags an iteration writes: one for the cold
// push, one for the warm re-push.
func tagOf(scenario string, number int) string {
	return scenario + "-i" + strconv.Itoa(number)
}

// run executes the iteration.
func (it *iteration) run(ctx context.Context) error {
	if it.allRecorded() {
		it.log("cell %s i%d: already recorded, skipping", it.cell.id, it.number)

		return nil
	}

	fixtureDir := it.spec.FixtureDir
	if fixtureDir == "" {
		fixtureDir = os.TempDir()
	}

	src, err := writeFixture(
		fixtureDir,
		it.spec.RunID,
		it.attemptID,
		it.cell.id,
		it.number,
		it.cell.fileSize,
	)
	if err != nil {
		return err
	}
	defer os.Remove(src.path)

	coldRef, err := it.push(ctx, src, scenarioColdPush)
	if err != nil || coldRef == "" {
		return err
	}
	if err := it.warmPush(ctx, src); err != nil {
		return err
	}

	return it.pull(ctx, src, coldRef)
}

// push performs one push and returns its reference. A previously recorded
// cold push still executes as an unrecorded prerequisite when a downstream
// scenario is missing, using this process attempt's fresh namespace.
func (it *iteration) push(ctx context.Context, src fixture, scenario string) (bigoci.Reference, error) {
	ref := it.reference(tagOf(scenario, it.number))

	selected := it.selected(scenario)
	record := selected && !it.skip[it.rowKey(scenario)]
	if selected && !record {
		it.log("cell %s i%d %s: already recorded; running fresh prerequisite", it.cell.id, it.number, scenario)
	}

	timing, pushErr := it.timed(ctx, func(ctx context.Context) error {
		_, err := it.client.Push(ctx, ref, bigoci.FromFile(src.path),
			bigoci.WithPartSize(bigoci.PartSize(it.cell.partSize)),
			bigoci.WithWorkers(it.cell.workers),
		)

		return err
	})

	if record {
		if err := it.emitRow(scenario, timing, pushErr); err != nil {
			return "", err
		}
	}
	if pushErr != nil {
		it.log("cell %s i%d %s FAILED: %v", it.cell.id, it.number, scenario, pushErr)
		if record {
			return "", nil
		}

		return "", fmt.Errorf("unrecorded %s prerequisite failed: %w", scenario, pushErr)
	}

	return ref, nil
}

// warmPush re-pushes the same bytes to a second tag when the spec selected
// the warm-push scenario. Every part is already in the repository, so the
// transfer is the per-part existence checks and a manifest write.
func (it *iteration) warmPush(ctx context.Context, src fixture) error {
	if !it.selected(scenarioWarmPush) {
		return nil
	}
	if it.skip[it.rowKey(scenarioWarmPush)] {
		it.log("cell %s i%d %s: already recorded, skipping", it.cell.id, it.number, scenarioWarmPush)

		return nil
	}

	_, err := it.push(ctx, src, scenarioWarmPush)

	return err
}

// pull pulls the cold artifact to a fresh path when the spec selected the
// cold-pull scenario, verifying the content under the spec's policy.
func (it *iteration) pull(ctx context.Context, src fixture, ref bigoci.Reference) error {
	if !it.selected(scenarioColdPull) {
		return nil
	}
	if it.skip[it.rowKey(scenarioColdPull)] {
		it.log("cell %s i%d %s: already recorded, skipping", it.cell.id, it.number, scenarioColdPull)

		return nil
	}

	dest := filepath.Join(
		filepath.Dir(src.path),
		it.cell.id+"-"+it.attemptID+"-i"+strconv.Itoa(it.number)+".pulled",
	)
	if err := clearPullFiles(dest); err != nil {
		return err
	}
	defer func() {
		if err := clearPullFiles(dest); err != nil {
			it.log("cell %s i%d %s cleanup FAILED: %v", it.cell.id, it.number, scenarioColdPull, err)
		}
	}()

	timing, pullErr := it.timed(ctx, func(ctx context.Context) error {
		err := it.client.Pull(ctx, ref, bigoci.ToFile(dest), bigoci.WithWorkers(it.cell.workers))

		return err
	})

	if pullErr == nil {
		pullErr = it.checkPulled(dest, src)
	}
	if err := it.emitRow(scenarioColdPull, timing, pullErr); err != nil {
		return err
	}
	if pullErr != nil {
		it.log("cell %s i%d %s FAILED: %v", it.cell.id, it.number, scenarioColdPull, pullErr)
	}

	return nil
}

// checkPulled confirms the pulled file matches what was pushed: size on
// every iteration, content under the spec's verification policy. The
// content check compares against the digest computed while the fixture was
// generated, so it costs one read of the pulled file and none of the
// source.
func (it *iteration) checkPulled(dest string, src fixture) error {
	info, err := os.Stat(dest)
	if err != nil {
		return fmt.Errorf("stat pulled file: %w", err)
	}
	if info.Size() != it.cell.fileSize {
		return fmt.Errorf("pulled %d bytes, pushed %d", info.Size(), it.cell.fileSize)
	}

	if !it.spec.verifyIteration(it.number) {
		return nil
	}
	digest, err := hashFile(dest)
	if err != nil {
		return err
	}
	if digest != src.digest {
		return fmt.Errorf("pulled content digest %s does not match pushed %s", digest, src.digest)
	}

	return nil
}

// timing is what a timed phase measures: how long it ran and what the
// registry answered.
type timing struct {
	// wall is the phase's wall-clock duration.
	wall time.Duration
	// statuses counts the trouble responses the phase drew.
	statuses map[string]int
}

// timed runs one phase against a reset counter and measures it.
func (it *iteration) timed(ctx context.Context, phase func(context.Context) error) (timing, error) {
	it.counter.reset()
	start := time.Now()
	err := phase(ctx)

	return timing{wall: time.Since(start), statuses: it.counter.snapshot()}, err
}

// emitRow records one finished phase as a result row.
func (it *iteration) emitRow(scenario string, t timing, phaseErr error) error {
	r := row{
		Schema:     rowSchema,
		RunID:      it.spec.RunID,
		CohortID:   it.cohortID,
		AttemptID:  it.attemptID,
		Timestamp:  time.Now().UTC(),
		CellID:     it.cell.id,
		Registry:   it.cell.target.Name,
		Scenario:   scenario,
		PartSize:   it.cell.partSize,
		Workers:    it.cell.workers,
		FileSize:   it.cell.fileSize,
		Parts:      it.cell.parts(),
		Iteration:  it.number,
		WallMS:     t.wall.Milliseconds(),
		HTTPStatus: t.statuses,
		Commit:     it.commit,
	}
	if phaseErr != nil {
		r.Error = phaseErr.Error()
	} else {
		r.MBPerS = throughput(it.cell.fileSize, t.wall)
		it.log(
			"cell %s i%d %s: %.1f MB/s (%s)",
			it.cell.id,
			it.number,
			scenario,
			r.MBPerS,
			t.wall.Round(time.Millisecond),
		)
	}

	return it.emit(r)
}

// selected reports whether the spec asked for scenario to be timed.
func (it *iteration) selected(scenario string) bool {
	return slices.Contains(it.spec.Scenarios, scenario)
}

// allRecorded reports whether every scenario the spec selected is already
// in the resume set, in which case the iteration has nothing left to do —
// not even fixture generation, which costs real time at large file sizes.
func (it *iteration) allRecorded() bool {
	for _, name := range it.spec.Scenarios {
		if !it.skip[it.rowKey(name)] {
			return false
		}
	}

	return true
}

// rowKey is the resume identity of one scenario of this iteration.
func (it *iteration) rowKey(scenario string) string {
	return measurementKey(it.cell.id, scenario, it.number)
}

// reference builds the full reference for tag in this cell's repository.
func (it *iteration) reference(tag string) bigoci.Reference {
	return bigoci.Reference(
		it.cell.target.Endpoint + "/" + it.cell.repository(it.spec.RunID, it.attemptID) + ":" + tag,
	)
}

// pullPartialSuffix is the sibling suffix documented by bigoci.ToFile.
// The benchmark removes it because a cold measurement must never resume.
const pullPartialSuffix = ".bigoci-partial"

// clearPullFiles removes both the published destination and bigoci's resumable
// partial. Missing paths are already clean and do not fail the benchmark.
func clearPullFiles(dest string) error {
	for _, path := range []string{dest, dest + pullPartialSuffix} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale pull file %s: %w", path, err)
		}
	}

	return nil
}

// throughput derives aggregate decimal megabytes per second from a byte count
// and the whole phase's wall-clock duration.
func throughput(bytes int64, wall time.Duration) float64 {
	seconds := wall.Seconds()
	if seconds <= 0 {
		return 0
	}

	const bytesPerMB = 1e6

	return float64(bytes) / bytesPerMB / seconds
}
