package transfer_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	ocimocks "github.com/componere/bigoci/internal/oci/mocks"
	"github.com/componere/bigoci/internal/retry"
	"github.com/componere/bigoci/internal/transfer"
)

// This file is about what happens inside one part once its fetch has started:
// where the attempt after a break asks to begin, how many bytes that costs,
// and which failures are worth the budget. Which parts get fetched at all is
// next door, in resume_test.go.
//
// Every row breaks [retriedPart] of a three-part file, so the parts around it
// prove that a continuation stays inside the part it belongs to. The counted
// evidence is the same three numbers everywhere: the offsets the fetches asked
// to start at, the waits between them, and the bytes the registry handed out.
// The last is the sharpest — a continuation that quietly refetched what it
// already held would move the same bytes twice and say so.

func TestPullContinuesAPartMidStream(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)

	tests := []struct {
		name string
		// answers is what the registry does to the part being broken, one
		// entry per fetch.
		answers []fetchAnswer
		// wantOffsets is the byte each of those fetches asked to start at.
		wantOffsets []int64
		// wantWaits is the backoff schedule the breaks cost.
		wantWaits []time.Duration
	}{
		{
			name: "a body that dies at 40% is continued from the byte it reached",
			answers: []fetchAnswer{
				{prefix: partBytes(content, 0, 400), breaks: retry.Transient(errBroken, 0)},
				{},
			},
			wantOffsets: []int64{0, 400},
			wantWaits:   []time.Duration{500 * time.Millisecond},
		},
		{
			// The second body is opened at byte 400 and breaks 300 bytes into
			// what it is serving. A continuation that could only be built on a
			// whole-blob body would go wrong here, or start over.
			name: "a body opened part way in that breaks mid-read is continued again",
			answers: []fetchAnswer{
				{prefix: partBytes(content, 0, 400), breaks: retry.Transient(errBroken, 0)},
				{prefix: partBytes(content, 400, 700), breaks: retry.Transient(errBroken, 0)},
				{},
			},
			wantOffsets: []int64{0, 400, 700},
			wantWaits:   []time.Duration{500 * time.Millisecond, time.Second},
		},
		{
			// A registry may answer an open-ended range with fewer bytes than
			// the range asked for and still be telling the truth — a CDN caps
			// them — so the body ends cleanly rather than breaking. The count
			// is the only thing that notices, and the attempt after it carries
			// on from the new total.
			name: "a ranged body that ends cleanly short of the remainder is continued from the new total",
			answers: []fetchAnswer{
				{prefix: partBytes(content, 0, 400), breaks: retry.Transient(errBroken, 0)},
				{prefix: partBytes(content, 400, 700), breaks: io.EOF},
				{},
			},
			wantOffsets: []int64{0, 400, 700},
			wantWaits:   []time.Duration{500 * time.Millisecond, time.Second},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			artifact, body := artifactFor(t, content, "model.bin")
			target := artifact.Parts[retriedPart]

			manifests := ocimocks.NewMockManifests(t)
			manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

			store := newBlobStore(artifact.Parts, content)
			blobs, calls := fetchingBlobs(t, store, fetchScript{target.Digest: tt.answers})
			policy, sleeps := testPolicy(t)

			file := newMemFile(nil)
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

			assert.Equal(t, tt.wantOffsets, calls.offsets(target.Digest), "the byte each fetch asked to start at")
			assert.Equal(t, tt.wantWaits, sleeps.waits(), "the waits between attempts, in order")
			assert.Equal(t, int64(multiPartSize), store.bytesServed(),
				"a continued part moves every byte exactly once, so the registry serves the file's length and no more")

			assert.Equal(t, content, file.bytes())
			assert.Equal(t, 1, file.commitCount())
			assertOnlyTargetRetried(t, artifact.Parts, retriedPart, calls.gets, "fetched")

			served, closed := store.counts()
			assert.Equal(t, served, closed, "every blob body must be closed, including a broken attempt's")
		})
	}
}

// TestPullContinuesTheRefetchOfAResumedPart crosses the two halves of resume
// that every other test exercises alone: a partial file schedules a refetch,
// and that refetch breaks mid-stream. The verify must run once, before the
// attempts, and never again between them — a verify that re-ran inside the
// retry budget would hash disk bytes into a hasher that already holds wire
// bytes and turn a healthy continuation into a false digest mismatch. This is
// the likeliest field shape there is: a killed pull, rerun, on the same flaky
// link that killed it.
func TestPullContinuesTheRefetchOfAResumedPart(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")
	target := artifact.Parts[retriedPart]

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	store := newBlobStore(artifact.Parts, content)
	blobs, calls := fetchingBlobs(t, store, fetchScript{target.Digest: {
		{prefix: partBytes(content, 0, 400), breaks: retry.Transient(errBroken, 0)},
		{},
	}})
	policy, sleeps := testPolicy(t)

	partial := slices.Clone(content)
	partial[int64(fixturePartSize)+corruptedByte] ^= 0xFF

	file := newMemFile(partial)
	sink := mockSink(t, file)
	sink.EXPECT().Commit().RunAndReturn(file.commit).Once()

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   2,
		Retry:     policy,
	})
	require.NoError(t, err)

	assert.Equal(t, []int{retriedPart}, fetchedParts(artifact.Parts, calls),
		"only the part the partial file spoiled is fetched")
	assert.Equal(t, []int64{0, 400}, calls.offsets(target.Digest),
		"the refetch of a resumed part continues like any other")
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, sleeps.waits())
	assert.Equal(t, content, file.bytes())
	assert.Equal(t, 1, file.commitCount())
}

// TestPullRefusesAnOverlongBlobOnAContinuedAttempt pins the boundary between a
// continuation's limit and the whole part's. A continued attempt must measure
// the one-extra-byte probe against the bytes it has left, not against the
// part: a limit sized to the part would swallow an over-long blob's surplus
// into the copy, report it as a truncation, and spend the budget retrying a
// registry that serves content the manifest does not describe.
func TestPullRefusesAnOverlongBlobOnAContinuedAttempt(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")
	target := artifact.Parts[retriedPart]

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	store := newBlobStore(artifact.Parts, content)
	store.bodies[target.Digest] = append(slices.Clone(store.bodies[target.Digest]), 0x00)
	blobs, _ := fetchingBlobs(t, store, fetchScript{target.Digest: {
		{prefix: partBytes(content, 0, 400), breaks: retry.Transient(errBroken, 0)},
		{},
	}})
	policy, sleeps := testPolicy(t)

	file := newMemFile(nil)
	sink := mockSink(t, file)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   len(artifact.Parts),
		Retry:     policy,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is longer than its declared size")

	assert.Equal(t, []time.Duration{500 * time.Millisecond}, sleeps.waits(),
		"the only wait is the one the break cost; too much content is not retried")
	sink.AssertNotCalled(t, "Commit")
	assert.Zero(t, file.commitCount())
}

// TestPullRestartsAPartWhenTheRegistryIgnoresTheRange is the free fallback the
// port's reported offset buys. A registry that will not serve a range answers
// with the whole blob instead, and the attempt that asked for a continuation
// consumes that body as a fetch from byte zero: no error, no second request,
// and no attempt spent, which is why the only wait recorded is the one the
// original break cost.
//
// The bytes the broken body served are deliberately not the part's own, so the
// restart writing over them is a byte comparison at the end rather than a
// digest that says nothing about why.
func TestPullRestartsAPartWhenTheRegistryIgnoresTheRange(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")
	target := artifact.Parts[retriedPart]

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	store := newBlobStore(artifact.Parts, content)
	blobs, calls := fetchingBlobs(t, store, fetchScript{target.Digest: {
		{prefix: garbage(400), breaks: retry.Transient(errBroken, 0)},
		{ignoreRange: true},
	}})
	policy, sleeps := testPolicy(t)

	file := newMemFile(nil)
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

	assert.Equal(t, []int64{0, 400}, calls.offsets(target.Digest),
		"the attempt that met the whole blob asked once and consumed what it was given")
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, sleeps.waits(),
		"a range the registry ignored costs no attempt, so it costs no wait")
	assert.Equal(t, int64(multiPartSize)+400, store.bytesServed(),
		"restarting the part is what the 400 bytes of the broken attempt cost")

	assert.Equal(t, content, file.bytes(), "the restarted part is written over what the broken attempt left")
	assert.Equal(t, 1, file.commitCount())

	served, closed := store.counts()
	assert.Equal(t, served, closed, "every blob body must be closed")
}

// TestPullRestartsAPartWhoseLastChunkArrivedWithAFailure pins the boundary
// case that a naive byte counter gets wrong. A body can deliver the last chunk
// of a part and the failure that ends it in one breath, which leaves nothing
// to continue from: a range starting at the end of the part is a range no
// registry can satisfy, and asking for one would turn a retry into a 416. The
// attempt asks for the whole part instead.
func TestPullRestartsAPartWhoseLastChunkArrivedWithAFailure(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")
	target := artifact.Parts[retriedPart]

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	store := newBlobStore(artifact.Parts, content)
	blobs, calls := fetchingBlobs(t, store, fetchScript{target.Digest: {
		// Every byte of the part, then the failure instead of the end of the
		// stream: the copy is already satisfied, so the break surfaces on the
		// read that proves the blob is no longer than the manifest says.
		{prefix: partBytes(content, 0, int64(fixturePartSize)), breaks: retry.Transient(errBroken, 0)},
		{},
	}})
	policy, sleeps := testPolicy(t)

	file := newMemFile(nil)
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

	assert.Equal(t, []int64{0, 0}, calls.offsets(target.Digest),
		"the second attempt asks for the whole part, never for the byte after its last one")
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, sleeps.waits())
	assert.Equal(t, content, file.bytes())
	assert.Equal(t, 1, file.commitCount())
}

// TestPullBudgetsAContinuedPartLikeAnyOther is the rule that keeps a part's
// budget finite. Progress is not a reason to hand out another attempt: a
// budget that reset whenever bytes moved would let a link that drops every few
// hundred bytes retry a part forever.
func TestPullBudgetsAContinuedPartLikeAnyOther(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	artifact, body := artifactFor(t, content, "model.bin")
	target := artifact.Parts[retriedPart]

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

	store := newBlobStore(artifact.Parts, content)
	blobs, calls := fetchingBlobs(t, store, fetchScript{target.Digest: {
		{prefix: partBytes(content, 0, 250), breaks: retry.Transient(errBroken, 0)},
		{prefix: partBytes(content, 250, 500), breaks: retry.Transient(errBroken, 0)},
		{prefix: partBytes(content, 500, 750), breaks: retry.Transient(errBroken, 0)},
		{err: retry.Transient(errLast, 0)},
	}})
	policy, sleeps := testPolicy(t)

	file := newMemFile(nil)
	sink := mockSink(t, file)

	err := transfer.Pull(t.Context(), transfer.PullSpec{
		Sink:      sink,
		Blobs:     blobs,
		Manifests: manifests,
		Workers:   len(artifact.Parts),
		Retry:     policy,
	})
	require.ErrorIs(t, err, errLast, "the failure that ended the pull stays reachable")
	require.ErrorContains(t, err, "after 4 attempts", "three quarters of a part is still four attempts")
	require.ErrorContains(t, err, "fetch part 1")

	assert.Equal(t, retry.DefaultAttempts, calls.gets(target.Digest))
	assert.Equal(t, []int64{0, 250, 500, 750}, calls.offsets(target.Digest),
		"every attempt carried on from the one before it")
	assert.Equal(t, []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}, sleeps.waits())

	sink.AssertNotCalled(t, "Commit")
	assert.Zero(t, file.commitCount(), "a pull that ran out of attempts publishes nothing")
}

// TestPullReportsARegistryBlobShorterThanTheManifest covers the two ways a
// registry says it holds less of a part than the manifest describes. Neither
// is a transfer that broke, and neither is worth pretending otherwise: the
// refusal ends the pull at once, and the body that keeps stopping early spends
// the part's budget and then reports how much of the part ever arrived.
func TestPullReportsARegistryBlobShorterThanTheManifest(t *testing.T) {
	t.Parallel()

	content := fileContent(multiPartSize)
	unsatisfiable := errors.New("GET /v2/team/model/blobs/sha256:...: 416 Requested Range Not Satisfiable")

	tests := []struct {
		name string
		// answers is what the registry does to the part being broken.
		answers []fetchAnswer
		// wantOffsets is the byte each fetch asked to start at.
		wantOffsets []int64
		// wantWaits is the backoff schedule the run took.
		wantWaits []time.Duration
		// wantErr is the failure that must stay reachable, if any.
		wantErr error
		// wantSays are the phrases the failure must carry.
		wantSays []string
		// wantSilent are the phrases it must not.
		wantSilent []string
	}{
		{
			name: "a range the registry refuses ends the pull at once",
			answers: []fetchAnswer{
				{prefix: partBytes(content, 0, 400), breaks: retry.Transient(errBroken, 0)},
				{err: unsatisfiable},
			},
			wantOffsets: []int64{0, 400},
			wantWaits:   []time.Duration{500 * time.Millisecond},
			wantErr:     unsatisfiable,
			wantSays:    []string{"fetch part 1", "416"},
			wantSilent:  []string{"after 4 attempts"},
		},
		{
			name: "a body that keeps ending short spends the part's budget and says how far it got",
			answers: []fetchAnswer{
				{prefix: partBytes(content, 0, 400), breaks: io.EOF},
				{prefix: partBytes(content, 400, 700), breaks: io.EOF},
				{prefix: partBytes(content, 700, 900), breaks: io.EOF},
				{prefix: partBytes(content, 900, 950), breaks: io.EOF},
			},
			wantOffsets: []int64{0, 400, 700, 900},
			wantWaits:   []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second},
			wantSays: []string{
				"after 4 attempts",
				"part 1 ended before its declared size",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			artifact, body := artifactFor(t, content, "model.bin")
			target := artifact.Parts[retriedPart]

			manifests := ocimocks.NewMockManifests(t)
			manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Once()

			store := newBlobStore(artifact.Parts, content)
			blobs, calls := fetchingBlobs(t, store, fetchScript{target.Digest: tt.answers})
			policy, sleeps := testPolicy(t)

			file := newMemFile(nil)
			sink := mockSink(t, file)

			err := transfer.Pull(t.Context(), transfer.PullSpec{
				Sink:      sink,
				Blobs:     blobs,
				Manifests: manifests,
				Workers:   len(artifact.Parts),
				Retry:     policy,
			})
			require.Error(t, err)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			}
			for _, says := range tt.wantSays {
				assert.Contains(t, err.Error(), says)
			}
			for _, silent := range tt.wantSilent {
				assert.NotContains(t, err.Error(), silent, "a failure attempted once says nothing about attempts")
			}

			assert.Equal(t, tt.wantOffsets, calls.offsets(target.Digest))
			assert.Equal(t, tt.wantWaits, sleeps.waits())

			sink.AssertNotCalled(t, "Commit")
			assert.Zero(t, file.commitCount())

			served, closed := store.counts()
			assert.Equal(t, served, closed, "every blob body must be closed")
		})
	}
}

// garbage returns bytes no part of any fixture holds, for a row whose point is
// that an attempt wrote over what an earlier one left.
func garbage(n int) []byte {
	return bytes.Repeat([]byte{0xAA}, n)
}
