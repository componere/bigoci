package oci

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/retry"
)

// rfc850 is the obsolete date format RFC 9110 still requires a recipient to
// accept, and [net/http.ParseTime] does. The layout is not in the standard
// library's exported set, so it is spelled here.
const rfc850 = "Monday, 02-Jan-06 15:04:05 MST"

// reflectedFailureReader returns a peer-controlled parser failure without
// producing bytes.
type reflectedFailureReader struct {
	// err is the failure returned by every read.
	err error
}

// Read returns the configured failure.
func (r *reflectedFailureReader) Read([]byte) (int, error) {
	return 0, r.err
}

// TestHTTPBodyReadErrorsDoNotRenderPeerText pins every response-body path that
// can receive an HTTP parser error containing malformed peer bytes. The cause
// remains reachable for cancellation and typed diagnosis while its text is
// excluded from the public message.
func TestHTTPBodyReadErrorsDoNotRenderPeerText(t *testing.T) {
	t.Parallel()

	const reusableBearer = "Bearer reusable-body-parser-bearer-a8f4c2"
	cause := errors.New("malformed response reflected " + reusableBearer)

	t.Run("blob body", func(t *testing.T) {
		t.Parallel()

		body := &blobBody{rc: io.NopCloser(&reflectedFailureReader{err: cause})}
		_, err := body.Read(make([]byte, 1))

		require.ErrorIs(t, err, cause)
		_, transient := retry.IsTransient(err)
		assert.True(t, transient)
		assert.Equal(t, "blob response body read failed", err.Error())
		assert.NotContains(t, err.Error(), reusableBearer)
	})

	t.Run("manifest body", func(t *testing.T) {
		t.Parallel()

		resp := &http.Response{Body: io.NopCloser(&reflectedFailureReader{err: cause})}
		_, err := readManifest(origin{method: http.MethodGet, path: "/v2/team/artifact/manifests/v1"}, resp)

		require.ErrorIs(t, err, cause)
		_, transient := retry.IsTransient(err)
		assert.True(t, transient)
		assert.Contains(t, err.Error(), "read manifest response body")
		assert.NotContains(t, err.Error(), reusableBearer)
	})

	t.Run("token body", func(t *testing.T) {
		t.Parallel()

		resp := &http.Response{Body: io.NopCloser(&reflectedFailureReader{err: cause})}
		_, _, err := readToken(resp)

		require.ErrorIs(t, err, cause)
		_, transient := retry.IsTransient(err)
		assert.True(t, transient)
		assert.Equal(t, "read the token endpoint's answer", err.Error())
		assert.NotContains(t, err.Error(), reusableBearer)
	})
}

func TestTransientStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "a rate limited request is worth repeating", status: http.StatusTooManyRequests, want: true},
		{name: "an internal server error is worth repeating", status: http.StatusInternalServerError, want: true},
		{name: "a bad gateway is worth repeating", status: http.StatusBadGateway, want: true},
		{name: "an unavailable registry is worth repeating", status: http.StatusServiceUnavailable, want: true},
		{name: "a gateway timeout is worth repeating", status: http.StatusGatewayTimeout, want: true},
		{name: "a code nobody named at the top of the class is still a 5xx", status: 599, want: true},
		{name: "a malformed request is not", status: http.StatusBadRequest},
		{name: "an unauthenticated request is not", status: http.StatusUnauthorized},
		{name: "a forbidden request is not", status: http.StatusForbidden},
		{name: "a missing blob is not", status: http.StatusNotFound},
		{name: "a method the registry refuses is not", status: http.StatusMethodNotAllowed},
		{name: "a request timeout is a 4xx and fails fast", status: http.StatusRequestTimeout},
		{name: "a conflict is not", status: http.StatusConflict},
		{name: "a part the registry says is too large is not", status: http.StatusRequestEntityTooLarge},
		{name: "an unprocessable request is not", status: http.StatusUnprocessableEntity},
		{name: "a permanent redirect is not", status: http.StatusMovedPermanently},
		{name: "a temporary redirect is not", status: http.StatusTemporaryRedirect},
		{name: "success is not a failure at all", status: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, transientStatus(tt.status))
		})
	}
}

func TestRetryAfterCountedInSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		absent bool
		value  string
		want   time.Duration
	}{
		{name: "no header asks for nothing", absent: true},
		{name: "a count of seconds is the wait", value: "5", want: 5 * time.Second},
		{name: "a long count is reported untrimmed for the policy to bound", value: "3600", want: time.Hour},
		{name: "a count of zero asks for nothing", value: "0"},
		{name: "a negative count asks for nothing", value: "-1"},
		{name: "a count too large to be a duration is unreadable", value: "9223372036854775807"},
		{name: "a word is unreadable", value: "soon"},
		{name: "a fraction is unreadable", value: "3.5"},
		{name: "a blank value is unreadable", value: " "},
		{name: "a count with its unit spelled out is unreadable", value: "7s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{Header: http.Header{}}
			if !tt.absent {
				resp.Header.Set(retryAfterHeader, tt.value)
			}

			assert.Equal(t, tt.want, retryAfter(resp))
		})
	}
}

func TestRetryAfterGivenAsADate(t *testing.T) {
	t.Parallel()

	// The header carries whole seconds, so a date built from now loses its
	// fraction on the way out and the wait that comes back is a little short
	// of the offset. The rows assert a window rather than a value, which is
	// the only honest assertion against a clock nobody injected.
	const offset = 12 * time.Second

	tests := []struct {
		name   string
		layout string
		past   bool
		want   bool
	}{
		{name: "the format registries send", layout: http.TimeFormat, want: true},
		{name: "the obsolete format a recipient still has to accept", layout: rfc850, want: true},
		{name: "the asctime format a recipient still has to accept", layout: time.ANSIC, want: true},
		{name: "a date already past asks for nothing", layout: http.TimeFormat, past: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			when := time.Now().UTC().Add(offset)
			if tt.past {
				when = time.Now().UTC().Add(-offset)
			}

			resp := &http.Response{Header: http.Header{}}
			resp.Header.Set(retryAfterHeader, when.Format(tt.layout))

			got := retryAfter(resp)

			if !tt.want {
				assert.Zero(t, got)

				return
			}

			assert.Positive(t, got, "a date in the future is a wait")
			assert.LessOrEqual(t, got, offset, "a date is never read as longer than the gap to it")
			// The lower bound leaves room for the second the format rounds off
			// plus a few seconds of scheduler stall on a loaded CI runner; the
			// bound only has to prove the wait is the gap and not zero.
			assert.Greater(t, got, offset-5*time.Second, "the wait is the gap to the date, give or take the slack")
		})
	}
}
