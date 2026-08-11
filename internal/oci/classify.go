package oci

import (
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/imgoci/bigoci/internal/retry"
)

// retryAfterHeader is the response header a registry asks for a pause in.
const retryAfterHeader = "Retry-After"

// serverErrorCeiling is the highest status in the 5xx class. The standard
// library names the codes the specs define but not the edge of the class, and
// a proxy in front of a registry can answer with a number nobody named, so
// the class is matched as a range rather than as a list.
const serverErrorCeiling = 599

// maxRetryAfterSeconds is the largest delta-seconds value that still fits in
// a [time.Duration]. A count past it is not a long wait but an unreadable
// one, and it is treated the same way as any other value the header cannot be
// made sense of. The bound on waits a registry can actually ask for lives in
// the retry policy, which clamps every wait to its cap.
const maxRetryAfterSeconds = int64(math.MaxInt64 / time.Second)

// transientStatus reports whether a status is worth another attempt.
//
// The set is the one the design fixes: 429, because it is the registry asking
// for a pause rather than refusing, and every 5xx, because a server that
// failed once may not fail again. Every other 4xx is the registry saying the
// request is wrong, and sending it three more times only makes it wrong three
// more times. 408 is not in the set — it is a 4xx, the design's table says
// other 4xx fail fast, and registries do not send it.
func transientStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		(status >= http.StatusInternalServerError && status <= serverErrorCeiling)
}

// retryAfter reads the wait a response asked for out of its Retry-After
// header.
//
// RFC 9110 gives the header two spellings and registries use both: a count of
// seconds, and an HTTP-date the wait ends at. A date is turned into a wait
// against the local clock, which is the only clock available — a skewed one
// costs an attempt's timing and nothing else, because the value is advice
// about a wait and not a deadline anything depends on. All three date formats
// [net/http.ParseTime] accepts are accepted here.
//
// Anything unusable is zero: an absent header, a value that is neither form,
// a count of zero or less, a date already past, and a count too large to be a
// duration at all. Zero tells the retry policy the far end asked for nothing
// and its own backoff applies. Nothing is clamped here beyond that — the
// adapter reports what the registry said, and bounding it is the policy's
// job.
func retryAfter(resp *http.Response) time.Duration {
	value := resp.Header.Get(retryAfterHeader)
	if value == "" {
		return 0
	}

	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 || seconds > maxRetryAfterSeconds {
			return 0
		}

		return time.Duration(seconds) * time.Second
	}

	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}

	wait := time.Until(when)
	if wait <= 0 {
		return 0
	}

	return wait
}

// blobBody is the reader [Blobs.Get] hands back: the response body, with
// every read failure classified before the core sees it.
//
// The classification has to happen here because this is the last place that
// knows the bytes are arriving over a connection. A body that stops mid-part
// is the same kind of failure as a request that never connected, and the core
// cannot tell the two apart — it holds an [io.Reader] and would have to
// inspect a network error to guess, which is exactly the knowledge the port
// exists to keep out of it.
type blobBody struct {
	// rc is the response body being read and closed.
	rc io.ReadCloser
}

// Read reads from the response body and marks every failure but [io.EOF] as
// worth another attempt. EOF passes through untouched: wrapping it would
// break every copy primitive that reads it as the end of the stream.
func (b *blobBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, retry.Transient(safeCause("blob response body read failed", err), 0)
	}

	return n, err
}

// Close closes the response body.
func (b *blobBody) Close() error {
	return b.rc.Close()
}

// safeError retains an underlying failure for [errors.Is] and [errors.As] while
// exposing only a package-chosen message. HTTP parser and body-read failures
// can quote peer-provided bytes, including a reflected credential.
type safeError struct {
	// message is the fixed structural diagnosis rendered by Error.
	message string
	// cause retains typed and sentinel identity without contributing its text.
	cause error
}

// Error returns the fixed structural diagnosis.
func (e *safeError) Error() string {
	return e.message
}

// Unwrap exposes the underlying failure to [errors.Is] and [errors.As].
func (e *safeError) Unwrap() error {
	return e.cause
}

// safeCause wraps cause without rendering its potentially peer-derived text.
func safeCause(message string, cause error) error {
	return &safeError{message: message, cause: cause}
}
