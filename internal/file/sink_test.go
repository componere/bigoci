package file_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/file"
)

// The content a pull assembles in these tests. It is three parts of partLen
// bytes, each filled with its own character, so a part written at the wrong
// offset shows up in the assembled file instead of hiding.
const (
	// partLen is the length of one part of pulledContent.
	partLen = 4
	// pulledContent is the complete file a committed sink must hold.
	pulledContent = "AAAABBBBCCCC"
)

// partialPerm is the mode [file.CreateSink] must give the partial file: owner
// read and write, nobody else, because the bytes in it are unverified until
// the pull finishes.
const partialPerm os.FileMode = 0o600

func TestCreateSinkOpensAPartialAndLeavesTheDestinationAlone(t *testing.T) {
	t.Parallel()

	dest, partial := newSinkPaths(t)

	sink := newSink(t, dest)

	info, err := os.Stat(partial)
	require.NoError(t, err)
	assert.Equal(t, partialPerm, info.Mode().Perm())
	assert.Zero(t, info.Size(), "a new partial starts empty")
	requireAbsent(t, dest, "creating a sink must not touch the destination")

	size, err := sink.Size()
	require.NoError(t, err)
	assert.Zero(t, size)
}

func TestSinkAssemblesPartsWrittenOutOfOrder(t *testing.T) {
	t.Parallel()

	dest, partial := newSinkPaths(t)

	sink := newSink(t, dest)
	require.NoError(t, sink.Truncate(int64(len(pulledContent))))

	size, err := sink.Size()
	require.NoError(t, err)
	assert.Equal(t, int64(len(pulledContent)), size, "Truncate sizes the sink before any part lands")

	// Last part first: a part's offset decides where it lands, never the
	// order the downloads happen to finish in.
	for _, index := range []int{2, 0, 1} {
		offset := int64(index * partLen)

		n, writeErr := sink.WriteAt([]byte(pulledContent[offset:offset+partLen]), offset)
		require.NoError(t, writeErr)
		assert.Equal(t, partLen, n)
	}

	buf := make([]byte, partLen)
	n, err := sink.ReadAt(buf, partLen)
	require.NoError(t, err)
	assert.Equal(t, pulledContent[partLen:2*partLen], string(buf[:n]), "a sink reads back what was written")

	require.NoError(t, sink.Commit())

	requireFileContent(t, dest, pulledContent)
	requireAbsent(t, partial, "Commit moves the partial rather than copying it")
}

func TestSinkAcceptsConcurrentPartWrites(t *testing.T) {
	t.Parallel()

	dest, partial := newSinkPaths(t)

	sink := newSink(t, dest)
	require.NoError(t, sink.Truncate(int64(len(pulledContent))))

	// Every part at once over the single shared handle, which is how pull
	// workers use a sink. Run under -race this is the check that the adapter
	// added no shared mutable state to the file handle.
	numParts := len(pulledContent) / partLen
	errs := make([]error, numParts)

	var wg sync.WaitGroup
	for i := range numParts {
		wg.Go(func() {
			offset := int64(i * partLen)
			_, errs[i] = sink.WriteAt([]byte(pulledContent[offset:offset+partLen]), offset)
		})
	}
	wg.Wait()

	require.NoError(t, errors.Join(errs...))
	require.NoError(t, sink.Commit())

	requireFileContent(t, dest, pulledContent)
	requireAbsent(t, partial, "Commit moves the partial rather than copying it")
}

func TestSinkIsSpentAfterCommit(t *testing.T) {
	t.Parallel()

	dest, _ := newSinkPaths(t)

	sink := newSink(t, dest)
	require.NoError(t, sink.Commit())

	_, err := sink.WriteAt([]byte("x"), 0)
	require.ErrorIs(t, err, os.ErrClosed, "a published destination is immutable")

	_, err = sink.ReadAt(make([]byte, 1), 0)
	require.ErrorIs(t, err, os.ErrClosed)

	require.ErrorIs(t, sink.Truncate(0), os.ErrClosed)

	_, err = sink.Size()
	require.ErrorIs(t, err, os.ErrClosed)

	require.ErrorContains(t, sink.Commit(), "already committed", "publishing twice is a caller bug")
	assert.NoError(t, sink.Close(), "Close after Commit is a no-op")
	assert.NoError(t, sink.Close(), "Close stays a no-op however often it is called")
}

func TestSinkDiscardRemovesThePartial(t *testing.T) {
	t.Parallel()

	dest, partial := newSinkPaths(t)

	sink := newSink(t, dest)
	_, err := sink.WriteAt([]byte(pulledContent[:partLen]), 0)
	require.NoError(t, err)

	require.NoError(t, sink.Discard())

	requireAbsent(t, partial, "Discard removes the partial a pull decided not to keep")
	requireAbsent(t, dest, "Discard never touches the destination")
}

func TestCreateSinkRefusesAPlantedSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dest := filepath.Join(dir, "pulled.bin")
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("precious bytes"), fixturePerm))
	require.NoError(t, os.Symlink(victim, dest+file.PartialSuffix))

	sink, err := file.CreateSink(dest)

	require.Error(t, err, "a partial that is a symlink must be refused, not followed")
	assert.Nil(t, sink)
	requireFileContent(t, victim, "precious bytes")
}

func TestSinkCloseWithoutCommitKeepsThePartialForResume(t *testing.T) {
	t.Parallel()

	dest, partial := newSinkPaths(t)

	sink := newSink(t, dest)
	require.NoError(t, sink.Truncate(int64(len(pulledContent))))

	_, err := sink.WriteAt([]byte(pulledContent[:partLen]), 0)
	require.NoError(t, err)
	require.NoError(t, sink.Close())
	assert.NoError(t, sink.Close(), "Close is idempotent")

	requireAbsent(t, dest, "an abandoned pull publishes nothing")
	requireFileContent(
		t, partial,
		pulledContent[:partLen]+strings.Repeat("\x00", len(pulledContent)-partLen),
	)

	resumed := newSink(t, dest)

	size, err := resumed.Size()
	require.NoError(t, err)
	assert.Equal(t, int64(len(pulledContent)), size, "reopening a partial must not truncate it")

	buf := make([]byte, partLen)
	_, err = resumed.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, pulledContent[:partLen], string(buf), "the earlier run's bytes seed the resume")
}

func TestSinkCommitReplacesAnExistingDestination(t *testing.T) {
	t.Parallel()

	dest, partial := newSinkPaths(t)
	require.NoError(t, os.WriteFile(dest, []byte("a stale file from an earlier pull"), fixturePerm))

	sink := newSink(t, dest)

	_, err := sink.WriteAt([]byte(pulledContent), 0)
	require.NoError(t, err)
	require.NoError(t, sink.Commit())

	requireFileContent(t, dest, pulledContent)
	requireAbsent(t, partial, "Commit moves the partial rather than copying it")
}

func TestSinkCommitPublishesAnEmptyFile(t *testing.T) {
	t.Parallel()

	dest, partial := newSinkPaths(t)

	sink := newSink(t, dest)
	require.NoError(t, sink.Truncate(0))
	require.NoError(t, sink.Commit())

	requireFileContent(t, dest, "")
	requireAbsent(t, partial, "Commit moves the partial rather than copying it")
}

func TestSinkTruncateSetsTheLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		sizes []int64
		want  string
	}{
		{
			name:  "growing an empty sink fills the new range with zeros",
			sizes: []int64{partLen},
			want:  strings.Repeat("\x00", partLen),
		},
		{
			name:  "shrinking cuts a leftover partial down to the size this pull needs",
			sizes: []int64{2 * partLen, partLen},
			want:  strings.Repeat("\x00", partLen),
		},
		{
			name:  "truncating to zero empties the sink",
			sizes: []int64{partLen, 0},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dest, partial := newSinkPaths(t)

			sink := newSink(t, dest)
			for _, size := range tt.sizes {
				require.NoError(t, sink.Truncate(size))
			}

			size, err := sink.Size()
			require.NoError(t, err)
			assert.Equal(t, int64(len(tt.want)), size)

			require.NoError(t, sink.Close())
			requireFileContent(t, partial, tt.want)
		})
	}
}

// newSinkPaths returns a destination path in a directory of its own, together
// with the partial path [file.CreateSink] derives from it.
func newSinkPaths(t *testing.T) (string, string) {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "pulled.bin")

	return dest, dest + file.PartialSuffix
}

// newSink creates a sink for dest and closes it when the test ends. Closing
// it earlier stays legal, because [file.Sink.Close] is idempotent.
func newSink(t *testing.T, dest string) *file.Sink {
	t.Helper()

	sink, err := file.CreateSink(dest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sink.Close() })

	return sink
}

// requireFileContent asserts that the file at path holds exactly want.
func requireFileContent(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

// requireAbsent asserts that nothing exists at path, explaining why with msg.
func requireAbsent(t *testing.T, path, msg string) {
	t.Helper()

	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist, msg)
}
