package manifest

import (
	"encoding/json"
	"fmt"
	"strconv"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Decode parses manifest JSON and returns the artifact it describes.
//
// Decode is liberal about formatting and strict about content. Any JSON that
// unmarshals into an OCI image manifest is accepted, whatever its whitespace
// or member order, so a manifest fetched from a registry decodes even though
// it was not encoded by [Encode]. Members bigoci does not use — a subject, a
// config "data" member, annotations outside the format — are ignored, so a
// future format revision that adds fields stays readable.
//
// What Decode does not accept is content that breaks the contract. A manifest
// whose media type is not an OCI image manifest, or whose artifactType is not
// [ArtifactType], fails with an error wrapping [ErrNotBigociArtifact]: it
// describes something that is not a bigoci artifact. Everything else — a
// schema version other than 2, a config that is not the OCI empty descriptor,
// a layer with the wrong media type, a missing or unparseable annotation, or
// parts that disagree with the split rule — fails with a descriptive error
// against an artifact that claims to be bigoci but is broken.
func Decode(data []byte) (Artifact, error) {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Artifact{}, fmt.Errorf("parse manifest JSON: %w", err)
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

// checkKind reports whether the manifest identifies itself as a bigoci
// artifact. Both mismatches wrap [ErrNotBigociArtifact], which is how a caller
// tells a reference that points at some other artifact apart from a broken
// bigoci one.
func checkKind(mediaType, artifactType string) error {
	if mediaType != ocispec.MediaTypeImageManifest {
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
// Only the three members the format pins are compared: a manifest that also
// inlines the two config bytes in a "data" member is accepted, because that
// spelling refers to the same blob.
func checkConfig(config ocispec.Descriptor) error {
	want := ocispec.DescriptorEmptyJSON

	switch {
	case config.MediaType != want.MediaType:
		return fmt.Errorf("config media type is %q, want %q", config.MediaType, want.MediaType)
	case config.Digest != want.Digest:
		return fmt.Errorf("config digest is %q, want %q", config.Digest.String(), want.Digest.String())
	case config.Size != want.Size:
		return fmt.Errorf("config size is %d, want %d", config.Size, want.Size)
	}

	return nil
}

// readLayers converts the manifest layers into parts, in file order, checking
// that every layer carries the part media type.
func readLayers(layers []ocispec.Descriptor) ([]Part, error) {
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
func readAnnotations(annotations map[string]string) (Artifact, error) {
	fileDigest, err := requiredAnnotation(annotations, AnnotationFileDigest)
	if err != nil {
		return Artifact{}, err
	}
	fileSize, err := sizeAnnotation(annotations, AnnotationFileSize)
	if err != nil {
		return Artifact{}, err
	}
	partSize, err := sizeAnnotation(annotations, AnnotationPartSize)
	if err != nil {
		return Artifact{}, err
	}

	return Artifact{
		FileDigest: digest.Digest(fileDigest),
		FileSize:   fileSize,
		PartSize:   partSize,
		Title:      annotations[ocispec.AnnotationTitle],
		Parts:      nil,
	}, nil
}

// requiredAnnotation returns the value of key, or an error naming the
// annotation the manifest is missing. An empty value counts as missing.
func requiredAnnotation(annotations map[string]string, key string) (string, error) {
	value := annotations[key]
	if value == "" {
		return "", fmt.Errorf("manifest is missing the %s annotation", key)
	}

	return value, nil
}

// sizeAnnotation returns the value of key parsed as a base-10 byte count.
func sizeAnnotation(annotations map[string]string, key string) (int64, error) {
	value, err := requiredAnnotation(annotations, key)
	if err != nil {
		return 0, err
	}

	size, err := strconv.ParseInt(value, decimalBase, sizeBits)
	if err != nil {
		return 0, fmt.Errorf("annotation %s is %q, want a base-10 byte count: %w", key, value, err)
	}

	return size, nil
}
