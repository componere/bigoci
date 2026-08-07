package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/componere/bigoci/internal/plan"
)

// The bigoci media types. The ".v1" suffix is the format version: a breaking
// change to the format means new media types, and readers keep accepting
// every version they know.
const (
	// ArtifactType is the artifactType of a bigoci manifest. A manifest that
	// carries any other value describes something that is not a bigoci
	// artifact.
	ArtifactType = "application/vnd.bigoci.file.v1"
	// MediaTypePart is the media type of a part layer. Parts are raw byte
	// ranges: no compression, tar, or framing.
	MediaTypePart = "application/vnd.bigoci.file.part.v1"
)

// The manifest-level annotation keys that describe the stored file. All three
// are required; the file name travels in the standard
// ocispec.AnnotationTitle annotation instead of a bigoci-specific key.
const (
	// AnnotationFileDigest holds the sha256 digest of the complete file, in
	// "sha256:<hex>" form.
	AnnotationFileDigest = "io.bigoci.file.digest"
	// AnnotationFileSize holds the size of the complete file in bytes, as a
	// base-10 string.
	AnnotationFileSize = "io.bigoci.file.size"
	// AnnotationPartSize holds the part size in bytes, as a base-10 string.
	AnnotationPartSize = "io.bigoci.part.size"
)

// How the size annotations are written and read: base-10 integers that fit in
// an int64.
const (
	// decimalBase is the numeric base of the size annotations.
	decimalBase = 10
	// sizeBits is the width the size annotations are parsed into.
	sizeBits = 64
)

// schemaVersion is the schema version every OCI image manifest carries. The
// image spec fixes it at 2.
const schemaVersion = 2

// ErrNotBigociArtifact reports a manifest that is not a bigoci artifact: its
// media type is not an OCI image manifest, or its artifactType is not
// [ArtifactType]. It is the one failure callers branch on, because it means
// "this reference points at something else" rather than "this artifact is
// broken", which is what every other [Decode] error means.
var ErrNotBigociArtifact = errors.New("not a bigoci artifact")

// Part is one layer of a bigoci artifact: the digest and size of the blob
// holding one byte range of the file. A part's position in the file is its
// index in [Artifact.Parts].
type Part struct {
	// Digest is the digest of the part's bytes, as stored in the registry.
	Digest digest.Digest
	// Size is the length of the part in bytes.
	Size int64
}

// Artifact is the decoded content of a bigoci manifest: everything the format
// records about one stored file. The zero value is not a valid artifact.
type Artifact struct {
	// FileDigest is the sha256 digest of the complete file. It is
	// informational — pulls verify parts against their own digests — and lets
	// anyone confirm what file an artifact carries without bigoci.
	FileDigest digest.Digest
	// FileSize is the size of the complete file in bytes.
	FileSize int64
	// PartSize is the part size the file was split at, in bytes. Pull reads it
	// instead of guessing.
	PartSize int64
	// Title is the file name recorded at push time. It is informational, and
	// empty when the manifest carries no title annotation.
	Title string
	// Parts are the parts in file order. Concatenating them reproduces the
	// file.
	Parts []Part
}

// Encode marshals a into the canonical JSON encoding of a bigoci manifest.
//
// The encoding is canonical in the sense the format reference means: compact
// JSON with no whitespace, a field order fixed by the OCI manifest struct, and
// annotation keys in sorted order because [encoding/json.Marshal] sorts map
// keys. The same [Artifact] therefore encodes to byte-identical output on
// every run and in every process, which is what lets a re-push of the same
// file at the same part size reproduce the same manifest digest.
//
// Encode validates a first and returns a descriptive error when it breaks the
// format contract: no parts, more parts than the format allows, part sizes
// that disagree with the split rule, a file digest that is unparseable or not
// sha256, an unparseable part digest, a part size that is not positive, or a
// negative file size. [Decode] enforces the same rules, so bigoci never writes
// a manifest it would later refuse to read.
func Encode(a Artifact) ([]byte, error) {
	if err := validate(a); err != nil {
		return nil, fmt.Errorf("invalid artifact: %w", err)
	}

	layers := make([]ocispec.Descriptor, len(a.Parts))
	for i, part := range a.Parts {
		layers[i] = ocispec.Descriptor{
			MediaType: MediaTypePart,
			Digest:    part.Digest,
			Size:      part.Size,
		}
	}

	annotations := map[string]string{
		AnnotationFileDigest: a.FileDigest.String(),
		AnnotationFileSize:   strconv.FormatInt(a.FileSize, decimalBase),
		AnnotationPartSize:   strconv.FormatInt(a.PartSize, decimalBase),
	}
	if a.Title != "" {
		annotations[ocispec.AnnotationTitle] = a.Title
	}

	data, err := json.Marshal(ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: schemaVersion},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType,
		Config:       emptyConfig(),
		Layers:       layers,
		Annotations:  annotations,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}

	return data, nil
}

// emptyConfig returns the OCI empty descriptor a bigoci manifest uses as its
// config.
//
// ocispec.DescriptorEmptyJSON carries the two config bytes inline in its Data
// field, which would marshal into a "data" member the format's manifest does
// not have. Clearing Data on the copy keeps the encoding canonical; readers
// accept the descriptor either way.
func emptyConfig() ocispec.Descriptor {
	config := ocispec.DescriptorEmptyJSON
	config.Data = nil

	return config
}

// validate checks an artifact against the format contract. [Encode] and
// [Decode] both call it, so a writer and a reader enforce identical
// invariants.
//
// The part checks defer to the plan package: the number of parts and every
// part's size must match the split the file size and part size imply, which
// keeps the split rule defined in exactly one place.
func validate(a Artifact) error {
	if a.PartSize <= 0 {
		return fmt.Errorf("part size must be positive, got %d", a.PartSize)
	}
	if a.FileSize < 0 {
		return fmt.Errorf("file size must not be negative, got %d", a.FileSize)
	}
	if err := validateFileDigest(a.FileDigest); err != nil {
		return err
	}

	return validateParts(a)
}

// validateFileDigest checks that the whole-file digest parses and uses sha256,
// the algorithm the format names.
func validateFileDigest(fileDigest digest.Digest) error {
	if err := fileDigest.Validate(); err != nil {
		return fmt.Errorf("file digest %q: %w", fileDigest.String(), err)
	}
	if algorithm := fileDigest.Algorithm(); algorithm != digest.SHA256 {
		return fmt.Errorf("file digest algorithm is %q, want %q", algorithm, digest.SHA256)
	}

	return nil
}

// validateParts checks the part count and every part against the split the
// artifact's file size and part size imply.
func validateParts(a Artifact) error {
	if len(a.Parts) == 0 {
		return errors.New("artifact has no parts, want at least one")
	}
	if len(a.Parts) > plan.MaxParts {
		return fmt.Errorf("artifact has %d parts, more than the maximum of %d", len(a.Parts), plan.MaxParts)
	}

	split, err := plan.New(a.FileSize, a.PartSize)
	if err != nil {
		return fmt.Errorf("split of %d bytes at part size %d: %w", a.FileSize, a.PartSize, err)
	}
	if split.NumParts() != len(a.Parts) {
		return fmt.Errorf(
			"artifact has %d parts, but %d bytes at part size %d split into %d",
			len(a.Parts), a.FileSize, a.PartSize, split.NumParts(),
		)
	}

	for i, part := range a.Parts {
		if err := part.Digest.Validate(); err != nil {
			return fmt.Errorf("part %d digest %q: %w", i, part.Digest.String(), err)
		}
		if want := split.Part(i).Size; part.Size != want {
			return fmt.Errorf("part %d is %d bytes, split rule requires %d", i, part.Size, want)
		}
	}

	return nil
}
