package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/componere/bigoci/internal/plan"
)

// Decode parses manifest JSON and returns the artifact it describes.
//
// Decode is liberal about formatting and strict about the content bigoci
// consumes. Whitespace and member order do not matter, so a manifest fetched
// from a registry decodes even though it was not encoded by [Encode]. Members
// bigoci does not use — a subject, descriptor URLs and annotations, manifest
// annotations outside the format — are skipped without materializing them. A
// config "data" member is accepted when it matches the config digest, and an
// absent body mediaType is tolerated because the image spec makes the embedded
// member optional.
//
// What Decode does not accept is content that breaks the contract. A manifest
// whose media type names something other than an OCI image manifest, or whose
// artifactType is not [ArtifactType], fails with an error wrapping
// [ErrNotBigociArtifact]: it describes something that is not a bigoci
// artifact. Everything else — a schema version other than 2, a config that is
// not the OCI empty descriptor, config data that contradicts the config
// digest, a layer with the wrong media type, a missing or unparseable
// annotation, or parts that disagree with the split rule (wrapping the plan
// package's errors, including [plan.ErrTooManyParts]) — fails with a
// descriptive error against an artifact that claims to be bigoci but is
// broken.
func Decode(data []byte) (Artifact, error) {
	manifest, decodeErr := decodeManifest(data)
	if decodeErr != nil {
		return Artifact{}, fmt.Errorf("parse manifest JSON: %w", decodeErr)
	}

	if err := checkKind(manifest.MediaType, manifest.ArtifactType); err != nil {
		return Artifact{}, err
	}
	if manifest.SchemaVersion != schemaVersion {
		return Artifact{}, fmt.Errorf(
			"manifest schema version is %d, want %d", manifest.SchemaVersion, schemaVersion,
		)
	}
	if err := checkConfig(manifest.Config); err != nil {
		return Artifact{}, err
	}

	artifact, err := readAnnotations(manifest.Annotations)
	if err != nil {
		return Artifact{}, err
	}
	artifact.Parts, err = readLayers(manifest.Layers)
	if err != nil {
		return Artifact{}, err
	}
	if err := validate(artifact); err != nil {
		return Artifact{}, fmt.Errorf("invalid manifest: %w", err)
	}

	return artifact, nil
}

// wireManifest is the bounded subset of an OCI manifest that Decode consumes.
// Optional OCI fields are intentionally absent: encoding/json skips them
// without materializing attacker-sized URL, annotation, platform, or subject
// structures that have no meaning in the bigoci format.
type wireManifest struct {
	// SchemaVersion is the OCI image manifest schema version.
	SchemaVersion int `json:"schemaVersion"`
	// MediaType is the optional embedded OCI manifest media type.
	MediaType string `json:"mediaType,omitempty"`
	// ArtifactType identifies the bigoci artifact format.
	ArtifactType string `json:"artifactType,omitempty"`
	// Config is the OCI empty config descriptor.
	Config wireConfig `json:"config"`
	// Layers are the bounded file-part descriptors.
	Layers wireLayers `json:"layers"`
	// Annotations are the four manifest annotations bigoci consumes.
	Annotations *wireAnnotations `json:"annotations,omitempty"`
}

// wireConfig is the subset of the OCI config descriptor that Decode checks.
type wireConfig struct {
	// MediaType names the config blob's media type.
	MediaType string `json:"mediaType"`
	// Digest identifies the config blob.
	Digest digest.Digest `json:"digest"`
	// Size is the config blob's byte length.
	Size int64 `json:"size"`
	// Data optionally inlines the config blob bytes.
	Data []byte `json:"data,omitempty"`
}

// wireLayer is the subset of an OCI layer descriptor that describes one part.
type wireLayer struct {
	// MediaType names the part blob's media type.
	MediaType string `json:"mediaType"`
	// Digest identifies the part blob.
	Digest digest.Digest `json:"digest"`
	// Size is the part blob's byte length.
	Size int64 `json:"size"`
}

// wireLayers is the bounded sequence of part descriptors on the wire.
type wireLayers []wireLayer

// wireAnnotations is the bounded subset of manifest annotations Decode reads.
type wireAnnotations struct {
	// FileDigest is the digest of the complete file.
	FileDigest string `json:"io.bigoci.file.digest"`
	// FileSize is the complete file size in decimal bytes.
	FileSize string `json:"io.bigoci.file.size"`
	// PartSize is the regular part size in decimal bytes.
	PartSize string `json:"io.bigoci.part.size"`
	// Title is the optional standard OCI artifact title.
	Title string `json:"org.opencontainers.image.title"`
}

// decodeManifest decodes only the bounded fields the bigoci format consumes.
// encoding/json retains its established field matching and duplicate-member
// behavior, while wireLayers refuses an oversized array before materializing
// any descriptor.
func decodeManifest(data []byte) (wireManifest, error) {
	var manifest wireManifest

	return manifest, json.Unmarshal(data, &manifest)
}

// UnmarshalJSON refuses an oversized layer array before decoding descriptors.
// The outer encoding/json syntax pass guarantees data is valid JSON. Decoding
// through the unnamed slice preserves the standard null and duplicate-member
// behavior and reuses capacity across repeated layers members.
func (l *wireLayers) UnmarshalJSON(data []byte) error {
	start := skipJSONSpace(data, 0)
	if data[start] == '[' && countArrayElements(data, start) > plan.MaxParts {
		return fmt.Errorf(
			"%w: manifest has at least %d layers, but the limit is %d",
			plan.ErrTooManyParts,
			plan.MaxParts+1,
			plan.MaxParts,
		)
	}

	return json.Unmarshal(data, (*[]wireLayer)(l))
}

// countArrayElements counts top-level values without retaining element data.
// data is known-valid JSON and start points at its opening array delimiter.
func countArrayElements(data []byte, start int) int {
	offset := skipJSONSpace(data, start+1)
	if data[offset] == ']' {
		return 0
	}

	depth := 1
	count := 1
	inString := false
	escaped := false
	for ; ; offset++ {
		character := data[offset]
		if inString {
			advanceJSONString(character, &inString, &escaped)

			continue
		}

		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return count
			}
		case ',':
			if depth == 1 {
				count++
				if count > plan.MaxParts {
					return count
				}
			}
		}
	}
}

// advanceJSONString updates string-scanner state for one character.
func advanceJSONString(character byte, inString, escaped *bool) {
	switch {
	case *escaped:
		*escaped = false
	case character == '\\':
		*escaped = true
	case character == '"':
		*inString = false
	}
}

// skipJSONSpace returns the first byte that is not JSON whitespace.
func skipJSONSpace(data []byte, start int) int {
	offset := start
	for ; offset < len(data); offset++ {
		switch data[offset] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return offset
		}
	}

	return offset
}

// checkKind reports whether the manifest identifies itself as a bigoci
// artifact. Both mismatches wrap [ErrNotBigociArtifact], which is how a caller
// tells a reference that points at some other artifact apart from a broken
// bigoci one.
//
// An empty media type is accepted when the artifactType matches: the image
// spec only recommends the embedded member, so a conforming third-party
// writer may omit it and let the registry's Content-Type carry the type.
func checkKind(mediaType, artifactType string) error {
	if mediaType != "" && mediaType != ocispec.MediaTypeImageManifest {
		return fmt.Errorf(
			"%w: media type is %q, want %q", ErrNotBigociArtifact, mediaType, ocispec.MediaTypeImageManifest,
		)
	}
	if artifactType != ArtifactType {
		return fmt.Errorf("%w: artifact type is %q, want %q", ErrNotBigociArtifact, artifactType, ArtifactType)
	}

	return nil
}

// checkConfig checks the config descriptor against the OCI empty descriptor.
// The three members the format pins must match exactly. A manifest may also
// inline the config bytes in a "data" member; when it does, the bytes must be
// the two the config digest addresses, because the image spec defines "data"
// as the embedded content of that very blob.
func checkConfig(config wireConfig) error {
	want := ocispec.DescriptorEmptyJSON

	switch {
	case config.MediaType != want.MediaType:
		return fmt.Errorf("config media type is %q, want %q", config.MediaType, want.MediaType)
	case config.Digest != want.Digest:
		return fmt.Errorf("config digest is %q, want %q", config.Digest.String(), want.Digest.String())
	case config.Size != want.Size:
		return fmt.Errorf("config size is %d, want %d", config.Size, want.Size)
	case len(config.Data) > 0 && !bytes.Equal(config.Data, want.Data):
		return fmt.Errorf("config data is %q, want %q or no data at all", config.Data, want.Data)
	}

	return nil
}

// readLayers converts the manifest layers into parts, in file order, checking
// that every layer carries the part media type.
func readLayers(layers wireLayers) ([]Part, error) {
	parts := make([]Part, len(layers))
	for i, layer := range layers {
		if layer.MediaType != MediaTypePart {
			return nil, fmt.Errorf("layer %d media type is %q, want %q", i, layer.MediaType, MediaTypePart)
		}
		parts[i] = Part{Digest: layer.Digest, Size: layer.Size}
	}

	return parts, nil
}

// readAnnotations reads the file description out of the manifest annotations.
// The three bigoci annotations are required; the title is optional and stays
// empty when absent.
func readAnnotations(annotations *wireAnnotations) (Artifact, error) {
	if annotations == nil {
		annotations = &wireAnnotations{}
	}

	fileDigest, err := requiredAnnotation(annotations.FileDigest, AnnotationFileDigest)
	if err != nil {
		return Artifact{}, err
	}
	fileSize, err := sizeAnnotation(annotations.FileSize, AnnotationFileSize)
	if err != nil {
		return Artifact{}, err
	}
	partSize, err := sizeAnnotation(annotations.PartSize, AnnotationPartSize)
	if err != nil {
		return Artifact{}, err
	}

	return Artifact{
		FileDigest: digest.Digest(fileDigest),
		FileSize:   fileSize,
		PartSize:   plan.PartSize(partSize),
		Title:      annotations.Title,
		Parts:      nil,
	}, nil
}

// requiredAnnotation returns value, or an error naming the annotation that is
// missing. An empty value counts as missing.
func requiredAnnotation(value, key string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("manifest is missing the %s annotation", key)
	}

	return value, nil
}

// sizeAnnotation returns value parsed as a base-10 byte count.
func sizeAnnotation(value, key string) (int64, error) {
	value, err := requiredAnnotation(value, key)
	if err != nil {
		return 0, err
	}

	size, err := strconv.ParseInt(value, decimalBase, sizeBits)
	if err != nil {
		return 0, fmt.Errorf("annotation %s is %q, want a base-10 byte count: %w", key, value, err)
	}

	return size, nil
}
