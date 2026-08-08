package transfer_test

import (
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

	filemocks "github.com/componere/bigoci/internal/file/mocks"
	ocimocks "github.com/componere/bigoci/internal/oci/mocks"
	"github.com/componere/bigoci/internal/transfer"
)

func TestPushUploadsIdenticalPartsOnce(t *testing.T) {
	t.Parallel()

	// Two byte-identical full parts plus a distinct tail: the blob behind the
	// twin parts must move exactly once however many workers race for it.
	block := fileContent(int64(fixturePartSize))
	content := slices.Concat(block, block, fileContent(singlePartSize))
	parts := splitParts(t, content)
	require.Equal(t, parts[0].Digest, parts[1].Digest, "the fixture needs twin parts to mean anything")

	blobs, recorded := mockBlobs(t, nil)
	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ string, body []byte) (digest.Digest, error) {
			return digest.FromBytes(body), nil
		},
	)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    mockSource(t, content),
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   4,
		Title:     "twins.bin",
	})
	require.NoError(t, err)

	uploaded := recorded.digests()
	assert.Equal(t, 1, count(uploaded, parts[0].Digest), "the twin parts' blob must upload exactly once")
	assert.Equal(t, 1, count(uploaded, parts[2].Digest))
}

func TestPushSurfacesAnExistsFailure(t *testing.T) {
	t.Parallel()

	checkFailed := errors.New("the registry hung up")

	blobs := ocimocks.NewMockBlobs(t)
	// Every existence check fails; Put carries no expectation at all, so a
	// push that treats a failed check as "not held" and uploads anyway fails
	// the test loudly.
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).Return(false, checkFailed).Maybe()

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    mockSource(t, fileContent(multiPartSize)),
		Blobs:     blobs,
		Manifests: ocimocks.NewMockManifests(t),
		PartSize:  fixturePartSize,
		Workers:   2,
	})

	require.ErrorIs(t, err, checkFailed)
	require.ErrorContains(t, err, "check whether part")
}

func TestPushSurfacesAManifestWriteFailure(t *testing.T) {
	t.Parallel()

	writeFailed := errors.New("the registry refused the manifest")

	blobs, _ := mockBlobs(t, nil)
	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).Return("", writeFailed)

	desc, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    mockSource(t, fileContent(multiPartSize)),
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   2,
	})

	require.ErrorIs(t, err, writeFailed)
	require.ErrorContains(t, err, "write the manifest")
	assert.Equal(t, ocispec.Descriptor{}, desc, "a failed push returns no descriptor")
}

func TestPushRejectsASourceThatShrinks(t *testing.T) {
	t.Parallel()

	// The source claims fixturePartSize bytes but holds fewer, the shape a
	// file truncated under a running push takes. The hash pass must refuse to
	// record a digest for bytes that are not there.
	held := fileContent(700)

	source := filemocks.NewMockSource(t)
	source.EXPECT().Size().Return(int64(fixturePartSize))
	source.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(
		func(p []byte, off int64) (int, error) {
			return readAt(held, p, off)
		},
	).Maybe()

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     ocimocks.NewMockBlobs(t),
		Manifests: ocimocks.NewMockManifests(t),
		PartSize:  fixturePartSize,
		Workers:   2,
	})

	require.ErrorContains(t, err, "the source changed while the push read it")
}

func TestPullSurfacesACommitFailure(t *testing.T) {
	t.Parallel()

	commitFailed := errors.New("the rename failed")

	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "commit.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil)

	assembled := &memFile{}
	sink := mockSink(t, assembled)
	sink.EXPECT().Commit().Return(commitFailed)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     mockBlobsServing(t, newBlobStore(artifact.Parts, content)),
		Manifests: manifests,
		Workers:   2,
	})

	require.ErrorIs(t, err, commitFailed)
	require.ErrorContains(t, err, "commit the destination")
}

func TestPullReportsABrokenBlobReadAsAFetchFailure(t *testing.T) {
	t.Parallel()

	readFailed := errors.New("connection reset mid-part")

	content := fileContent(singlePartSize)
	_, body := artifactFor(t, content, "broken.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil)

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ digest.Digest, _ int64) (io.ReadCloser, int64, error) {
			return &brokenBody{fail: readFailed}, 0, nil
		},
	)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      mockSink(t, &memFile{}),
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
	})

	require.ErrorIs(t, err, readFailed)
	require.ErrorContains(t, err, "fetch part 0", "a registry that hangs up is a fetch failure")
	require.NotContains(t, err.Error(), "into the destination", "a read failure must not be blamed on the disk")
}

// count returns how many times want appears in digests.
func count(digests []digest.Digest, want digest.Digest) int {
	total := 0
	for _, dgst := range digests {
		if dgst == want {
			total++
		}
	}

	return total
}

// readAt answers a Source.ReadAt from held, with os-like semantics past the
// end: the bytes that exist come back with [io.EOF].
func readAt(held []byte, p []byte, off int64) (int, error) {
	if off >= int64(len(held)) {
		return 0, io.EOF
	}

	n := copy(p, held[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// brokenBody is a blob body whose read fails partway: one byte arrives, then
// the failure. It stands in for a registry that hangs up mid-part.
type brokenBody struct {
	// fail is the error the second read raises.
	fail error
	// sent records that the single good byte went out.
	sent bool
}

// Read serves one byte, then fails.
func (b *brokenBody) Read(p []byte) (int, error) {
	if !b.sent && len(p) > 0 {
		b.sent = true
		p[0] = 'x'

		return 1, nil
	}

	return 0, b.fail
}

// Close never fails.
func (b *brokenBody) Close() error {
	return nil
}
