//go:build unix

package file

import (
	"os"
	"syscall"
)

// noFollow keeps [CreateSink]'s open from traversing a symbolic link planted
// at the partial path, closing the race between the pre-open check and the
// open itself.
const noFollow = syscall.O_NOFOLLOW

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
