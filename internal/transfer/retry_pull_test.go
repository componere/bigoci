package transfer_test

import (
	"bytes"
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
	"github.com/imgoci/bigoci/internal/retry"
	"github.com/imgoci/bigoci/internal/transfer"
)

// retriedPart is the part every row of the fetch table breaks. Two parts sit
// after it and one before, so a row also proves the parts around a retried
// one are unaffected.
const retriedPart = 1

// brokenPrefix returns what a body that dies mid-part serves before it dies:
// the first half of the part every fetch row breaks, byte for byte as the
// registry holds it in content.
//
// The bytes have to be the part's own, because the attempt after the break
// carries on from where the body stopped and hashes what arrives onto them. A
// fixture serving anything else would be a registry that lied about the
// content, not a connection that dropped. What catches an attempt that wrote
// at the wrong offset is the byte comparison against the whole file each row
// ends with.
func brokenPrefix(content []byte) []byte {
	return partBytes(content, 0, int64(fixturePartSize)/2)
}

// partBytes returns the bytes of [retriedPart] between from and to, counted
// from that part's own first byte: what a body serves before it breaks, and
// what the attempt after it has left to fetch.
func partBytes(content []byte, from, to int64) []byte {
	offset := int64(retriedPart) * int64(fixturePartSize)

	return content[offset+from : offset+to]
}

func TestPullRetriesPartFetches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		script     func(dgst digest.Digest) fetchScript
		spoil      func(store *blobStore, part manifest.Part)
		wantGets   int
		wantWaits  []time.Duration
		fails      bool
		wantErr    error
		wantSays   []string
		wantSilent []string
	}{
		{
			name: "a fetch that fails is attempted again",
			script: func(dgst digest.Digest) fetchScript {
				return fetchScript{dgst: {{err: retry.Transient(errBroken, 0)}, {}}}
			},
			wantGets:  2,
			wantWaits: []time.Duration{500 * time.Millisecond},
		},
		{
			name: "a body that breaks mid-part is continued from the byte it reached",
			script: func(dgst digest.Digest) fetchScript {
				return fetchScript{dgst: {
					{prefix: brokenPrefix(fileContent(multiPartSize)), breaks: retry.Transient(errBroken, 0)},
					{},
				}}
			},
			wantGets:  2,
			wantWaits: []time.Duration{500 * time.Millisecond},
		},
		{
			name: "a body that ends before the manifest says is continued",
			script: func(dgst digest.Digest) fetchScript {
				return fetchScript{dgst: {{prefix: brokenPrefix(fileContent(multiPartSize)), breaks: io.EOF}, {}}}
			},
			wantGets:  2,
			wantWaits: []time.Duration{500 * time.Millisecond},
		},
		{
			name: "a wait the registry asked for is honored",
			script: func(dgst digest.Digest) fetchScript {
				return fetchScript{dgst: {{err: retry.Transient(errBroken, 7*time.Second)}, {}}}
			},
			wantGets:  2,
			wantWaits: []time.Duration{7 * time.Second},
		},
		{
			name: "attempts running out surfaces the last failure",
			script: func(dgst digest.Digest) fetchScript {
				return fetchScript{dgst: {
					{err: retry.Transient(errBroken, 0)},
					{err: retry.Transient(errBroken, 0)},
					{err: retry.Transient(errBroken, 0)},
					{err: retry.Transient(errLast, 0)},
				}}
			},
			wantGets:  retry.DefaultAttempts,
			wantWaits: []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second},
			fails:     true,
			wantErr:   errLast,
			wantSays:  []string{"after 4 attempts", "fetch part 1"},
		},
		{
			name: "a failure nobody classified ends the pull at once",
			script: func(dgst digest.Digest) fetchScript {
				return fetchScript{dgst: {{err: errRefused}}}
			},
			wantGets:   1,
			fails:      true,
			wantErr:    errRefused,
			wantSays:   []string{"fetch part 1"},
			wantSilent: []string{"attempts"},
		},
		{
			name: "a part that hashes wrong is not fetched again",
			spoil: func(store *blobStore, part manifest.Part) {
				store.bodies[part.Digest] = bytes.Repeat([]byte{0xAA}, int(part.Size))
			},
			wantGets:   1,
			fails:      true,
			wantErr:    transfer.ErrDigestMismatch,
			wantSays:   []string{"part 1"},
			wantSilent: []string{"attempts"},
		},
		{
			name: "a body longer than the manifest declares is not fetched again",
			spoil: func(store *blobStore, part manifest.Part) {
				store.bodies[part.Digest] = append(slices.Clone(store.bodies[part.Digest]), 0x00)
			},
			wantGets:   1,
			fails:      true,
			wantSays:   []string{"is longer than its declared size"},
			wantSilent: []string{"attempts"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := fileContent(multiPartSize)
			artifact, body := artifactFor(t, content, "model.bin")
			target := artifact.Parts[retriedPart]

			store := newBlobStore(artifact.Parts, content)
			if tt.spoil != nil {
				tt.spoil(store, target)
			}

			script := fetchScript{}
			if tt.script != nil {
				script = tt.script(target.Digest)
			}

			blobs, calls := fetchingBlobs(t, store, script)
			policy, sleeps := testPolicy(t)

			manifests := ocimocks.NewMockManifests(t)
			manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

			file := &memFile{}
			sink := mockSink(t, file)
			sink.EXPECT().Commit().RunAndReturn(file.commit).Maybe()

			err := transfer.Pull(t.Context(), transfer.PullSpec{
				Sink:      sink,
				Blobs:     blobs,
				Manifests: manifests,
				Workers:   len(artifact.Parts),
				Retry:     policy,
			})

			assert.Equal(t, tt.wantGets, calls.gets(target.Digest), "fetches of the part")
			assert.Equal(t, tt.wantWaits, sleeps.waits(), "the waits between attempts, in order")
			assertOnlyTargetRetried(t, artifact.Parts, retriedPart, calls.gets, "fetched")

			served, closed := store.counts()
			assert.Equal(t, served, closed, "every blob body must be closed, including a failed attempt's")

			if !tt.fails {
				require.NoError(t, err)
				assert.Equal(t, content, file.bytes(), "the attempt that succeeded wrote over whatever came before it")
				assert.Equal(t, 1, file.commitCount())

				return
			}

			require.Error(t, err)
			assert.Zero(t, file.commitCount(), "a pull that gave up publishes nothing")

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr, "the failure that ended the pull stays reachable")
			}
			for _, says := range tt.wantSays {
				assert.Contains(t, err.Error(), says)
			}
			for _, silent := range tt.wantSilent {
				assert.NotContains(
					t,
					err.Error(),
					silent,
					"a failure that was attempted once says nothing about attempts",
				)
			}
		})
	}
}

func TestPullRetriesTheManifestFetch(t *testing.T) {
	t.Parallel()

	content := fileContent(singlePartSize)
	artifact, body := artifactFor(t, content, "model.bin")

	fetches := 0

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).RunAndReturn(
		func(context.Context) ([]byte, ocispec.Descriptor, error) {
			fetches++
			if fetches == 1 {
				return nil, ocispec.Descriptor{}, retry.Transient(errBroken, 0)
			}

			return body, manifestDescriptor(body), nil
		},
	)

	blobs, _ := fetchingBlobs(t, newBlobStore(artifact.Parts, content), fetchScript{})
	policy, sleeps := testPolicy(t)

	file := &memFile{}
	sink := mockSink(t, file)
	sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
		Retry:     policy,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, fetches, "the manifest fetch has a budget of its own")
	assert.Equal(t, content, file.bytes())
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, sleeps.waits())
}

// TestPullDoesNotRetryADestinationItCannotWrite is the read-versus-write
// tagging surviving into the retry decision: the copy that failed had a
// registry read and a disk write in it, and only one of the two is worth
// another attempt.
func TestPullDoesNotRetryADestinationItCannotWrite(t *testing.T) {
	t.Parallel()

	full := errors.New("the disk is full")

	content := fileContent(singlePartSize)
	artifact, body := artifactFor(t, content, "model.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	blobs, calls := fetchingBlobs(t, newBlobStore(artifact.Parts, content), fetchScript{})
	policy, sleeps := testPolicy(t)

	sink := filemocks.NewMockSink(t)
	sink.EXPECT().Size().Return(0, nil).Once()
	sink.EXPECT().Truncate(mock.Anything).Return(nil).Once()
	sink.EXPECT().WriteAt(mock.Anything, mock.Anything).Return(0, full)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
		Retry:     policy,
	})
	require.ErrorIs(t, err, full)
	require.ErrorContains(t, err, "write part 0 into the destination")

	assert.Equal(t, 1, calls.gets(artifact.Parts[0].Digest), "a disk that will not take bytes is not asked twice")
	assert.Empty(t, sleeps.waits())
	sink.AssertNotCalled(t, "Commit")
}

func TestPullDoesNotRetryADestinationItCannotTruncate(t *testing.T) {
	t.Parallel()

	full := errors.New("the disk is full")

	content := fileContent(singlePartSize)
	_, body := artifactFor(t, content, "model.bin")

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	// The blobs mock carries no expectation: a pull that cannot size the
	// destination must not fetch a part, let alone fetch one four times.
	blobs := ocimocks.NewMockBlobs(t)
	policy, sleeps := testPolicy(t)

	sink := filemocks.NewMockSink(t)
	sink.EXPECT().Size().Return(0, nil).Once()
	sink.EXPECT().Truncate(mock.Anything).Return(full).Once()

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   1,
		Retry:     policy,
	})
	require.ErrorIs(t, err, full)

	assert.Empty(t, sleeps.waits())
	blobs.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)
}

func TestPullStopsWhenABackoffIsInterrupted(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")
	target := artifact.Parts[retriedPart]

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	blobs, calls := fetchingBlobs(t, newBlobStore(artifact.Parts, content), fetchScript{
		target.Digest: {{err: retry.Transient(errBroken, 0)}},
	})
	policy, sleeps := interruptedPolicy(t, context.Canceled)

	file := &memFile{}
	sink := mockSink(t, file)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   len(artifact.Parts),
		Retry:     policy,
	})
	require.ErrorIs(t, err, context.Canceled, "why the pull stopped")
	require.ErrorIs(t, err, errBroken, "what it was retrying when it did")

	assert.Equal(t, 1, calls.gets(target.Digest), "a wait that was cut short is not followed by another attempt")
	assert.Len(t, sleeps.waits(), 1)
	sink.AssertNotCalled(t, "Commit")
	assert.Zero(t, file.commitCount())
}

// TestPullAssemblesTheFileThroughARetry is the byte-level claim behind
// attempting a part again: the part that broke is stitched back together
// across three attempts, the parts around it are written by workers that never
// waited, and the file they assemble together is exact.
func TestPullAssemblesTheFileThroughARetry(t *testing.T) {
	t.Parallel()

	content := fileContent(4 * int64(fixturePartSize))
	artifact, body := artifactFor(t, content, "model.bin")
	target := artifact.Parts[retriedPart]

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	blobs, calls := fetchingBlobs(t, newBlobStore(artifact.Parts, content), fetchScript{
		target.Digest: {
			{prefix: brokenPrefix(content), breaks: retry.Transient(errBroken, 0)},
			{err: retry.Transient(errBroken, 0)},
			{},
		},
	})
	policy, sleeps := testPolicy(t)

	file := &memFile{}
	sink := mockSink(t, file)
	sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   len(artifact.Parts),
		Retry:     policy,
	})
	require.NoError(t, err)

	assert.Equal(t, content, file.bytes())
	assert.Equal(t, 1, file.commitCount())
	assert.Equal(t, 3, calls.gets(target.Digest))
	assert.Equal(t, []int64{0, 500, 500}, calls.offsets(target.Digest),
		"a Get that never opened a body moved no byte, so the next attempt asks from the same place")
	assert.Equal(t, []time.Duration{500 * time.Millisecond, time.Second}, sleeps.waits())
}
