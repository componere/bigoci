package transfer

import (
	"context"
	"io"
	"sync"

	"github.com/imgoci/bigoci/internal/retry"
)

// progressStep is how many bytes of movement pile up before they are worth a
// snapshot of their own.
//
// The threshold counts bytes rather than time because the core reads no clock
// and performs no I/O: a rate limiter here would be a side effect in the one
// place bigoci keeps free of them. Four MiB is small enough that a slow link
// still reports often and large enough that a fast one does not spend the
// transfer inside somebody's callback — a 512 MiB part is roughly a hundred
// snapshots. Milestones ignore the threshold entirely.
const progressStep = 4 << 20

// Phase says how far along a transfer is. It only ever advances, and it is
// the field that says whether a transfer is over: byte counts that have
// reached their totals say nothing about whether the manifest was written.
type Phase uint8

const (
	// PhaseResolving is a pull fetching and decoding the manifest. A push
	// never reports it, because a push knows what it is moving before it
	// starts.
	PhaseResolving Phase = iota + 1
	// PhaseTransferring is parts moving: the hash pass and the uploads of a
	// push, the resume verify and the fetches of a pull.
	PhaseTransferring
	// PhaseFinalizing is the work left after the parts: the empty config blob
	// and the manifest write of a push, the commit of a pull.
	PhaseFinalizing
	// PhaseDone is a transfer that succeeded. Exactly one snapshot carries
	// it, and it is the last one delivered.
	PhaseDone
	// PhaseFailed is a transfer that failed. Exactly one snapshot carries it,
	// and it is the last one delivered; why it failed is the error [Push] or
	// [Pull] returned.
	PhaseFailed
)

// Snapshot is one absolute account of a transfer, not a delta and not an
// event: every field is the running total at the moment the snapshot left the
// orchestrator, and every one of them only ever grows.
//
// The two byte counters are the reason this is worth reporting at all.
// CompletedBytes is how much of the file is provably where it belongs, and
// WireBytes is how much had to cross the registry boundary to put it there.
// They agree when nothing goes wrong and diverge honestly the moment
// something does.
//
// Snapshots carry no direction, because the core does not have one: [Push]
// and [Pull] report through the same type and whoever adapts it knows which
// call it came from.
type Snapshot struct {
	// Phase is how far along the transfer is.
	Phase Phase
	// TotalBytes is the length of the file being moved. A push knows it on
	// every snapshot; a pull reports zero until the manifest is decoded.
	TotalBytes int64
	// TotalParts is how many parts the file splits into. It counts parts and
	// not blobs, so two parts that share a digest still count twice.
	TotalParts int
	// CompletedBytes is how many of the file's bytes are provably in their
	// final place. It moves in whole parts, once per part.
	CompletedBytes int64
	// CompletedParts is how many parts are provably in their final place.
	CompletedParts int
	// SkippedParts is how many of those parts got there without this transfer
	// moving their own bytes over the wire.
	SkippedParts int
	// WireBytes is how many bytes crossed the registry boundary. A retry that
	// re-sends a part counts twice, so this is unbounded against TotalBytes.
	WireBytes int64
	// HashedBytes is how many bytes were read and hashed locally: the push
	// hash pass, and a pull's verify of a partial file it is resuming into.
	HashedBytes int64
	// Retries is how many times some unit of work re-entered its retry budget
	// after the first attempt.
	Retries int
}

// Report receives the snapshots of one transfer.
//
// Calls never overlap and arrive in order, because the orchestrator holds one
// lock across every one of them — which also means a slow Report slows the
// transfer down.
type Report func(Snapshot)

// reporter is the accounting one watched transfer runs: the snapshot being
// filled in, the lock that keeps it whole, and the latch that stops it after
// the transfer has ended.
//
// A nil *reporter is the transfer nobody is watching, and every method here
// takes one: that is what keeps the accounting out of an unwatched transfer
// without a conditional at each of the twenty places one is recorded.
//
// The lock is held across the callback rather than around a copy of the
// snapshot. Serializing the calls is most of the contract — a consumer that
// may be handed two snapshots at once has to lock, and one that may be handed
// them out of order cannot draw a bar — and atomics would buy back the
// concurrency at the price of tearing a snapshot across a read.
type reporter struct {
	// mu guards every field below and is held across each call to fn.
	mu sync.Mutex
	// fn is the callback the snapshots go to. It is never nil: a nil callback
	// is a nil *reporter instead.
	fn Report
	// snap is the running account, delivered by value so nothing a consumer
	// keeps can alias it.
	snap Snapshot
	// credited records which part indexes have already been counted, so a
	// part cannot be credited twice however many times it is reported.
	credited []bool
	// pending is how many bytes have moved since the last snapshot went out,
	// which is what the coalescing threshold is measured against.
	pending int64
	// closed is the latch: once the terminal snapshot has been delivered,
	// every later recording is dropped. It is what makes "nothing arrives
	// after the transfer returns" true even though a straggling read on
	// [net/http]'s write goroutine can still be counting bytes.
	closed bool
}

// newReporter returns the reporter a transfer records through, or nil when fn
// is nil and there is nothing to record.
func newReporter(fn Report) *reporter {
	if fn == nil {
		return nil
	}

	return &reporter{fn: fn}
}

// begin delivers the first snapshot of a transfer, in phase p and with
// whatever totals are known: both of a push's, neither of a pull's, which
// learns them from a manifest it has not fetched yet.
func (r *reporter) begin(p Phase, totalBytes int64, totalParts int) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.snap.Phase = p
	r.setTotals(totalBytes, totalParts)
	r.deliver()
}

// measured records the totals a pull read out of the manifest and moves it
// into [PhaseTransferring].
//
// The two happen together, in one snapshot rather than two, because they are
// one event: the pull now knows what it is moving, and is about to start
// moving it. It is also the only time totals ever change.
func (r *reporter) measured(totalBytes int64, totalParts int) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.snap.Phase = PhaseTransferring
	r.setTotals(totalBytes, totalParts)
	r.deliver()
}

// finalizing moves the transfer into [PhaseFinalizing] and delivers a
// snapshot saying so: the parts are all in place and what is left is the tail
// work, the empty config blob and the manifest for a push, the commit for a
// pull.
//
// It is a milestone, and it is the one that matters most to a watcher. A
// transfer sitting at every byte moved is either writing its manifest or
// hung, and this is what tells the two apart.
func (r *reporter) finalizing() {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.snap.Phase = PhaseFinalizing
	r.deliver()
}

// complete credits the part at index as provably in its final place, and
// skipped says this transfer moved none of that part's own bytes to put it
// there.
//
// It counts a part at most once however often it is called. The ledger the
// callers keep should already guarantee that; this is the belt to its braces,
// and it is cheap because the transfer already knows how many parts there
// are.
func (r *reporter) complete(index int, size int64, skipped bool) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || index < 0 || index >= len(r.credited) || r.credited[index] {
		return
	}

	r.credited[index] = true
	r.snap.CompletedParts++
	r.snap.CompletedBytes += size
	if skipped {
		r.snap.SkippedParts++
	}

	r.deliver()
}

// completeTwins credits the count parts that shared a digest with one a
// worker has just uploaded and were waiting on it, each of them size bytes
// and every one of them skipped, because none moved a byte of its own.
//
// Their indexes are not tracked, and deliberately: what the claim ledger
// knows about a waiting worker is that it is waiting, and the worker that
// settles the digest is the only one in a position to credit them. The ledger
// hands each waiter over exactly once, which is the guarantee the index would
// otherwise be providing.
func (r *reporter) completeTwins(count int, size int64) {
	if r == nil || count <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.snap.CompletedParts += count
	r.snap.SkippedParts += count
	r.snap.CompletedBytes += size * int64(count)

	r.deliver()
}

// wire records n bytes that crossed the registry boundary.
func (r *reporter) wire(n int64) {
	if r == nil || n <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.snap.WireBytes += n
	r.coalesce(n)
}

// hashed records n bytes read off the local file and hashed.
func (r *reporter) hashed(n int64) {
	if r == nil || n <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.snap.HashedBytes += n
	r.coalesce(n)
}

// retried records one re-entry into a retry budget.
//
// It is a milestone rather than a coalesced count because it is the whole of
// what a watcher can see during a backoff: nothing else moves, and a line
// that repeats with a climbing retry count is how a wait looks from outside.
func (r *reporter) retried() {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.snap.Retries++
	r.deliver()
}

// finish delivers the terminal snapshot — [PhaseDone] when err is nil,
// [PhaseFailed] otherwise — and closes the reporter against everything after
// it.
//
// The close is what makes the contract enforceable rather than aspirational.
// A push hands the reader it uploads from to [net/http], which reads it on a
// goroutine of its own, so a read can still be counting bytes after the call
// that started it has returned. Closing under the same lock the callback runs
// under means such a straggler either recorded before the terminal snapshot
// or is dropped, and never lands after it.
func (r *reporter) finish(err error) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.closed = true
	r.snap.Phase = PhaseDone
	if err != nil {
		r.snap.Phase = PhaseFailed
	}

	r.deliver()
}

// setTotals records what the transfer is moving and sizes the ledger of
// credited parts to it. The caller holds the lock.
func (r *reporter) setTotals(totalBytes int64, totalParts int) {
	r.snap.TotalBytes = totalBytes
	r.snap.TotalParts = totalParts

	if totalParts > 0 {
		r.credited = make([]bool, totalParts)
	}
}

// coalesce counts n bytes toward the threshold and delivers a snapshot once
// enough of them have piled up. The caller holds the lock.
func (r *reporter) coalesce(n int64) {
	r.pending += n
	if r.pending >= progressStep {
		r.deliver()
	}
}

// deliver hands the current snapshot to the callback and resets the
// coalescing threshold, so a milestone and the bytes before it are one
// report and not two. The caller holds the lock.
func (r *reporter) deliver() {
	r.pending = 0
	r.fn(r.snap)
}

// wireWriter credits everything written through it as bytes that crossed the
// registry boundary. It takes every chunk whole and never fails, so putting
// one at the end of an [io.MultiWriter] counts exactly the chunks every
// writer before it accepted.
type wireWriter struct {
	// to is the reporter the bytes are credited to.
	to *reporter
}

// Write credits the chunk and reports it fully written.
func (w wireWriter) Write(p []byte) (int, error) {
	w.to.wire(int64(len(p)))

	return len(p), nil
}

// hashWriter credits everything written through it as bytes read off the
// local file and hashed. It is the same shape as [wireWriter] and exists so
// the two counts cannot be confused at a call site.
type hashWriter struct {
	// to is the reporter the bytes are credited to.
	to *reporter
}

// Write credits the chunk and reports it fully written.
func (w hashWriter) Write(p []byte) (int, error) {
	w.to.hashed(int64(len(p)))

	return len(p), nil
}

// attempted runs op under policy and counts every entry after the first as a
// retry.
//
// Counting here rather than inside the retry package keeps the two apart: the
// policy decides whether a failure is worth repeating, and this decides that
// somebody watching would like to know it was. A transfer nobody is watching
// calls [retry.Do] directly, so the accounting costs one nil check and not a
// closure per unit of work.
func attempted(
	ctx context.Context,
	report *reporter,
	policy retry.Policy,
	op func(context.Context) error,
) error {
	if report == nil {
		return retry.Do(ctx, policy, op)
	}

	// retry.Do runs the attempts one after another on this goroutine, so the
	// count needs no synchronization of its own.
	attempts := 0

	return retry.Do(ctx, policy, func(ctx context.Context) error {
		attempts++
		if attempts > 1 {
			report.retried()
		}

		return op(ctx)
	})
}

// hashesInto returns the writer a local hash pass copies through: the hashers
// alone when nobody is watching, and the hashers followed by a counter when
// somebody is.
//
// The counter goes last for the reason it goes last everywhere in this
// package: [io.MultiWriter] stops at the first writer that fails, so a
// counter behind the writers that matter never counts a chunk they refused.
func hashesInto(hashers io.Writer, report *reporter) io.Writer {
	if report == nil {
		return hashers
	}

	return io.MultiWriter(hashers, hashWriter{to: report})
}
