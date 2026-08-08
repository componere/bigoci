package retry

import (
	"errors"
	"time"
)

// Transient marks err as a failure that repeating the request could fix: a
// connection that dropped, a registry that answered 429 or 5xx, a part whose
// body ended early.
//
// after is how long the far end asked the caller to wait before the next
// attempt, and zero when it asked for nothing. A wait that arrives this way
// is a floor under the policy's own backoff rather than a replacement for it,
// so a registry that says how long its rate limit lasts is listened to
// without ever shortening the escalation that keeps retrying workers apart.
// Bounding such a wait is [Do]'s job, not the caller's: whoever tags a
// failure reports what the far end actually said.
//
// The tag is a wrapper. [errors.Is] and [errors.As] see straight through it,
// and the message it renders is err's own, so tagging a failure never changes
// what a caller reads or what a sentinel underneath it matches. Only the
// layer that diagnosed a failure should tag it — the adapter for anything the
// transport or the registry reported, the orchestrator for what it can tell
// from the bytes it received.
//
// A nil err returns nil, so a caller may tag a result unconditionally.
func Transient(err error, after time.Duration) error {
	if err == nil {
		return nil
	}

	return &fault{err: err, after: after}
}

// IsTransient reports whether some layer under err marked it worth repeating,
// and returns the wait the far end asked for — zero when it asked for none,
// and zero for an untagged error.
//
// An untagged error is terminal. That default is deliberate: a failure no
// layer recognized is one bigoci does not understand, and repeating it four
// times turns an immediate answer into a slow one without making it better.
//
// Tags nest without conflict. The walk [errors.As] performs finds the
// outermost one, which is the verdict of the layer closest to the caller and
// therefore the verdict with the most context behind it.
func IsTransient(err error) (time.Duration, bool) {
	var tagged *fault

	if !errors.As(err, &tagged) {
		return 0, false
	}

	return tagged.after, true
}

// fault is the tag [Transient] attaches. It is unexported because the only
// question anyone asks of it is the one [IsTransient] answers, and keeping
// the type inside the package means no other package can build a fault by
// hand and skip the contract that comes with the constructor.
type fault struct {
	// err is the failure being classified.
	err error
	// after is the wait the far end asked for, zero when it asked for none.
	after time.Duration
}

// Error renders the underlying failure unchanged: the tag is for the retry
// loop to read, not for a caller to see.
func (f *fault) Error() string {
	return f.err.Error()
}

// Unwrap exposes the underlying failure to [errors.Is] and [errors.As], so
// every sentinel beneath the tag keeps matching.
func (f *fault) Unwrap() error {
	return f.err
}
