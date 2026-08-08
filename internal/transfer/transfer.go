package transfer

import (
	"errors"

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
