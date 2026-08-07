package file

import (
	"fmt"
	"os"
)

// Source is an open file a push reads from: the readable end of a transfer.
// Workers read their own byte ranges out of it concurrently, and nothing ever
// writes through it.
//
// The zero value is not usable; [OpenSource] returns a ready one. A Source
// holds an operating system file handle, so whoever opens one closes it with
// [Source.Close].
type Source struct {
	// file is the open handle parts are read from. Its ReadAt is a positional
	// read that carries no shared cursor, so every worker calls it on the one
	// handle without coordination.
	file *os.File
	// size is the file's length in bytes, read once at open time. Push plans
	// the split from it; a file that changes length underneath a running push
	// is a caller error rather than something to re-stat for.
	size int64
}

// OpenSource opens the file at path for reading and records its size.
//
// The returned Source reads from a single handle for the life of the push, so
// retrying a part re-streams it from disk instead of holding it in memory.
//
// Errors come back exactly as the operating system reported them, because
// those errors already name both the path and the operation that failed. A
// path that does not exist reports an error matching [os.ErrNotExist]. A path
// that is not a regular file — a directory, a device — is refused here, where
// the answer is clear, instead of surfacing later as a read error mid-push.
func OpenSource(path string) (*Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	if !info.Mode().IsRegular() {
		_ = f.Close()

		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	return &Source{file: f, size: info.Size()}, nil
}

// ReadAt reads len(p) bytes from the source starting at byte offset off.
//
// It keeps the ReaderAt contract of the underlying file: a read that returns
// fewer than len(p) bytes returns a non-nil error explaining why, and a read
// that runs into the end of the file returns the bytes it found along with an
// end-of-file error. Calls are safe from many goroutines at once.
func (s *Source) ReadAt(p []byte, off int64) (int, error) {
	return s.file.ReadAt(p, off)
}

// Size returns the length of the file in bytes as recorded when [OpenSource]
// opened it. It never fails and never re-stats, so the split plan a push
// computes from it stays fixed for the whole transfer.
func (s *Source) Size() int64 {
	return s.size
}

// Close releases the file handle. Reads after Close fail with an error
// matching [os.ErrClosed].
func (s *Source) Close() error {
	return s.file.Close()
}
