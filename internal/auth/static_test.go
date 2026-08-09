package auth_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/auth"
	"github.com/componere/bigoci/internal/oci"
)

// TestStaticAnswersEveryRegistryWithTheSameCredential pins what Static is
// for: one fixed credential, whatever registry asks.
func TestStaticAnswersEveryRegistryWithTheSameCredential(t *testing.T) {
	t.Parallel()

	want := oci.Credential{Username: "ci", Password: "token"}
	static := auth.NewStatic(want)

	for _, registry := range []oci.Registry{fixtureRegistry, hubRegistry, "ghcr.io"} {
		got, err := static.Credential(noExec(t), registry)

		require.NoError(t, err)
		assert.Equal(t, want, got, "a static credential is the caller's choice for whatever host they named")
	}
}

// TestStaticCarriesTheAnonymousCredentialUnchanged pins that a zero Static
// is the anonymous source rather than an error.
func TestStaticCarriesTheAnonymousCredentialUnchanged(t *testing.T) {
	t.Parallel()

	got, err := auth.NewStatic(oci.Credential{}).Credential(noExec(t), fixtureRegistry)

	require.NoError(t, err)
	assert.True(t, got.Empty(), "the zero credential stays the anonymous one rather than becoming a lookup failure")
}
