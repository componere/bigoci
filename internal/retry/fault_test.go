package retry

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errHungUp is the sentinel the transparency tests look for underneath a tag.
// It stands in for the sentinels adapters really do bury there, such as the
// oci package's not-found.
var errHungUp = errors.New("the registry hung up")

// probeError is a concrete error type the transparency tests find with
// [errors.As]. It stands in for the [net/url.Error] an adapter tags, which is
// a type the core has no way of naming but every caller can still reach.
type probeError struct {
	// wraps is the sentinel the probe carries.
	wraps error
}

// Error renders the probe and the sentinel under it.
func (p *probeError) Error() string {
	return "probe: " + p.wraps.Error()
}

// Unwrap exposes the sentinel, so a tag placed over the probe does not hide
// it.
func (p *probeError) Unwrap() error {
	return p.wraps
}

func TestTransient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		after       time.Duration
		wantNil     bool
		wantMessage string
	}{
		{
			name:    "a nil failure is nothing to tag",
			err:     nil,
			after:   time.Second,
			wantNil: true,
		},
		{
			name:        "the tag renders the failure's own message",
			err:         errHungUp,
			wantMessage: "the registry hung up",
		},
		{
			name:        "a wait from the far end does not reach the message",
			err:         errHungUp,
			after:       12 * time.Second,
			wantMessage: "the registry hung up",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tagged := Transient(tt.err, tt.after)

			if tt.wantNil {
				assert.NoError(t, tagged)

				return
			}

			require.Error(t, tagged)
			assert.Equal(t, tt.wantMessage, tagged.Error())
			assert.Equal(t, tt.err, errors.Unwrap(tagged))
		})
	}
}

func TestTransientLeavesTheFailureReachable(t *testing.T) {
	t.Parallel()

	tagged := Transient(&probeError{wraps: errHungUp}, 5*time.Second)
	buried := fmt.Errorf(
		"pull artifact: %w",
		fmt.Errorf("fetch part 3: %w", fmt.Errorf("GET /v2/team/artifact/blobs/sha256:abc: %w", tagged)),
	)

	require.ErrorIs(t, buried, errHungUp, "a sentinel under the tag keeps matching however deep it ends up")

	var probe *probeError

	require.ErrorAs(t, buried, &probe, "a concrete type under the tag stays findable")
	assert.Contains(t, buried.Error(), "probe: the registry hung up", "the tag adds nothing to the message")

	after, transient := IsTransient(buried)
	assert.True(t, transient)
	assert.Equal(t, 5*time.Second, after)
}

func TestIsTransient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		wantAfter time.Duration
		wantOK    bool
	}{
		{
			name: "nothing at all is terminal",
			err:  nil,
		},
		{
			name: "a failure nobody classified is terminal",
			err:  errors.New("registry returned 400 Bad Request"),
		},
		{
			name:   "a tagged failure with no wait is worth repeating",
			err:    Transient(errHungUp, 0),
			wantOK: true,
		},
		{
			name:      "a tagged failure carries the wait the far end asked for",
			err:       Transient(errHungUp, 7*time.Second),
			wantAfter: 7 * time.Second,
			wantOK:    true,
		},
		{
			name: "a tag survives the messages stacked over it",
			err: fmt.Errorf(
				"a: %w",
				fmt.Errorf("b: %w", fmt.Errorf("c: %w", Transient(errHungUp, 3*time.Second))),
			),
			wantAfter: 3 * time.Second,
			wantOK:    true,
		},
		{
			name:      "the outermost tag is the verdict",
			err:       Transient(fmt.Errorf("read: %w", Transient(errHungUp, 2*time.Second)), 9*time.Second),
			wantAfter: 9 * time.Second,
			wantOK:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			after, transient := IsTransient(tt.err)

			assert.Equal(t, tt.wantOK, transient)
			assert.Equal(t, tt.wantAfter, after)
		})
	}
}
