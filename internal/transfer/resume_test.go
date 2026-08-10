package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	filemocks "github.com/componere/bigoci/internal/file/mocks"
	"github.com/componere/bigoci/internal/manifest"
	ocimocks "github.com/componere/bigoci/internal/oci/mocks"
	"github.com/componere/bigoci/internal/plan"
	"github.com/componere/bigoci/internal/transfer"
)

// This file is about the pull that finds something already on disk: which
// parts a partial file spares, which it does not, and what a pull does with
// one it cannot read or measure. What happens inside a part once a fetch has
// started is next door, in continue_test.go.

// corruptedByte is the byte a row spoils inside a part of the partial file. It
// is past the first byte on purpose: a part that only ever fails on its first
// byte would pass a check that hashed nothing but the beginning.
const corruptedByte = 7

func TestPullResumesFromAPartialFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// fileSize is the length of the artifact being pulled.
		fileSize int64
		// seed is the partial file the pull finds, built from the content the
		// artifact holds.
		seed func(content []byte) []byte
		// wantFetched are the parts, by index, that must be fetched off the
		// registry. Every other part has to come out of the partial file.
		wantFetched []int
	}{
		{
			name:     "a complete partial is verified and committed without a single fetch",
			fileSize: multiPartSize,
			seed:     slices.Clone[[]byte],
		},
		{
			name:     "a partial holding only a prefix fetches exactly the parts that are missing",
			fileSize: multiPartSize,
			seed: func(content []byte) []byte {
				partial := make([]byte, len(content))
				copy(partial, content[:fixturePartSize])

				return partial
			},
			wantFetched: []int{1, 2},
		},
		{
			name:     "one spoiled part in the middle is the only one fetched",
			fileSize: multiPartSize,
			seed: func(content []byte) []byte {
				partial := slices.Clone(content)
				partial[int64(fixturePartSize)+corruptedByte] ^= 0xFF

				return partial
			},
			wantFetched: []int{1},
		},
		{
			name:        "a partial of nothing but zeros fetches every part",
			fileSize:    multiPartSize,
			seed:        func(content []byte) []byte { return make([]byte, len(content)) },
			wantFetched: []int{0, 1, 2},
		},
		{
			name:        "a partial longer than the manifest is cut to fit and every part fetched",
			fileSize:    multiPartSize,
			seed:        func(content []byte) []byte { return append(slices.Clone(content), make([]byte, 64)...) },
			wantFetched: []int{0, 1, 2},
		},
		{
			name:        "a partial shorter than the manifest is grown and every part fetched",
			fileSize:    multiPartSize,
			seed:        func(content []byte) []byte { return slices.Clone(content[:len(content)-1]) },
			wantFetched: []int{0, 1, 2},
		},
		{
			// A zero-byte destination is the same length as a zero-byte
			// artifact, so length alone would call an untouched file complete.
			// The empty part is fetched instead, exactly as on a first run.
			name:        "a zero-byte artifact still fetches its one empty part",
			fileSize:    0,
			seed:        func([]byte) []byte { return nil },
			wantFetched: []int{0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := fileContent(tt.fileSize)
			artifact, body := artifactFor(t, content, "model.bin")

			manifests := ocimocks.NewMockManifests(t)
			manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

			store := newBlobStore(artifact.Parts, content)
			blobs, calls := fetchingBlobs(t, store, fetchScript{})

			file := newMemFile(tt.seed(content))
			sink := mockSink(t, file)
			sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

			err := transfer.Pull(t.Context(), transfer.PullSpec{
				Sink:      sink,
				Blobs:     blobs,
				Manifests: manifests,
				Workers:   2,
			})
			require.NoError(t, err)

			assert.Equal(t, tt.wantFetched, fetchedParts(artifact.Parts, calls),
				"the parts fetched off the registry, in file order")
			assert.Equal(t, content, file.bytes())
			assert.Equal(t, 1, file.commitCount())
			sink.AssertCalled(t, "Truncate", tt.fileSize)

			served, closed := store.counts()
			assert.Equal(t, len(tt.wantFetched), served, "a part that verified out of the partial file opens no body")
			assert.Equal(t, served, closed, "every blob body must be closed")
		})
	}
}

// TestPullKeepsAResumeMismatchOutOfTheDigestSentinel is the pair of rows that
// hold the two mismatches apart. A part of the partial file that hashes wrong
// is a part still to fetch and never an error value; a part the registry then
// serves wrong is [transfer.ErrDigestMismatch], which is what a caller
// branches on. Merging them would make a stale file on disk look like a
// registry serving content the artifact does not describe.
func TestPullKeepsAResumeMismatchOutOfTheDigestSentinel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// serves is what the registry holds under the spoiled part's digest.
		serves func(part []byte) []byte
		// wantErr says the pull must fail with the digest sentinel.
		wantErr bool
	}{
		{
			name:   "a partial that does not match is fetched, not refused",
			serves: func(part []byte) []byte { return part },
		},
		{
			name:    "a partial the registry cannot correct is a digest mismatch",
			serves:  func(part []byte) []byte { return bytes.Repeat([]byte{0xAA}, len(part)) },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := fileContent(multiPartSize)
			artifact, body := artifactFor(t, content, "model.bin")
			target := artifact.Parts[retriedPart]

			manifests := ocimocks.NewMockManifests(t)
			manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

			store := newBlobStore(artifact.Parts, content)
			store.bodies[target.Digest] = tt.serves(store.bodies[target.Digest])
			blobs, calls := fetchingBlobs(t, store, fetchScript{})

			partial := slices.Clone(content)
			partial[int64(fixturePartSize)+corruptedByte] ^= 0xFF

			file := newMemFile(partial)
			sink := mockSink(t, file)
			sink.EXPECT().Commit().RunAndReturn(file.commit).Maybe()

			err := transfer.Pull(t.Context(), transfer.PullSpec{
				Sink:      sink,
				Blobs:     blobs,
				Manifests: manifests,
				Workers:   2,
				Retry:     noRetry(),
			})

			assert.Equal(t, []int{retriedPart}, fetchedParts(artifact.Parts, calls),
				"only the part the partial file spoiled is fetched")
			assert.Equal(t, 1, calls.gets(target.Digest), "a partial that hashes wrong costs no attempt")

			if tt.wantErr {
				require.ErrorIs(t, err, transfer.ErrDigestMismatch)
				require.ErrorContains(t, err, "part 1")
				assert.Zero(t, file.commitCount())

				return
			}

			require.NoError(t, err)
			require.NotErrorIs(t, err, transfer.ErrDigestMismatch)
			assert.Equal(t, content, file.bytes())
			assert.Equal(t, 1, file.commitCount())
		})
	}
}

// TestPullDoesNotRetryAPartialItCannotRead is the local half of the retry
// rule, on the resume path: the orchestrator retries the registry and never
// the disk, so a partial file that will not read ends the pull before a single
// request goes out.
func TestPullDoesNotRetryAPartialItCannotRead(t *testing.T) {
	t.Parallel()

	unreadable := errors.New("the partial file will not read")

	content := fileContent(multiPartSize)
	_, body := artifactFor(t, content, "model.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	// The blobs mock carries no expectation: a pull that cannot read what it
	// is resuming into must not fall back to fetching the part anyway.
	blobs := ocimocks.NewMockBlobs(t)
	policy, sleeps := testPolicy(t)

	sink := filemocks.NewMockSink(t)
	sink.EXPECT().Size().Return(multiPartSize, nil).Once()
	sink.EXPECT().Truncate(int64(multiPartSize)).Return(nil).Once()
	sink.EXPECT().ReadAt(mock.Anything, mock.Anything).Return(0, unreadable)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
		Retry:     policy,
	})
	require.ErrorIs(t, err, unreadable)
	require.ErrorContains(t, err, "read part 0 of the existing file")

	assert.Empty(t, sleeps.waits(), "a disk that will not read is not waited on")
	blobs.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)
	sink.AssertNotCalled(t, "Commit")
}

// TestPullStopsResumeVerificationWhenContextIsCancelled proves that hashing a
// large existing part observes cancellation between bounded reads. The first
// read ends the context deliberately; no second disk read, blob request, or
// commit may happen after that point.
func TestPullStopsResumeVerificationWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	const size = 1 << 20

	content := fileContent(size)
	partDigest := digest.FromBytes(content)
	artifact := manifest.Artifact{
		FileDigest: partDigest,
		FileSize:   size,
		PartSize:   plan.PartSize(size),
		Title:      "model.bin",
		Parts: []manifest.Part{{
			Digest: partDigest,
			Size:   size,
		}},
	}
	body, err := manifest.Encode(artifact)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	blobs := ocimocks.NewMockBlobs(t)

	reads := 0
	sink := filemocks.NewMockSink(t)
	sink.EXPECT().Size().Return(int64(size), nil).Once()
	sink.EXPECT().Truncate(int64(size)).Return(nil).Once()
	sink.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(
		func(p []byte, off int64) (int, error) {
			reads++
			n := copy(p, content[off:])
			if reads == 1 {
				cancel()
			}
			if n < len(p) {
				return n, io.EOF
			}

			return n, nil
		},
	).Maybe()

	err = transfer.Pull(ctx, transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
	})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "read part 0 of the existing file")

	assert.Equal(t, 1, reads, "cancellation after the first bounded read stops the hash pass")
	blobs.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)
	sink.AssertNotCalled(t, "Commit")
}

// TestPullRefusesAPartialThatReadsShort pins the second local failure the
// verify can meet: a partial whose range ends before the manifest says it
// should, from a sink that shrank underneath the pull or reads short without
// an error. The pull just sized the file, so a short range is the destination
// misbehaving — terminal, like every other local failure, and never quietly
// treated as a part to fetch.
func TestPullRefusesAPartialThatReadsShort(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	_, body := artifactFor(t, content, "model.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	blobs := ocimocks.NewMockBlobs(t)
	policy, sleeps := testPolicy(t)

	sink := filemocks.NewMockSink(t)
	sink.EXPECT().Size().Return(multiPartSize, nil).Once()
	sink.EXPECT().Truncate(int64(multiPartSize)).Return(nil).Once()
	sink.EXPECT().ReadAt(mock.Anything, mock.Anything).Return(0, io.EOF)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
		Retry:     policy,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "read part 0 of the existing file: length does not match the manifest")

	assert.Empty(t, sleeps.waits())
	blobs.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)
	sink.AssertNotCalled(t, "Commit")
}

// TestPullDoesNotRetryADestinationItCannotMeasure pins the other terminal
// failure the resume decision rests on. The length of the destination is the
// only evidence a resume has, so a pull that cannot read it stops there rather
// than guessing — and it stops before the truncate, which would have thrown
// the evidence away.
func TestPullDoesNotRetryADestinationItCannotMeasure(t *testing.T) {
	t.Parallel()

	unmeasurable := errors.New("the filesystem will not say")

	content := fileContent(singlePartSize)
	_, body := artifactFor(t, content, "model.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	blobs := ocimocks.NewMockBlobs(t)
	policy, sleeps := testPolicy(t)

	sink := filemocks.NewMockSink(t)
	sink.EXPECT().Size().Return(0, unmeasurable).Once()

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
		Retry:     policy,
	})
	require.ErrorIs(t, err, unmeasurable)
	require.ErrorContains(t, err, "measure the destination")

	assert.Empty(t, sleeps.waits())
	sink.AssertNotCalled(t, "Truncate", mock.Anything)
	sink.AssertNotCalled(t, "Commit")
	blobs.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)
}

// TestPullMeasuresTheDestinationBeforeItSizesIt pins the one ordering the
// whole resume rests on. Truncate is what makes a leftover partial the right
// length, so a pull that truncated first would find every destination
// convincing and hash a file it had just created itself.
//
// The claim is the mock's, not an assertion at the end: the truncate is
// declared as a call that must not come before the measurement, so a pull that
// reversed them fails here rather than somewhere downstream.
func TestPullMeasuresTheDestinationBeforeItSizesIt(t *testing.T) {
	t.Parallel()

	content := fileContent(singlePartSize)
	artifact, body := artifactFor(t, content, "model.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	file := newMemFile(nil)

	sink := filemocks.NewMockSink(t)
	measured := sink.EXPECT().Size().RunAndReturn(file.size)
	measured.Once()
	sink.EXPECT().Truncate(int64(singlePartSize)).RunAndReturn(file.truncate).Once().NotBefore(measured.Call)
	sink.EXPECT().WriteAt(mock.Anything, mock.Anything).RunAndReturn(file.writeAt).Maybe()
	sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     mockBlobsServing(t, newBlobStore(artifact.Parts, content)),
		Manifests: manifests,
		Workers:   1,
	})
	require.NoError(t, err)

	assert.Equal(t, content, file.bytes())
	assert.Equal(t, 1, file.commitCount())
}

// fetchedParts returns the indexes of the parts that were fetched off the
// registry, in file order. A part missing from the list came out of the
// partial file instead.
func fetchedParts(parts []manifest.Part, calls *blobCalls) []int {
	var fetched []int

	for i, part := range parts {
		if calls.gets(part.Digest) > 0 {
			fetched = append(fetched, i)
		}
	}

	return fetched
}
