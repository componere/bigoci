package transfer_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/manifest"
	ocimocks "github.com/imgoci/bigoci/internal/oci/mocks"
	"github.com/imgoci/bigoci/internal/retry"
	"github.com/imgoci/bigoci/internal/transfer"
)

// This file is the pull half of the progress accounting. The rows that matter
// most are the ones where the two byte counters part company: a continuation
// costs the file and no more, a whole-blob fallback costs the file plus what
// the broken attempt had already taken, and a part whose break landed on its
// last byte costs the part twice.

func TestPullProgressAccounting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// seed is the partial file the pull finds, or nil for a first run.
		seed func(content []byte) []byte
		// script is what the registry does to the part the row is about.
		script func(parts []manifest.Part) fetchScript
		// manifests builds the manifest port, or is nil for one that answers
		// the first time it is asked.
		manifests func(t *testing.T, body []byte) *ocimocks.MockManifests
		// want is the terminal snapshot, which is the whole account.
		want transfer.Snapshot
	}{
		{
			// The baseline every other row is read against: one fetch per
			// part, nothing hashed off the disk, nothing repeated.
			name: "a cold pull fetches every part exactly once",
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize,
			},
		},
		{
			// Case a: the attempt after a break asks for the rest of the
			// part, so a stream that died most of the way through costs only
			// the bytes that never landed. The wire count says so by being
			// the file exactly.
			name: "a part continued after a break costs the file and no more",
			script: func(parts []manifest.Part) fetchScript {
				return fetchScript{parts[retriedPart].Digest: {
					{prefix: partBytes(fileContent(multiPartSize), 0, brokenAt), breaks: retry.Transient(errBroken, 0)},
					{},
				}}
			},
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize,
				Retries:        1,
			},
		},
		{
			// Case b: a registry that will not serve a byte range answers a
			// continuation with the whole blob, which the attempt writes over
			// what it already held. The bytes of the broken attempt are spent
			// either way, and this is the row where a single byte counter
			// would have to choose between lying and going backwards.
			name: "a whole-blob fallback pays for the broken attempt as well",
			script: func(parts []manifest.Part) fetchScript {
				return fetchScript{parts[retriedPart].Digest: {
					{prefix: partBytes(fileContent(multiPartSize), 0, brokenAt), breaks: retry.Transient(errBroken, 0)},
					{ignoreRange: true},
				}}
			},
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize + brokenAt,
				Retries:        1,
			},
		},
		{
			// Case c: the break arrived with the part's last byte, so there
			// is nothing left to continue from and the attempt after it asks
			// for the part whole. That part crosses the wire twice.
			name: "a break on the last byte of a part costs that part twice",
			script: func(parts []manifest.Part) fetchScript {
				whole := partBytes(fileContent(multiPartSize), 0, int64(fixturePartSize))

				return fetchScript{parts[retriedPart].Digest: {
					{prefix: whole, breaks: retry.Transient(errBroken, 0)},
					{},
				}}
			},
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize + int64(fixturePartSize),
				Retries:        1,
			},
		},
		{
			// Case d: a resume that finds everything intact. Every part is
			// complete, every one of them skipped, and the whole file was
			// read off the disk to prove it — which is what HashedBytes is
			// for, and the only counter moving during that pass.
			name: "a resume into a complete partial file moves nothing over the wire",
			seed: slices.Clone[[]byte],
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				SkippedParts:   3,
				HashedBytes:    multiPartSize,
			},
		},
		{
			// Case d': the part that hashes wrong is read off the disk and
			// then fetched, so it counts under both local and wire bytes, and
			// it is not skipped. The parts around it still are.
			name: "a resume fetches the one part that does not verify, and skips the rest",
			seed: func(content []byte) []byte {
				partial := slices.Clone(content)
				partial[int64(fixturePartSize)*retriedPart+corruptedByte] ^= 0xFF

				return partial
			},
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				SkippedParts:   2,
				WireBytes:      int64(fixturePartSize),
				HashedBytes:    multiPartSize,
			},
		},
		{
			// Case i: a manifest fetch that has to be repeated. No byte of
			// the file has moved yet and none of these bytes are the file, so
			// the retry count and the resolving phase are the whole of what a
			// watcher sees — which is exactly the difference between a pull
			// that is stuck and one that has not started.
			name:      "a manifest fetch that has to be repeated is a retry before anything moves",
			manifests: flakyManifestReads,
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize,
				Retries:        1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := fileContent(multiPartSize)
			artifact, body := artifactFor(t, content, "model.bin")

			newManifests := answeringManifests
			if tt.manifests != nil {
				newManifests = tt.manifests
			}
			manifests := newManifests(t, body)

			script := fetchScript{}
			if tt.script != nil {
				script = tt.script(artifact.Parts)
			}
			blobs, _ := fetchingBlobs(t, newBlobStore(artifact.Parts, content), script)

			var seed []byte
			if tt.seed != nil {
				seed = tt.seed(content)
			}
			file := newMemFile(seed)
			sink := mockSink(t, file)
			sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

			policy, _ := testPolicy(t)
			recorded := newSnapshots(t)

			require.NoError(t, transfer.Pull(t.Context(), transfer.PullSpec{
				Sink:      sink,
				Blobs:     blobs,
				Manifests: manifests,
				Workers:   2,
				Retry:     policy,
				Progress:  recorded.record,
			}))

			assert.Equal(t, content, file.bytes(), "the pull must still assemble the file it was accounting for")
			assertReported(t, recorded, transfer.PhaseResolving, transfer.PhaseDone)
			assert.Equal(t, tt.want, recorded.last())
		})
	}
}

// TestPullLearnsItsTotalsExactlyOnce pins the one moment a pull's totals
// change, and what they are before it.
//
// A pull cannot know what it is moving until it has decoded the manifest, and
// pretending otherwise would mean either delaying the first snapshot past the
// manifest fetch — the one step a watcher most wants to see retry — or
// guessing. So the resolving snapshots carry zeros, the totals arrive with
// the move into transferring, and they never change again.
func TestPullLearnsItsTotalsExactlyOnce(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")

	blobs, _ := fetchingBlobs(t, newBlobStore(artifact.Parts, content), fetchScript{})

	file := newMemFile(nil)
	sink := mockSink(t, file)
	sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

	policy, _ := testPolicy(t)
	recorded := newSnapshots(t)

	require.NoError(t, transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: flakyManifestReads(t, body),
		Workers:   2,
		Retry:     policy,
		Progress:  recorded.record,
	}))

	all := recorded.all()
	require.GreaterOrEqual(t, len(all), 3, "a retried manifest fetch reports at least an open, a retry, and the end")

	resolving := 0
	for _, snap := range all {
		if snap.Phase != transfer.PhaseResolving {
			break
		}

		assert.Zero(t, snap.TotalBytes, "a pull that has not read the manifest cannot know the file size")
		assert.Zero(t, snap.TotalParts, "a pull that has not read the manifest cannot know the part count")
		resolving++
	}

	require.Equal(t, 2, resolving, "the open and the retry are the resolving snapshots")
	assert.Equal(t, transfer.PhaseTransferring, all[resolving].Phase)
	assert.Equal(t, int64(multiPartSize), all[resolving].TotalBytes, "the totals arrive with the first part moving")
	assert.Equal(t, 3, all[resolving].TotalParts)
	assert.Equal(t, 1, all[resolving].Retries, "the manifest retry is carried into the phase after it")
}

// answeringManifests returns a manifest port that serves body the first time
// it is asked and refuses to be asked twice.
func answeringManifests(t *testing.T, body []byte) *ocimocks.MockManifests {
	t.Helper()

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	return manifests
}

// flakyManifestReads returns a manifest port whose first read reports a
// failure worth repeating and whose second serves body.
func flakyManifestReads(t *testing.T, body []byte) *ocimocks.MockManifests {
	t.Helper()

	var (
		mu       sync.Mutex
		attempts int
	)

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).RunAndReturn(
		func(context.Context) ([]byte, ocispec.Descriptor, error) {
			mu.Lock()
			attempts++
			first := attempts == 1
			mu.Unlock()

			if first {
				return nil, ocispec.Descriptor{}, retry.Transient(errBroken, 0)
			}

			return body, manifestDescriptor(body), nil
		},
	).Maybe()

	return manifests
}
