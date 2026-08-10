package main

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci"
)

// progressPrefix is what every progress line starts with. It shares no prefix
// with any other line this CLI writes, which is what lets a recipe grep the
// progress out of a log without disturbing the frozen ones.
const progressPrefix = "bigoci: progress "

// signalTimeout bounds the rendezvous in the tick test. Nothing waits for it
// on the passing path: it only turns a bug that would hang the suite into a
// failure with a message.
const signalTimeout = 30 * time.Second

// TestRenderProgressLine asserts the line byte for byte, which is the whole
// reason render is a pure function of a snapshot and an elapsed time.
//
// Every field is present in every row, whatever the phase and whatever is
// zero. A gate reads a column out of a line without knowing what the transfer
// was doing when it was written, so a shape that varied would be a shape
// nobody could grep.
func TestRenderProgressLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		p       bigoci.Progress
		elapsed time.Duration
		want    string
	}{
		{
			name: "a push part way through",
			p: bigoci.Progress{
				Direction:      bigoci.DirectionPush,
				Phase:          bigoci.PhaseTransferring,
				TotalBytes:     21474836480,
				TotalParts:     40,
				CompletedBytes: 8589934592,
				CompletedParts: 17,
				WireBytes:      9126805504,
				HashedBytes:    21474836480,
				Retries:        1,
			},
			elapsed: 29300 * time.Millisecond,
			want: "bigoci: progress push transferring pct=40 parts=17/40 skipped=0 " +
				"bytes=8589934592/21474836480 wire=9126805504 hashed=21474836480 retries=1 elapsed=29.3s\n",
		},
		{
			name:    "a pull that has not read the manifest yet",
			p:       bigoci.Progress{Direction: bigoci.DirectionPull, Phase: bigoci.PhaseResolving, Retries: 2},
			elapsed: 4 * time.Second,
			want: "bigoci: progress pull resolving pct=0 parts=0/0 skipped=0 " +
				"bytes=?/? wire=? hashed=? retries=2 elapsed=4s\n",
		},
		{
			name: "a warm re-push, every part skipped",
			p: bigoci.Progress{
				Direction:      bigoci.DirectionPush,
				Phase:          bigoci.PhaseFinalizing,
				TotalBytes:     2048,
				TotalParts:     2,
				CompletedBytes: 2048,
				CompletedParts: 2,
				SkippedParts:   2,
				HashedBytes:    2048,
			},
			elapsed: 1500 * time.Millisecond,
			want: "bigoci: progress push finalizing pct=100 parts=2/2 skipped=2 " +
				"bytes=2048/2048 wire=0 hashed=2048 retries=0 elapsed=1.5s\n",
		},
		{
			name: "an empty artifact, finished",
			p: bigoci.Progress{
				Direction:      bigoci.DirectionPush,
				Phase:          bigoci.PhaseDone,
				TotalParts:     1,
				CompletedParts: 1,
			},
			elapsed: 100 * time.Millisecond,
			want: "bigoci: progress push done pct=0 parts=1/1 skipped=0 " +
				"bytes=0/0 wire=0 hashed=0 retries=0 elapsed=100ms\n",
		},
		{
			name: "a transfer that failed",
			p: bigoci.Progress{
				Direction:      bigoci.DirectionPull,
				Phase:          bigoci.PhaseFailed,
				TotalBytes:     3000,
				TotalParts:     3,
				CompletedBytes: 1000,
				CompletedParts: 1,
				WireBytes:      1400,
			},
			elapsed: 2 * time.Minute,
			want: "bigoci: progress pull failed pct=33 parts=1/3 skipped=0 " +
				"bytes=?/? wire=? hashed=? retries=0 elapsed=2m0s\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, render(tt.p, tt.elapsed))
		})
	}
}

// TestPercentIsFloored checks the one number on the line that is computed
// rather than copied, and the promise it carries: 100 means every byte is
// placed, and nothing short of that rounds up to it.
func TestPercentIsFloored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    bigoci.Progress
		want int
	}{
		{name: "nothing known yet", p: bigoci.Progress{}, want: 0},
		{name: "nothing placed", p: bigoci.Progress{TotalBytes: 300}, want: 0},
		{name: "a third", p: bigoci.Progress{TotalBytes: 3, CompletedBytes: 1}, want: 33},
		{name: "just short of everything", p: bigoci.Progress{TotalBytes: 1000, CompletedBytes: 999}, want: 99},
		{
			name: "one byte short of a very large file",
			p:    bigoci.Progress{TotalBytes: 1 << 55, CompletedBytes: (1 << 55) - 1},
			want: 99,
		},
		{name: "everything", p: bigoci.Progress{TotalBytes: 1000, CompletedBytes: 1000}, want: 100},
		{name: "an empty artifact", p: bigoci.Progress{TotalParts: 1, CompletedParts: 1}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, percent(tt.p))
		})
	}
}

// TestProgressUnsetChangesNothing is the flag's own unset-means-absent rule,
// checked at the only place it can be: the bytes the program wrote.
func TestProgressUnsetChangesNothing(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	src, _ := fixture(t)

	for _, args := range [][]string{nil, {"-progress", "0"}} {
		got := pushFixture(t, reg, src, args...)
		require.Equal(t, exitOK, got.code, got.stderr)
		assert.NotContains(t, got.stderr, progressPrefix, "an unset -progress must print nothing extra")
	}
}

// TestProgressPrintsOneFinalLineWithNoTicks is the driven-ticker run: the
// ticker never fires, so the only line is the one stop prints, and it is the
// terminal snapshot.
//
// It is also where the whole line is asserted against a real transfer rather
// than a hand-built snapshot, so the counters are the library's own. A cold
// push of the fixture sends every byte once and skips nothing, and the byte
// count is the same one the preflight line above it reports.
func TestProgressPrintsOneFinalLineWithNoTicks(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	src, _ := fixture(t)

	got := runProgress(t, make(chan time.Time), clockAfter(1500*time.Millisecond),
		cmdPush, "-plain-http", "-part-size", fixturePartSize, "-progress", "1s",
		src, reg.taggedRef(fakeTag),
	)
	require.Equal(t, exitOK, got.code, got.stderr)

	assert.Equal(t, []string{
		"bigoci: progress push done pct=100 parts=4/4 skipped=0 " +
			"bytes=204800/204800 wire=204800 hashed=204800 retries=0 elapsed=1.5s",
	}, progressLines(got.stderr))
	assert.Contains(t, got.stdout, "sha256:", "the digest still goes to stdout, untouched by the progress lines")
	assert.NotContains(t, got.stdout, progressPrefix, "no progress line may ever reach stdout")
}

// TestProgressLineComesBeforeEverythingSaidAfterTheTransfer pins the ordering
// stop exists to guarantee: the last thing said about the transfer while it
// ran sits above the summary of what it did and the line saying it finished.
func TestProgressLineComesBeforeEverythingSaidAfterTheTransfer(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	src, _ := fixture(t)

	got := runProgress(t, make(chan time.Time), clockAfter(time.Second),
		cmdPush, "-plain-http", "-part-size", fixturePartSize, "-debug", "-progress", "1s",
		src, reg.taggedRef(fakeTag),
	)
	require.Equal(t, exitOK, got.code, got.stderr)

	progress := strings.Index(got.stderr, progressPrefix)
	summary := strings.Index(got.stderr, "bigoci: http requests=")
	result := strings.Index(got.stderr, "bigoci: pushed ")

	require.Positive(t, progress)
	require.Positive(t, summary)
	require.Positive(t, result)
	assert.Less(t, progress, summary, "the final progress line comes before the request summary")
	assert.Less(t, summary, result, "the request summary still comes before the result line")
}

// TestProgressReportsAWarmRePushAsSkipped is the half-skipped push read from
// the outside: every part complete, every one of them skipped, and not a byte
// on the wire. It is the line a gate greps to prove a re-push moved nothing.
func TestProgressReportsAWarmRePushAsSkipped(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	src, _ := fixture(t)

	require.Equal(t, exitOK, pushFixture(t, reg, src).code)

	got := runProgress(t, make(chan time.Time), clockAfter(2*time.Second),
		cmdPush, "-plain-http", "-part-size", fixturePartSize, "-progress", "1s",
		src, reg.taggedRef(fakeTag),
	)
	require.Equal(t, exitOK, got.code, got.stderr)

	assert.Equal(t, []string{
		"bigoci: progress push done pct=100 parts=4/4 skipped=4 " +
			"bytes=204800/204800 wire=0 hashed=204800 retries=0 elapsed=2s",
	}, progressLines(got.stderr))
}

// TestProgressReportsAPull checks the other direction end to end: a pull's
// bytes all come off the wire and none off the disk, which is the pair of
// columns that tells a cold pull from a resume.
func TestProgressReportsAPull(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	src, _ := fixture(t)

	push := pushFixture(t, reg, src)
	require.Equal(t, exitOK, push.code, push.stderr)
	dgst := strings.TrimSuffix(push.stdout, "\n")

	dest := t.TempDir() + "/out.bin"
	got := runProgress(t, make(chan time.Time), clockAfter(900*time.Millisecond),
		cmdPull, "-plain-http", "-progress", "500ms", reg.digestRef(dgst), dest,
	)
	require.Equal(t, exitOK, got.code, got.stderr)

	assert.Equal(t, []string{
		"bigoci: progress pull done pct=100 parts=4/4 skipped=0 " +
			"bytes=?/? wire=? hashed=? retries=0 elapsed=900ms",
	}, progressLines(got.stderr))
}

// TestProgressReportsAFailedTransfer checks that a run that ends badly still
// ends with one line, and that the line says so.
//
// Which counters it carries depends on how far the workers got, so only the
// phase is asserted: what matters is that the renderer stops with a terminal
// snapshot rather than leaving the last mid-transfer line as the final word.
func TestProgressReportsAFailedTransfer(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	src, _ := fixture(t)
	reg.refuseUploads()

	got := runProgress(t, make(chan time.Time), clockAfter(time.Second),
		cmdPush, "-plain-http", "-part-size", fixturePartSize, "-progress", "1s",
		src, reg.taggedRef(fakeTag),
	)
	require.NotEqual(t, exitOK, got.code)

	lines := progressLines(got.stderr)
	require.Len(t, lines, 1, "a failed transfer still prints exactly one final progress line")
	assert.Contains(t, lines[0], "bigoci: progress push failed pct=")
	assert.Contains(t, lines[0], "elapsed=1s")
}

// TestProgressTicksPrintWhileTheTransferRuns proves the renderer really is
// driven by its ticker, and not only by the final line stop prints.
//
// Getting that to be a claim a test can falsify takes a rendezvous. A tick
// sent before the first snapshot arrives renders nothing, and a tick sent
// after the transfer ends renders nothing either, so a test that just buffers
// ticks and hopes is a test that passes on the final line alone — whether the
// ticker arm exists or not. Instead the transfer is held at its first
// request, which is a moment when the opening snapshot has provably been
// delivered and the push has provably not finished. One tick goes out there,
// and the registry does not answer until that tick has reached the stream as
// a whole line.
func TestProgressTicksPrintWhileTheTransferRuns(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	src, _ := fixture(t)

	ticks := make(chan time.Time)
	stderr := &signalingWriter{on: progressPrefix, signal: make(chan struct{})}

	// Both waits are bounded, and that is not ceremony. A renderer that has
	// stopped serving ticks would otherwise leave this handler blocked on a
	// send nobody receives and hang the suite instead of failing it.
	reg.holdFirstRequest(func() {
		select {
		case ticks <- time.Unix(0, 0):
		case <-time.After(signalTimeout):
			assert.Fail(t, "the renderer never took a tick while the push was running")

			return
		}

		awaitSignal(t, stderr.signal, "the tick produced no progress line while the push was still running")
	})

	var stdout bytes.Buffer
	code := run(t.Context(), env{
		args: []string{
			cmdPush, "-plain-http", "-part-size", fixturePartSize, "-progress", "1s",
			src, reg.taggedRef(fakeTag),
		},
		stdout: &stdout,
		stderr: stderr,
		ticks:  ticks,
		now:    clockAfter(time.Second),
	}, nil)
	require.Equal(t, exitOK, code, stderr.String())

	lines := progressLines(stderr.String())
	require.GreaterOrEqual(t, len(lines), 2, "the tick and the final line are two lines, not one")

	last := lines[len(lines)-1]
	assert.Contains(t, last, "push done", "the terminal snapshot must be the last line")

	running := lines[:len(lines)-1]
	require.NotEmpty(t, running, "a line must have been printed while the push was still running")
	for i, line := range running {
		assert.NotContains(t, line, "push done", "line %d is terminal but is not last", i)
		assert.NotContains(t, line, "push failed", "line %d is terminal but is not last", i)
	}

	for i, line := range lines {
		assert.Regexp(t,
			`^bigoci: progress push (transferring|finalizing|done) pct=\d+ parts=\d+/4 skipped=\d+ `+
				`bytes=\d+/204800 wire=\d+ hashed=\d+ retries=0 elapsed=1s$`,
			line, "line %d does not have the progress grammar", i)
	}
}

// TestProgressPrintsNothingWhenTheLibraryReportedNothing is the CLI end of
// the library's zero-call contract.
//
// A malformed reference fails before there is a transfer to report on, so no
// snapshot ever arrives. The renderer has nothing to print and prints nothing
// — not a line of zeros, and not a line claiming a direction and a phase the
// run never had.
func TestProgressPrintsNothingWhenTheLibraryReportedNothing(t *testing.T) {
	t.Parallel()

	got := runProgress(t, make(chan time.Time), clockAfter(time.Second),
		cmdPush, "-progress", "1s", "model.bin", "not a reference",
	)

	assert.Equal(t, exitFailure, got.code)
	assert.Empty(t, progressLines(got.stderr), "a run the library never reported on prints no progress line")
	assert.Contains(t, got.stderr, "no sentinel matched (exit 1)")
}

// TestValidateProgress checks the one -progress value no run can use, and the
// explicit zero beside it that means the same as leaving the flag alone.
func TestValidateProgress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		progress time.Duration
		set      bool
		wantErr  string
	}{
		{name: "left alone", progress: 0, set: false},
		{name: "explicitly off", progress: 0, set: true},
		{name: "a real interval", progress: 5 * time.Second, set: true},
		{
			name:     "negative",
			progress: -time.Second,
			set:      true,
			wantErr:  "pull: -progress must not be negative, got -1s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			set := map[string]bool{}
			if tt.set {
				set[flagProgress] = true
			}

			c := commonFlags{progress: tt.progress}
			err := c.validate(set, cmdPull, pullUsage())
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.EqualError(t, err, tt.wantErr)

			var usage *usageError
			require.ErrorAs(t, err, &usage, "a refused interval must exit 2 and print the usage block")
			assert.Contains(t, usage.usage, "usage: bigoci pull")
		})
	}
}

// TestProgressFlagIsRefusedNegativeFromACommandLine is the same guard from
// the outside, where it is worth two bytes of exit code.
func TestProgressFlagIsRefusedNegativeFromACommandLine(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{cmdPush, "-progress", "-1s", "model.bin", "reg/repo:v1"},
		{cmdPull, "-progress", "-1s", "reg/repo:v1", "out.bin"},
	} {
		got := runCLI(t, args...)
		assert.Equal(t, exitUsage, got.code)
		assert.Empty(t, got.stdout)
		assert.Contains(t, got.stderr, "-progress must not be negative, got -1s")
		assert.Contains(t, got.stderr, "usage:")
	}
}

// TestLineWriterKeepsLinesWhole checks the guard the tap and the renderer
// share: whole lines, one at a time, onto a writer that is not concurrency
// safe on its own.
func TestLineWriterKeepsLinesWhole(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newLineWriter(&buf)

	const writers, each = 8, 50

	done := make(chan struct{})
	for i := range writers {
		go func() {
			defer func() { done <- struct{}{} }()

			line := strings.Repeat(string(rune('a'+i)), 40) + "\n"
			for range each {
				_, _ = w.Write([]byte(line))
			}
		}()
	}
	for range writers {
		<-done
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	require.Len(t, lines, writers*each)
	for i, line := range lines {
		assert.Len(t, line, 40, "line %d was interleaved with another writer's", i)
	}
}

// signalingWriter is a buffer that says when a line it was handed starts with
// a prefix, so a test can wait for output instead of sleeping for it.
//
// It stands where the run's standard error goes, under the lock the CLI wraps
// it in when progress is on, so it is handed one whole line at a time. The
// signal is a close rather than a send: the writer must never block, because
// the goroutine writing through it is the one the waiter is waiting for.
type signalingWriter struct {
	// mu guards buf against the renderer and whoever reads it back.
	mu sync.Mutex
	// buf holds everything written.
	buf bytes.Buffer
	// on is the line prefix worth signalling.
	on string
	// signal closes when a line starting with on has been written.
	signal chan struct{}
	// once keeps the close to one, however many matching lines arrive.
	once sync.Once
}

// Write records the line and signals when it is the one being waited for.
func (w *signalingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()

	if strings.HasPrefix(string(p), w.on) {
		w.once.Do(func() { close(w.signal) })
	}

	return n, err
}

// String returns everything written so far.
func (w *signalingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.String()
}

// awaitSignal waits for ch, failing the test with why rather than hanging the
// suite if the thing it is waiting for never happens.
func awaitSignal(t *testing.T, ch <-chan struct{}, why string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(signalTimeout):
		assert.Fail(t, why)
	}
}

// runProgress runs one command line in process with the progress clock and
// tick source under the test's control, so a rendered line is a fact rather
// than a timing and the number of them is the test's choice.
func runProgress(t *testing.T, ticks <-chan time.Time, now func() time.Time, args ...string) result {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), env{args: args, stdout: &stdout, stderr: &stderr, ticks: ticks, now: now}, nil)

	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// clockAfter returns a clock reading zero the first time it is asked and
// elapsed every time after.
//
// The first read is the watcher's own start, and every later one renders a
// line, so every line a run prints carries exactly elapsed and can be
// asserted byte for byte.
func clockAfter(elapsed time.Duration) func() time.Time {
	base := time.Unix(0, 0)

	var reads atomic.Int64

	return func() time.Time {
		if reads.Add(1) == 1 {
			return base
		}

		return base.Add(elapsed)
	}
}

// progressLines returns the progress lines of a run's standard error, without
// their trailing newlines and in the order they were written.
func progressLines(stderr string) []string {
	var found []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(line, progressPrefix) {
			found = append(found, line)
		}
	}

	return found
}
