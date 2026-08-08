package manifest

import (
	"bytes"

	// Register sha256 with go-digest. Digest validation reports any
	// unregistered algorithm as unsupported, and nothing else in this
	// package's non-test dependency graph links the one hash the format
	// requires — the tests cannot catch its absence because the testing
	// framework links it for them.
	_ "crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

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
	// Digest is the sha256 digest of the part's bytes, as stored in the
	// registry.
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
	PartSize plan.PartSize
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
// JSON with no whitespace, a member order fixed by the OCI manifest struct,
// annotation keys in sorted order because [encoding/json] sorts map keys, and
// raw UTF-8 with no HTML escaping, so a title like "a&b.bin" reads back
// byte-for-byte. The same [Artifact] therefore encodes to byte-identical
// output on every run, in every process, and in any conforming third-party
// implementation, which is what lets a re-push of the same file at the same
// part size reproduce the same manifest digest.
//
// Encode validates a first and returns a descriptive error when it breaks the
// format contract: no parts, a split the plan package rejects (wrapping its
// errors, including [plan.ErrTooManyParts]), part sizes that disagree with
// the split rule, a digest that is unparseable or not sha256, or a title that
// is not valid UTF-8. [Decode] enforces the same rules, so bigoci never
// writes a manifest it would later refuse to read.
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
		AnnotationPartSize:   strconv.FormatInt(int64(a.PartSize), decimalBase),
	}
	if a.Title != "" {
		annotations[ocispec.AnnotationTitle] = a.Title
	}

	return encodeCanonical(ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: schemaVersion},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType,
		Config:       emptyConfig(),
		Layers:       layers,
		Annotations:  annotations,
	})
}

// encodeCanonical marshals the manifest without the HTML escaping
// [encoding/json.Marshal] applies. Escaping "&", "<", and ">" would make
// bigoci's bytes diverge from every non-Go implementation of the format the
// moment a title contains one of them, breaking the shared manifest digest
// the format's determinism promises.
func encodeCanonical(manifest ocispec.Manifest) ([]byte, error) {
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(manifest); err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// EmptyConfig returns the OCI empty descriptor a bigoci manifest carries as
// its config, together with the two bytes that descriptor addresses.
//
// A push needs both halves. A registry rejects a manifest that references a
// blob it does not hold, so the pusher asks for the descriptor's digest and
// uploads the content when the repository is missing it, before it writes the
// manifest. Everything else in bigoci only ever needs the descriptor.
//
// The content is a fresh copy on every call, so a caller may stream it, wrap
// it, or hold onto it without reaching the slice the image spec exports.
func EmptyConfig() (ocispec.Descriptor, []byte) {
	// ocispec.DescriptorEmptyJSON carries the two config bytes inline in its
	// Data field, which would marshal into a "data" member the format's
	// manifest does not have. Clearing Data on the copy keeps the encoding
	// canonical; readers accept the descriptor either way.
	descriptor := ocispec.DescriptorEmptyJSON
	descriptor.Data = nil

	return descriptor, bytes.Clone(ocispec.DescriptorEmptyJSON.Data)
}

// emptyConfig returns the descriptor half of [EmptyConfig], which is all the
// encoder needs.
func emptyConfig() ocispec.Descriptor {
	descriptor, _ := EmptyConfig()

	return descriptor
}

// validate checks an artifact against the format contract. [Encode] and
// [Decode] both call it, so a writer and a reader enforce identical
// invariants.
//
// The size checks and the part checks defer to the plan package: the file
// size, the part size, the number of parts, and every part's size must match
// the split they imply, which keeps the split rule defined in exactly one
// place. Split errors wrap the plan package's errors, including
// [plan.ErrTooManyParts].
func validate(a Artifact) error {
	if a.Title != "" && !utf8.ValidString(a.Title) {
		return fmt.Errorf("title %q is not valid UTF-8", a.Title)
	}
	if err := validateDigest("file", a.FileDigest); err != nil {
		return err
	}

	return validateParts(a)
}

// validateDigest checks that a digest parses and uses sha256, the one
// algorithm the format allows. The what prefix names the digest in the error:
// "file", "part 0", and so on.
func validateDigest(what string, d digest.Digest) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("%s digest %q: %w", what, d.String(), err)
	}
	if algorithm := d.Algorithm(); algorithm != digest.SHA256 {
		return fmt.Errorf("%s digest algorithm is %q, want %q", what, algorithm, digest.SHA256)
	}

	return nil
}

// validateParts checks the part count and every part against the split the
// artifact's file size and part size imply.
func validateParts(a Artifact) error {
	if len(a.Parts) == 0 {
		return errors.New("artifact has no parts, want at least one")
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
		if err := validateDigest(fmt.Sprintf("part %d", i), part.Digest); err != nil {
			return err
		}
		if want := split.Part(i).Size; part.Size != want {
			return fmt.Errorf("part %d is %d bytes, split rule requires %d", i, part.Size, want)
		}
	}

	return nil
}
