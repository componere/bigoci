package transfer

import (
	"bytes"
	"crypto/sha256"
	"io"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/manifest"
	"github.com/imgoci/bigoci/internal/plan"
)

// shortSource is a file that holds fewer bytes than the split plan claimed,
// the shape a truncated source takes during the hash pass.
type shortSource []byte

// ReadAt answers from the bytes the source still holds.
func (s shortSource) ReadAt(p []byte, off int64) (int, error) {
	return bytes.NewReader(s).ReadAt(p, off)
}

// Size reports the length the source still holds. hashParts does not call it;
// the plan already carries the length claimed at the start of the push.
func (s shortSource) Size() int64 {
	return int64(len(s))
}

func TestSourceChangedMatchesBothPaths(t *testing.T) {
	t.Parallel()

	t.Run("a short part in the hash pass", func(t *testing.T) {
		t.Parallel()

		split, err := plan.New(1000, 1000)
		require.NoError(t, err)

		jobs := make(chan partJob, 1)
		_, err = hashParts(
			t.Context(),
			shortSource(bytes.Repeat([]byte{'a'}, 40)),
			split,
			make([]manifest.Part, 1),
			jobs,
			nil,
		)
		require.ErrorIs(t, err, errSourceChanged)
		require.EqualError(t, err, "part 0 is 40 bytes, but the plan expects 1000: the source changed while the push read it")
	})

	t.Run("a same-length mutation the upload hashes", func(t *testing.T) {
		t.Parallel()

		original := []byte("original")
		mutated := []byte("mutated!")
		require.Len(t, mutated, len(original))

		reader := &tagSourceReads{
			r:      bytes.NewReader(mutated),
			hasher: sha256.New(),
			expect: digest.FromBytes(original),
			size:   int64(len(original)),
		}

		_, err := io.ReadFull(reader, make([]byte, len(original)))
		require.ErrorIs(t, err, errSourceChanged)
		require.EqualError(t, err, errSourceChanged.Error())
	})
}
