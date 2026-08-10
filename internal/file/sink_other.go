//go:build !unix

package file

import "os"

// noFollow is zero where the platform has no O_NOFOLLOW. [openPartial]'s
// exclusive create cannot follow an existing link, and its pre-open regular
// file check still refuses a statically planted symbolic link.
const noFollow = 0

// validatePartialAccess adds no ownership rule on platforms without Unix UID
// metadata. [openPartial] still preserves static symlink refusal and checks
// [os.SameFile] where the platform can substantiate it, without claiming an
// ownership or race-free pathname boundary this adapter cannot establish.
func validatePartialAccess(_ os.FileInfo, _ bool) error {
	return nil
}

// syncDir is a no-op where directories cannot be opened and flushed the unix
// way; the rename in [Sink.Commit] is still atomic, its durability is just
// left to the filesystem.
func syncDir(string) error {
	return nil
}
