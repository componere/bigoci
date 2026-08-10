//go:build unix

package bigoci_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci"
	"github.com/componere/bigoci/internal/file"
)

// TestPullRefusesAnUnsafePartialBeforeTheNetwork proves the public client opens
// and validates its destination before asking even for the manifest. The
// refused file is left byte-for-byte unchanged.
func TestPullRefusesAnUnsafePartialBeforeTheNetwork(t *testing.T) {
	reg := newRegistry(t)
	dest := newPath(t, destName)
	partial := dest + file.PartialSuffix
	const original = "bytes controlled outside this pull"

	require.NoError(t, os.WriteFile(partial, []byte(original), fixturePerm))
	require.NoError(t, os.Chmod(partial, 0o644))

	err := newClient(t, bigoci.WithPlainHTTP()).Pull(t.Context(), reg.taggedRef(tag), bigoci.ToFile(dest))

	require.ErrorContains(t, err, "grant access to group or other users")
	requests, _ := reg.counts()
	assert.Zero(t, requests, "an unsafe partial must be refused before the registry is contacted")

	content, readErr := os.ReadFile(partial)
	require.NoError(t, readErr)
	assert.Equal(t, original, string(content))
	assert.NoFileExists(t, dest)

	info, statErr := os.Stat(partial)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "the refused partial must not be chmodded")
}
