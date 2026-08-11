//go:build unix

package file_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/file"
)

// TestCreateSinkRefusesPartialsAccessibleByAnotherUser proves that reopening
// never changes or writes a permissive partial, even when the current user
// owns it and the operating system would allow the open.
func TestCreateSinkRefusesPartialsAccessibleByAnotherUser(t *testing.T) {
	tests := []struct {
		name        string
		permissions os.FileMode
	}{
		{name: "group-readable partial", permissions: 0o640},
		{name: "group-writable partial", permissions: 0o620},
		{name: "other-readable partial", permissions: 0o604},
		{name: "other-writable partial", permissions: 0o602},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest, partial := newSinkPaths(t)
			const original = "bytes that must stay untouched"

			require.NoError(t, os.WriteFile(partial, []byte(original), partialPerm))
			require.NoError(t, os.Chmod(partial, tt.permissions))

			sink, err := file.CreateSink(dest)

			require.ErrorContains(t, err, "grant access to group or other users")
			assert.Nil(t, sink)
			requireFileContent(t, partial, original)
			requireAbsent(t, dest, "refusing a permissive partial must not publish a destination")

			info, statErr := os.Stat(partial)
			require.NoError(t, statErr)
			assert.Equal(t, tt.permissions, info.Mode().Perm(), "a refused partial must never be chmodded")
		})
	}
}
