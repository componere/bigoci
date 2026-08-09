package bigoci

import (
	"errors"
	"fmt"

	"github.com/componere/bigoci/internal/manifest"
	"github.com/componere/bigoci/internal/oci"
	"github.com/componere/bigoci/internal/transfer"
)

// ErrNotFound reports that the registry does not hold what a transfer named:
// the manifest a reference resolves to, or a part a pull tried to read.
//
// A pull of a tag that was never pushed reports it, and so does a pull whose
// artifact lost a blob to garbage collection between the manifest fetch and
// the part read.
var ErrNotFound = errors.New("not found")

// ErrUnauthorized reports that the registry refused the transfer rather than
// answering it. A 401 is the registry asking for credentials the transfer
// did not present. A 403 is the registry saying the credentials it read do
// not reach the repository the reference names. Both report here — and so
// does a proxy or firewall in front of the registry answering 403 about
// something else, because bigoci cannot tell those apart and does not guess.
//
// The fix is to log in to that registry, which for most setups is
// "docker login <registry>", and to check that the account it logs in as may
// read the repository — or write it, for a push.
var ErrUnauthorized = errors.New("unauthorized")

// ErrNotBigociArtifact reports that the reference resolves to something else:
// a container image, an artifact of another kind, or a manifest whose
// artifactType is not the one the bigoci format defines.
//
// It is the failure that means "look somewhere else", as opposed to every
// other manifest error, which means the artifact claims to be bigoci and is
// broken.
var ErrNotBigociArtifact = errors.New("not a bigoci artifact")

// ErrPartTooLarge reports that the registry refused a part as larger than it
// accepts.
//
// This is how a registry's layer cap surfaces. bigoci ships no table of
// vendor limits — the caps differ per registry, they move, and a stale table
// is worse than none — so the limit is discovered by being told about it,
// once, by the registry that enforces it.
//
// The fix is to push again with a smaller [WithPartSize]. Note that the same
// file at a different part size is a different artifact with a different
// manifest digest, because the part size is part of what the manifest
// describes: the second push is not another route to the first one's result.
// The wrapped error names the part and the status the registry answered with.
var ErrPartTooLarge = errors.New("part too large")

// ErrDigestMismatch reports that pulled bytes hash differently than the
// manifest says they should. The part is named in the wrapped error.
//
// The destination is untouched when this happens: a pull publishes nothing
// until every part verifies, so a mismatch leaves the partial file behind and
// the destination absent or holding its previous content.
var ErrDigestMismatch = errors.New("digest mismatch")

// classify attaches the sentinel that describes err, so a caller can branch
// on it with [errors.Is] without knowing which layer underneath produced the
// failure. err must not be nil.
//
// The mapping lives here, in one place, because the internal packages own
// their own sentinels and none of them may name a public one: the dependency
// only points inward. An error that matches nothing public comes back
// unchanged rather than being dressed up as a case it is not.
func classify(err error) error {
	switch {
	case errors.Is(err, oci.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, oci.ErrUnauthorized):
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	case errors.Is(err, oci.ErrTooLarge):
		return fmt.Errorf("%w: %w", ErrPartTooLarge, err)
	case errors.Is(err, manifest.ErrNotBigociArtifact):
		return fmt.Errorf("%w: %w", ErrNotBigociArtifact, err)
	case errors.Is(err, transfer.ErrDigestMismatch):
		return fmt.Errorf("%w: %w", ErrDigestMismatch, err)
	default:
		return err
	}
}
