package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The failures the decision matrix scripts. Their identity is what the rows
// assert on, so each one says which verdict it stands for rather than what
// went wrong.
var (
	// errUnwell stands for a failure an adapter marked worth repeating.
	errUnwell = errors.New("registry returned 503 Service Unavailable")
	// errRefused stands for a failure nobody marked, which is terminal.
	errRefused = errors.New("registry returned 400 Bad Request")
	// errLast is the final failure of an exhausted run, so a row can prove
	// the error that comes back is the last one and not the first.
	errLast = errors.New("registry returned 502 Bad Gateway")
)

// halved is the draw the matrix runs under: half of whatever ceiling it is
// offered. With the default base and cap it makes the schedule exactly
// 500ms, 1s, 2s, which is short enough to name in a row.
func halved(n int64) int64 {
	return n / 2
}

// clock is the pair of seams a [Policy] takes as data, recording what the
// loop asked of them. It is how a row reads an entire run's schedule off a
// slice with no wall clock anywhere near the test.
type clock struct {
	// waits are the durations Sleep was asked for, in order.
	waits []time.Duration
	// ceilings are the exclusive bounds Rand was asked to draw under, in
	// order. They are the evidence that a wait a registry asked for is a
	// floor under the jitter rather than a replacement for it.
	ceilings []int64
	// draw turns a ceiling into the value Rand answers with.
	draw func(n int64) int64
	// interrupt is what the Sleep at position interruptAt returns, standing
	// in for a context that ended mid-wait.
	interrupt error
	// interruptAt is the one-based Sleep call that returns interrupt. Zero
	// leaves every wait successful.
	interruptAt int
}

// sleep records the wait it was asked for and reports the interruption the
// fixture was built with, if this is the call that carries it.
func (c *clock) sleep(_ context.Context, d time.Duration) error {
	c.waits = append(c.waits, d)

	if c.interrupt != nil && len(c.waits) == c.interruptAt {
		return c.interrupt
	}

	return nil
}

// rand records the ceiling it was offered and draws under it.
func (c *clock) rand(n int64) int64 {
	c.ceilings = append(c.ceilings, n)

	return c.draw(n)
}

// policy returns a four-attempt policy wired to this clock, with the default
// base and cap so the rows can talk in the numbers the design fixes.
func (c *clock) policy() Policy {
	return Policy{
		Attempts: DefaultAttempts,
		Base:     DefaultBase,
		Cap:      DefaultCap,
		Sleep:    c.sleep,
		Rand:     c.rand,
	}
}

// scripted returns an operation that answers with the next failure in script
// and counts the attempts made. An attempt past the end of the script fails
// the test, which is how a row proves the loop stopped when it should have.
func scripted(t *testing.T, script []error) (func(context.Context) error, *int) {
	t.Helper()

	calls := 0

	return func(context.Context) error {
		calls++
		require.LessOrEqual(t, calls, len(script), "the loop made more attempts than the row scripted")

		return script[calls-1]
	}, &calls
}

func TestDo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		attempts     int
		script       []error
		wantCalls    int
		wantWaits    []time.Duration
		wantCeilings []int64
		wantErrIs    error
		wantMessage  string
	}{
		{
			name:      "a first attempt that works is the only attempt",
			script:    []error{nil},
			wantCalls: 1,
		},
		{
			name:         "a transient failure is repeated until it works",
			script:       []error{Transient(errUnwell, 0), Transient(errUnwell, 0), nil},
			wantCalls:    3,
			wantWaits:    []time.Duration{500 * time.Millisecond, time.Second},
			wantCeilings: []int64{int64(time.Second), int64(2 * time.Second)},
		},
		{
			name:        "a failure nobody classified comes back untouched",
			script:      []error{errRefused},
			wantCalls:   1,
			wantErrIs:   errRefused,
			wantMessage: "registry returned 400 Bad Request",
		},
		{
			name:         "a terminal failure ends a run that had been retrying",
			script:       []error{Transient(errUnwell, 0), errRefused},
			wantCalls:    2,
			wantWaits:    []time.Duration{500 * time.Millisecond},
			wantCeilings: []int64{int64(time.Second)},
			wantErrIs:    errRefused,
			wantMessage:  "registry returned 400 Bad Request",
		},
		{
			name: "attempts running out returns the last failure with the count",
			script: []error{
				Transient(errUnwell, 0),
				Transient(errUnwell, 0),
				Transient(errUnwell, 0),
				Transient(errLast, 0),
			},
			wantCalls:    4,
			wantWaits:    []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second},
			wantCeilings: []int64{int64(time.Second), int64(2 * time.Second), int64(4 * time.Second)},
			wantErrIs:    errLast,
			wantMessage:  "after 4 attempts: registry returned 502 Bad Gateway",
		},
		{
			name:         "a wait the registry asked for is a floor under the jitter",
			script:       []error{Transient(errUnwell, 7*time.Second), nil},
			wantCalls:    2,
			wantWaits:    []time.Duration{7 * time.Second},
			wantCeilings: []int64{int64(time.Second)},
		},
		{
			name:         "a wait shorter than the jitter never shortens it",
			script:       []error{Transient(errUnwell, 100*time.Millisecond), nil},
			wantCalls:    2,
			wantWaits:    []time.Duration{500 * time.Millisecond},
			wantCeilings: []int64{int64(time.Second)},
		},
		{
			name:         "a wait past the cap waits the cap",
			script:       []error{Transient(errUnwell, 24*time.Hour), nil},
			wantCalls:    2,
			wantWaits:    []time.Duration{DefaultCap},
			wantCeilings: []int64{int64(time.Second)},
		},
		{
			name:         "a tag that carries no wait leaves the jitter alone",
			script:       []error{Transient(errUnwell, 0), nil},
			wantCalls:    2,
			wantWaits:    []time.Duration{500 * time.Millisecond},
			wantCeilings: []int64{int64(time.Second)},
		},
		{
			name:        "a cancelled attempt outranks the tag over it",
			script:      []error{Transient(fmt.Errorf("GET /v2/blobs: %w", context.Canceled), 0)},
			wantCalls:   1,
			wantErrIs:   context.Canceled,
			wantMessage: "GET /v2/blobs: context canceled",
		},
		{
			name:        "an expired deadline outranks the tag over it",
			script:      []error{Transient(fmt.Errorf("GET /v2/blobs: %w", context.DeadlineExceeded), 0)},
			wantCalls:   1,
			wantErrIs:   context.DeadlineExceeded,
			wantMessage: "GET /v2/blobs: context deadline exceeded",
		},
		{
			name:        "a policy of one attempt reports the failure without a count",
			attempts:    1,
			script:      []error{Transient(errUnwell, 0)},
			wantCalls:   1,
			wantErrIs:   errUnwell,
			wantMessage: "registry returned 503 Service Unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorded := &clock{draw: halved}
			policy := recorded.policy()

			if tt.attempts > 0 {
				policy.Attempts = tt.attempts
			}

			op, calls := scripted(t, tt.script)

			err := Do(t.Context(), policy, op)

			assert.Equal(t, tt.wantCalls, *calls, "attempts made")
			assert.Equal(t, tt.wantWaits, recorded.waits, "the waits between attempts, in order")
			assert.Equal(t, tt.wantCeilings, recorded.ceilings, "the ceilings the jitter was drawn under")

			if tt.wantErrIs == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tt.wantErrIs, "the failure that ended the run stays reachable")
			assert.Equal(t, tt.wantMessage, err.Error())
		})
	}
}

func TestDoStopsBeforeTheFirstAttemptWhenTheContextIsDone(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	recorded := &clock{draw: halved}
	op, calls := scripted(t, []error{nil})

	err := Do(ctx, recorded.policy(), op)

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, *calls, "a transfer that is already over does not touch the registry")
	assert.Empty(t, recorded.waits)
}

func TestDoReportsAnInterruptedWaitWithTheFailureThatCausedIt(t *testing.T) {
	t.Parallel()

	recorded := &clock{draw: halved, interrupt: context.Canceled, interruptAt: 1}
	op, calls := scripted(t, []error{Transient(errUnwell, 0)})

	err := Do(t.Context(), recorded.policy(), op)

	require.Error(t, err)
	assert.Equal(t, 1, *calls, "a wait that was cut short is not followed by another attempt")
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, recorded.waits)
	require.ErrorIs(t, err, context.Canceled, "why the run stopped")
	require.ErrorIs(t, err, errUnwell, "why the run was waiting")
	assert.Equal(
		t,
		"context canceled after 1 attempts: registry returned 503 Service Unavailable",
		err.Error(),
		"one line, because the CLI prints a failure on one line",
	)
}

func TestDoRunsAPolicyThatSaysNothingAsTheDefaultOne(t *testing.T) {
	t.Parallel()

	recorded := &clock{draw: halved}
	op, calls := scripted(t, []error{
		Transient(errUnwell, 0),
		Transient(errUnwell, 0),
		Transient(errUnwell, 0),
		Transient(errLast, 0),
	})

	// Only the two seams are supplied, so the attempt count, the base, and
	// the cap all come from the defaults a zero Policy stands for.
	err := Do(t.Context(), Policy{Sleep: recorded.sleep, Rand: recorded.rand}, op)

	require.ErrorIs(t, err, errLast)
	assert.Equal(t, DefaultAttempts, *calls)
	assert.Equal(t, []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}, recorded.waits)
}
