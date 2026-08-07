package plan

import (
	"errors"
	"fmt"
	"iter"
)

// MaxParts is the largest number of parts a bigoci artifact may hold. The
// format caps the count so manifests stay well under the manifest size limit
// registries enforce; a file that needs more parts must use a larger part
// size.
const MaxParts = 4096

// ErrTooManyParts reports that a file and part size would split into more
// than [MaxParts] parts. Callers recover by retrying with a larger part size;
// the wrapped message names the smallest one that fits.
var ErrTooManyParts = errors.New("too many parts")

// Part is one fixed-size piece of a file: the bytes in the half-open range
// starting at Offset and running for Size bytes.
type Part struct {
	// Index is the zero-based position of the part in the file, which is also
	// its position in the manifest's layer list.
	Index int
	// Offset is the byte offset in the file where the part starts.
	Offset int64
	// Size is the length of the part in bytes. Every part but the last is a
	// full part size; the last may be shorter.
	Size int64
}

// Plan is the split of one file into parts. It is an immutable value: the
// zero Plan holds no parts, and a Plan returned by [New] never changes.
// Copying a Plan is free and safe to share across goroutines.
type Plan struct {
	// fileSize is the total length of the planned file in bytes.
	fileSize int64
	// partSize is the part size P the split was computed with.
	partSize int64
	// numParts is how many parts the file splits into: at least one, never
	// more than MaxParts.
	numParts int
}

// New computes the split plan for a file of fileSize bytes at partSize bytes
// per part.
//
// Every file has at least one part, including an empty one: a zero-byte file
// plans a single zero-byte part, which the split rule allows because part 0
// covers the empty range at offset 0.
//
// New returns an error when partSize is not positive, when fileSize is
// negative, or when the split would need more than [MaxParts] parts. The last
// case wraps [ErrTooManyParts] and can be tested with [errors.Is].
func New(fileSize, partSize int64) (Plan, error) {
	if partSize <= 0 {
		return Plan{}, fmt.Errorf("part size must be positive, got %d", partSize)
	}

	if fileSize < 0 {
		return Plan{}, fmt.Errorf("file size must not be negative, got %d", fileSize)
	}

	numParts := countParts(fileSize, partSize)
	if numParts > MaxParts {
		return Plan{}, fmt.Errorf(
			"%w: a %d-byte file needs %d parts at part size %d, but the limit is %d; "+
				"retry with a part size of at least %d bytes",
			ErrTooManyParts, fileSize, numParts, partSize, MaxParts, smallestPartSize(fileSize),
		)
	}

	return Plan{fileSize: fileSize, partSize: partSize, numParts: int(numParts)}, nil
}

// FileSize returns the total length of the planned file in bytes.
func (p Plan) FileSize() int64 {
	return p.fileSize
}

// PartSize returns the part size the plan was computed with. Only the last
// part may be shorter than this.
func (p Plan) PartSize() int64 {
	return p.partSize
}

// NumParts returns how many parts the file splits into. It is at least one
// for any plan [New] returned.
func (p Plan) NumParts() int {
	return p.numParts
}

// Part returns the part at index i.
//
// It panics when i is negative or not less than [Plan.NumParts], because an
// out-of-range index is a bug in the caller rather than a condition worth
// reporting. Use [Plan.Parts] to walk every part without bounds handling.
func (p Plan) Part(i int) Part {
	if i < 0 || i >= p.numParts {
		panic(fmt.Sprintf("plan: part index %d out of range, plan has %d parts", i, p.numParts))
	}

	return p.part(i)
}

// Parts returns an [iter.Seq] over the plan's parts in file order.
//
// Breaking out of the loop early is safe, and the sequence may be ranged over
// as many times as the caller likes: a Plan carries no iteration state.
func (p Plan) Parts() iter.Seq[Part] {
	return func(yield func(Part) bool) {
		for i := range p.numParts {
			if !yield(p.part(i)) {
				return
			}
		}
	}
}

// part returns the part at index i without checking the index. The offset
// cannot overflow: a plan only holds index i when i*partSize is a byte offset
// inside the file, so the product is bounded by fileSize.
func (p Plan) part(i int) Part {
	offset := int64(i) * p.partSize

	return Part{
		Index:  i,
		Offset: offset,
		Size:   min(p.partSize, p.fileSize-offset),
	}
}

// countParts returns how many parts a file of fileSize bytes splits into at
// partSize bytes per part, where partSize is positive and fileSize is not
// negative. It divides first and adds the remainder afterwards instead of
// rounding up with (fileSize+partSize-1)/partSize, which overflows for file
// sizes near the top of the int64 range.
func countParts(fileSize, partSize int64) int64 {
	parts := fileSize / partSize
	if parts == 0 || fileSize%partSize != 0 {
		parts++
	}

	return parts
}

// smallestPartSize returns the smallest part size that splits a file of
// fileSize bytes into at most [MaxParts] parts, for use in the error a
// too-large split reports.
func smallestPartSize(fileSize int64) int64 {
	size := fileSize / MaxParts
	if fileSize%MaxParts != 0 {
		size++
	}

	return size
}
