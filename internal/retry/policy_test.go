package retry

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drawnCeiling is what a recording Rand reports for a draw it was never
// asked to make, so a row can say "the policy drew nothing" without a second
// field.
const drawnCeiling = -1

func TestPolicyBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		policy      Policy
		attempt     int
		wantCeiling int64
	}{
		{
			name:        "the first wait is drawn under the base",
			policy:      Policy{Base: DefaultBase, Cap: DefaultCap},
			attempt:     1,
			wantCeiling: int64(time.Second),
		},
		{
			name:        "the second wait doubles the first",
			policy:      Policy{Base: DefaultBase, Cap: DefaultCap},
			attempt:     2,
			wantCeiling: int64(2 * time.Second),
		},
		{
			name:        "the third wait doubles again",
			policy:      Policy{Base: DefaultBase, Cap: DefaultCap},
			attempt:     3,
			wantCeiling: int64(4 * time.Second),
		},
		{
			name:        "the doubling keeps going while it fits under the cap",
			policy:      Policy{Base: DefaultBase, Cap: DefaultCap},
			attempt:     5,
			wantCeiling: int64(16 * time.Second),
		},
		{
			name:        "a doubling that would pass the cap lands on it",
			policy:      Policy{Base: DefaultBase, Cap: DefaultCap},
			attempt:     6,
			wantCeiling: int64(DefaultCap),
		},
		{
			name:        "an attempt count far past the cap still returns the cap",
			policy:      Policy{Base: DefaultBase, Cap: DefaultCap},
			attempt:     1000,
			wantCeiling: int64(DefaultCap),
		},
		{
			name:        "a base already past the cap is the cap",
			policy:      Policy{Base: time.Second, Cap: 500 * time.Millisecond},
			attempt:     1,
			wantCeiling: int64(500 * time.Millisecond),
		},
		{
			name:        "a base too large to double cannot overflow the ceiling",
			policy:      Policy{Base: math.MaxInt64, Cap: DefaultCap},
			attempt:     1000,
			wantCeiling: int64(DefaultCap),
		},
		{
			name:        "a cap too large to reach cannot overflow the ceiling",
			policy:      Policy{Base: DefaultBase, Cap: math.MaxInt64},
			attempt:     1000,
			wantCeiling: math.MaxInt64,
		},
		{
			name:        "a policy with no room to wait draws nothing",
			policy:      Policy{Base: DefaultBase},
			attempt:     1,
			wantCeiling: drawnCeiling,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			asked := int64(drawnCeiling)
			policy := tt.policy
			policy.Rand = func(n int64) int64 {
				asked = n

				return n - 1
			}

			wait := policy.backoff(tt.attempt)

			assert.Equal(t, tt.wantCeiling, asked, "the ceiling the jitter is drawn under")

			if tt.wantCeiling == drawnCeiling {
				assert.Zero(t, wait, "a ceiling of nothing is a wait of nothing")

				return
			}

			assert.Equal(t, time.Duration(tt.wantCeiling-1), wait, "the draw is the wait, unrounded")
		})
	}
}

func TestPolicyNormalized(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		policy       Policy
		wantAttempts int
		wantBase     time.Duration
		wantCap      time.Duration
	}{
		{
			name:         "an unset policy is the default one",
			policy:       Policy{},
			wantAttempts: DefaultAttempts,
			wantBase:     DefaultBase,
			wantCap:      DefaultCap,
		},
		{
			name:         "an attempt count is kept and the rest defaulted",
			policy:       Policy{Attempts: 2},
			wantAttempts: 2,
			wantBase:     DefaultBase,
			wantCap:      DefaultCap,
		},
		{
			name:         "an impossible attempt count falls back to the default",
			policy:       Policy{Attempts: -1},
			wantAttempts: DefaultAttempts,
			wantBase:     DefaultBase,
			wantCap:      DefaultCap,
		},
		{
			name:         "a base of its own is kept",
			policy:       Policy{Base: time.Millisecond},
			wantAttempts: DefaultAttempts,
			wantBase:     time.Millisecond,
			wantCap:      DefaultCap,
		},
		{
			name:         "a cap of its own is kept",
			policy:       Policy{Cap: time.Minute},
			wantAttempts: DefaultAttempts,
			wantBase:     DefaultBase,
			wantCap:      time.Minute,
		},
		{
			name:         "a policy that says everything is returned as it stands",
			policy:       Policy{Attempts: 9, Base: time.Hour, Cap: 2 * time.Hour},
			wantAttempts: 9,
			wantBase:     time.Hour,
			wantCap:      2 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			filled := tt.policy.normalized()

			assert.Equal(t, tt.wantAttempts, filled.Attempts)
			assert.Equal(t, tt.wantBase, filled.Base)
			assert.Equal(t, tt.wantCap, filled.Cap)
			assert.NotNil(t, filled.Sleep, "an unset seam is filled, never left nil for the loop to trip over")
			assert.NotNil(t, filled.Rand)
		})
	}
}

func TestPolicyNormalizedKeepsTheSeamsItWasGiven(t *testing.T) {
	t.Parallel()

	var slept, drawn bool

	filled := Policy{
		Sleep: func(context.Context, time.Duration) error {
			slept = true

			return nil
		},
		Rand: func(int64) int64 {
			drawn = true

			return 0
		},
	}.normalized()

	require.NoError(t, filled.Sleep(t.Context(), 0))
	assert.Zero(t, filled.Rand(1))
	assert.True(t, slept, "the caller's sleep is the one the loop waits in")
	assert.True(t, drawn, "the caller's rand is the one the jitter comes from")
}

func TestDefaultPinsTheDocumentedPolicy(t *testing.T) {
	t.Parallel()

	// These numbers are published in docs/docs/explanation/design.md and in
	// the how-to guide. Changing one here without changing them there leaves
	// the documentation lying about what bigoci does, so this test is the
	// place that forces the two to move together.
	policy := Default()

	assert.Equal(t, 4, policy.Attempts)
	assert.Equal(t, time.Second, policy.Base)
	assert.Equal(t, 30*time.Second, policy.Cap)
	assert.NotNil(t, policy.Sleep)
	assert.NotNil(t, policy.Rand)
}

func TestDefaultRandDrawsWithinTheCeilingFromEveryWorker(t *testing.T) {
	t.Parallel()

	const (
		workers = 4
		draws   = 250
		ceiling = int64(time.Second)
	)

	rand := Default().Rand
	results := make([][]int64, workers)

	var group sync.WaitGroup

	// Every worker of a transfer shares one policy, so the default draw has to
	// be safe under -race as well as inside its bounds.
	for worker := range workers {
		group.Go(func() {
			drawn := make([]int64, 0, draws)
			for range draws {
				drawn = append(drawn, rand(ceiling))
			}

			results[worker] = drawn
		})
	}

	group.Wait()

	distinct := make(map[int64]struct{})

	for _, drawn := range results {
		for _, value := range drawn {
			assert.GreaterOrEqual(t, value, int64(0))
			assert.Less(t, value, ceiling)

			distinct[value] = struct{}{}
		}
	}

	assert.Greater(t, len(distinct), 1, "a jitter that always draws the same value is not jitter")
}

func TestDefaultSleep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cancelled bool
		wait      time.Duration
		wantErr   error
	}{
		{name: "a short wait passes", wait: time.Millisecond},
		{name: "no wait at all passes", wait: 0},
		{
			name:      "a long wait on a dead context returns at once",
			cancelled: true,
			wait:      time.Hour,
			wantErr:   context.Canceled,
		},
		{name: "no wait on a dead context still reports it", cancelled: true, wait: 0, wantErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if tt.cancelled {
				cancel()
			}

			err := sleep(ctx, tt.wait)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
		})
	}
}
