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

// ErrNotBigociArtifact reports that the reference resolves to something else:
// a container image, an artifact of another kind, or a manifest whose
// artifactType is not the one the bigoci format defines.
//
// It is the failure that means "look somewhere else", as opposed to every
// other manifest error, which means the artifact claims to be bigoci and is
// broken.
var ErrNotBigociArtifact = errors.New("not a bigoci artifact")

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
//
// The design also names an unauthorized case and a part the registry rejected
// as too large. Neither can happen yet — this phase talks to registries
// anonymously and does not classify a failed upload — so they arrive with the
// authentication and retry phases rather than being declared empty here.
func classify(err error) error {
	switch {
	case errors.Is(err, oci.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, manifest.ErrNotBigociArtifact):
		return fmt.Errorf("%w: %w", ErrNotBigociArtifact, err)
	case errors.Is(err, transfer.ErrDigestMismatch):
		return fmt.Errorf("%w: %w", ErrDigestMismatch, err)
	default:
		return err
	}
}
