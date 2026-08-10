package bigoci

// FileSource is the file end of a push: the path [Client.Push] reads the
// bytes from.
//
// The zero value names no file. [FromFile] builds a usable one.
type FileSource struct {
	// path is the file to upload.
	path string
}

// FromFile names the local file a push uploads.
//
// Nothing is opened here. The file is opened and measured when
// [Client.Push] runs, and closed before it returns, so building a source
// cannot fail and holding one costs no file handle. A file that is missing,
// unreadable, or not a regular file is reported by the push that tries to
// read it.
//
// The file must not change while the push runs: its size is read once and the
// split plan, the part digests, and the manifest all describe what was there
// at that moment.
func FromFile(path string) FileSource {
	return FileSource{path: path}
}

// FileDest is the file end of a pull: the path [Client.Pull] writes the
// assembled file to.
//
// The zero value names no file. [ToFile] builds a usable one.
type FileDest struct {
	// path is the file to write.
	path string
}

// ToFile names the local file a pull writes.
//
// The pull writes into a sibling file named path plus ".bigoci-partial" and
// renames it onto path only once every part has verified, so the destination
// is never observed half written. The directory holding path must already
// exist.
//
// A pull that fails leaves the partial file behind on purpose: those bytes
// are what a later resume starts from. On Unix, a later pull resumes only from
// the same private regular file it observed before opening, owned by the
// current identity. Other platforms reject statically planted links and
// non-regular files but cannot make the Unix ownership guarantee. A partial
// that fails is left untouched. A pull that finishes replaces the destination.
func ToFile(path string) FileDest {
	return FileDest{path: path}
}
