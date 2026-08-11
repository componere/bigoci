package mocks

import (
	"github.com/imgoci/bigoci/internal/transfer"
)

// The generated mocks must keep satisfying the ports they double. These
// asserts fail the build the moment a port changes without the mocks being
// regenerated, because nothing else ties the two together at compile time.
var (
	_ transfer.Blobs     = (*MockBlobs)(nil)
	_ transfer.Manifests = (*MockManifests)(nil)
)
