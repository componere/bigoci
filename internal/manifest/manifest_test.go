package manifest_test

import (
	// Blank import registers sha512 with go-digest, so the non-sha256 fixture
	// below is a well-formed digest and reaches the algorithm check.
	_ "crypto/sha512"
	"fmt"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/manifest"
)

// Sizes shared by the fixtures. They are small on purpose: neither the split
// rule nor the encoding cares how large a part is.
const (
	// partSize is the part size every fixture splits at.
	partSize = 1000
	// multiPartFileSize splits into three parts, the last one short.
	multiPartFileSize = 2500
	// shortPartSize is the length of that short last part.
	shortPartSize = 500
	// exactMultipleFileSize splits into two full parts with no short tail.
	exactMultipleFileSize = 2000
	// smallFileSize is below the part size and splits into one part.
	smallFileSize = 10
)

// escapedTitle is a file name built from every character
// [encoding/json.Marshal] would HTML-escape. The canonical encoding must
// carry it raw, or bigoci's manifest digests diverge from every non-Go
// implementation of the format.
const escapedTitle = `a&b<c>.bin`

// emptyConfigDigest is the digest of the OCI empty config blob, copied from
// the format reference so the value the code returns is checked against the
// documented one rather than against itself.
const emptyConfigDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

// goldenManifest is the exact encoding of the artifact in TestEncodeGolden. It
// is written out in full so any drift in the canonical encoding — field order,
// annotation order, whitespace, escaping — fails loudly instead of quietly
// changing every manifest digest bigoci would ever publish.
const goldenManifest = `{"schemaVersion":2,` +
	`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
	`"artifactType":"application/vnd.bigoci.file.v1",` +
	`"config":{"mediaType":"application/vnd.oci.empty.v1+json",` +
	`"digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},` +
	`"layers":[` +
	`{"mediaType":"application/vnd.bigoci.file.part.v1",` +
	`"digest":"sha256:fdf9aadb4eab87b259634f1b43ecfa46b7064582e217e370120661577bc6fdd4","size":1000},` +
	`{"mediaType":"application/vnd.bigoci.file.part.v1",` +
	`"digest":"sha256:0543cf94c5dce225bc7708829029e777b6a73381cda354ca07bd81199e4ddcca","size":1000},` +
	`{"mediaType":"application/vnd.bigoci.file.part.v1",` +
	`"digest":"sha256:2bb41b3bc344d2a5c1f31d662d86d78d7e98198b1eef7be3209d4f85da4ef14d","size":500}],` +
	`"annotations":{` +
	`"io.bigoci.file.digest":"sha256:8d11726942452cf799b3d1ce7915502c0339238779b77d36cbbc81eca817eda3",` +
	`"io.bigoci.file.size":"2500",` +
	`"io.bigoci.part.size":"1000",` +
	`"org.opencontainers.image.title":"golden.bin"}}`

func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		fileSize int64
		title    string
	}{
		{name: "file larger than the part size keeps every part", fileSize: multiPartFileSize, title: "model.bin"},
		{name: "file smaller than the part size becomes one part", fileSize: smallFileSize, title: "small.bin"},
		{name: "file that is an exact multiple has no short tail", fileSize: exactMultipleFileSize, title: "disk.img"},
		{name: "empty file becomes one empty part", fileSize: 0, title: "empty.bin"},
		{name: "artifact without a title round trips", fileSize: multiPartFileSize, title: ""},
		{name: "title with HTML-escapable characters round trips", fileSize: smallFileSize, title: escapedTitle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := fixtureArtifact(t, tt.fileSize, tt.title)

			encoded, err := manifest.Encode(want)
			require.NoError(t, err)

			decoded, err := manifest.Decode(encoded)
			require.NoError(t, err)
			assert.Equal(t, want, decoded)

			reencoded, err := manifest.Encode(decoded)
			require.NoError(t, err)
			assert.Equal(t, string(encoded), string(reencoded), "encoding must be deterministic")
		})
	}
}

func TestEncodeGolden(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{
			name:  "the canonical encoding is pinned byte for byte",
			title: "golden.bin",
			want:  goldenManifest,
		},
		{
			name:  "a title with HTML-escapable characters stays raw",
			title: escapedTitle,
			want:  strings.Replace(goldenManifest, "golden.bin", escapedTitle, 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifact := manifest.Artifact{
				FileDigest: digest.FromString("bigoci golden file"),
				FileSize:   multiPartFileSize,
				PartSize:   partSize,
				Title:      tt.title,
				Parts: []manifest.Part{
					{Digest: digest.FromString("part-0"), Size: partSize},
					{Digest: digest.FromString("part-1"), Size: partSize},
					{Digest: digest.FromString("part-2"), Size: shortPartSize},
				},
			}

			encoded, err := manifest.Encode(artifact)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(encoded))
		})
	}
}

func TestEncodeOmitsTheEmptyConfigData(t *testing.T) {
	encoded, err := manifest.Encode(fixtureArtifact(t, multiPartFileSize, "model.bin"))
	require.NoError(t, err)

	assert.NotContains(t, string(encoded), `"data"`, "the config descriptor must not inline its bytes")
}

func TestEncodeOmitsTheTitleWhenItIsEmpty(t *testing.T) {
	encoded, err := manifest.Encode(fixtureArtifact(t, multiPartFileSize, ""))
	require.NoError(t, err)

	assert.NotContains(t, string(encoded), ocispec.AnnotationTitle)
}

func TestEncodeRejectsArtifactsThatBreakTheFormat(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(a *manifest.Artifact)
		wantErr string
	}{
		{
			name:    "part size of zero",
			corrupt: func(a *manifest.Artifact) { a.PartSize = 0 },
			wantErr: "part size must be positive",
		},
		{
			name:    "negative file size",
			corrupt: func(a *manifest.Artifact) { a.FileSize = -1 },
			wantErr: "file size must not be negative",
		},
		{
			name:    "no parts",
			corrupt: func(a *manifest.Artifact) { a.Parts = nil },
			wantErr: "no parts",
		},
		{
			name:    "part count that disagrees with the file size",
			corrupt: func(a *manifest.Artifact) { a.Parts = a.Parts[:1] },
			wantErr: "artifact has 1 parts, but 2500 bytes at part size 1000 split into 3",
		},
		{
			name:    "part shorter than the split rule allows",
			corrupt: func(a *manifest.Artifact) { a.Parts[1].Size-- },
			wantErr: "part 1 is 999 bytes, split rule requires 1000",
		},
		{
			name:    "unparseable part digest",
			corrupt: func(a *manifest.Artifact) { a.Parts[0].Digest = "not-a-digest" },
			wantErr: `part 0 digest "not-a-digest"`,
		},
		{
			name:    "file digest that is not sha256",
			corrupt: func(a *manifest.Artifact) { a.FileDigest = otherAlgorithmDigest(t) },
			wantErr: `file digest algorithm is "sha512"`,
		},
		{
			name:    "unparseable file digest",
			corrupt: func(a *manifest.Artifact) { a.FileDigest = "sha256:short" },
			wantErr: `file digest "sha256:short"`,
		},
		{
			name:    "part digest that is not sha256",
			corrupt: func(a *manifest.Artifact) { a.Parts[0].Digest = otherAlgorithmDigest(t) },
			wantErr: `part 0 digest algorithm is "sha512"`,
		},
		{
			name:    "title that is not valid UTF-8",
			corrupt: func(a *manifest.Artifact) { a.Title = "\xff\xfe.bin" },
			wantErr: "not valid UTF-8",
		},
		{
			name: "split that needs more parts than the format allows",
			corrupt: func(a *manifest.Artifact) {
				a.FileSize = 10_000_000
				a.PartSize = 1
			},
			wantErr: "split of 10000000 bytes at part size 1: too many parts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifact := fixtureArtifact(t, multiPartFileSize, "model.bin")
			tt.corrupt(&artifact)

			encoded, err := manifest.Encode(artifact)
			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, encoded)
		})
	}
}

func TestEmptyConfig(t *testing.T) {
	t.Run("the descriptor addresses the content it comes with", func(t *testing.T) {
		descriptor, content := manifest.EmptyConfig()

		assert.Equal(t, digest.FromBytes(content), descriptor.Digest)
		assert.Equal(t, int64(len(content)), descriptor.Size)
	})

	t.Run("the descriptor is the one the format pins", func(t *testing.T) {
		descriptor, content := manifest.EmptyConfig()

		assert.Equal(t, ocispec.MediaTypeEmptyJSON, descriptor.MediaType)
		assert.Equal(t, emptyConfigDigest, descriptor.Digest.String())
		assert.Equal(t, "{}", string(content))
		assert.Nil(t, descriptor.Data, "inline data would marshal into a member the format does not have")
	})

	t.Run("the content is a fresh copy on every call", func(t *testing.T) {
		_, first := manifest.EmptyConfig()
		first[0] = 'X'

		_, second := manifest.EmptyConfig()
		assert.Equal(t, "{}", string(second))
	})
}

// fixtureArtifact builds a valid artifact for a file of fileSize bytes split at
// [partSize].
func fixtureArtifact(t *testing.T, fileSize int64, title string) manifest.Artifact {
	t.Helper()

	return manifest.Artifact{
		FileDigest: digest.FromString(fmt.Sprintf("file of %d bytes", fileSize)),
		FileSize:   fileSize,
		PartSize:   partSize,
		Title:      title,
		Parts:      fixtureParts(t, fileSize),
	}
}

// fixtureParts builds the parts of a file of fileSize bytes split at
// [partSize], giving each part a stable digest. It applies the split rule by
// hand so a test states its own expectation instead of borrowing the one the
// code under test uses.
func fixtureParts(t *testing.T, fileSize int64) []manifest.Part {
	t.Helper()

	if fileSize == 0 {
		return []manifest.Part{{Digest: digest.FromBytes(nil), Size: 0}}
	}

	numParts := (fileSize + partSize - 1) / partSize
	parts := make([]manifest.Part, 0, numParts)
	for i := range numParts {
		parts = append(parts, manifest.Part{
			Digest: digest.FromString(fmt.Sprintf("part-%d", i)),
			Size:   min(partSize, fileSize-i*partSize),
		})
	}

	return parts
}

// otherAlgorithmDigest returns a well-formed digest that does not use sha256,
// for the checks that insist on the algorithm the format names.
func otherAlgorithmDigest(t *testing.T) digest.Digest {
	t.Helper()
	require.True(t, digest.SHA512.Available(), "sha512 must be linked for this fixture")

	return digest.SHA512.FromString("some other algorithm")
}
