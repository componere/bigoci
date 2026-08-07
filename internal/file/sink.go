package file

import (
	"fmt"
	"os"
	"path/filepath"
)

// PartialSuffix is appended to the destination path to name the file a pull
// writes into while it runs: pulling to "model.bin" fills
// "model.bin.bigoci-partial". The partial file is a sibling of the
// destination, which keeps the rename that publishes it inside one directory
// and therefore inside one filesystem, where rename is atomic.
const PartialSuffix = ".bigoci-partial"

// partialPerm is the mode the partial file is created with: owner read and
// write, nobody else. The bytes in it come off a network and are unverified
// until the pull finishes, so the conservative mode is the right default; the
// process umask can only narrow it further. Renaming does not change a mode,
// so this is also the mode of a destination the pull created.
const partialPerm os.FileMode = 0o600

// Sink is the destination of a pull: the writable end of a transfer. Workers
// write their parts into it at their byte offsets, in whatever order the
// downloads finish, and a resume reads back what an earlier run left behind.
//
// A Sink never writes to the destination path. Everything lands in a partial
// file next to it, and [Sink.Commit] renames that file onto the destination
// in one step, so the destination is either absent or the complete content.
// Closing a sink without committing leaves the partial file on disk: it is
// the seed the next pull resumes from.
//
// The zero value is not usable; [CreateSink] returns a ready one.
type Sink struct {
	// file is the open handle on the partial file. Its ReadAt and WriteAt are
	// positional and carry no shared cursor, so workers share the one handle
	// without a lock.
	file *os.File
	// partial is the path being written: path plus [PartialSuffix].
	partial string
	// path is the destination [Sink.Commit] renames the partial onto. Nothing
	// else in this package touches it.
	path string
	// closed records that the handle is closed, which makes Close idempotent.
	// Reads, writes, and truncates then fail with an error matching
	// [os.ErrClosed].
	closed bool
	// committed records that Commit renamed the partial onto the destination,
	// which makes a second Commit an error.
	committed bool
}

// CreateSink opens the partial file for path, creating it when it is not
// already there, and returns a sink that writes into it.
//
// CreateSink never touches path itself. An existing partial file is opened as
// it stands and deliberately not truncated: an interrupted pull left those
// bytes, and a later phase hashes the part ranges to decide which of them
// still need fetching. The directory holding path must already exist.
//
// A partial path that exists but is not a regular file — a symbolic link
// planted there, a directory, a device — is refused. The partial name is
// predictable from the destination, so following a pre-planted link would
// hand a writable handle on an arbitrary file to whatever put the link there;
// on platforms with O_NOFOLLOW the open itself enforces this without a race.
//
// Errors come back exactly as the operating system reported them, because
// those errors already name both the path and the operation that failed.
func CreateSink(path string) (*Sink, error) {
	partial := path + PartialSuffix

	if info, err := os.Lstat(partial); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("partial file %s exists but is not a regular file", partial)
	}

	f, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE|noFollow, partialPerm)
	if err != nil {
		return nil, err
	}

	return &Sink{file: f, partial: partial, path: path}, nil
}

// ReadAt reads len(p) bytes from the partial file starting at byte offset
// off. Resume uses it to hash the ranges an earlier run wrote; bytes no run
// ever wrote read back as zeros and fail their part's digest check, which is
// what makes a hole indistinguishable from missing data.
//
// It keeps the ReaderAt contract of the underlying file: a short read returns
// a non-nil error, and a read that runs into the end of the file returns an
// end-of-file error. Calls are safe from many goroutines at once, including
// alongside [Sink.WriteAt].
func (s *Sink) ReadAt(p []byte, off int64) (int, error) {
	return s.file.ReadAt(p, off)
}

// WriteAt writes len(p) bytes to the partial file starting at byte offset
// off. Workers call it concurrently at disjoint offsets, which is what lets
// parts land in whatever order they arrive; [Sink.Truncate] sizes the file up
// front so no write has to extend it.
//
// Writing after [Sink.Commit] or [Sink.Close] fails with an error matching
// [os.ErrClosed]: a published destination is immutable.
func (s *Sink) WriteAt(p []byte, off int64) (int, error) {
	return s.file.WriteAt(p, off)
}

// Size returns the current length of the partial file in bytes.
//
// It stats the open handle rather than a cached number, so it reports what
// [Sink.Truncate] and [Sink.WriteAt] have made of the file. A resume compares
// it against the file size the manifest records to decide whether the leftover
// partial belongs to this pull at all.
func (s *Sink) Size() (int64, error) {
	info, err := s.file.Stat()
	if err != nil {
		return 0, err
	}

	return info.Size(), nil
}

// Truncate sets the partial file's length to size.
//
// A pull calls it once, before any part is fetched, so every part can be
// written at its own offset without the file growing write by write. Growing
// a file this way leaves a hole that reads back as zeros rather than as
// stored bytes, so it costs no disk space until the parts arrive. Truncate
// also shrinks: a leftover partial from some other pull is cut down to the
// size this one needs.
func (s *Sink) Truncate(size int64) error {
	return s.file.Truncate(size)
}

// Commit publishes the partial file's content at the destination path.
//
// It flushes the file to stable storage, closes the handle, renames the
// partial onto the destination, and flushes the directory holding the new
// entry. The file flush has to come first: a crash between a rename and the
// data reaching disk would publish a destination whose name exists while its
// bytes do not, which is exactly the torn file the pull path promises never
// to leave. The directory flush comes last, so a crash right after Commit
// returns cannot lose the rename itself on filesystems that journal metadata
// lazily. The rename replaces any existing destination in one step, so a
// concurrent reader sees either the old file or the new one.
//
// After Commit the sink is spent. Reads, writes, and truncates fail with an
// error matching [os.ErrClosed]; [Sink.Close] becomes a no-op returning nil;
// and a second Commit returns an error. A Commit that fails after the rename
// reports the directory flush that failed: the destination is then in place
// and complete, but its directory entry may not survive a crash.
func (s *Sink) Commit() error {
	if s.committed {
		return fmt.Errorf("sink for %s is already committed", s.path)
	}

	if err := s.file.Sync(); err != nil {
		return err
	}

	if err := s.file.Close(); err != nil {
		return err
	}
	s.closed = true

	if err := os.Rename(s.partial, s.path); err != nil {
		return err
	}
	s.committed = true

	return syncDir(filepath.Dir(s.path))
}

// Close releases the file handle and leaves the partial file on disk. That is
// deliberate: the bytes an abandoned pull already fetched are what the next
// pull resumes from, so nothing here deletes them. A caller that wants the
// partial gone removes it.
//
// Close is idempotent, and it returns nil after [Sink.Commit], which already
// closed the handle.
func (s *Sink) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true

	return s.file.Close()
}
