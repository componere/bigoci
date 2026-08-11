package auth

import (
	"context"

	"github.com/imgoci/bigoci/internal/oci"
)

// Static answers every registry with one credential the caller supplied.
//
// It is the direct source, for a caller who already holds the secret: a CI job
// with a registry token in its environment, or a program that reads its own
// configuration. Nothing is looked up, no file is read, and no program is run.
//
// Every registry is deliberate. A Static credential goes to whatever host the
// reference names, so the caller — who chose both the secret and the
// reference — is the one deciding who sees it. A [Store] is the other shape:
// it answers only for the host a credential was stored under.
type Static struct {
	// cred is what every lookup answers with.
	cred oci.Credential
}

// NewStatic returns a source that answers every registry with cred.
func NewStatic(cred oci.Credential) *Static {
	return &Static{cred: cred}
}

// Credential returns the fixed credential, whichever registry asks.
//
// It cannot fail and it cannot block, so the context is the port's shape
// rather than anything this implementation needs.
func (s *Static) Credential(_ context.Context, _ oci.Registry) (oci.Credential, error) {
	return s.cred, nil
}
