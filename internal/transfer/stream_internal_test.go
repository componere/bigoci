package transfer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/plan"
)

// This file probes stream from inside the package, because the rule it pins
// is invisible at the Pull boundary: a write-side failure is untagged and
// therefore never retried, so whether progress was recorded for one cannot be
// observed through any sequence of port calls. The rule still matters. After
// a refused write the hasher holds bytes the sink never took, and a
// continuation from a count that included them would hash the two histories
// together — if write failures ever became retryable, recording progress here
// would surface as a false [ErrDigestMismatch].

// errRefused is the failure the probe's sink raises once it has taken its fill.
var errRefused = errors.New("the disk is full")

// refusingSink is storage for the probe: a Sink whose WriteAt accepts a few
// bytes and then refuses the rest, the way a disk that ran out of space does.
// The methods stream never touches fail the test if anything reaches them.
type refusingSink struct {
	// t reports a call stream has no business making.
	t *testing.T
	// take is how many bytes WriteAt accepts before it starts refusing.
	take int
	// wrote counts the bytes WriteAt accepted.
	wrote int
}

// WriteAt takes what fits under the sink's fill and refuses the rest, keeping
// the [io.WriterAt] contract of a non-nil error for a short write.
func (s *refusingSink) WriteAt(p []byte, _ int64) (int, error) {
	room := s.take - s.wrote
	if room >= len(p) {
		s.wrote += len(p)

		return len(p), nil
	}

	s.wrote += room

	return room, errRefused
}

// ReadAt is never part of a stream.
func (s *refusingSink) ReadAt([]byte, int64) (int, error) {
	s.t.Fatal("stream read from the sink")

	return 0, nil
}

// Size is never part of a stream.
func (s *refusingSink) Size() (int64, error) {
	s.t.Fatal("stream measured the sink")

	return 0, nil
}

// Truncate is never part of a stream.
func (s *refusingSink) Truncate(int64) error {
	s.t.Fatal("stream truncated the sink")

	return nil
}

// Commit is never part of a stream.
func (s *refusingSink) Commit() error {
	s.t.Fatal("stream committed the sink")

	return nil
}

// TestStreamRecordsNoProgressForARefusedWrite pins the write-side half of the
// recording rule: a copy that ends because the destination refused bytes
// leaves the progress count exactly where the attempt found it.
func TestStreamRecordsNoProgressForARefusedWrite(t *testing.T) {
	t.Parallel()

	content := bytes.Repeat([]byte{0x5A}, 1000)
	part := plan.Part{Index: 0, Offset: 0, Size: 1000}

	fetcher := &partFetcher{
		sink:   &refusingSink{t: t, take: 40},
		buf:    make([]byte, 100),
		hasher: sha256.New(),
	}

	var done int64

	err := fetcher.stream(bytes.NewReader(content), part, &done)
	require.ErrorIs(t, err, errRefused)
	require.ErrorContains(t, err, "write part 0 into the destination")

	// The hasher holds bytes the sink never took, so no later attempt may
	// build on this count.
	assert.Zero(t, done, "a refused write records nothing")
}
