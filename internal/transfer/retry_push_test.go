package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	filemocks "github.com/imgoci/bigoci/internal/file/mocks"
	"github.com/imgoci/bigoci/internal/manifest"
	ocimocks "github.com/imgoci/bigoci/internal/oci/mocks"
	"github.com/imgoci/bigoci/internal/retry"
	"github.com/imgoci/bigoci/internal/transfer"
)

// hostileWait is a wait a registry has no business asking for. The policy
// honors what a far end says up to its cap and no further, so this one is
// worth exactly [retry.DefaultCap].
const hostileWait = 24 * time.Hour

func TestPushRetriesPartUploads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fileSize   int64
		part       int
		script     func(dgst digest.Digest) blobScript
		wantChecks int
		wantPuts   int
		wantReads  int
		wantWaits  []time.Duration
		wantErr    error
		wantSays   []string
		wantSilent []string
	}{
		{
			name:     "a failed upload is attempted again, check and all",
			fileSize: multiPartSize,
			part:     1,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{put: map[digest.Digest][]error{
					dgst: {retry.Transient(errBroken, 0), nil},
				}}
			},
			wantChecks: 2,
			wantPuts:   2,
			wantReads:  3,
			wantWaits:  []time.Duration{500 * time.Millisecond},
		},
		{
			name:     "an upload whose answer was lost is found by the next check",
			fileSize: multiPartSize,
			part:     1,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{
					exists: map[digest.Digest][]existsAnswer{dgst: {{held: false}, {held: true}}},
					put:    map[digest.Digest][]error{dgst: {retry.Transient(errBroken, 0)}},
				}
			},
			wantChecks: 2,
			wantPuts:   1,
			wantReads:  2,
			wantWaits:  []time.Duration{500 * time.Millisecond},
		},
		{
			name:     "a failed existence check is attempted again",
			fileSize: multiPartSize,
			part:     1,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{exists: map[digest.Digest][]existsAnswer{
					dgst: {{err: retry.Transient(errBroken, 0)}, {held: false}},
				}}
			},
			wantChecks: 2,
			wantPuts:   1,
			wantReads:  2,
			wantWaits:  []time.Duration{500 * time.Millisecond},
		},
		{
			name:     "a wait the registry asked for is honored",
			fileSize: singlePartSize,
			part:     0,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{put: map[digest.Digest][]error{
					dgst: {retry.Transient(errBroken, 7*time.Second), nil},
				}}
			},
			wantChecks: 2,
			wantPuts:   2,
			wantReads:  3,
			wantWaits:  []time.Duration{7 * time.Second},
		},
		{
			name:     "a wait past the cap waits the cap",
			fileSize: singlePartSize,
			part:     0,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{put: map[digest.Digest][]error{
					dgst: {retry.Transient(errBroken, hostileWait), nil},
				}}
			},
			wantChecks: 2,
			wantPuts:   2,
			wantReads:  3,
			wantWaits:  []time.Duration{retry.DefaultCap},
		},
		{
			name:     "attempts running out surfaces the last failure",
			fileSize: multiPartSize,
			part:     1,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{put: map[digest.Digest][]error{dgst: {
					retry.Transient(errBroken, 0),
					retry.Transient(errBroken, 0),
					retry.Transient(errBroken, 0),
					retry.Transient(errLast, 0),
				}}}
			},
			wantChecks: retry.DefaultAttempts,
			wantPuts:   retry.DefaultAttempts,
			wantReads:  1 + retry.DefaultAttempts,
			wantWaits:  []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second},
			wantErr:    errLast,
			wantSays:   []string{"after 4 attempts", "upload part 1"},
		},
		{
			name:     "a failure nobody classified ends the push at once",
			fileSize: multiPartSize,
			part:     1,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{put: map[digest.Digest][]error{dgst: {errRefused}}}
			},
			wantChecks: 1,
			wantPuts:   1,
			wantReads:  2,
			wantErr:    errRefused,
			wantSays:   []string{"upload part 1"},
			wantSilent: []string{"attempts"},
		},
		{
			name:     "a part the registry refuses as too large is not sent again",
			fileSize: multiPartSize,
			part:     1,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{put: map[digest.Digest][]error{dgst: {errTooLarge}}}
			},
			wantChecks: 1,
			wantPuts:   1,
			wantReads:  2,
			wantErr:    errTooLarge,
			wantSilent: []string{"attempts"},
		},
		{
			name:     "a changed kind of failure neither resets the budget nor the escalation",
			fileSize: multiPartSize,
			part:     1,
			script: func(dgst digest.Digest) blobScript {
				return blobScript{put: map[digest.Digest][]error{dgst: {
					retry.Transient(errBroken, 0),
					retry.Transient(errBroken, 1500*time.Millisecond),
					retry.Transient(errBroken, 0),
					retry.Transient(errLast, 0),
				}}}
			},
			wantChecks: retry.DefaultAttempts,
			wantPuts:   retry.DefaultAttempts,
			wantReads:  1 + retry.DefaultAttempts,
			wantWaits: []time.Duration{
				500 * time.Millisecond, 1500 * time.Millisecond, 2 * time.Second,
			},
			wantErr:  errLast,
			wantSays: []string{"after 4 attempts"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := fileContent(tt.fileSize)
			parts := splitParts(t, content)
			target := parts[tt.part]
			offset := int64(tt.part) * int64(fixturePartSize)

			source, reads := countingSource(t, content)
			blobs, calls := scriptedBlobs(t, tt.script(target.Digest))
			policy, sleeps := testPolicy(t)

			_, err := transfer.Push(t.Context(), transfer.PushSpec{
				Source:    source,
				Blobs:     blobs,
				Manifests: acceptingManifests(t),
				PartSize:  fixturePartSize,
				Workers:   len(parts),
				Title:     "model.bin",
				Retry:     policy,
			})

			assert.Equal(t, tt.wantChecks, calls.checks(target.Digest), "existence checks for the part")
			assert.Equal(t, tt.wantPuts, calls.puts(target.Digest), "uploads of the part")
			assert.Equal(t, tt.wantReads, reads.at(offset), "reads that began at the part's first byte")
			assert.Equal(t, tt.wantWaits, sleeps.waits(), "the waits between attempts, in order")
			assertOnlyTargetRetried(t, parts, tt.part, calls.checks, "checked")

			if tt.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t,
					content[offset:offset+target.Size],
					calls.blob(target.Digest).content,
					"the attempt that succeeded must have streamed the part's own bytes",
				)

				return
			}

			require.ErrorIs(t, err, tt.wantErr, "the failure that ended the push stays reachable")
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

// TestPushRetriesTheEmptyConfigBlobFromAFreshReader is the row that catches a
// reader hoisted out of the attempt. The two bytes are read by the upload that
// failed, so a second attempt handed the same reader would send nothing at all
// under a Content-Length promising two.
func TestPushRetriesTheEmptyConfigBlobFromAFreshReader(t *testing.T) {
	t.Parallel()

	content := fileContent(singlePartSize)
	config, configContent := manifest.EmptyConfig()

	blobs, calls := scriptedBlobs(t, blobScript{put: map[digest.Digest][]error{
		config.Digest: {retry.Transient(errBroken, 0), nil},
	}})
	policy, sleeps := testPolicy(t)

	source, _ := countingSource(t, content)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: acceptingManifests(t),
		PartSize:  fixturePartSize,
		Workers:   1,
		Title:     "model.bin",
		Retry:     policy,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, calls.checks(config.Digest), "the check and the upload are one unit of work")
	assert.Equal(t, 2, calls.puts(config.Digest))
	assert.Equal(t, configContent, calls.blob(config.Digest).content, "every attempt streams the two bytes again")
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, sleeps.waits())
}

func TestPushRetriesTheManifestWrite(t *testing.T) {
	t.Parallel()

	content := fileContent(singlePartSize)
	_, wantBody := artifactFor(t, content, "model.bin")

	writes := 0

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ string, body []byte) (digest.Digest, error) {
			writes++
			if writes == 1 {
				return "", retry.Transient(errBroken, 0)
			}

			return digest.FromBytes(body), nil
		},
	)

	blobs, _ := scriptedBlobs(t, blobScript{})
	policy, sleeps := testPolicy(t)
	source, _ := countingSource(t, content)

	descriptor, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   1,
		Title:     "model.bin",
		Retry:     policy,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, writes, "the manifest write has a budget of its own")
	assert.Equal(t, digest.FromBytes(wantBody), descriptor.Digest, "the attempt that landed describes the same bytes")
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, sleeps.waits())
}

// TestPushDoesNotRetryASourceItCannotRead pins the constraint the port states:
// the orchestrator retries the registry and never the disk, so a file that
// will not read ends the push on the first failure.
func TestPushDoesNotRetryASourceItCannotRead(t *testing.T) {
	t.Parallel()

	unreadable := errors.New("the disk went away")

	source := filemocks.NewMockSource(t)
	source.EXPECT().Size().Return(multiPartSize)
	source.EXPECT().ReadAt(mock.Anything, mock.Anything).Return(0, unreadable)

	blobs := ocimocks.NewMockBlobs(t)
	policy, sleeps := testPolicy(t)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: ocimocks.NewMockManifests(t),
		PartSize:  fixturePartSize,
		Workers:   2,
		Title:     "model.bin",
		Retry:     policy,
	})
	require.ErrorIs(t, err, unreadable)

	assert.Empty(t, sleeps.waits(), "the hash pass runs under no budget")
	blobs.AssertNotCalled(t, "Put", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestPushDoesNotRetryASourceThatFailsMidUpload pins the harder half of the
// Source contract. A range that fails DURING an upload surfaces from inside
// the adapter, which cannot tell it from a broken connection and tags it
// worth repeating — so the orchestrator must unwrap its own marker and keep
// the disk terminal, or a dead source costs the whole budget per part.
func TestPushDoesNotRetryASourceThatFailsMidUpload(t *testing.T) {
	t.Parallel()

	unreadable := errors.New("the disk went away")
	content := fileContent(multiPartSize)
	parts := splitParts(t, content)
	target := 1
	offset := int64(target) * int64(fixturePartSize)

	// The range reads once for the hash pass and fails on every read after
	// it, which is a disk that died between hashing and uploading.
	var mu sync.Mutex

	readsAtTarget := 0

	source := filemocks.NewMockSource(t)
	source.EXPECT().Size().Return(int64(len(content))).Maybe()
	source.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(
		func(p []byte, off int64) (int, error) {
			mu.Lock()
			defer mu.Unlock()

			if off == offset {
				readsAtTarget++
				if readsAtTarget > 1 {
					return 0, unreadable
				}
			}

			return bytes.NewReader(content).ReadAt(p, off)
		},
	).Maybe()

	blobs, calls := scriptedBlobs(t, blobScript{})
	policy, sleeps := testPolicy(t)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: ocimocks.NewMockManifests(t),
		PartSize:  fixturePartSize,
		Workers:   len(parts),
		Title:     "model.bin",
		Retry:     policy,
	})
	require.ErrorIs(t, err, unreadable, "the disk failure stays reachable")
	require.ErrorContains(t, err, "read part 1 of the source at offset")
	assert.NotContains(t, err.Error(), "attempts", "a source failure is attempted exactly once")

	assert.Equal(t, 1, calls.puts(parts[target].Digest), "the registry is retried, never the disk")
	assert.Empty(t, sleeps.waits())
}

func TestPushStopsWhenABackoffIsInterrupted(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	parts := splitParts(t, content)

	blobs, calls := scriptedBlobs(t, blobScript{put: map[digest.Digest][]error{
		parts[1].Digest: {retry.Transient(errBroken, 0)},
	}})
	policy, sleeps := interruptedPolicy(t, context.Canceled)
	source, _ := countingSource(t, content)

	// The manifests mock carries no expectation: a push that gave up must not
	// go on to publish an artifact.
	manifests := ocimocks.NewMockManifests(t)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   len(parts),
		Title:     "model.bin",
		Retry:     policy,
	})
	require.ErrorIs(t, err, context.Canceled, "why the push stopped")
	require.ErrorIs(t, err, errBroken, "what it was retrying when it did")

	assert.Equal(t, 1, calls.puts(parts[1].Digest), "a wait that was cut short is not followed by another attempt")
	assert.Len(t, sleeps.waits(), 1)
}

func TestPushRecordsPartsInFileOrderThroughARetry(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	want, _ := artifactFor(t, content, "model.bin")

	blobs, calls := scriptedBlobs(t, blobScript{put: map[digest.Digest][]error{
		want.Parts[0].Digest: {retry.Transient(errBroken, 0), retry.Transient(errBroken, 0), nil},
	}})
	policy, _ := testPolicy(t)
	source, _ := countingSource(t, content)

	var body []byte

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ string, manifestBody []byte) (digest.Digest, error) {
			body = manifestBody

			return digest.FromBytes(manifestBody), nil
		},
	).Once()

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: manifests,
		PartSize:  fixturePartSize,
		Workers:   len(want.Parts),
		Title:     "model.bin",
		Retry:     policy,
	})
	require.NoError(t, err)

	decoded, err := manifest.Decode(body)
	require.NoError(t, err)
	assert.Equal(t, want.Parts, decoded.Parts, "the order is the file's, whatever order the uploads finished in")

	offset := int64(0)
	for i, part := range want.Parts {
		assert.Equal(
			t,
			content[offset:offset+part.Size],
			calls.blob(part.Digest).content,
			"part %d carried other bytes",
			i,
		)
		offset += part.Size
	}
}

func TestPushUploadsADuplicatedPartOnceUnderRetry(t *testing.T) {
	t.Parallel()

	// Two byte-identical full parts plus a distinct tail, the fixture the
	// claim set exists for. A retry is the claiming worker carrying on, so the
	// twin's blob still moves under exactly one worker's budget.
	block := fileContent(int64(fixturePartSize))
	content := slices.Concat(block, block, fileContent(singlePartSize))
	parts := splitParts(t, content)
	require.Equal(t, parts[0].Digest, parts[1].Digest, "the fixture needs twin parts to mean anything")

	blobs, calls := scriptedBlobs(t, blobScript{put: map[digest.Digest][]error{
		parts[0].Digest: {retry.Transient(errBroken, 0), nil},
	}})
	policy, sleeps := testPolicy(t)
	source, _ := countingSource(t, content)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: acceptingManifests(t),
		PartSize:  fixturePartSize,
		Workers:   4,
		Title:     "twins.bin",
		Retry:     policy,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, calls.puts(parts[0].Digest), "the twins' blob moves once, retried once")
	assert.Equal(t, 1, calls.puts(parts[2].Digest))
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, sleeps.waits())
}

func TestPushFailsWhenTheOwnerOfADuplicatedPartExhaustsItsAttempts(t *testing.T) {
	t.Parallel()

	block := fileContent(int64(fixturePartSize))
	content := slices.Concat(block, block, fileContent(singlePartSize))
	parts := splitParts(t, content)

	blobs, calls := scriptedBlobs(t, blobScript{put: map[digest.Digest][]error{
		parts[0].Digest: {retry.Transient(errBroken, 0)},
	}})
	policy, sleeps := testPolicy(t)
	source, _ := countingSource(t, content)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: ocimocks.NewMockManifests(t),
		PartSize:  fixturePartSize,
		Workers:   4,
		Title:     "twins.bin",
		Retry:     policy,
	})
	require.ErrorIs(t, err, errBroken)

	assert.Equal(t,
		retry.DefaultAttempts,
		calls.puts(parts[0].Digest),
		"the claiming worker spends the budget; the worker that skipped the twin never adds to it",
	)
	assert.Equal(t, []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}, sleeps.waits())
}

// TestPushWakesAWorkerBackingOffWhenAnotherFails is the regression guard for a
// sleep that ignores its context. The injected wait blocks until the transfer
// ends, exactly as the real one does, so a worker parked in a backoff can only
// leave because the group was cancelled — and Push returns only once every
// worker has. Reaching the assertions at all is therefore the proof.
func TestPushWakesAWorkerBackingOffWhenAnotherFails(t *testing.T) {
	t.Parallel()

	content := fileContent(exactMultipleSize)
	parts := splitParts(t, content)
	require.Len(t, parts, 2, "the fixture needs one part per worker")
	require.NotEqual(t, parts[0].Digest, parts[1].Digest,
		"twin parts would be claimed once, leaving nobody to back off")

	policy, sleeps := blockingPolicy(t)

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	blobs.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, _ int64, _ io.Reader) error {
			if dgst == parts[1].Digest {
				return retry.Transient(errBroken, 0)
			}

			// The terminal failure waits until the other worker is provably
			// inside a backoff, so the race this test is about really happens.
			<-sleeps.backingOff()

			return errRefused
		},
	).Maybe()

	source, _ := countingSource(t, content)

	_, err := transfer.Push(t.Context(), transfer.PushSpec{
		Source:    source,
		Blobs:     blobs,
		Manifests: ocimocks.NewMockManifests(t),
		PartSize:  fixturePartSize,
		Workers:   len(parts),
		Title:     "model.bin",
		Retry:     policy,
	})
	require.ErrorIs(t, err, errRefused, "the push reports the failure that caused the cancellation")
	require.NotErrorIs(t, err, errBroken, "not the cancellation it provoked in the worker that was waiting")

	assert.Len(t, sleeps.waits(), 1, "the worker that backed off did so once and then left")
}
