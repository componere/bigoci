package retry

import (
	"context"
	"math/rand/v2"
	"time"
)

// The policy the design fixes, and the value every unset [Policy] field
// takes. The numbers are documented for users in
// docs/docs/explanation/design.md and docs/docs/reference/api.md, which have
// to move whenever they do.
const (
	// DefaultAttempts is how many times an operation is tried in total,
	// counting the first. Four covers the transient failures registries
	// actually produce without turning a real outage into a minute of
	// waiting.
	DefaultAttempts = 4
	// DefaultBase is the ceiling of the first wait. It doubles every further
	// attempt, so the three waits of a default run are drawn from one, two,
	// and four seconds.
	DefaultBase = time.Second
	// DefaultCap is the longest ceiling the doubling reaches, and the bound
	// on every wait including one a registry asked for. A pause past half a
	// minute belongs to a transfer that should fail and be started again
	// rather than one that is still trying.
	DefaultCap = 30 * time.Second
)

// Sleep waits for d and returns nil, or gives up early and returns ctx's
// error.
//
// A Sleep that ignores ctx makes a transfer outlive its cancellation by up to
// [Policy.Cap], so an implementation must select on both. It is called even
// for a zero d, which is what lets a test count the pauses a run took without
// owning a clock.
type Sleep func(ctx context.Context, d time.Duration) error

// Rand returns a pseudo-random value in [0,n), with the contract of
// [math/rand/v2.Int64N].
//
// It is what makes the backoff full jitter: a wait is drawn uniformly from
// zero up to the attempt's ceiling, so workers that failed together do not
// come back together. It is never called with a non-positive n.
type Rand func(n int64) int64

// Policy is how an operation is retried: how many times, how the waits
// between attempts grow, and the two seams that make both testable.
//
// A field that is not positive takes its default, so the zero Policy is the
// policy the design fixes and a caller with nothing to say about retries says
// nothing. [Default] returns the same policy spelled out.
//
// One Policy is shared by every worker of a transfer, so Sleep and Rand must
// be safe for concurrent use. The defaults are.
type Policy struct {
	// Attempts is the total number of tries, counting the first. One means no
	// retry at all.
	Attempts int
	// Base is the ceiling of the wait after the first failure. Every further
	// attempt doubles it, up to Cap.
	Base time.Duration
	// Cap is the largest ceiling the doubling reaches, and the bound on a
	// wait a far end asked for.
	Cap time.Duration
	// Sleep is how the loop waits between attempts.
	Sleep Sleep
	// Rand is where the jitter in each wait comes from.
	Rand Rand
}

// Default returns the retry policy from the design's defaults table: four
// attempts, one second of base backoff doubling to a thirty second ceiling,
// full jitter, and a sleep that gives up when the context does.
func Default() Policy {
	return Policy{
		Attempts: DefaultAttempts,
		Base:     DefaultBase,
		Cap:      DefaultCap,
		Sleep:    sleep,
		Rand:     rand.Int64N,
	}
}

// normalized returns p with every unset field filled from [Default]. [Do]
// calls it once, so the loop reads fields without checking them.
func (p Policy) normalized() Policy {
	filled := Default()

	if p.Attempts > 0 {
		filled.Attempts = p.Attempts
	}

	if p.Base > 0 {
		filled.Base = p.Base
	}

	if p.Cap > 0 {
		filled.Cap = p.Cap
	}

	if p.Sleep != nil {
		filled.Sleep = p.Sleep
	}

	if p.Rand != nil {
		filled.Rand = p.Rand
	}

	return filled
}

// backoff returns the wait after the given attempt, counting from one: a
// value drawn uniformly from zero up to that attempt's ceiling. Drawing from
// zero rather than from half the ceiling is what "full jitter" means, and it
// is the variant that spreads a thundering herd widest.
//
// The ceiling starts at Base, or at Cap when Base is already past it, and
// doubles once per attempt already made. The doubling runs as a guarded loop
// rather than a shift so that no attempt count, however large, can overflow
// it into a negative wait: a ceiling more than halfway to Cap goes straight
// to Cap, which is where the doubling was heading anyway.
//
// A ceiling of zero or less — a Policy built by hand with no room to wait in
// — draws nothing and waits nothing.
func (p Policy) backoff(attempt int) time.Duration {
	ceiling := min(p.Base, p.Cap)

	for range attempt - 1 {
		if ceiling >= p.Cap {
			break
		}

		if ceiling > p.Cap/2 {
			ceiling = p.Cap

			break
		}

		ceiling *= 2
	}

	if ceiling <= 0 {
		return 0
	}

	return time.Duration(p.Rand(int64(ceiling)))
}

// sleep is the default [Policy.Sleep]: it waits for d, or returns as soon as
// ctx is done, whichever comes first. A non-positive d returns at once, after
// one look at ctx.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
