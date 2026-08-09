package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFixtureIsDeterministicPerIdentity(t *testing.T) {
	t.Parallel()

	const size = 256 << 10

	first, err := writeFixture(t.TempDir(), "run", "cell", 0, size)
	require.NoError(t, err)
	again, err := writeFixture(t.TempDir(), "run", "cell", 0, size)
	require.NoError(t, err)

	assert.Equal(t, first.digest, again.digest,
		"the same run, cell, and iteration must regenerate identical bytes — resume depends on it")

	info, err := os.Stat(first.path)
	require.NoError(t, err)
	assert.EqualValues(t, size, info.Size())
}

func TestWriteFixtureVariesAcrossIdentity(t *testing.T) {
	t.Parallel()

	const size = 64 << 10

	base, err := writeFixture(t.TempDir(), "run", "cell", 0, size)
	require.NoError(t, err)

	tests := []struct {
		name      string
		runID     string
		cellID    string
		iteration int
	}{
		{name: "different iteration", runID: "run", cellID: "cell", iteration: 1},
		{name: "different cell", runID: "run", cellID: "other", iteration: 0},
		{name: "different run", runID: "other", cellID: "cell", iteration: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			variant, err := writeFixture(t.TempDir(), tt.runID, tt.cellID, tt.iteration, size)
			require.NoError(t, err)
			assert.NotEqual(t, base.digest, variant.digest,
				"any identity change must change the bytes, or a deduplicating registry skips the upload")
		})
	}
}

func TestWriteFixtureDigestMatchesTheFileOnDisk(t *testing.T) {
	t.Parallel()

	fx, err := writeFixture(t.TempDir(), "run", "cell", 0, 128<<10)
	require.NoError(t, err)

	hashed, err := hashFile(fx.path)
	require.NoError(t, err)
	assert.Equal(t, fx.digest, hashed, "the digest computed while writing must match a re-read")
}

func TestWriteFixtureRefusesToOverwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := writeFixture(dir, "run", "cell", 0, 1<<10)
	require.NoError(t, err)

	_, err = writeFixture(dir, "run", "cell", 0, 1<<10)
	require.Error(t, err, "a fixture path collision must fail loudly, not silently truncate")
}
