package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	filemocks "github.com/imgoci/bigoci/internal/file/mocks"
	"github.com/imgoci/bigoci/internal/manifest"
	ocimocks "github.com/imgoci/bigoci/internal/oci/mocks"
	"github.com/imgoci/bigoci/internal/retry"
	"github.com/imgoci/bigoci/internal/transfer"
)

// This file holds the fixtures the retry suites drive transfers with: the
// injected failures, the recording policies, and the scripted ports. The
// shapes every transfer test shares stay in fixtures_test.go; what lives here
// exists to choreograph a failure and read the schedule back.

// The failures the retry tests inject. Their identity is what the assertions
// hang on, so each says which verdict it stands for rather than what went
// wrong: a tagged failure is one some layer diagnosed as worth repeating, and
// a plain one is everything else, which is terminal.
var (
	// errBroken stands for a connection that dropped, tagged by whichever
	// layer watched it drop.
	errBroken = errors.New("the registry hung up")
	// errLast stands for the failure that ends an exhausted run, so a test
	// can prove what comes back is the last failure and not the first.
	errLast = errors.New("the registry hung up for the last time")
	// errRefused stands for a failure nobody classified: the registry saying
	// the request itself is wrong.
	errRefused = errors.New("the registry rejected the request")
	// errTooLarge stands for a part a registry will not accept. Mapping the
	// status that carries it onto a sentinel is the adapter's test; here the
	// only claim is that a terminal failure is attempted exactly once.
	errTooLarge = errors.New("registry returned 413 Request Entity Too Large")
)

// A test that says nothing about retries runs on the zero policy, which is
// the real one, and that is deliberately harmless: a mock returns an untagged
// error, an untagged error is terminal, and a terminal failure comes back
// from the first attempt exactly as the mock reported it. Only a test that
// injects a tagged failure — or one that exercises the short-part case the
// orchestrator tags itself — needs a policy from the helpers below.

// testPolicy returns the policy the retry fixtures run under and the log of
// the waits it took.
//
// Rand halves every ceiling, so the schedule of a four-attempt run is exactly
// 500ms, 1s, and 2s and a test can name it. Sleep records and returns at
// once, so the suite reads a whole backoff schedule off a slice with no clock
// anywhere near it.
func testPolicy(t *testing.T) (retry.Policy, *sleepLog) {
	t.Helper()

	log := newSleepLog()

	return log.policy(), log
}

// interruptedPolicy returns a policy whose waits report err instead of
// completing, standing in for a transfer whose context ended while a worker
// was backing off.
func interruptedPolicy(t *testing.T, err error) (retry.Policy, *sleepLog) {
	t.Helper()

	log := newSleepLog()
	log.interrupt = err

	return log.policy(), log
}

// blockingPolicy returns a policy whose waits block until the transfer's
// context ends and then report its error, which is what the real sleep does.
// It is how a test parks one worker in a backoff while another worker fails.
func blockingPolicy(t *testing.T) (retry.Policy, *sleepLog) {
	t.Helper()

	log := newSleepLog()
	log.hold = true

	return log.policy(), log
}

// noRetry returns a policy of a single attempt: what a transfer did before
// there was a retry policy, for a test about what a failure says rather than
// about how often it is repeated.
func noRetry() retry.Policy {
	return retry.Policy{Attempts: 1}
}

// sleepLog is the record of the waits one transfer took, and the seam that
// takes them. Workers back off from several goroutines at once, so every
// field lives behind the mutex or is fixed before the transfer starts.
type sleepLog struct {
	// mu guards taken.
	mu sync.Mutex
	// taken are the waits Sleep was asked for, in order.
	taken []time.Duration
	// interrupt is what every wait reports instead of completing, nil when
	// the waits complete.
	interrupt error
	// hold makes a wait block until the transfer's context ends.
	hold bool
	// entered closes when the first wait begins, so a test can hold one
	// worker in a backoff until another has provably reached one.
	entered chan struct{}
	// first closes entered once, however many workers back off at once.
	first sync.Once
}

// newSleepLog returns a log whose waits complete at once.
func newSleepLog() *sleepLog {
	return &sleepLog{entered: make(chan struct{})}
}

// policy returns the four-attempt policy this log records the waits of, with
// the default base and cap so a test can talk in the numbers the design fixes.
func (l *sleepLog) policy() retry.Policy {
	return retry.Policy{
		Attempts: retry.DefaultAttempts,
		Base:     retry.DefaultBase,
		Cap:      retry.DefaultCap,
		Sleep:    l.sleep,
		Rand:     halved,
	}
}

// sleep records the wait it was asked for and then does whatever the fixture
// was built to do about it.
func (l *sleepLog) sleep(ctx context.Context, d time.Duration) error {
	l.record(d)

	if l.hold {
		<-ctx.Done()

		return ctx.Err()
	}

	return l.interrupt
}

// record notes one wait and reports that a worker has reached a backoff.
func (l *sleepLog) record(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.taken = append(l.taken, d)
	l.first.Do(func() { close(l.entered) })
}

// waits returns the waits taken so far, in order.
func (l *sleepLog) waits() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.taken)
}

// backingOff closes once some worker has begun a wait.
func (l *sleepLog) backingOff() <-chan struct{} {
	return l.entered
}

// halved is the jitter draw the fixtures run under: half of whatever ceiling
// it is offered, which turns the default backoff into a schedule a test can
// name instead of a range it can only bound.
func halved(n int64) int64 {
	return n / 2
}

// assertOnlyTargetRetried checks the budget stayed with the part that failed:
// however many attempts part target cost, every other part cost at most one
// call. It is the assertion that separates a per-part budget from a
// per-transfer one, which every happy row would otherwise let through.
func assertOnlyTargetRetried(
	t *testing.T,
	parts []manifest.Part,
	target int,
	count func(digest.Digest) int,
	verb string,
) {
	t.Helper()

	for i, part := range parts {
		if i == target {
			continue
		}

		assert.LessOrEqual(t, count(part.Digest), 1,
			"part %d was %s again for a failure that was not its own", i, verb)
	}
}

// readLog records where a push read its source, keyed by offset. The hash
// pass and the workers read at once, so it lives behind a mutex.
type readLog struct {
	// mu guards offsets.
	mu sync.Mutex
	// offsets counts the reads that began at each offset.
	offsets map[int64]int
}

// record notes one read beginning at off.
func (l *readLog) record(off int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.offsets[off]++
}

// at returns how many reads began at off.
func (l *readLog) at(off int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.offsets[off]
}

// countingSource returns a [transfer.Source] double serving content from
// memory, and the log of the offsets it was read at.
//
// The count that matters is how many reads began at a part's first byte. A
// section reader reads its base first, and at the fixture part size one pass
// over a part is a single read, so the reads at part.Offset are exactly one
// per pass: one for the hash pass, and one more for every upload attempt that
// streamed the part. That is how a test proves a retry opened a fresh reader
// over the file instead of re-sending a spent one.
func countingSource(t *testing.T, content []byte) (*filemocks.MockSource, *readLog) {
	t.Helper()

	reads := &readLog{offsets: make(map[int64]int)}

	source := filemocks.NewMockSource(t)
	source.EXPECT().Size().Return(int64(len(content))).Maybe()
	source.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(
		func(p []byte, off int64) (int, error) {
			reads.record(off)

			return bytes.NewReader(content).ReadAt(p, off)
		},
	).Maybe()

	return source, reads
}

// existsAnswer is one answer to an existence check: whether the registry
// holds the blob, or the failure that stopped it saying.
type existsAnswer struct {
	// held is what the check answers when it answers at all.
	held bool
	// err is the failure the check reports instead.
	err error
}

// blobScript is what a scripted [transfer.Blobs] answers with, per digest and
// per call: the nth call for a digest takes the nth entry of its list and the
// last entry repeats, so a run that makes one call too many is still refused
// rather than quietly succeeding. A digest the script does not name gets the
// ordinary answer — not held, and an accepted upload.
type blobScript struct {
	// exists is what each existence check for a digest answers.
	exists map[digest.Digest][]existsAnswer
	// put is what each upload of a digest reports, nil for one the registry
	// accepts.
	put map[digest.Digest][]error
}

// scriptedBlobs returns a [transfer.Blobs] double answering from script, and
// the record of what it was asked to do. The double is the generated mock;
// the script only drives its hooks.
func scriptedBlobs(t *testing.T, script blobScript) (*ocimocks.MockBlobs, *blobCalls) {
	t.Helper()

	calls := newBlobCalls()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest) (bool, error) {
			answers, scripted := script.exists[dgst]
			if !scripted {
				calls.check(dgst)

				return false, nil
			}

			answer := answers[nth(calls.check(dgst), len(answers))]

			return answer.held, answer.err
		},
	).Maybe()
	blobs.EXPECT().
		Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, dgst digest.Digest, size int64, r io.Reader, wire transfer.WireProgress) error { // The bytes are drained whatever the answer turns out to be: a
			// registry that refuses an upload has still read the request body,
			// and a test that skipped the read could not tell a fresh section
			// reader from a spent one.
			content, err := readUpload(r, wire)
			if err != nil {
				// What the adapter really does with a body its transport could
				// not read: the failure comes back tagged, because from where
				// the adapter stands a source that stopped reading looks like
				// a connection that stopped carrying. Discarding the tag when
				// the failure is the source's own is the orchestrator's job,
				// and the double has to hand it the same puzzle. The attempt
				// still counts — an upload that died mid-body was an upload.
				calls.put(dgst, size, content)

				return retry.Transient(fmt.Errorf("PUT %s: %w", dgst, err), 0)
			}

			answers, scripted := script.put[dgst]
			if !scripted {
				calls.put(dgst, size, content)

				return nil
			}

			return answers[nth(calls.put(dgst, size, content), len(answers))]
		}).
		Maybe()

	return blobs, calls
}

// fetchAnswer is one answer to a part fetch: a failure instead of a body, a
// body that stops after a prefix, or the whole blob from a registry that threw
// the byte range away. The zero value serves the rest of the part from the
// store, starting where the fetch asked.
type fetchAnswer struct {
	// err is the failure the fetch reports instead of opening a body.
	err error
	// prefix is what a body serves before it breaks. A row that means the next
	// attempt to carry on from the break passes the part's own bytes, because
	// that is what a registry serves and what the continued digest has to come
	// out of; a row about an attempt that starts the part over passes bytes
	// that are deliberately not the part's, so the overwrite is a byte
	// comparison rather than a digest that says nothing about why.
	prefix []byte
	// breaks is what that body raises once the prefix runs out. [io.EOF] is a
	// body that simply ended before the manifest said it would.
	breaks error
	// ignoreRange makes the answer the whole blob reported as starting at byte
	// zero, whatever offset was asked for: the registry that will not serve a
	// range and sends everything instead.
	ignoreRange bool
}

// fetchScript is what a scripted [transfer.Blobs.Get] answers with, per
// digest and per call, under the same rule [blobScript] follows: the nth call
// takes the nth entry, the last entry repeats, and a digest the script does
// not name is served from the store.
type fetchScript map[digest.Digest][]fetchAnswer

// fetchingBlobs returns a [transfer.Blobs] double whose Get answers from
// script and otherwise from store, and the record of what it was asked for.
//
// Unless a row says otherwise the double is a registry that honors byte
// ranges: it serves the blob from the offset it was given and reports that
// offset back, so a test reads a continuation off the offsets in [blobCalls].
func fetchingBlobs(
	t *testing.T,
	store *blobStore,
	script fetchScript,
) (*ocimocks.MockBlobs, *blobCalls) {
	t.Helper()

	calls := newBlobCalls()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, offset int64) (io.ReadCloser, int64, error) {
			answers, scripted := script[dgst]
			if !scripted {
				calls.get(dgst, offset)

				return store.serve(dgst, offset)
			}

			answer := answers[nth(calls.get(dgst, offset), len(answers))]
			switch {
			case answer.err != nil:
				return nil, 0, answer.err
			case answer.ignoreRange:
				// The whole blob from its first byte, reported as starting
				// there however far into the part the fetch asked to begin.
				return store.serve(dgst, 0)
			case answer.breaks == nil:
				return store.serve(dgst, offset)
			default:
				return store.serveFlaky(answer.prefix, answer.breaks), offset, nil
			}
		},
	).Maybe()

	return blobs, calls
}

// nth returns the index the call-numbered call takes in a script of length
// entries, where the last entry answers every call past the end.
func nth(call, entries int) int {
	return min(call, entries) - 1
}

// blobCalls is what a scripted [transfer.Blobs] was asked to do: how many
// times each digest was checked, uploaded, and fetched, and what arrived
// under it. Workers call from several goroutines at once, so every field
// lives behind the mutex.
type blobCalls struct {
	// mu guards the three maps below.
	mu sync.Mutex
	// checked counts the existence checks each digest got.
	checked map[digest.Digest]int
	// uploaded counts the uploads each digest got, refused ones included.
	uploaded map[digest.Digest]int
	// fetched counts the fetches each digest got.
	fetched map[digest.Digest]int
	// starts holds the offset each fetch of a digest asked to begin at, in the
	// order the fetches were made. It is what a continuation is read off: the
	// second entry of a part that broke is the byte the first attempt reached.
	starts map[digest.Digest][]int64
	// blobs holds what arrived under each digest.
	blobs map[digest.Digest]upload
}

// newBlobCalls returns an empty record.
func newBlobCalls() *blobCalls {
	return &blobCalls{
		checked:  make(map[digest.Digest]int),
		uploaded: make(map[digest.Digest]int),
		fetched:  make(map[digest.Digest]int),
		starts:   make(map[digest.Digest][]int64),
		blobs:    make(map[digest.Digest]upload),
	}
}

// check records one existence check and returns how many dgst has now had.
func (c *blobCalls) check(dgst digest.Digest) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.checked[dgst]++

	return c.checked[dgst]
}

// put records one upload of content under dgst, declared as size bytes, and
// returns how many uploads that digest has now had.
func (c *blobCalls) put(dgst digest.Digest, size int64, content []byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.uploaded[dgst]++
	c.blobs[dgst] = upload{size: size, content: content}

	return c.uploaded[dgst]
}

// get records one fetch of dgst beginning at off and returns how many fetches
// that digest has now had.
func (c *blobCalls) get(dgst digest.Digest, off int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.fetched[dgst]++
	c.starts[dgst] = append(c.starts[dgst], off)

	return c.fetched[dgst]
}

// checks returns how many existence checks dgst got.
func (c *blobCalls) checks(dgst digest.Digest) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.checked[dgst]
}

// puts returns how many uploads dgst got.
func (c *blobCalls) puts(dgst digest.Digest) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.uploaded[dgst]
}

// gets returns how many fetches dgst got.
func (c *blobCalls) gets(dgst digest.Digest) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.fetched[dgst]
}

// offsets returns the byte each fetch of dgst asked to start at, in order.
func (c *blobCalls) offsets(dgst digest.Digest) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.starts[dgst])
}

// blob returns what arrived under dgst, and the zero upload when nothing did.
func (c *blobCalls) blob(dgst digest.Digest) upload {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.blobs[dgst]
}

// acceptingManifests returns a [transfer.Manifests] double that takes
// whatever it is written and answers with the digest of those bytes, for a
// test whose subject is somewhere else. The expectation is optional because a
// push that fails never reaches the manifest.
func acceptingManifests(t testing.TB) *ocimocks.MockManifests {
	t.Helper()

	manifests := ocimocks.NewMockManifests(t)
	manifests.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ string, body []byte) (digest.Digest, error) {
			return digest.FromBytes(body), nil
		},
	).Maybe()

	return manifests
}
