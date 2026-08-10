package main

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/componere/bigoci"
)

const (
	// progressPrecision is how finely a progress line reports elapsed time.
	// Tenths of a second is enough to watch a backoff go by and steady enough
	// that the column does not jitter between lines.
	progressPrecision = 100 * time.Millisecond
	// percentScale turns a fraction of the file into the integer a progress
	// line prints.
	percentScale = 100
)

// lineWriter serializes whole lines onto a writer two goroutines share.
//
// It exists because -progress adds the only concurrency this CLI's output has
// ever had: the request tap writes from whichever goroutine made a request,
// and the renderer writes from its own. [os.Stderr] would serialize them
// itself, one Write being one write call on the descriptor, but the tests
// drive the whole program with a [bytes.Buffer] in its place, and a buffer
// serializes nothing. So the guard is here, where both writers can share it,
// rather than in the one of them that happened to need it first.
type lineWriter struct {
	// mu serializes the writes.
	mu sync.Mutex
	// out is the stream the lines go to.
	out io.Writer
}

// newLineWriter returns a writer that hands out whole lines one at a time.
func newLineWriter(out io.Writer) *lineWriter {
	return &lineWriter{out: out}
}

// Write writes p under the lock. Every caller passes one whole line, which is
// what makes the lock enough.
func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.out.Write(p)
}

// watcher is the CLI's whole progress apparatus: the snapshot the library
// hands it, and the goroutine that prints one.
//
// The split is the point, and it is the worked example of what
// [bigoci.ProgressFunc]'s documentation asks for. The library's callback only
// stores a snapshot and returns, so nothing this CLI does can slow a transfer
// down. A goroutine on a clock of its own renders whatever the last stored
// snapshot was, which is also what keeps a line printing through a thirty
// second backoff — a callback-driven printer would go silent through exactly
// the window worth watching.
//
// A nil *watcher is a run that asked for no progress, and its methods do
// nothing, so no caller has to check.
type watcher struct {
	// mu guards latest and seen.
	mu sync.Mutex
	// latest is the last snapshot the library delivered.
	latest bigoci.Progress
	// seen says a snapshot has arrived, before which there is nothing to
	// print and nothing worth printing a placeholder for.
	seen bool

	// out is where the lines go, shared with the request tap.
	out io.Writer
	// now reads the clock the elapsed column is measured on. Tests supply
	// their own, so a rendered line is a fact rather than a timing.
	now func() time.Time
	// started is when the watcher began, the zero of that column.
	started time.Time
	// ticks asks for a line. Production is a [time.Ticker]'s channel; a test
	// sends on one it owns and drives the renderer beat by beat.
	ticks <-chan time.Time
	// stopTicks releases whatever produces ticks.
	stopTicks func()
	// quit tells the renderer to leave.
	quit chan struct{}
	// finished closes when the renderer has left, so the final line is never
	// racing one it was already writing.
	finished chan struct{}
	// once keeps stop to a single run however many paths reach it.
	once sync.Once
}

// newWatcher starts the renderer for one transfer.
//
// The clock and the tick source are handed in rather than built here: a test
// that owns both can assert a rendered line byte for byte and choose exactly
// how many lines a run produces.
func newWatcher(out io.Writer, ticks <-chan time.Time, stopTicks func(), now func() time.Time) *watcher {
	w := &watcher{
		out:       out,
		now:       now,
		started:   now(),
		ticks:     ticks,
		stopTicks: stopTicks,
		quit:      make(chan struct{}),
		finished:  make(chan struct{}),
	}

	go w.render()

	return w
}

// record is the callback the library is given. It stores the snapshot and
// returns, which is the whole of what a progress callback should do.
func (w *watcher) record(p bigoci.Progress) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.latest = p
	w.seen = true
}

// stop ends the renderer and prints the final line.
//
// It runs the instant the transfer returns, and before anything else the run
// still has to say, so the last progress line is the terminal snapshot and
// sits above the summary and the result rather than after them. Waiting for
// the renderer to leave is what keeps that line the last one: a tick being
// served at the moment of the stop finishes first, and then nothing else
// writes.
func (w *watcher) stop() {
	if w == nil {
		return
	}

	w.once.Do(func() {
		close(w.quit)
		<-w.finished
		w.stopTicks()
		w.writeLine()
	})
}

// render prints a line on every tick until the transfer ends.
func (w *watcher) render() {
	defer close(w.finished)

	for {
		select {
		case <-w.quit:
			return
		case <-w.ticks:
			w.writeLine()
		}
	}
}

// writeLine prints one line for the latest snapshot, or nothing at all while
// none has arrived.
//
// Printing nothing is deliberate. Until the first snapshot the run has no
// direction, no phase, and no totals, and a line of zeros would be a claim
// about a transfer that has not started reporting yet.
func (w *watcher) writeLine() {
	w.mu.Lock()
	line, ready := "", w.seen
	if ready {
		line = render(w.latest, w.now().Sub(w.started))
	}
	w.mu.Unlock()

	if ready {
		_, _ = io.WriteString(w.out, line)
	}
}

// render is the progress line: one shape, every field present every time,
// whatever the transfer is doing.
//
// A fixed shape is what makes the line greppable. A gate reads a column out
// of it without knowing which phase the run was in, and two lines are
// comparable field by field. The byte counts are exact rather than rounded to
// units, so `bytes=` compares directly against the preflight line's count.
//
// There is no rate and no estimate. Every honest one needs a window this CLI
// does not keep, and every dishonest one is worse than none on an instrument
// whose whole job is to show what really happened: `wire=` across two lines
// and their `elapsed=` is the throughput, computed by whoever wants it.
func render(p bigoci.Progress, elapsed time.Duration) string {
	return fmt.Sprintf(
		"bigoci: progress %s %s pct=%d parts=%d/%d skipped=%d bytes=%d/%d wire=%d hashed=%d retries=%d elapsed=%s\n",
		p.Direction, p.Phase, percent(p),
		p.CompletedParts, p.TotalParts, p.SkippedParts,
		p.CompletedBytes, p.TotalBytes,
		p.WireBytes, p.HashedBytes, p.Retries,
		elapsed.Round(progressPrecision),
	)
}

// percent is how much of the file is in place, floored to a whole number.
//
// It divides in integers rather than rounding [bigoci.Progress.Fraction], so
// that 100 means every byte is placed and cannot appear one rounding step
// early on a file large enough for a float to lose the difference. Totals a
// pull has not learned yet read zero, which is what Fraction answers too.
func percent(p bigoci.Progress) int {
	if p.TotalBytes <= 0 {
		return 0
	}

	return int(percentScale * p.CompletedBytes / p.TotalBytes)
}

// sharedStderr returns the writer a run's concurrent output goes through:
// stderr itself when no progress line will ever be printed, and a guarded one
// when they will. Nothing else in this CLI writes from two goroutines at
// once, so a run without -progress is left exactly as it was.
func sharedStderr(stderr io.Writer, every time.Duration) io.Writer {
	if every <= 0 {
		return stderr
	}

	return newLineWriter(stderr)
}

// startProgress starts the renderer for a transfer, or returns nil when
// -progress asked for none.
//
// An unset flag and an explicit zero are the same answer here, which is the
// rule -timeout already documents: the flag is how long to wait between
// lines, and no interval is no lines.
func startProgress(e env, out io.Writer, every time.Duration) *watcher {
	if every <= 0 {
		return nil
	}

	if e.ticks != nil {
		return newWatcher(out, e.ticks, func() {}, e.clock())
	}

	ticker := time.NewTicker(every)

	return newWatcher(out, ticker.C, ticker.Stop, time.Now)
}
