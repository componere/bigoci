package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"

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

// otherArtifactManifest is a well-formed OCI image manifest that is not a
// bigoci artifact: everything about it holds up except the artifact type.
const otherArtifactManifest = `{"schemaVersion":2,` +
	`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
	`"artifactType":"application/vnd.example.other.v1",` +
	`"config":{"mediaType":"application/vnd.oci.empty.v1+json",` +
	`"digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},` +
	`"layers":[]}`

func TestPullAssemblesTheFile(t *testing.T) {
	tests := []struct {
		name     string
		fileSize int64
	}{
		{name: "a file larger than the part size fetches every part", fileSize: multiPartSize},
		{name: "a file that is an exact multiple has no short tail", fileSize: exactMultipleSize},
		{name: "a file smaller than the part size fetches one part", fileSize: singlePartSize},
		{name: "an empty file fetches one empty part", fileSize: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := fileContent(tt.fileSize)
			artifact, body := artifactFor(t, content, "model.bin")
			descriptor := manifestDescriptor(body)

			manifests := ocimocks.NewMockManifests(t)
			manifests.EXPECT().Get(mock.Anything).Return(body, descriptor, nil).Once()

			store := newBlobStore(artifact.Parts, content)
			file := &memFile{}
			sink := mockSink(t, file)
			sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

			err := transfer.Pull(t.Context(), transfer.PullSpec{
				Sink:      sink,
				Blobs:     mockBlobsServing(t, store),
				Manifests: manifests,
				Workers:   2,
			})
			require.NoError(t, err)

			assert.Equal(t, content, file.bytes())
			assert.Equal(t, 1, file.commitCount())
			sink.AssertCalled(t, "Truncate", tt.fileSize)

			served, closed := store.counts()
			assert.Equal(t, len(artifact.Parts), served, "every part must be fetched")
			assert.Equal(t, served, closed, "every blob body must be closed")
		})
	}
}

func TestPullRefusesAPartThatDoesNotMatchItsDigest(t *testing.T) {
	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	// Same length, other bytes: the length checks pass and the digest check is
	// the one that has to catch it.
	corrupt := artifact.Parts[1]
	store := newBlobStore(artifact.Parts, content)
	store.bodies[corrupt.Digest] = bytes.Repeat([]byte{0xAA}, int(corrupt.Size))

	file := &memFile{}
	sink := mockSink(t, file)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     mockBlobsServing(t, store),
		Manifests: manifests,
		Workers:   2,
	})
	require.ErrorIs(t, err, transfer.ErrDigestMismatch)
	require.ErrorContains(t, err, "part 1")

	sink.AssertNotCalled(t, "Commit")
	assert.Zero(t, file.commitCount())

	served, closed := store.counts()
	assert.Equal(t, served, closed, "every blob body must be closed, including a rejected one")
}

func TestPullRefusesAPartOfTheWrongLength(t *testing.T) {
	tests := []struct {
		name    string
		body    func(part []byte) []byte
		wantErr string
	}{
		{
			name:    "a part that ends early is refused",
			body:    func(part []byte) []byte { return part[:len(part)-1] },
			wantErr: "ended before its declared size",
		},
		{
			name:    "a part that runs long is refused",
			body:    func(part []byte) []byte { return append(slices.Clone(part), 0x00) },
			wantErr: "is longer than its declared size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := fileContent(multiPartSize)
			artifact, body := artifactFor(t, content, "model.bin")

			manifests := ocimocks.NewMockManifests(t)
			manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

			wrong := artifact.Parts[1]
			store := newBlobStore(artifact.Parts, content)
			store.bodies[wrong.Digest] = tt.body(store.bodies[wrong.Digest])

			file := &memFile{}
			sink := mockSink(t, file)

			// One attempt, because this is about what a wrong length says and
			// not about how often it is asked for again. A part that ends
			// early is the one failure the orchestrator itself marks worth
			// repeating, and the budget it gets has its own rows in
			// retry_pull_test.go.
			err := transfer.Pull(t.Context(), transfer.PullSpec{
				Sink:      sink,
				Blobs:     mockBlobsServing(t, store),
				Manifests: manifests,
				Workers:   2,
				Retry:     noRetry(),
			})
			require.ErrorContains(t, err, tt.wantErr)
			require.ErrorContains(t, err, "part 1")
			require.NotErrorIs(t, err, transfer.ErrDigestMismatch, "a length failure is not a digest failure")

			sink.AssertNotCalled(t, "Commit")
			assert.Zero(t, file.commitCount())

			served, closed := store.counts()
			assert.Equal(t, served, closed, "every blob body must be closed")
		})
	}
}

// TestPullRefusesAStreamThatStartsSomewhereElse is what makes this phase's
// whole-part fetching provable rather than merely intended: every fetch asks
// to begin at byte zero, so a port reporting any other start has handed back a
// stream the attempt cannot place, and the pull says so instead of writing it
// into the wrong bytes of the file.
func TestPullRefusesAStreamThatStartsSomewhereElse(t *testing.T) {
	content := fileContent(singlePartSize)
	artifact, body := artifactFor(t, content, "model.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	store := newBlobStore(artifact.Parts, content)

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, offset int64) (io.ReadCloser, int64, error) {
			assert.Zero(t, offset, "a part nothing has fetched yet is asked for from its first byte")

			part, _, err := store.serve(dgst, 0)

			return part, 1, err
		},
	)

	policy, sleeps := testPolicy(t)

	file := &memFile{}
	sink := mockSink(t, file)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
		Retry:     policy,
	})
	require.ErrorContains(t, err, "blob port returned an unusable stream offset")
	require.ErrorContains(t, err, "fetch part 0")

	assert.Empty(t, sleeps.waits(), "a stream at a byte nobody asked for is not worth another attempt")

	served, closed := store.counts()
	assert.Equal(t, served, closed, "a refused stream's body must still be closed")

	sink.AssertNotCalled(t, "Commit")
	assert.Zero(t, file.commitCount())
	assert.Equal(t, make([]byte, singlePartSize), file.bytes(), "nothing is written from a stream that was refused")
}

func TestPullRefusesAManifestThatIsNotABigociArtifact(t *testing.T) {
	body := []byte(otherArtifactManifest)

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	// Neither mock carries an expectation: a pull that decides the reference
	// is not a bigoci artifact must not touch the file or fetch a blob.
	blobs := ocimocks.NewMockBlobs(t)
	sink := filemocks.NewMockSink(t)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   2,
	})
	require.ErrorIs(t, err, manifest.ErrNotBigociArtifact)

	blobs.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)
	sink.AssertNotCalled(t, "Size")
	sink.AssertNotCalled(t, "Truncate", mock.Anything)
	sink.AssertNotCalled(t, "Commit")
}

func TestPullReportsAFailureFromEachPort(t *testing.T) {
	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")

	unreachable := errors.New("the registry is unreachable")
	full := errors.New("the disk is full")

	tests := []struct {
		name    string
		wire    func(manifests *ocimocks.MockManifests, blobs *ocimocks.MockBlobs, sink *filemocks.MockSink)
		wantErr error
	}{
		{
			name: "a manifest that cannot be fetched surfaces",
			wire: func(manifests *ocimocks.MockManifests, _ *ocimocks.MockBlobs, _ *filemocks.MockSink) {
				manifests.EXPECT().Get(mock.Anything).Return(nil, ocispec.Descriptor{}, unreachable).Once()
			},
			wantErr: unreachable,
		},
		{
			name: "a destination that cannot be measured surfaces",
			wire: func(manifests *ocimocks.MockManifests, _ *ocimocks.MockBlobs, sink *filemocks.MockSink) {
				manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()
				sink.EXPECT().Size().Return(0, full).Once()
			},
			wantErr: full,
		},
		{
			name: "a destination that cannot be sized surfaces",
			wire: func(manifests *ocimocks.MockManifests, _ *ocimocks.MockBlobs, sink *filemocks.MockSink) {
				manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()
				sink.EXPECT().Size().Return(0, nil).Once()
				sink.EXPECT().Truncate(mock.Anything).Return(full).Once()
			},
			wantErr: full,
		},
		{
			name: "a part that cannot be fetched surfaces",
			wire: func(manifests *ocimocks.MockManifests, blobs *ocimocks.MockBlobs, sink *filemocks.MockSink) {
				manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()
				sink.EXPECT().Size().Return(0, nil).Once()
				sink.EXPECT().Truncate(mock.Anything).Return(nil).Once()
				blobs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).Return(nil, 0, unreachable)
			},
			wantErr: unreachable,
		},
		{
			name: "a part that cannot be written surfaces",
			wire: func(manifests *ocimocks.MockManifests, blobs *ocimocks.MockBlobs, sink *filemocks.MockSink) {
				manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()
				sink.EXPECT().Size().Return(0, nil).Once()
				sink.EXPECT().Truncate(mock.Anything).Return(nil).Once()
				sink.EXPECT().WriteAt(mock.Anything, mock.Anything).Return(0, full)

				store := newBlobStore(artifact.Parts, content)
				blobs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
					func(_ context.Context, dgst digest.Digest, offset int64) (io.ReadCloser, int64, error) {
						assert.Zero(t, offset, "a destination that took no bytes leaves nothing to continue from")

						return store.serve(dgst, offset)
					},
				)
			},
			wantErr: full,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifests := ocimocks.NewMockManifests(t)
			blobs := ocimocks.NewMockBlobs(t)
			sink := filemocks.NewMockSink(t)
			tt.wire(manifests, blobs, sink)

			err := transfer.Pull(t.Context(), transfer.PullSpec{
				Sink:      sink,
				Blobs:     blobs,
				Manifests: manifests,
				Workers:   1,
			})
			require.ErrorIs(t, err, tt.wantErr)

			sink.AssertNotCalled(t, "Commit")
		})
	}
}

func TestPullStopsWhenTheContextIsCancelled(t *testing.T) {
	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	store := newBlobStore(artifact.Parts, content)

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, offset int64) (io.ReadCloser, int64, error) {
			assert.Zero(t, offset, "the first fetch of a part starts at its first byte")
			cancel()

			return store.serve(dgst, offset)
		},
	).Maybe()

	file := &memFile{}
	sink := mockSink(t, file)

	// Pull returns only after every worker it started has returned, so
	// reaching the assertions at all is the proof that none was left running.
	err := transfer.Pull(ctx, transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
	})
	require.ErrorIs(t, err, context.Canceled)

	served, closed := store.counts()
	assert.Equal(t, 1, served, "the worker must abandon the parts still queued")
	assert.Equal(t, served, closed, "every blob body must be closed")

	sink.AssertNotCalled(t, "Commit")
	assert.Zero(t, file.commitCount())
}

func TestPullRefusesAnIncompleteSpec(t *testing.T) {
	tests := []struct {
		name    string
		spoil   func(spec *transfer.PullSpec)
		wantErr string
	}{
		{
			name:    "a spec without a sink is refused",
			spoil:   func(spec *transfer.PullSpec) { spec.Sink = nil },
			wantErr: "no sink",
		},
		{
			name:    "a spec without a blobs port is refused",
			spoil:   func(spec *transfer.PullSpec) { spec.Blobs = nil },
			wantErr: "no blobs port",
		},
		{
			name:    "a spec without a manifests port is refused",
			spoil:   func(spec *transfer.PullSpec) { spec.Manifests = nil },
			wantErr: "no manifests port",
		},
		{
			name:    "a spec without workers is refused",
			spoil:   func(spec *transfer.PullSpec) { spec.Workers = 0 },
			wantErr: "worker count must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The mocks carry no expectations, so any call at all fails the
			// test. A bad spec is refused before the pull touches anything.
			spec := transfer.PullSpec{
				Sink:      filemocks.NewMockSink(t),
				Blobs:     ocimocks.NewMockBlobs(t),
				Manifests: ocimocks.NewMockManifests(t),
				Workers:   2,
			}
			tt.spoil(&spec)

			err := transfer.Pull(t.Context(), spec)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// manifestDescriptor returns the descriptor a manifests port hands back with
// body: the digest is computed over the bytes themselves, never taken from a
// header.
func manifestDescriptor(body []byte) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: manifest.ArtifactType,
		Digest:       digest.FromBytes(body),
		Size:         int64(len(body)),
	}
}
