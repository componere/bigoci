package auth_test

import (
	"github.com/componere/bigoci/internal/auth"
	"github.com/componere/bigoci/internal/oci"
)

// The adapter satisfies the credentials port structurally: the oci package
// defines it and knows nothing about this one, so these assertions are the
// only thing holding the two shapes together. They live in a test file so the
// dependency stays out of the adapter's own import graph, and they fail at
// compile time the moment the port changes.
var (
	_ oci.Credentials = (*auth.Store)(nil)
	_ oci.Credentials = (*auth.Static)(nil)
)
