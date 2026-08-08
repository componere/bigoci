package retry

import (
	"context"
	"fmt"
)

// Do runs op until it succeeds, until it fails in a way repeating cannot fix,
// or until the policy runs out of attempts.
//
// op must be safe to run again: it opens whatever it reads from, so a retried
// upload streams its part from a fresh reader into a fresh session rather
// than from a spent one. Do hands op the context it was given and nothing
// else, so an operation that must not outlive the transfer does not have to
// be told twice, and a cancellation reaches an attempt in flight and a wait
// between attempts alike.
//
// A failure is repeated only when some layer under it called [Transient].
// Anything else comes back on the first attempt: bigoci does not guess that
// an unrecognized failure might be temporary. Cancellation outranks any tag,
// and it is read off ctx itself and never off the failure's shape: Go's
// transport renders an ordinary dial or header timeout as an error that
// matches [context.DeadlineExceeded], and a transfer that mistook one of
// those for the caller ending it would refuse to retry exactly the failure a
// retry exists for. Only ctx knows whether the transfer is over.
//
// The wait between attempts is the policy's jittered backoff, raised to meet
// a wait the failure carried from the far end. A registry's Retry-After is
// therefore a floor and never a ceiling: it cannot shorten the escalation
// that keeps retrying workers apart, and it cannot park a transfer past
// [Policy.Cap], which bounds every wait this package takes.
//
// The error Do returns is the last one op produced. A failure that ended the
// run on its first attempt comes back exactly as op returned it, so nothing
// reads as retry bookkeeping that never happened; attempts running out wraps
// it with the count, which is the one thing the caller could not otherwise
// know. A context that ends between attempts comes back wrapped together
// with the failure the run was retrying, on one line, and both match under
// [errors.Is].
func Do(ctx context.Context, p Policy, op func(ctx context.Context) error) error {
	p = p.normalized()

	var err error

	for attempt := 1; attempt <= p.Attempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return interrupted(ctxErr, attempt-1, err)
		}

		err = op(ctx)
		if err == nil {
			return nil
		}

		// The guard reads ctx, not the error: a transport timeout matches
		// context.DeadlineExceeded by design in net and net/http, and only the
		// context can say whether the transfer itself is over.
		if ctx.Err() != nil {
			return err
		}

		after, transient := IsTransient(err)
		if !transient {
			return err
		}

		if attempt == p.Attempts {
			break
		}

		// The far end's wait is a floor under the jittered backoff, bounded by
		// Cap like every other wait: a hostile header cannot park a transfer
		// for a day, and a modest one cannot send every rate-limited worker
		// back at the same instant by replacing the escalation.
		wait := p.backoff(attempt)
		if after > 0 {
			wait = max(wait, min(after, p.Cap))
		}

		if waitErr := p.Sleep(ctx, wait); waitErr != nil {
			return interrupted(waitErr, attempt, err)
		}
	}

	if p.Attempts > 1 {
		return fmt.Errorf("after %d attempts: %w", p.Attempts, err)
	}

	return err
}

// interrupted renders a run the context ended: the reason it stopped wrapped
// together with the failure it was retrying, on one line because the CLI
// prints a failure on one line, and both reachable under [errors.Is]. A run
// that had not failed yet — ended before its first attempt — reports the
// cause alone.
func interrupted(cause error, attempts int, last error) error {
	if last == nil {
		return cause
	}

	return fmt.Errorf("%w after %d attempts: %w", cause, attempts, last)
}
