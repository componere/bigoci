//go:build unix

package file

import (
	"fmt"
	"os"
	"syscall"
)

// noFollow keeps [CreateSink]'s open from traversing a symbolic link planted
// at the partial path, closing the race between the pre-open check and the
// open itself.
const noFollow = syscall.O_NOFOLLOW

// validatePartialAccess proves that info describes a file owned privately by
// the process's effective user. The check applies to new and existing files;
// existing is unused because Unix exposes the same trustworthy metadata for
// both.
func validatePartialAccess(info os.FileInfo, _ bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect owner: unexpected file metadata %T", info.Sys())
	}

	want := os.Geteuid()
	if int64(stat.Uid) != int64(want) {
		return fmt.Errorf("owned by user %d, current effective user is %d", stat.Uid, want)
	}
	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		return fmt.Errorf("permissions %04o grant access to group or other users", permissions)
	}

	return nil
}

// syncDir flushes the directory at path, making a just-renamed entry in it
// durable. [Sink.Commit] calls it after the rename that publishes the
// destination, because a rename is metadata and metadata can reach disk after
// the data it names.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}

	if err := dir.Sync(); err != nil {
		_ = dir.Close()

		return err
	}

	return dir.Close()
}
