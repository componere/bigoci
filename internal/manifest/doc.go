// Package manifest encodes and decodes the bigoci artifact manifest.
//
// A bigoci artifact is a standard OCI image manifest whose layers are
// consecutive byte ranges of one file, called parts. This package owns how
// those bytes look on the wire: [Encode] turns an [Artifact] into the
// canonical manifest JSON, [Decode] turns manifest JSON back into an
// [Artifact], and both enforce the same invariants, so bigoci never writes a
// manifest it would refuse to read.
//
// The split rule the two sides check against lives in the plan package, which
// stays the single source of truth for how a file divides into parts.
//
// The format contract — media types, annotations, manifest layout, and limits
// — is documented at https://componere.github.io/bigoci/reference/format/.
//
// The package is pure: it performs no I/O and holds no state.
package manifest
