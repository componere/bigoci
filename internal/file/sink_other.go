//go:build !unix

package file

// noFollow is zero where the platform has no O_NOFOLLOW. The pre-open
// [os.Lstat] check in [CreateSink] still refuses a symbolic link, only
// without the race-free guarantee unix provides.
const noFollow = 0

// syncDir is a no-op where directories cannot be opened and flushed the unix
// way; the rename in [Sink.Commit] is still atomic, its durability is just
// left to the filesystem.
func syncDir(string) error {
	return nil
}
