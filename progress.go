package bigoci

// Direction says which way a transfer is moving.
type Direction uint8

const (
	// DirectionPush is a file on its way into a registry.
	DirectionPush Direction = iota + 1
	// DirectionPull is an artifact on its way out of one.
	DirectionPull
)

// Phase says how far along a transfer is, and it is the field that says
// whether it is over. Counters that have reached their totals do not: the
// last part of a push can be in the registry while the manifest that names it
// is still being written.
//
// A phase only ever advances.
type Phase uint8

const (
	// PhaseResolving is a pull fetching and decoding the manifest, which is
	// what tells it how large the file is and which blobs make it up. A push
	// never reports it: a push knows what it is moving before it starts.
	PhaseResolving Phase = iota + 1
	// PhaseTransferring is parts moving: for a push the hash pass and the
	// uploads it feeds, for a pull the verify of anything already on disk and
	// the fetches of everything else.
	PhaseTransferring
	// PhaseFinalizing is the work left once the parts are in place: for a
	// push the empty config blob and the manifest write, for a pull the
	// commit that publishes the destination.
	PhaseFinalizing
	// PhaseDone is a transfer that succeeded. Exactly one snapshot carries
	// it, and it is the last one delivered.
	PhaseDone
	// PhaseFailed is a transfer that failed. Exactly one snapshot carries it,
	// and it is the last one delivered. Why it failed is the error
	// [Client.Push] or [Client.Pull] returned; a snapshot never carries one.
	PhaseFailed
)

// Progress is one absolute account of a transfer: what it is moving, how much
// of that is done, and what it cost so far. It is not a delta and not an
// event, so a consumer that misses a snapshot has missed nothing but a
// redraw.
//
// The zero value describes no transfer. Both enums start at one, so a
// [Progress] that arrived from anywhere has a real Direction and a real
// Phase.
//
// # Reading it
//
// Percentages, bars, and "how much is left" are [Progress.CompletedBytes]
// against TotalBytes: that counter is the file, and it only moves when a part
// is provably where it belongs. Throughput is WireBytes, which is what
// actually crossed the registry boundary — including the bytes a broken
// attempt sent twice. Reporting the second as the first would show a transfer
// at 103%; reporting the first as the second would show a re-push of an
// unchanged file moving gigabytes it never sent.
//
// # What it guarantees
//
// Every numeric field is non-decreasing and Phase only advances, so nothing a
// consumer draws ever has to go backwards. Snapshots are totally ordered and
// never overlap: the library delivers them one at a time, under a lock it
// holds across the call, so a callback needs no synchronization of its own to
// keep the last one it saw. Totals change at most once, from zero, and only a
// pull's ever do. CompletedBytes never exceeds TotalBytes and CompletedParts
// never exceeds TotalParts, but WireBytes is bounded by neither.
type Progress struct {
	// Direction is which way this transfer is moving.
	Direction Direction
	// Phase is how far along it is.
	Phase Phase
	// TotalBytes is the length of the file being moved. A push knows it on
	// every snapshot. A pull reports zero through [PhaseResolving] and the
	// real length from the moment it has decoded the manifest.
	TotalBytes int64
	// TotalParts is how many parts the file splits into. It counts parts and
	// not blobs, so two parts that hold identical bytes — and are therefore
	// one blob in the registry — still count twice, and this number never
	// shrinks because of a shortcut the transfer found.
	TotalParts int
	// CompletedBytes is how many of the file's bytes are provably in their
	// final place: held by the registry for a push, written and verified for
	// a pull. It moves in whole parts, once per part, and never as a
	// half-uploaded one.
	CompletedBytes int64
	// CompletedParts is how many parts are provably in their final place.
	CompletedParts int
	// SkippedParts is how many of those parts cost no bytes of their own: a
	// part is skipped when this transfer moved none of that part's own bytes
	// over the wire, decided across the part's whole retry budget, not per
	// attempt. Parts the registry already held, parts a resume found intact
	// on disk, and parts that shared a blob with another part of the same
	// transfer are the three ways it happens — while an upload that landed
	// and lost its answer is not one of them, because the first attempt did
	// send the bytes.
	SkippedParts int
	// WireBytes is how many bytes of the file crossed the registry boundary,
	// counting every attempt. An attempt that broke and was made again counts
	// both times, so this can pass TotalBytes: it says what the transfer cost
	// rather than what it achieved. The manifest and the empty config blob
	// are not in it — they are a few kilobytes of bookkeeping either way, and
	// counting them would put noise in the one number throughput comes from.
	WireBytes int64
	// HashedBytes is how many bytes were read off the local disk and hashed:
	// a push's single pass over the file, and a pull's verify of a partial
	// file it is resuming into. It never exceeds TotalBytes.
	HashedBytes int64
	// Retries is how many times some unit of work re-entered its retry budget
	// after its first attempt — a part, the empty config blob, or a manifest
	// read or write. A climbing count with nothing else moving is a transfer
	// in a backoff, which is the one thing no byte counter can show.
	Retries int
}

// ProgressFunc receives the snapshots of one transfer.
//
// It is called on whichever goroutine did the work being reported, which
// means the workers moving parts, the goroutine a push hashes the file on,
// and — for a push's wire bytes — the goroutine [net/http] writes a request
// body from. The library serializes the calls, so an implementation may read
// and write its own state without a lock of its own; what it may not do is
// take one the transfer's caller might already hold.
//
// The transfer waits for it. That is deliberate — it is what makes the
// snapshots ordered and non-overlapping — and it is why an implementation
// must do almost nothing: store the snapshot, or print a line. Sending on a
// channel nobody is receiving from stalls the transfer outright; so does a
// network call, and so does calling back into the [Client] that is running
// it.
//
// Nothing arrives after the transfer's own call has returned. A push hands
// the reader it uploads from to the transport, which reads it on a goroutine
// of its own, so a report can genuinely be raised late; those are dropped
// rather than delivered.
type ProgressFunc func(Progress)

// WithProgress hands fn a snapshot of the transfer whenever there is one
// worth delivering.
//
// The snapshots start with [PhaseResolving] for a pull and
// [PhaseTransferring] for a push, and the last one is always [PhaseDone] or
// [PhaseFailed]. Either fn is called that way or it is never called at all:
// a failure before the transfer begins — settings that describe no transfer,
// a reference that will not parse, a source that will not open, a destination
// that cannot be created — reports nothing, because there is nothing yet to
// report on. A transfer that has begun always ends with a terminal snapshot,
// whatever went wrong.
//
// How often is not a promise. Milestones — a phase change, a part completing,
// a retry, the end — always deliver, and movement between them is coalesced,
// so a fast link reports about as often as a slow one rather than thousands
// of times more. A caller that wants a line every few seconds should render
// on its own clock from the last snapshot it stored, which is also the shape
// [ProgressFunc] asks for.
//
// Naming this option twice leaves the last fn in effect. A nil fn installs no
// callback and costs the transfer nothing at all.
func WithProgress(fn ProgressFunc) TransferOption {
	return progressOption{fn: fn}
}

// String renders the direction as the word a log line uses: "push", "pull",
// or "unknown" for a value that came from nowhere this package built.
func (d Direction) String() string {
	switch d {
	case DirectionPush:
		return "push"
	case DirectionPull:
		return "pull"
	default:
		return "unknown"
	}
}

// String renders the phase as one lowercase word, or "unknown" for a value
// that came from nowhere this package built.
func (p Phase) String() string {
	switch p {
	case PhaseResolving:
		return "resolving"
	case PhaseTransferring:
		return "transferring"
	case PhaseFinalizing:
		return "finalizing"
	case PhaseDone:
		return "done"
	case PhaseFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Fraction returns how much of the file is provably in place, between 0 and
// 1: [Progress.CompletedBytes] over TotalBytes.
//
// It is zero whenever TotalBytes is not positive, which covers two real
// cases rather than only guarding a division. A pull reports zero totals
// until it has decoded the manifest, and an empty file is a genuine artifact
// of one empty part — its fraction is zero on every snapshot it ever
// delivers, including the [PhaseDone] one. Neither is stuck, and neither
// should be drawn as a bar. Phase is what says a transfer finished; this
// never does.
func (p Progress) Fraction() float64 {
	if p.TotalBytes <= 0 {
		return 0
	}

	return float64(p.CompletedBytes) / float64(p.TotalBytes)
}

// progressOption carries [WithProgress].
type progressOption struct {
	// fn receives the transfer's snapshots, nil for no callback at all.
	fn ProgressFunc
}

// applyPush records the callback a push reports through.
func (o progressOption) applyPush(s *pushSettings) {
	s.progress = o.fn
}

// applyPull records the callback a pull reports through.
func (o progressOption) applyPull(s *pullSettings) {
	s.progress = o.fn
}
