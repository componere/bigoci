package transfer_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	filemocks "github.com/imgoci/bigoci/internal/file/mocks"
	"github.com/imgoci/bigoci/internal/manifest"
	ocimocks "github.com/imgoci/bigoci/internal/oci/mocks"
	"github.com/imgoci/bigoci/internal/transfer"
)

// gateTimeout bounds the wait in the ordering test. Nothing sleeps for it on
// the passing path: it only turns a bug that would hang the suite into a
// failure with a message.
const gateTimeout = 30 * time.Second

func TestPushWritesTheArtifact(t *testing.T) {
	tests := []struct {
		name     string
		fileSize int64
		title    string
	}{
		{name: "a file larger than the part size uploads every part", fileSize: multiPartSize, title: "model.bin"},
		{name: "a file that is an exact multiple has no short tail", fileSize: exactMultipleSize, title: "disk.img"},
		{name: "a file smaller than the part size uploads one part", fileSize: singlePartSize, title: "small.bin"},
		{name: "an empty file uploads one empty part", fileSize: 0, title: "empty.bin"},
		{name: "a file without a title is pushed untitled", fileSize: multiPartSize, title: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := fileContent(tt.fileSize)
			want, wantBody := artifactFor(t, content, tt.title)
			config, configContent := manifest.EmptyConfig()

			blobs, recorded := mockBlobs(t, nil)

			var body []byte
			var uploadedFirst []digest.Digest

			manifests := ocimocks.NewMockManifests(t)
			manifests.EXPECT().Put(mock.Anything, ocispec.MediaTypeImageManifest, mock.Anything).RunAndReturn(
				func(_ context.Context, _ string, manifestBody []byte) (digest.Digest, error) {
					body = manifestBody
					uploadedFirst = recorded.digests()

					return digest.FromBytes(manifestBody), nil
				},
			).Once()

			descriptor, err := transfer.Push(t.Context(), transfer.PushSpec{
				Source:    mockSource(t, content),
				Blobs:     blobs,
				Manifests: manifests,
				PartSize:  fixturePartSize,
				Workers:   2,
				Title:     tt.title,
			})
			require.NoError(t, err)

			assert.Equal(t, ocispec.Descriptor{
				MediaType:    ocispec.MediaTypeImageManifest,
				ArtifactType: manifest.ArtifactType,
				Digest:       digest.FromBytes(wantBody),
				Size:         int64(len(wantBody)),
			}, descriptor)
			assert.Equal(t, string(wantBody), string(body), "the manifest must be the canonical encoding")

			decoded, err := manifest.Decode(body)
			require.NoError(t, err)
			assert.Equal(t, want, decoded)

			offset := int64(0)
			for i, part := range want.Parts {
				got := recorded.blob(part.Digest)
				assert.Equal(t, part.Size, got.size, "part %d was declared with the wrong length", i)
				assert.Equal(t, content[offset:offset+part.Size], got.content, "part %d carried other bytes", i)
				offset += part.Size
			}

			assert.Equal(t, configContent, recorded.blob(config.Digest).content, "the empty config must be uploaded")

			// Every blob the manifest references was already in the registry
			// when the manifest was written, which is what makes an
			// interrupted push leave no artifact behind.
			wantUploaded := make([]digest.Digest, 0, len(want.Parts)+1)
			for _, part := range want.Parts {
				wantUploaded = append(wantUploaded, part.Digest)
			}
			assert.ElementsMatch(t, append(wantUploaded, config.Digest), uploadedFirst)
		})
	}
}

func TestPushSkipsPartsTheRegistryHolds(t *testing.T) {
	content := fileContent(multiPartSize)
	want, _ := artifactFor(t, content, "model.bin")
	config, _ := manifest.EmptyConfig()

	held := map[digest.Digest]bool{want.Parts[0].Digest: true, want.Parts[2].Digest: true}
	blobs, recorded := mockBlobs(t, held)

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().
		Put(mock.Anything, mock.Anything, mock.Anything).
		Return(digest.FromString("manifest"), nil).
		Once()

	// One worker, so the order the remaining uploads arrive in is the file's.
	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    mockSource(t, content),
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   1,
		Title:     "model.bin",
	})
	require.NoError(t, err)

	assert.Equal(t, []digest.Digest{want.Parts[1].Digest, config.Digest}, recorded.digests())
	blobs.AssertNotCalled(t, "Put", mock.Anything, want.Parts[0].Digest, mock.Anything, mock.Anything)
	blobs.AssertNotCalled(t, "Put", mock.Anything, want.Parts[2].Digest, mock.Anything, mock.Anything)
}

func TestPushRecordsPartsInFileOrder(t *testing.T) {
	content := fileContent(multiPartSize)
	want, _ := artifactFor(t, content, "model.bin")

	// The last part finishes uploading before the first one does, so a
	// manifest built from completion order would come out with its layers
	// scrambled.
	lastUploaded := make(chan struct{})
	recorded := newUploads()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).Return(false, nil)
	blobs.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, size int64, r io.Reader) error {
			if dgst == want.Parts[len(want.Parts)-1].Digest {
				defer close(lastUploaded)
			}

			if dgst == want.Parts[0].Digest {
				select {
				case <-lastUploaded:
				case <-time.After(gateTimeout):
					return errors.New("the last part never finished uploading")
				}
			}

			return recorded.record(dgst, size, r)
		},
	)

	var body []byte

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ string, manifestBody []byte) (digest.Digest, error) {
			body = manifestBody

			return digest.FromBytes(manifestBody), nil
		},
	).Once()

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    mockSource(t, content),
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   len(want.Parts),
		Title:     "model.bin",
	})
	require.NoError(t, err)

	decoded, err := manifest.Decode(body)
	require.NoError(t, err)
	assert.Equal(t, want.Parts, decoded.Parts)

	uploaded := recorded.digests()
	assert.Greater(t,
		slices.Index(uploaded, want.Parts[0].Digest),
		slices.Index(uploaded, want.Parts[len(want.Parts)-1].Digest),
		"the gate must have made the last part finish before the first",
	)
}

func TestPushStopsAtTheFirstFailure(t *testing.T) {
	content := fileContent(multiPartSize)
	want, _ := artifactFor(t, content, "model.bin")

	rejected := errors.New("the registry rejected the upload")
	recorded := newUploads()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	blobs.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, size int64, r io.Reader) error {
			if dgst == want.Parts[1].Digest {
				return rejected
			}

			return recorded.record(dgst, size, r)
		},
	).Maybe()

	manifests := ocimocks.NewMockManifests(t)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    mockSource(t, content),
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   1,
		Title:     "model.bin",
	})
	require.ErrorIs(t, err, rejected)
	require.ErrorContains(t, err, "part 1")

	assert.Equal(t,
		[]digest.Digest{want.Parts[0].Digest},
		recorded.digests(),
		"the parts queued after the failure are dropped",
	)
	manifests.AssertNotCalled(t, "Put", mock.Anything, mock.Anything, mock.Anything)
}

func TestPushReportsASourceThatCannotBeRead(t *testing.T) {
	unreadable := errors.New("the disk went away")

	source := filemocks.NewMockSource(t)
	source.EXPECT().Size().Return(multiPartSize)
	source.EXPECT().ReadAt(mock.Anything, mock.Anything).Return(0, unreadable)

	blobs := ocimocks.NewMockBlobs(t)
	manifests := ocimocks.NewMockManifests(t)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   2,
		Title:     "model.bin",
	})
	require.ErrorIs(t, err, unreadable)
	require.ErrorContains(t, err, "part 0")

	blobs.AssertNotCalled(t, "Put", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	manifests.AssertNotCalled(t, "Put", mock.Anything, mock.Anything, mock.Anything)
}

func TestPushStopsWhenTheContextIsCancelled(t *testing.T) {
	content := fileContent(multiPartSize)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	recorded := newUploads()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	blobs.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, size int64, r io.Reader) error {
			cancel()

			return recorded.record(dgst, size, r)
		},
	).Maybe()

	manifests := ocimocks.NewMockManifests(t)

	// Push returns only after every goroutine it started has returned, so
	// reaching the assertions at all is the proof that none was left running.
	_, err := transfer.Push(ctx, transfer.PushSpec{
		Source:    mockSource(t, content),
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   1,
		Title:     "model.bin",
	})
	require.ErrorIs(t, err, context.Canceled)

	assert.Len(t, recorded.digests(), 1, "the worker must abandon the parts still queued")
	manifests.AssertNotCalled(t, "Put", mock.Anything, mock.Anything, mock.Anything)
}

func TestPushRefusesAnIncompleteSpec(t *testing.T) {
	tests := []struct {
		name    string
		spoil   func(t *testing.T, spec *transfer.PushSpec)
		wantErr string
	}{
		{
			name:    "a spec without a source is refused",
			spoil:   func(_ *testing.T, spec *transfer.PushSpec) { spec.Source = nil },
			wantErr: "no source",
		},
		{
			name:    "a spec without a blobs port is refused",
			spoil:   func(_ *testing.T, spec *transfer.PushSpec) { spec.Blobs = nil },
			wantErr: "no blobs port",
		},
		{
			name:    "a spec without a manifests port is refused",
			spoil:   func(_ *testing.T, spec *transfer.PushSpec) { spec.Manifests = nil },
			wantErr: "no manifests port",
		},
		{
			// The plan is the split rule's single gate, so the source's
			// recorded size is read on the way to the rejection — but no
			// port is ever touched.
			name: "a spec without a part size is refused by the plan",
			spoil: func(t *testing.T, spec *transfer.PushSpec) {
				t.Helper()
				spec.PartSize = 0
				spec.Source = mockSource(t, nil)
			},
			wantErr: "part size must be positive",
		},
		{
			name:    "a spec without workers is refused",
			spoil:   func(_ *testing.T, spec *transfer.PushSpec) { spec.Workers = 0 },
			wantErr: "worker count must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The mocks carry no expectations, so any call at all — even the
			// source's size — fails the test. A bad spec is refused before the
			// push touches anything.
			spec := transfer.PushSpec{
				Source:    filemocks.NewMockSource(t),
				Blobs:     ocimocks.NewMockBlobs(t),
				Manifests: ocimocks.NewMockManifests(t),
				PartSize:  fixturePartSize,
				Workers:   2,
				Title:     "model.bin",
			}
			tt.spoil(t, &spec)

			descriptor, err := transfer.Push(t.Context(), spec)
			require.ErrorContains(t, err, tt.wantErr)
			assert.Equal(t, ocispec.Descriptor{}, descriptor)
		})
	}
}
