package manifest_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/manifest"
)

// The example artifact from the format reference: a 732.5 MiB file pushed at
// the default 512 MiB part size.
const (
	// docFileSize is the size of the example file in bytes.
	docFileSize = 768081920
	// docPartSize is the part size the example was pushed at.
	docPartSize = 536870912
	// docTailSize is the length of the example's short last part.
	docTailSize = 231211008
	// docFileDigest is the digest of the example's complete file.
	docFileDigest = "sha256:9c56cc51b374c3ba189210d5b6d4bf57790d351c96c47c02190ecf1e430c14ed"
	// docFirstPartDigest is the digest of the example's first part.
	docFirstPartDigest = "sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eb7b7e3e6e0e4b6ed6a1a77e"
	// docSecondPartDigest is the digest of the example's second part.
	docSecondPartDigest = "sha256:1f8f9a0a3e9c0f5a02cf47a1a2f8c0be7a05a2f0d5a5b3c67e2f0a8f9b0c1d2e"
)

// overLimitPartCount is one part past the 4096-part cap the format sets.
const overLimitPartCount = 4097

// docExampleManifest is the example manifest from the format reference, copied
// verbatim. Its indentation is deliberate: a reader must accept a manifest it
// did not encode itself.
const docExampleManifest = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "artifactType": "application/vnd.bigoci.file.v1",
  "config": {
    "mediaType": "application/vnd.oci.empty.v1+json",
    "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
    "size": 2
  },
  "layers": [
    {
      "mediaType": "application/vnd.bigoci.file.part.v1",
      "digest": "sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eb7b7e3e6e0e4b6ed6a1a77e",
      "size": 536870912
    },
    {
      "mediaType": "application/vnd.bigoci.file.part.v1",
      "digest": "sha256:1f8f9a0a3e9c0f5a02cf47a1a2f8c0be7a05a2f0d5a5b3c67e2f0a8f9b0c1d2e",
      "size": 231211008
    }
  ],
  "annotations": {
    "io.bigoci.file.digest": "sha256:9c56cc51b374c3ba189210d5b6d4bf57790d351c96c47c02190ecf1e430c14ed",
    "io.bigoci.file.size": "768081920",
    "io.bigoci.part.size": "536870912",
    "org.opencontainers.image.title": "model.bin"
  }
}`

func TestDecodeTheFormatReferenceExample(t *testing.T) {
	decoded, err := manifest.Decode([]byte(docExampleManifest))
	require.NoError(t, err)

	assert.Equal(t, manifest.Artifact{
		FileDigest: docFileDigest,
		FileSize:   docFileSize,
		PartSize:   docPartSize,
		Title:      "model.bin",
		Parts: []manifest.Part{
			{Digest: docFirstPartDigest, Size: docPartSize},
			{Digest: docSecondPartDigest, Size: docTailSize},
		},
	}, decoded)
}

func TestDecodeIgnoresWhatTheFormatDoesNotDefine(t *testing.T) {
	tests := []struct {
		name string
		add  func(m map[string]any)
	}{
		{
			name: "config that also inlines its two bytes",
			add:  func(m map[string]any) { configOf(m)["data"] = "e30=" },
		},
		{
			name: "annotation from outside the format",
			add:  func(m map[string]any) { annotationsOf(m)["com.example.build"] = "42" },
		},
		{
			name: "subject linking the manifest to another one",
			add: func(m map[string]any) {
				m["subject"] = map[string]any{
					"mediaType": ocispec.MediaTypeImageManifest,
					"digest":    digest.FromString("some other manifest").String(),
					"size":      int64(1),
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, err := manifest.Decode(manifestJSON(t, tt.add))
			require.NoError(t, err)
			assert.Equal(t, fixtureArtifact(t, multiPartFileSize, "model.bin"), decoded)
		})
	}
}

func TestDecodeRejectsManifestsThatAreNotBigociArtifacts(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(m map[string]any)
		wantErr string
	}{
		{
			name:    "artifact type of another artifact",
			corrupt: func(m map[string]any) { m["artifactType"] = "application/vnd.example.thing.v1" },
			wantErr: "artifact type is",
		},
		{
			name:    "media type of an image index",
			corrupt: func(m map[string]any) { m["mediaType"] = ocispec.MediaTypeImageIndex },
			wantErr: "media type is",
		},
		{
			name:    "no artifact type at all",
			corrupt: func(m map[string]any) { delete(m, "artifactType") },
			wantErr: "artifact type is",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, err := manifest.Decode(manifestJSON(t, tt.corrupt))
			require.ErrorIs(t, err, manifest.ErrNotBigociArtifact)
			require.ErrorContains(t, err, tt.wantErr)
			assert.Equal(t, manifest.Artifact{}, decoded)
		})
	}
}

func TestDecodeRejectsBrokenArtifacts(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(m map[string]any)
		wantErr string
	}{
		{
			name:    "schema version from another spec",
			corrupt: func(m map[string]any) { m["schemaVersion"] = 1 },
			wantErr: "schema version is 1, want 2",
		},
		{
			name:    "config media type of a real image config",
			corrupt: func(m map[string]any) { configOf(m)["mediaType"] = ocispec.MediaTypeImageConfig },
			wantErr: "config media type is",
		},
		{
			name:    "config digest of some other blob",
			corrupt: func(m map[string]any) { configOf(m)["digest"] = digest.FromString("{}\n").String() },
			wantErr: "config digest is",
		},
		{
			name:    "config size that is not two bytes",
			corrupt: func(m map[string]any) { configOf(m)["size"] = 3 },
			wantErr: "config size is 3, want 2",
		},
		{
			name:    "no layers",
			corrupt: func(m map[string]any) { m["layers"] = []any{} },
			wantErr: "no parts",
		},
		{
			name:    "more layers than the format allows",
			corrupt: overLimitLayers,
			wantErr: "artifact has 4097 parts, more than the maximum of 4096",
		},
		{
			name:    "layer that is a tar layer",
			corrupt: func(m map[string]any) { layersOf(m)[1]["mediaType"] = ocispec.MediaTypeImageLayerGzip },
			wantErr: "layer 1 media type is",
		},
		{
			name:    "missing file digest annotation",
			corrupt: func(m map[string]any) { delete(annotationsOf(m), manifest.AnnotationFileDigest) },
			wantErr: "missing the io.bigoci.file.digest annotation",
		},
		{
			name:    "missing file size annotation",
			corrupt: func(m map[string]any) { delete(annotationsOf(m), manifest.AnnotationFileSize) },
			wantErr: "missing the io.bigoci.file.size annotation",
		},
		{
			name:    "missing part size annotation",
			corrupt: func(m map[string]any) { delete(annotationsOf(m), manifest.AnnotationPartSize) },
			wantErr: "missing the io.bigoci.part.size annotation",
		},
		{
			name:    "no annotations at all",
			corrupt: func(m map[string]any) { delete(m, "annotations") },
			wantErr: "missing the io.bigoci.file.digest annotation",
		},
		{
			name:    "file size that is not a number",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationFileSize] = "2.5 KB" },
			wantErr: "annotation io.bigoci.file.size is \"2.5 KB\", want a base-10 byte count",
		},
		{
			name:    "part size that is not a number",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationPartSize] = "0x3e8" },
			wantErr: "annotation io.bigoci.part.size is \"0x3e8\", want a base-10 byte count",
		},
		{
			name:    "negative file size",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationFileSize] = "-1" },
			wantErr: "file size must not be negative",
		},
		{
			name:    "part size of zero",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationPartSize] = "0" },
			wantErr: "part size must be positive",
		},
		{
			name:    "file size that needs more parts than the manifest has",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationFileSize] = "3500" },
			wantErr: "artifact has 3 parts, but 3500 bytes at part size 1000 split into 4",
		},
		{
			name:    "middle part shorter than the split rule allows",
			corrupt: func(m map[string]any) { layersOf(m)[1]["size"] = partSize - 1 },
			wantErr: "part 1 is 999 bytes, split rule requires 1000",
		},
		{
			name:    "part digest that does not parse",
			corrupt: func(m map[string]any) { layersOf(m)[0]["digest"] = "sha256:nothex" },
			wantErr: `part 0 digest "sha256:nothex"`,
		},
		{
			name:    "file digest from another algorithm",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationFileDigest] = otherDigest(t) },
			wantErr: `file digest algorithm is "sha512"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, err := manifest.Decode(manifestJSON(t, tt.corrupt))
			require.Error(t, err)
			require.NotErrorIs(t, err, manifest.ErrNotBigociArtifact)
			require.ErrorContains(t, err, tt.wantErr)
			assert.Equal(t, manifest.Artifact{}, decoded)
		})
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "truncated document", data: `{"schemaVersion": 2,`},
		{name: "not JSON at all", data: "this is not a manifest"},
		{name: "empty input", data: ""},
		{name: "layers of the wrong JSON type", data: `{"layers": "none"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, err := manifest.Decode([]byte(tt.data))
			require.ErrorContains(t, err, "parse manifest JSON")
			assert.Equal(t, manifest.Artifact{}, decoded)
		})
	}
}

// manifestJSON encodes a valid manifest after change has had its way with it,
// so a test can break exactly one thing about an otherwise correct document.
func manifestJSON(t *testing.T, change func(m map[string]any)) []byte {
	t.Helper()

	document := validManifest(t)
	change(document)

	data, err := json.Marshal(document)
	require.NoError(t, err)

	return data
}

// validManifest builds the manifest of [fixtureArtifact] as a generic JSON
// document, which a test can reshape in ways the typed API would not allow.
func validManifest(t *testing.T) map[string]any {
	t.Helper()

	artifact := fixtureArtifact(t, multiPartFileSize, "model.bin")

	layers := make([]any, 0, len(artifact.Parts))
	for _, part := range artifact.Parts {
		layers = append(layers, map[string]any{
			"mediaType": manifest.MediaTypePart,
			"digest":    part.Digest.String(),
			"size":      part.Size,
		})
	}

	return map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageManifest,
		"artifactType":  manifest.ArtifactType,
		"config": map[string]any{
			"mediaType": ocispec.DescriptorEmptyJSON.MediaType,
			"digest":    ocispec.DescriptorEmptyJSON.Digest.String(),
			"size":      ocispec.DescriptorEmptyJSON.Size,
		},
		"layers": layers,
		"annotations": map[string]any{
			manifest.AnnotationFileDigest: artifact.FileDigest.String(),
			manifest.AnnotationFileSize:   strconv.FormatInt(artifact.FileSize, 10),
			manifest.AnnotationPartSize:   strconv.FormatInt(artifact.PartSize, 10),
			ocispec.AnnotationTitle:       artifact.Title,
		},
	}
}

// overLimitLayers replaces the layers and sizes with one more one-byte part
// than the format's cap allows.
func overLimitLayers(m map[string]any) {
	layers := make([]any, 0, overLimitPartCount)
	for i := range overLimitPartCount {
		layers = append(layers, map[string]any{
			"mediaType": manifest.MediaTypePart,
			"digest":    digest.FromString(fmt.Sprintf("byte-%d", i)).String(),
			"size":      1,
		})
	}

	m["layers"] = layers
	annotationsOf(m)[manifest.AnnotationFileSize] = strconv.Itoa(overLimitPartCount)
	annotationsOf(m)[manifest.AnnotationPartSize] = "1"
}

// configOf returns the config descriptor of a generic manifest document.
func configOf(m map[string]any) map[string]any {
	return m["config"].(map[string]any)
}

// annotationsOf returns the annotations of a generic manifest document.
func annotationsOf(m map[string]any) map[string]any {
	return m["annotations"].(map[string]any)
}

// layersOf returns the layer descriptors of a generic manifest document.
func layersOf(m map[string]any) []map[string]any {
	raw := m["layers"].([]any)

	layers := make([]map[string]any, 0, len(raw))
	for _, layer := range raw {
		layers = append(layers, layer.(map[string]any))
	}

	return layers
}

// otherDigest returns a well-formed digest string that does not use sha256.
func otherDigest(t *testing.T) string {
	t.Helper()

	return otherAlgorithmDigest(t).String()
}
