package file_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/file"
)

// sourceContent is the fixture the read tests index into. Every byte differs
// from every other, so a read that lands at the wrong offset cannot pass by
// accident.
const sourceContent = "0123456789abcdef"

// fixturePerm is the mode the tests write their own fixture files with. It
// only has to be readable by the test process; the mode [file.CreateSink]
// picks is asserted separately.
const fixturePerm os.FileMode = 0o600

func TestOpenSourceReportsFileSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "an empty file is a legal source of size zero", content: ""},
		{name: "a file reports the number of bytes it holds", content: sourceContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			source := newSource(t, tt.content)

			assert.Equal(t, int64(len(tt.content)), source.Size())
		})
	}
}

func TestSourceReadAtReadsTheRequestedRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		offset  int64
		length  int
		want    string
		wantErr error
	}{
		{name: "a read at the start returns the first bytes", offset: 0, length: 4, want: "0123"},
		{name: "a read at an interior offset returns that range", offset: 6, length: 4, want: "6789"},
		{name: "a read of the final byte returns it", offset: 15, length: 1, want: "f"},
		{name: "a read spanning the file returns every byte", offset: 0, length: 16, want: sourceContent},
		{
			name:    "a read running past the end returns what is there and reports EOF",
			offset:  12,
			length:  8,
			want:    "cdef",
			wantErr: io.EOF,
		},
		{
			name:    "a read starting at the end reports EOF and returns nothing",
			offset:  16,
			length:  1,
			want:    "",
			wantErr: io.EOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			source := newSource(t, sourceContent)

			buf := make([]byte, tt.length)
			n, err := source.ReadAt(buf, tt.offset)

			if tt.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.wantErr)
			}
			assert.Equal(t, tt.want, string(buf[:n]))
		})
	}
}

func TestSourceReadAtServesConcurrentReaders(t *testing.T) {
	t.Parallel()

	source := newSource(t, sourceContent)

	// One reader per byte over the single shared handle: push workers stream
	// their parts at the same time, so nothing here may carry a cursor or
	// other mutable state. Run under -race this is the check that says so.
	got := make([]byte, len(sourceContent))
	errs := make([]error, len(sourceContent))

	var wg sync.WaitGroup
	for i := range sourceContent {
		wg.Go(func() {
			_, errs[i] = source.ReadAt(got[i:i+1], int64(i))
		})
	}
	wg.Wait()

	require.NoError(t, errors.Join(errs...))
	assert.Equal(t, sourceContent, string(got))
}

func TestOpenSourceRefusesADirectory(t *testing.T) {
	t.Parallel()

	source, err := file.OpenSource(t.TempDir())

	require.ErrorContains(t, err, "not a regular file")
	assert.Nil(t, source)
}

func TestOpenSourceFailsWhenTheFileIsMissing(t *testing.T) {
	t.Parallel()

	source, err := file.OpenSource(filepath.Join(t.TempDir(), "absent.bin"))

	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, source)
}

func TestSourceReadAtFailsAfterClose(t *testing.T) {
	t.Parallel()

	source := newSource(t, sourceContent)
	require.NoError(t, source.Close())

	_, err := source.ReadAt(make([]byte, 1), 0)

	assert.ErrorIs(t, err, os.ErrClosed)
}

// newSource writes content to a fresh file and opens it as a source. The
// source closes when the test ends, and closing it earlier stays legal
// because [file.Source.Close] is only called again by the cleanup, which
// tolerates the error.
func newSource(t *testing.T, content string) *file.Source {
	t.Helper()

	path := filepath.Join(t.TempDir(), "source.bin")
	require.NoError(t, os.WriteFile(path, []byte(content), fixturePerm))

	source, err := file.OpenSource(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	return source
}
