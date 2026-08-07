package mocks

import (
	"github.com/componere/bigoci/internal/transfer"
)

// The generated mocks must keep satisfying the ports they double. These
// asserts fail the build the moment a port changes without the mocks being
// regenerated, because nothing else ties the two together at compile time.
var (
	_ transfer.Source = (*MockSource)(nil)
	_ transfer.Sink   = (*MockSink)(nil)
)
