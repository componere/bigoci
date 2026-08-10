package transfer

import (
	"errors"
	"fmt"

	digest "github.com/opencontainers/go-digest"

	"github.com/componere/bigoci/internal/plan"
)

// copyBufferSize is the size of the scratch buffer a transfer streams a part
// through. It is large enough that moving a part costs few system calls, and
// small enough that one buffer per worker is nothing beside the file being
// moved. It is also the most of a part that is ever in memory at once: a part
// is bytes passing through, never a value the orchestrator holds.
const copyBufferSize = 256 << 10

// ErrDigestMismatch reports a part whose bytes hash to something other than
// the digest the manifest records for it.
//
// It is the one pull failure a caller branches on, because it says the
// registry served content this artifact does not describe rather than that
// the transfer broke on the way. The wrapped message names the part.
var ErrDigestMismatch = errors.New("part digest mismatch")

// safeError retains an underlying failure for [errors.Is] and [errors.As] while
// exposing only a structural message selected by the transfer package.
type safeError struct {
	// message is the safe diagnosis rendered by Error.
	message string
	// cause retains typed and sentinel identity without contributing its text.
	cause error
}

// Error returns the safe structural diagnosis.
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

// partJob is one unit of work a transfer hands its workers: which byte range
// of the file to move, and the digest that range is stored under. A push
// learns the digest by hashing the range; a pull reads it out of the
// manifest.
type partJob struct {
	// part is the byte range of the file the job covers.
	part plan.Part
	// dgst is the digest of the range's bytes: what a push uploads the part
	// under, and what a pull verifies the part against.
	dgst digest.Digest
}

// validateRegistryPorts checks the spec fields a push and a pull share: the
// two registry ports and the worker count. The worker check is load bearing,
// not ceremony — zero workers would start no goroutines, drain nothing, and
// let a transfer report success over work that never happened.
func validateRegistryPorts(blobs Blobs, manifests Manifests, workers int) error {
	switch {
	case blobs == nil:
		return errors.New("transfer spec has no blobs port")
	case manifests == nil:
		return errors.New("transfer spec has no manifests port")
	case workers <= 0:
		return fmt.Errorf("worker count must be positive, got %d", workers)
	}

	return nil
}
