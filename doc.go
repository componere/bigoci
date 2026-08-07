// Package bigoci uploads and downloads large files to and from OCI
// registries. Large means 5 GB and up, into the tens of GB. That is the
// whole library.
//
// bigoci stores a file as fixed-size parts pushed as OCI blobs, listed in
// order as the layers of a standard OCI image manifest. This makes push and
// pull parallel, retryable, and resumable on every registry. The design and
// the artifact format are documented at
// https://componere.github.io/bigoci/.
//
// The public API is not yet implemented; see the design document for the
// planned surface.
//
// bigoci is dual-licensed under Apache-2.0 and MIT, at your option.
package bigoci
