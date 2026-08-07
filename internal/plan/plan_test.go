package plan_test

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/plan"
)

const (
	// partSize is the part size most fixtures below split at. Its value is
	// arbitrary; the arithmetic under test does not depend on it.
	partSize = int64(100)
	// capacity is the largest file that fits the part cap at partSize.
	capacity = int64(plan.MaxParts) * partSize
	// hugePartSize is a part size large enough that a maximum-length file
	// still fits under the part cap, used to prove offsets do not overflow.
	hugePartSize = int64(1) << 50
)

// newPlan builds a plan and fails the test when the inputs are not plannable.
func newPlan(t *testing.T, fileSize, size int64) plan.Plan {
	t.Helper()

	p, err := plan.New(fileSize, size)
	require.NoError(t, err)

	return p
}

func TestNewCountsParts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileSize int64
		partSize int64
		want     int
	}{
		{
			name:     "an empty file still has one part",
			fileSize: 0,
			partSize: partSize,
			want:     1,
		},
		{
			name:     "a file smaller than the part size has one part",
			fileSize: partSize - 1,
			partSize: partSize,
			want:     1,
		},
		{
			name:     "a file exactly the part size has one part",
			fileSize: partSize,
			partSize: partSize,
			want:     1,
		},
		{
			name:     "a file one byte over the part size has two parts",
			fileSize: partSize + 1,
			partSize: partSize,
			want:     2,
		},
		{
			name:     "a whole multiple of the part size has no short tail",
			fileSize: 3 * partSize,
			partSize: partSize,
			want:     3,
		},
		{
			name:     "one byte past a whole multiple adds a tail part",
			fileSize: 3*partSize + 1,
			partSize: partSize,
			want:     4,
		},
		{
			name:     "a file that fills the part cap exactly is planned",
			fileSize: capacity,
			partSize: partSize,
			want:     plan.MaxParts,
		},
		{
			name:     "a part size larger than the file collapses to one part",
			fileSize: 1,
			partSize: math.MaxInt64,
			want:     1,
		},
		{
			name:     "the largest possible file fits in one maximal part",
			fileSize: math.MaxInt64,
			partSize: math.MaxInt64,
			want:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := plan.New(tt.fileSize, tt.partSize)
			require.NoError(t, err)

			assert.Equal(t, tt.want, p.NumParts())
			assert.Equal(t, tt.fileSize, p.FileSize())
			assert.Equal(t, tt.partSize, p.PartSize())
		})
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileSize int64
		partSize int64
	}{
		{
			name:     "a zero part size is rejected",
			fileSize: partSize,
			partSize: 0,
		},
		{
			name:     "a negative part size is rejected",
			fileSize: partSize,
			partSize: -partSize,
		},
		{
			name:     "a negative file size is rejected",
			fileSize: -1,
			partSize: partSize,
		},
		{
			name:     "a negative file size is rejected before the part size",
			fileSize: -1,
			partSize: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := plan.New(tt.fileSize, tt.partSize)

			require.Error(t, err)
			assert.NotErrorIs(t, err, plan.ErrTooManyParts)
		})
	}
}

func TestNewRejectsSplitsOverThePartCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileSize int64
		partSize int64
	}{
		{
			name:     "one byte past the part cap needs a larger part size",
			fileSize: capacity + 1,
			partSize: partSize,
		},
		{
			name:     "a file far past the part cap needs a larger part size",
			fileSize: 2 * capacity,
			partSize: partSize,
		},
		{
			name:     "the largest possible file overflows a one-byte part size",
			fileSize: math.MaxInt64,
			partSize: 1,
		},
		{
			name:     "the largest possible file overflows a large part size",
			fileSize: math.MaxInt64,
			partSize: hugePartSize,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := plan.New(tt.fileSize, tt.partSize)

			require.ErrorIs(t, err, plan.ErrTooManyParts)
			assert.Zero(t, p.NumParts())
		})
	}
}

func TestNewSuggestsAPartSizeThatFits(t *testing.T) {
	t.Parallel()

	_, err := plan.New(capacity+1, partSize)
	require.ErrorIs(t, err, plan.ErrTooManyParts)

	assert.Contains(t, err.Error(), fmt.Sprintf("at least %d bytes", partSize+1))

	p, retryErr := plan.New(capacity+1, partSize+1)
	require.NoError(t, retryErr)
	assert.LessOrEqual(t, p.NumParts(), plan.MaxParts)
}

func TestPartCoversItsByteRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileSize int64
		partSize int64
		index    int
		want     plan.Part
	}{
		{
			name:     "the only part of an empty file is empty",
			fileSize: 0,
			partSize: partSize,
			index:    0,
			want:     plan.Part{Index: 0, Offset: 0, Size: 0},
		},
		{
			name:     "the only part of a short file is the whole file",
			fileSize: partSize - 1,
			partSize: partSize,
			index:    0,
			want:     plan.Part{Index: 0, Offset: 0, Size: partSize - 1},
		},
		{
			name:     "the first part starts at offset zero",
			fileSize: 2*partSize + 50,
			partSize: partSize,
			index:    0,
			want:     plan.Part{Index: 0, Offset: 0, Size: partSize},
		},
		{
			name:     "a middle part is a whole part size",
			fileSize: 2*partSize + 50,
			partSize: partSize,
			index:    1,
			want:     plan.Part{Index: 1, Offset: partSize, Size: partSize},
		},
		{
			name:     "the last part is the short tail",
			fileSize: 2*partSize + 50,
			partSize: partSize,
			index:    2,
			want:     plan.Part{Index: 2, Offset: 2 * partSize, Size: 50},
		},
		{
			name:     "the last part of a whole multiple is a full part",
			fileSize: 3 * partSize,
			partSize: partSize,
			index:    2,
			want:     plan.Part{Index: 2, Offset: 2 * partSize, Size: partSize},
		},
		{
			name:     "a one-byte tail is its own part",
			fileSize: 3*partSize + 1,
			partSize: partSize,
			index:    3,
			want:     plan.Part{Index: 3, Offset: 3 * partSize, Size: 1},
		},
		{
			name:     "the last part of a cap-filling plan lands on a huge offset",
			fileSize: int64(plan.MaxParts) * hugePartSize,
			partSize: hugePartSize,
			index:    plan.MaxParts - 1,
			want: plan.Part{
				Index:  plan.MaxParts - 1,
				Offset: int64(plan.MaxParts-1) * hugePartSize,
				Size:   hugePartSize,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newPlan(t, tt.fileSize, tt.partSize)

			assert.Equal(t, tt.want, p.Part(tt.index))
		})
	}
}

func TestPartsCoverTheFileExactly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileSize int64
		partSize int64
	}{
		{
			name:     "an empty file",
			fileSize: 0,
			partSize: partSize,
		},
		{
			name:     "a file shorter than one part",
			fileSize: 1,
			partSize: partSize,
		},
		{
			name:     "a whole multiple of the part size",
			fileSize: 4 * partSize,
			partSize: partSize,
		},
		{
			name:     "a file with a one-byte tail",
			fileSize: 4*partSize + 1,
			partSize: partSize,
		},
		{
			name:     "a file that fills the part cap",
			fileSize: capacity,
			partSize: partSize,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newPlan(t, tt.fileSize, tt.partSize)
			parts := slices.Collect(p.Parts())

			require.Len(t, parts, p.NumParts())

			var covered int64

			for i, part := range parts {
				assert.Equal(t, p.Part(i), part)
				assert.Equal(t, i, part.Index)
				assert.Equal(t, covered, part.Offset)

				covered += part.Size
			}

			assert.Equal(t, tt.fileSize, covered)
			assert.NotEmpty(t, parts)
		})
	}
}

func TestPartsStopsWhenTheCallerBreaks(t *testing.T) {
	t.Parallel()

	p := newPlan(t, capacity, partSize)

	var seen []plan.Part

	for part := range p.Parts() {
		seen = append(seen, part)

		if len(seen) == 2 {
			break
		}
	}

	require.Len(t, seen, 2)
	assert.Equal(t, p.Part(0), seen[0])
	assert.Equal(t, p.Part(1), seen[1])
}

func TestPartsCanBeIteratedAgain(t *testing.T) {
	t.Parallel()

	p := newPlan(t, 3*partSize+1, partSize)

	first := slices.Collect(p.Parts())
	second := slices.Collect(p.Parts())

	require.Len(t, first, p.NumParts())
	assert.Equal(t, first, second)
}

func TestPartPanicsOutsideThePlan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		index int
	}{
		{
			name:  "a negative index panics",
			index: -1,
		},
		{
			name:  "the index one past the last part panics",
			index: 3,
		},
		{
			name:  "a far out-of-range index panics",
			index: plan.MaxParts,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newPlan(t, 3*partSize, partSize)

			assert.Panics(t, func() { _ = p.Part(tt.index) })
		})
	}
}

func TestZeroPlanHasNoParts(t *testing.T) {
	t.Parallel()

	var p plan.Plan

	assert.Zero(t, p.NumParts())
	assert.Zero(t, p.FileSize())
	assert.Zero(t, p.PartSize())
	assert.Empty(t, slices.Collect(p.Parts()))
}

func ExamplePlan_Parts() {
	p, err := plan.New(250, 100)
	if err != nil {
		panic(err)
	}

	for part := range p.Parts() {
		fmt.Printf("part %d covers [%d, %d)\n", part.Index, part.Offset, part.Offset+part.Size)
	}
	// Output:
	// part 0 covers [0, 100)
	// part 1 covers [100, 200)
	// part 2 covers [200, 250)
}
