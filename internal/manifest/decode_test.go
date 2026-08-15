package manifest_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/manifest"
	"github.com/imgoci/bigoci/internal/plan"
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

const (
	// overLimitPartCount is one part past the format's part cap.
	overLimitPartCount = plan.MaxParts + 1
	// layerBombPartCount fills almost the complete four-MiB manifest-body limit
	// with empty descriptors, matching the compact audit payload.
	layerBombPartCount = 1_397_950
	// maxLayerBombAlloc is the allocation ceiling for rejecting a layer bomb.
	// It leaves headroom over the bounded decoder's measured cost while staying
	// far below the descriptor storage an unbounded decoder requires.
	maxLayerBombAlloc = 8 << 20
	// duplicateLayerArrayCount is the number of maximum-size arrays that fit in
	// the audit payload while keeping its JSON body below four MiB.
	duplicateLayerArrayCount = 341
	// duplicateLayerBombSize is the exact size of the compact audit payload.
	duplicateLayerBombSize = 4_193_960
	// maxDuplicateLayerBombAlloc allows parser overhead while ensuring repeated
	// layers members cannot allocate a fresh bounded slice apiece.
	maxDuplicateLayerBombAlloc = 16 << 20
	// descriptorURLBombSize is the exact size of one allowed descriptor carrying
	// almost four MiB of ignored URL entries.
	descriptorURLBombSize = 4_193_873
	// maxIgnoredFieldBombAlloc leaves room for syntax parsing while ensuring an
	// ignored descriptor field cannot allocate storage per nested entry.
	maxIgnoredFieldBombAlloc = 8 << 20
	// unknownAnnotationBombCount is how many unique, unused annotation keys fit
	// comfortably under the manifest body limit.
	unknownAnnotationBombCount = 300_000
	// unknownAnnotationBombSize is the exact compact payload size at that count.
	unknownAnnotationBombSize = 3_488_907
)

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
			name: "manifest without the optional body media type",
			add:  func(m map[string]any) { delete(m, "mediaType") },
		},
		{
			name: "mixed-case manifest media type",
			add:  func(m map[string]any) { m["mediaType"] = strings.ToUpper(ocispec.MediaTypeImageManifest) },
		},
		{
			name: "mixed-case artifact type",
			add:  func(m map[string]any) { m["artifactType"] = strings.ToUpper(manifest.ArtifactType) },
		},
		{
			name: "mixed-case config media type",
			add: func(m map[string]any) {
				configOf(m)["mediaType"] = strings.ToUpper(ocispec.DescriptorEmptyJSON.MediaType)
			},
		},
		{
			name: "mixed-case layer media type",
			add: func(m map[string]any) {
				layersOf(m)[0]["mediaType"] = strings.ToUpper(manifest.MediaTypePart)
			},
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
		{
			name: "top-level member from outside the OCI schema",
			add: func(m map[string]any) {
				m["com.example.metadata"] = map[string]any{
					"nested": []any{map[string]any{"build": "42"}},
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
			name: "mixed-case artifact type of another artifact",
			corrupt: func(m map[string]any) {
				m["artifactType"] = strings.ToUpper("application/vnd.example.thing.v1")
			},
			wantErr: "artifact type is",
		},
		{
			name:    "media type of an image index",
			corrupt: func(m map[string]any) { m["mediaType"] = ocispec.MediaTypeImageIndex },
			wantErr: "media type is",
		},
		{
			name: "mixed-case media type of an image index",
			corrupt: func(m map[string]any) {
				m["mediaType"] = strings.ToUpper(ocispec.MediaTypeImageIndex)
			},
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

// TestDecodeErrorsDoNotReflectRegistrySelectedManifestValues pins the public
// logging boundary around manifests. A registry can place a reusable bearer
// in any field it returns, so Decode reports the field and rule without
// repeating the value.
func TestDecodeErrorsDoNotReflectRegistrySelectedManifestValues(t *testing.T) {
	const reusableBearer = "application/vnd.registry.reusable-bearer-a8f4c2+json"

	tests := []struct {
		name    string
		data    func() []byte
		wantIs  error
		wantErr string
	}{
		{
			name: "artifact type",
			data: func() []byte {
				return manifestJSON(t, func(m map[string]any) { m["artifactType"] = reusableBearer })
			},
			wantIs:  manifest.ErrNotBigociArtifact,
			wantErr: "manifest artifact type is not bigoci",
		},
		{
			name: "config media type",
			data: func() []byte {
				return manifestJSON(t, func(m map[string]any) { configOf(m)["mediaType"] = reusableBearer })
			},
			wantErr: "config media type does not match",
		},
		{
			name: "size annotation",
			data: func() []byte {
				return manifestJSON(t, func(m map[string]any) {
					annotationsOf(m)[manifest.AnnotationFileSize] = reusableBearer
				})
			},
			wantErr: "annotation io.bigoci.file.size is not a base-10 byte count",
		},
		{
			name:    "malformed JSON",
			data:    func() []byte { return []byte(`{"artifactType":"` + reusableBearer) },
			wantErr: "parse manifest JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := manifest.Decode(tt.data())
			require.Error(t, err)
			if tt.wantIs != nil {
				require.ErrorIs(t, err, tt.wantIs)
			}
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), reusableBearer)
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
			wantErr: "schema version must be 2",
		},
		{
			name:    "config media type of a real image config",
			corrupt: func(m map[string]any) { configOf(m)["mediaType"] = ocispec.MediaTypeImageConfig },
			wantErr: "config media type does not match",
		},
		{
			name: "mixed-case config media type of a real image config",
			corrupt: func(m map[string]any) {
				configOf(m)["mediaType"] = strings.ToUpper(ocispec.MediaTypeImageConfig)
			},
			wantErr: "config media type does not match",
		},
		{
			name:    "config digest of some other blob",
			corrupt: func(m map[string]any) { configOf(m)["digest"] = digest.FromString("{}\n").String() },
			wantErr: "config digest does not match",
		},
		{
			name:    "config size that is not two bytes",
			corrupt: func(m map[string]any) { configOf(m)["size"] = 3 },
			wantErr: "config size does not match",
		},
		{
			name:    "config data that contradicts the config digest",
			corrupt: func(m map[string]any) { configOf(m)["data"] = "Zm9vYmFy" },
			wantErr: "config data does not match",
		},
		{
			name:    "no layers",
			corrupt: func(m map[string]any) { m["layers"] = []any{} },
			wantErr: "no parts",
		},
		{
			name: "file size that needs more parts than the format allows",
			corrupt: func(m map[string]any) {
				annotationsOf(m)[manifest.AnnotationFileSize] = "10000000"
				annotationsOf(m)[manifest.AnnotationPartSize] = "1"
			},
			wantErr: "manifest split is invalid",
		},
		{
			name:    "layer that is a tar layer",
			corrupt: func(m map[string]any) { layersOf(m)[1]["mediaType"] = ocispec.MediaTypeImageLayerGzip },
			wantErr: "layer 1 media type is",
		},
		{
			name: "mixed-case layer that is a tar layer",
			corrupt: func(m map[string]any) {
				layersOf(m)[1]["mediaType"] = strings.ToUpper(ocispec.MediaTypeImageLayerGzip)
			},
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
			wantErr: "annotation io.bigoci.file.size is not a base-10 byte count",
		},
		{
			name:    "part size that is not a number",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationPartSize] = "0x3e8" },
			wantErr: "annotation io.bigoci.part.size is not a base-10 byte count",
		},
		{
			name:    "negative file size",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationFileSize] = "-1" },
			wantErr: "manifest split is invalid",
		},
		{
			name:    "part size of zero",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationPartSize] = "0" },
			wantErr: "manifest split is invalid",
		},
		{
			name:    "file size that needs more parts than the manifest has",
			corrupt: func(m map[string]any) { annotationsOf(m)[manifest.AnnotationFileSize] = "3500" },
			wantErr: "artifact part count does not match its split",
		},
		{
			name:    "middle part shorter than the split rule allows",
			corrupt: func(m map[string]any) { layersOf(m)[1]["size"] = partSize - 1 },
			wantErr: "part 1 size does not match the split rule",
		},
		{
			name:    "part digest that does not parse",
			corrupt: func(m map[string]any) { layersOf(m)[0]["digest"] = "sha256:nothex" },
			wantErr: "part 0 digest is invalid",
		},
		{
			name: "file digest from another algorithm",
			corrupt: func(m map[string]any) {
				annotationsOf(m)[manifest.AnnotationFileDigest] = otherAlgorithmDigest(t).String()
			},
			wantErr: "file digest algorithm must be sha256",
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

func TestDecodeEnforcesTheLayerLimit(t *testing.T) {
	tests := []struct {
		name    string
		count   int
		wantErr bool
	}{
		{name: "the maximum layer count is accepted", count: plan.MaxParts},
		{name: "the first layer over the maximum is refused", count: overLimitPartCount, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := manifestJSON(t, func(m map[string]any) {
				setOneByteLayers(m, tt.count)
			})

			decoded, err := manifest.Decode(data)
			if tt.wantErr {
				require.ErrorIs(t, err, plan.ErrTooManyParts)
				assert.Equal(t, manifest.Artifact{}, decoded)

				return
			}

			require.NoError(t, err)
			assert.Len(t, decoded.Parts, plan.MaxParts)
			assert.Equal(t, digest.FromString("byte-4095"), decoded.Parts[plan.MaxParts-1].Digest)
		})
	}
}

func TestDecodeEnforcesTheLayerLimitAcrossJSONFieldSpellings(t *testing.T) {
	tests := []string{
		"layers",
		"Layers",
		"LAYERS",
		`lay\u0065rs`,
		`layer\u017f`,
	}

	for _, field := range tests {
		t.Run(field, func(t *testing.T) {
			decoded, err := manifest.Decode(compactLayerBombField(field, overLimitPartCount))
			require.ErrorIs(t, err, plan.ErrTooManyParts)
			assert.Equal(t, manifest.Artifact{}, decoded)
		})
	}
}

func TestDecodeUsesTheLastDuplicateLayersMember(t *testing.T) {
	valid := manifestJSON(t, func(map[string]any) {})
	maximumArray := compactLayerBomb(plan.MaxParts)
	withValidLast := make([]byte, 0, len(maximumArray)+len(valid))
	withValidLast = append(withValidLast, maximumArray[:len(maximumArray)-1]...)
	withValidLast = append(withValidLast, ',')
	withValidLast = append(withValidLast, valid[1:]...)

	decoded, err := manifest.Decode(withValidLast)
	require.NoError(t, err)
	assert.Equal(t, fixtureArtifact(t, multiPartFileSize, "model.bin"), decoded)

	withNullLast := make([]byte, 0, len(valid)+len(`,"layers":null`))
	withNullLast = append(withNullLast, valid[:len(valid)-1]...)
	withNullLast = append(withNullLast, `,"layers":null}`...)

	decoded, err = manifest.Decode(withNullLast)
	require.ErrorContains(t, err, "no parts")
	assert.Equal(t, manifest.Artifact{}, decoded)
}

func TestDecodeRejectsAnOversizedEarlierDuplicateLayersMember(t *testing.T) {
	oversizedArray := compactLayerBomb(overLimitPartCount)
	valid := manifestJSON(t, func(map[string]any) {})
	withValidLast := make([]byte, 0, len(oversizedArray)+len(valid))
	withValidLast = append(withValidLast, oversizedArray[:len(oversizedArray)-1]...)
	withValidLast = append(withValidLast, ',')
	withValidLast = append(withValidLast, valid[1:]...)

	decoded, err := manifest.Decode(withValidLast)
	require.ErrorIs(t, err, plan.ErrTooManyParts)
	assert.Equal(t, manifest.Artifact{}, decoded)
}

func TestDecodePreservesDuplicateAnnotationObjectBehavior(t *testing.T) {
	valid := manifestJSON(t, func(map[string]any) {})
	withEmptyLast := make([]byte, 0, len(valid)+len(`,"annotations":{}`))
	withEmptyLast = append(withEmptyLast, valid[:len(valid)-1]...)
	withEmptyLast = append(withEmptyLast, `,"annotations":{}}`...)

	decoded, err := manifest.Decode(withEmptyLast)
	require.NoError(t, err)
	assert.Equal(t, fixtureArtifact(t, multiPartFileSize, "model.bin"), decoded)

	withNullLast := make([]byte, 0, len(valid)+len(`,"annotations":null`))
	withNullLast = append(withNullLast, valid[:len(valid)-1]...)
	withNullLast = append(withNullLast, `,"annotations":null}`...)

	decoded, err = manifest.Decode(withNullLast)
	require.ErrorContains(t, err, manifest.AnnotationFileDigest)
	assert.Equal(t, manifest.Artifact{}, decoded)
}

func TestDecodeBoundsAllocationForExcessLayers(t *testing.T) {
	data := compactLayerBomb(layerBombPartCount)

	decoded, err := manifest.Decode(data)
	require.ErrorIs(t, err, plan.ErrTooManyParts)
	assert.Equal(t, manifest.Artifact{}, decoded)

	var benchmarkErr error
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, benchmarkErr = manifest.Decode(data)
		}
	})

	require.ErrorIs(t, benchmarkErr, plan.ErrTooManyParts)
	t.Logf(
		"bounded rejection of a %d-byte manifest allocated %d bytes/op",
		len(data),
		result.AllocedBytesPerOp(),
	)
	assert.Less(
		t,
		result.AllocedBytesPerOp(),
		int64(maxLayerBombAlloc),
		"rejecting %d layers must not allocate storage proportional to the complete array",
		layerBombPartCount,
	)
}

func TestDecodeBoundsAllocationAcrossDuplicateLayerMembers(t *testing.T) {
	data := compactDuplicateLayerBomb(duplicateLayerArrayCount)
	require.Len(t, data, duplicateLayerBombSize)
	require.Less(t, len(data), 4<<20)

	decoded, err := manifest.Decode(data)
	require.Error(t, err)
	require.NotErrorIs(t, err, plan.ErrTooManyParts)
	assert.Equal(t, manifest.Artifact{}, decoded)

	var benchmarkErr error
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, benchmarkErr = manifest.Decode(data)
		}
	})

	require.Error(t, benchmarkErr)
	require.NotErrorIs(t, benchmarkErr, plan.ErrTooManyParts)
	t.Logf(
		"bounded decoding of a %d-byte manifest with %d duplicate layers arrays allocated %d bytes/op",
		len(data),
		duplicateLayerArrayCount,
		result.AllocedBytesPerOp(),
	)
	assert.Less(
		t,
		result.AllocedBytesPerOp(),
		int64(maxDuplicateLayerBombAlloc),
		"duplicate layers members must reuse bounded descriptor storage",
	)
}

func TestDecodeBoundsAllocationForIgnoredDescriptorFields(t *testing.T) {
	data := compactDescriptorURLBomb(layerBombPartCount)
	require.Len(t, data, descriptorURLBombSize)
	require.Less(t, len(data), 4<<20)

	decoded, err := manifest.Decode(data)
	require.ErrorIs(t, err, manifest.ErrNotBigociArtifact)
	assert.Equal(t, manifest.Artifact{}, decoded)

	var benchmarkErr error
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, benchmarkErr = manifest.Decode(data)
		}
	})

	require.ErrorIs(t, benchmarkErr, manifest.ErrNotBigociArtifact)
	t.Logf(
		"bounded decoding of a %d-byte ignored descriptor field allocated %d bytes/op",
		len(data),
		result.AllocedBytesPerOp(),
	)
	assert.Less(
		t,
		result.AllocedBytesPerOp(),
		int64(maxIgnoredFieldBombAlloc),
		"ignored OCI descriptor fields must not allocate storage proportional to nested entries",
	)
}

func TestDecodeBoundsAllocationForUnknownAnnotations(t *testing.T) {
	data := compactUnknownAnnotationsBomb(unknownAnnotationBombCount)
	require.Len(t, data, unknownAnnotationBombSize)
	require.Less(t, len(data), 4<<20)

	decoded, err := manifest.Decode(data)
	require.ErrorIs(t, err, manifest.ErrNotBigociArtifact)
	assert.Equal(t, manifest.Artifact{}, decoded)

	var benchmarkErr error
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, benchmarkErr = manifest.Decode(data)
		}
	})

	require.ErrorIs(t, benchmarkErr, manifest.ErrNotBigociArtifact)
	t.Logf(
		"bounded decoding of %d unknown annotation keys allocated %d bytes/op",
		unknownAnnotationBombCount,
		result.AllocedBytesPerOp(),
	)
	assert.Less(t, result.AllocedBytesPerOp(), int64(maxIgnoredFieldBombAlloc))
}

func TestDecodeBoundsAllocationBeforeReportingMalformedJSON(t *testing.T) {
	data := append(compactLayerBomb(layerBombPartCount), '!')

	decoded, err := manifest.Decode(data)
	require.ErrorContains(t, err, "parse manifest JSON")
	assert.Equal(t, manifest.Artifact{}, decoded)

	var benchmarkErr error
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, benchmarkErr = manifest.Decode(data)
		}
	})

	require.ErrorContains(t, benchmarkErr, "parse manifest JSON")
	t.Logf(
		"rejecting a malformed %d-byte layer payload allocated %d bytes/op",
		len(data),
		result.AllocedBytesPerOp(),
	)
	assert.Less(t, result.AllocedBytesPerOp(), int64(maxLayerBombAlloc))
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
		{name: "second document after a manifest", data: `{} {}`},
		{name: "second document after null", data: `null {}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, err := manifest.Decode([]byte(tt.data))
			require.ErrorContains(t, err, "parse manifest JSON")
			assert.Equal(t, manifest.Artifact{}, decoded)
		})
	}
}

func TestTooManyPartsWrapsThePlanSentinel(t *testing.T) {
	artifact := fixtureArtifact(t, multiPartFileSize, "model.bin")
	artifact.FileSize = 10_000_000
	artifact.PartSize = 1

	_, err := manifest.Encode(artifact)
	require.ErrorIs(t, err, plan.ErrTooManyParts, "encode must surface the plan sentinel")

	data := manifestJSON(t, func(m map[string]any) {
		annotationsOf(m)[manifest.AnnotationFileSize] = "10000000"
		annotationsOf(m)[manifest.AnnotationPartSize] = "1"
	})
	_, err = manifest.Decode(data)
	require.ErrorIs(t, err, plan.ErrTooManyParts, "decode must surface the plan sentinel")
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
			manifest.AnnotationPartSize:   strconv.FormatInt(int64(artifact.PartSize), 10),
			ocispec.AnnotationTitle:       artifact.Title,
		},
	}
}

// setOneByteLayers replaces the layers and sizes with count one-byte parts.
func setOneByteLayers(m map[string]any, count int) {
	layers := make([]any, 0, count)
	for i := range count {
		layers = append(layers, map[string]any{
			"mediaType": manifest.MediaTypePart,
			"digest":    digest.FromString(fmt.Sprintf("byte-%d", i)).String(),
			"size":      1,
		})
	}

	m["layers"] = layers
	annotationsOf(m)[manifest.AnnotationFileSize] = strconv.Itoa(count)
	annotationsOf(m)[manifest.AnnotationPartSize] = "1"
}

// compactLayerBomb builds a small-on-the-wire array whose empty descriptors
// used to expand into allocation proportional to count before validation.
func compactLayerBomb(count int) []byte {
	return compactLayerBombField("layers", count)
}

// compactLayerBombField builds a compact layer array under field, which is
// already JSON-escaped but does not include its surrounding quotes.
func compactLayerBombField(field string, count int) []byte {
	var document strings.Builder
	document.Grow(len(field) + len(`{"":[]}`) + count*3)
	document.WriteString(`{"`)
	document.WriteString(field)
	document.WriteString(`":[`)
	for i := range count {
		if i > 0 {
			document.WriteByte(',')
		}
		document.WriteString(`{}`)
	}
	document.WriteString(`]}`)

	return []byte(document.String())
}

// compactDescriptorURLBomb builds one allowed layer descriptor whose ignored
// URLs field used to allocate a string slice proportional to count.
func compactDescriptorURLBomb(count int) []byte {
	var document strings.Builder
	document.Grow(len(`{"layers":[{"urls":[]}]}`) + count*3)
	document.WriteString(`{"layers":[{"urls":[`)
	for i := range count {
		if i > 0 {
			document.WriteByte(',')
		}
		document.WriteString(`""`)
	}
	document.WriteString(`]}]}`)

	return []byte(document.String())
}

// compactUnknownAnnotationsBomb builds distinct annotations that bigoci does
// not consume and therefore must not retain in a map.
func compactUnknownAnnotationsBomb(count int) []byte {
	var document strings.Builder
	document.Grow(12 * count)
	document.WriteString(`{"annotations":{`)
	for i := range count {
		if i > 0 {
			document.WriteByte(',')
		}
		document.WriteByte('"')
		document.WriteString(strconv.Itoa(i))
		document.WriteString(`":""`)
	}
	document.WriteString(`}}`)

	return []byte(document.String())
}

// compactDuplicateLayerBomb builds arrayCount duplicate layers members, each
// containing the maximum individually valid number of descriptors.
func compactDuplicateLayerBomb(arrayCount int) []byte {
	var document strings.Builder
	document.Grow(arrayCount * (len(`"layers":[]`) + plan.MaxParts*3))
	document.WriteByte('{')
	for arrayIndex := range arrayCount {
		if arrayIndex > 0 {
			document.WriteByte(',')
		}
		document.WriteString(`"layers":[`)
		for layerIndex := range plan.MaxParts {
			if layerIndex > 0 {
				document.WriteByte(',')
			}
			document.WriteString(`{}`)
		}
		document.WriteByte(']')
	}
	document.WriteByte('}')

	return []byte(document.String())
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
