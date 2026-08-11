package file_test

import (
	"github.com/imgoci/bigoci/internal/file"
	"github.com/imgoci/bigoci/internal/transfer"
)

// The adapter satisfies the transfer ports structurally: nothing in the
// package imports the core, so these assertions are the only thing holding
// the two shapes together. They live in a test file so the dependency stays
// out of the adapter's own import graph, and they fail at compile time the
// moment a port method changes.
var (
	_ transfer.Source = (*file.Source)(nil)
	_ transfer.Sink   = (*file.Sink)(nil)
)
