package transfer_test

import (
	"context"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/manifest"
	ocimocks "github.com/imgoci/bigoci/internal/oci/mocks"
	"github.com/imgoci/bigoci/internal/retry"
	"github.com/imgoci/bigoci/internal/transfer"
)

// This file is the push half of the progress accounting: what each way a part
// can be satisfied does to the counters. The rules it pins are the ones a
// consumer reads off a snapshot — that CompletedBytes is the file and
// WireBytes is what the file cost — so every row states both.

// brokenAt is how far into a part the interrupted upload gets before the
// connection drops. It is not a part boundary and not half of one, so a count
// that happened to be rounded to either would fail the row.
const brokenAt = 400

func TestPushProgressAccounting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// blobs builds the registry the push runs against, given the parts of
		// the artifact it is about to write.
		blobs func(t *testing.T, parts []manifest.Part) *ocimocks.MockBlobs
		// manifests builds the manifest port, or is nil for one that takes
		// whatever it is given.
		manifests func(t *testing.T) *ocimocks.MockManifests
		// want is the terminal snapshot, which is the whole account of the
		// push: every counter it ever moved has settled by then.
		want transfer.Snapshot
	}{
		{
			// Case h: the hash pass reads the file once, so HashedBytes is
			// the file whatever else happens, and it is the only counter
			// moving while the first part is still being read.
			name: "a cold push sends every byte once and hashes the file once",
			blobs: func(t *testing.T, _ []manifest.Part) *ocimocks.MockBlobs {
				t.Helper()
				blobs, _ := mockBlobs(t, nil)

				return blobs
			},
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize,
				HashedBytes:    multiPartSize,
			},
		},
		{
			// Case e: a warm re-push. Every part is in place and none of them
			// cost a byte, which is the difference the two counters exist to
			// show — and the file was still read, so HashedBytes is not zero.
			name: "a warm re-push completes every part and sends nothing",
			blobs: func(t *testing.T, parts []manifest.Part) *ocimocks.MockBlobs {
				t.Helper()

				held := make(map[digest.Digest]bool, len(parts))
				for _, part := range parts {
					held[part.Digest] = true
				}
				blobs, _ := mockBlobs(t, held)

				return blobs
			},
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
			// Case g: the bytes of the broken attempt are spent and counted,
			// and the part is credited once. This is the row where the two
			// byte counters must differ.
			name: "an upload that breaks part way through spends the bytes it sent",
			blobs: func(t *testing.T, parts []manifest.Part) *ocimocks.MockBlobs {
				t.Helper()

				return breakingBlobs(t, parts[0].Digest, brokenAt)
			},
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize + brokenAt,
				HashedBytes:    multiPartSize,
				Retries:        1,
			},
		},
		{
			// Case g': the upload landed and the answer was lost. The next
			// attempt finds the part already there and sends nothing, so the
			// wire count is the file — but the part is not skipped, because
			// this transfer did move its bytes. That is why the skip rule is
			// decided across the whole budget and not per attempt.
			name: "an upload whose answer was lost is not a skipped part",
			blobs: func(t *testing.T, parts []manifest.Part) *ocimocks.MockBlobs {
				t.Helper()

				blobs, _ := scriptedBlobs(t, blobScript{
					exists: map[digest.Digest][]existsAnswer{
						parts[0].Digest: {{held: false}, {held: true}},
					},
					put: map[digest.Digest][]error{
						parts[0].Digest: {retry.Transient(errBroken, 0)},
					},
				})

				return blobs
			},
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize,
				HashedBytes:    multiPartSize,
				Retries:        1,
			},
		},
		{
			// Case i: the empty config blob. Its two bytes are not the file
			// and are not counted; that the step happened at all is what the
			// finalizing phase says, and a repeat of it is a retry.
			name: "a config blob that has to be asked about twice moves no counted bytes",
			blobs: func(t *testing.T, _ []manifest.Part) *ocimocks.MockBlobs {
				t.Helper()

				config, _ := manifest.EmptyConfig()
				blobs, _ := scriptedBlobs(t, blobScript{
					exists: map[digest.Digest][]existsAnswer{
						config.Digest: {{err: retry.Transient(errBroken, 0)}, {held: false}},
					},
				})

				return blobs
			},
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize,
				HashedBytes:    multiPartSize,
				Retries:        1,
			},
		},
		{
			// Case i again, at the other end: a push sitting at every byte
			// moved with a manifest that will not write is exactly what a
			// climbing retry count in the finalizing phase describes.
			name: "a manifest write that has to be repeated is a retry and nothing else",
			blobs: func(t *testing.T, _ []manifest.Part) *ocimocks.MockBlobs {
				t.Helper()
				blobs, _ := mockBlobs(t, nil)

				return blobs
			},
			manifests: flakyManifests,
			want: transfer.Snapshot{
				Phase:          transfer.PhaseDone,
				TotalBytes:     multiPartSize,
				TotalParts:     3,
				CompletedBytes: multiPartSize,
				CompletedParts: 3,
				WireBytes:      multiPartSize,
				HashedBytes:    multiPartSize,
				Retries:        1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := fileContent(multiPartSize)
			artifact, _ := artifactFor(t, content, "model.bin")

			manifests := acceptingManifests(t)
			if tt.manifests != nil {
				manifests = tt.manifests(t)
			}

			policy, _ := testPolicy(t)
			recorded := newSnapshots(t)

			_, err := transfer.Push(t.Context(), transfer.PushSpec{
				Source:    mockSource(t, content),
				Blobs:     tt.blobs(t, artifact.Parts),
				Manifests: manifests,
				PartSize:  fixturePartSize,
				Workers:   2,
				Title:     "model.bin",
				Retry:     policy,
				Progress:  recorded.record,
			})
			require.NoError(t, err)

			assertReported(t, recorded, transfer.PhaseTransferring)
			assert.Equal(t, tt.want, recorded.last())
		})
	}
}

// TestPushCreditsADedupeFollowerOnlyWhenItsBlobIsInTheRegistry is case f, and
// the reason the claim ledger holds a count instead of a flag.
//
// A worker that meets a digest another worker is already uploading skips its
// part at once — that has always been true and is what keeps a file of
// identical parts to one upload. What it must not do is report the part
// complete, because the bytes are not in the registry yet: a highly deduped
// file would otherwise read as nearly finished for the whole duration of the
// one upload that is really moving it.
//
// The push is held at two points to prove it. While the twin's upload is
// blocked, nothing at all is credited; the credit for both parts arrives
// together, when the upload that put their bytes in the registry settles.
func TestPushCreditsADedupeFollowerOnlyWhenItsBlobIsInTheRegistry(t *testing.T) {
	t.Parallel()

	content := twinContent()
	artifact, _ := artifactFor(t, content, "twins.bin")
	twin, tail := artifact.Parts[0].Digest, artifact.Parts[2].Digest
	require.Equal(t, twin, artifact.Parts[1].Digest, "the fixture's first two parts must share a digest")

	gate := newUploadGate(twin, tail)
	recorded := newSnapshots(t)

	pushed := make(chan error, 1)
	go func() {
		_, err := transfer.Push(t.Context(), transfer.PushSpec{
			Source:    mockSource(t, content),
			Blobs:     gate.blobs(t),
			Manifests: acceptingManifests(t),
			PartSize:  fixturePartSize,
			Workers:   2,
			Title:     "twins.bin",
			Progress:  recorded.record,
		})
		pushed <- err
	}()

	// The tail is the second part its worker took, so an upload of it that
	// has started proves the twin beside it was already skipped.
	awaitClosed(t, gate.tailEntered, "the tail part's upload never started")
	for i, snap := range recorded.all() {
		assert.Zero(t, snap.CompletedParts, "snapshot %d credited a part while every upload was still running", i)
	}

	close(gate.tailRelease)
	close(gate.twinRelease)
	require.NoError(t, <-pushed)

	assertReported(t, recorded, transfer.PhaseTransferring)
	assert.Equal(t, transfer.Snapshot{
		Phase:          transfer.PhaseDone,
		TotalBytes:     int64(len(content)),
		TotalParts:     3,
		CompletedBytes: int64(len(content)),
		CompletedParts: 3,
		SkippedParts:   1,
		WireBytes:      2 * int64(fixturePartSize),
		HashedBytes:    int64(len(content)),
	}, recorded.last())
}

// TestEmptyArtifactReportsDoneWithZeroTotals covers the artifact whose
// fraction is zero for its whole life.
//
// An empty file is a real artifact of one empty part, so it reaches
// [transfer.PhaseDone] with a completed part and not a single byte. Nothing
// derived from the byte counters can tell that apart from a transfer that has
// not started, which is exactly why the phase and not the counters is what
// says a transfer finished.
func TestEmptyArtifactReportsDoneWithZeroTotals(t *testing.T) {
	t.Parallel()

	t.Run("push", func(t *testing.T) {
		t.Parallel()

		blobs, _ := mockBlobs(t, nil)
		recorded := newSnapshots(t)

		_, err := transfer.Push(t.Context(), transfer.PushSpec{
			Source:    mockSource(t, nil),
			Blobs:     blobs,
			Manifests: acceptingManifests(t),
			PartSize:  fixturePartSize,
			Workers:   2,
			Title:     "empty.bin",
			Progress:  recorded.record,
		})
		require.NoError(t, err)

		assertReported(t, recorded, transfer.PhaseTransferring)
		assertEmptyArtifact(t, recorded)
	})

	t.Run("pull", func(t *testing.T) {
		t.Parallel()

		artifact, body := artifactFor(t, nil, "empty.bin")

		manifests := ocimocks.NewMockManifests(t)
		manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

		file := newMemFile(nil)
		sink := mockSink(t, file)
		sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

		recorded := newSnapshots(t)

		require.NoError(t, transfer.Pull(t.Context(), transfer.PullSpec{
			Sink:      sink,
			Blobs:     mockBlobsServing(t, newBlobStore(artifact.Parts, nil)),
			Manifests: manifests,
			Workers:   2,
			Progress:  recorded.record,
		}))

		assertReported(t, recorded, transfer.PhaseResolving)
		assertEmptyArtifact(t, recorded)
	})
}

// assertEmptyArtifact checks the shape every snapshot of an empty transfer
// has: no bytes anywhere, one part, and a completed part by the end.
func assertEmptyArtifact(t *testing.T, recorded *snapshots) {
	t.Helper()

	for i, snap := range recorded.all() {
		assert.Zero(t, snap.TotalBytes, "snapshot %d claims an empty file has bytes", i)
		assert.Zero(t, snap.CompletedBytes, "snapshot %d completed bytes an empty file does not have", i)
	}

	last := recorded.last()
	assert.Equal(t, 1, last.TotalParts, "an empty file is still one part")
	assert.Equal(t, 1, last.CompletedParts, "the empty part must complete")
}

// twinContent returns a file whose first two parts hold identical bytes and
// whose third holds bytes neither of them does.
//
// Two identical parts are one blob in the registry and two parts in the
// manifest, which is the whole of the dedupe case. The third part is the
// fixture's clock: it is the second part its worker takes, so an upload of it
// proves the twin beside it has already been skipped.
func twinContent() []byte {
	block := fileContent(int64(fixturePartSize))

	return slices.Concat(block, block, garbage(int(fixturePartSize)))
}

// uploadGate is a registry whose uploads of two named digests stop until a
// test lets them through.
type uploadGate struct {
	// twin is the digest two parts of the fixture share.
	twin digest.Digest
	// tail is the digest of the part that shares with nothing.
	tail digest.Digest
	// twinEntered closes when the twin's upload has read its bytes and is
	// about to answer.
	twinEntered chan struct{}
	// twinRelease lets that upload answer.
	twinRelease chan struct{}
	// tailEntered closes when the tail's upload has read its bytes.
	tailEntered chan struct{}
	// tailRelease lets that upload answer.
	tailRelease chan struct{}
}

// newUploadGate returns a gate holding both digests closed.
func newUploadGate(twin, tail digest.Digest) *uploadGate {
	return &uploadGate{
		twin:        twin,
		tail:        tail,
		twinEntered: make(chan struct{}),
		twinRelease: make(chan struct{}),
		tailEntered: make(chan struct{}),
		tailRelease: make(chan struct{}),
	}
}

// blobs returns the [transfer.Blobs] double the gate drives. Everything it is
// not holding — the empty config blob — passes straight through.
func (g *uploadGate) blobs(t *testing.T) *ocimocks.MockBlobs {
	t.Helper()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	blobs.EXPECT().
		Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, dgst digest.Digest, _ int64, r io.Reader, wire transfer.WireProgress) error {
			if _, err := readUpload(r, wire); err != nil {
				return err
			}

			switch dgst {
			case g.twin:
				close(g.twinEntered)
				<-g.twinRelease
			case g.tail:
				close(g.tailEntered)
				<-g.tailRelease
			}

			return nil
		}).
		Maybe()

	return blobs
}

// breakingBlobs returns a registry whose first upload of target reads take
// bytes off the reader and then reports a connection that dropped, and which
// takes every other upload whole.
//
// Reading part of the body before failing is what makes it the case worth
// pinning: those bytes really did cross the boundary, and an accounting that
// only counted successful uploads would lose them.
func breakingBlobs(t *testing.T, target digest.Digest, take int64) *ocimocks.MockBlobs {
	t.Helper()

	var (
		mu       sync.Mutex
		attempts int
	)

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	blobs.EXPECT().
		Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, dgst digest.Digest, _ int64, r io.Reader, wire transfer.WireProgress) error {
			mu.Lock()
			if dgst == target {
				attempts++
			}
			first := dgst == target && attempts == 1
			mu.Unlock()

			if first {
				n, err := io.ReadFull(r, make([]byte, take))
				if wire != nil {
					wire(int64(n))
				}
				if err != nil {
					return err
				}

				return retry.Transient(errBroken, 0)
			}

			_, err := readUpload(r, wire)

			return err
		}).
		Maybe()

	return blobs
}

// flakyManifests returns a manifest port whose first write reports a failure
// worth repeating and whose second takes the bytes.
func flakyManifests(t *testing.T) *ocimocks.MockManifests {
	t.Helper()

	var (
		mu       sync.Mutex
		attempts int
	)

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ string, body []byte) (digest.Digest, error) {
			mu.Lock()
			attempts++
			first := attempts == 1
			mu.Unlock()

			if first {
				return "", retry.Transient(errBroken, 0)
			}

			return digest.FromBytes(body), nil
		},
	).Maybe()

	return manifests
}

// awaitClosed waits for ch and fails the test with why if it takes longer
// than a bug would.
func awaitClosed(t *testing.T, ch <-chan struct{}, why string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(gateTimeout):
		require.FailNow(t, why)
	}
}
