package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	filemocks "github.com/componere/bigoci/internal/file/mocks"
	"github.com/componere/bigoci/internal/manifest"
	ocimocks "github.com/componere/bigoci/internal/oci/mocks"
	"github.com/componere/bigoci/internal/plan"
)

// The sizes every fixture is built from. They are tiny on purpose: the
// orchestrator treats a 1000-byte part exactly like a 512 MiB one, and small
// parts keep the assertions readable.
const (
	// fixturePartSize is the part size the fixtures split at.
	fixturePartSize plan.PartSize = 1000
	// multiPartSize splits into three parts, the last one short.
	multiPartSize = 2500
	// exactMultipleSize splits into two full parts with no short tail.
	exactMultipleSize = 2000
	// singlePartSize is below the part size and splits into one part.
	singlePartSize = 400
)

// contentSeed seeds the generator behind [fileContent], so every run of every
// test moves the same bytes.
const contentSeed = 0x6269676f6369

// fileContent returns size bytes of deterministic pseudo-random content.
//
// The content is pseudo-random rather than a pattern so that a part written at
// the wrong offset, or two parts swapped, cannot pass a byte comparison by
// accident.
func fileContent(size int64) []byte {
	data := make([]byte, size)

	source := rand.New(rand.NewPCG(contentSeed, contentSeed))
	for i := range data {
		data[i] = byte(source.Uint32())
	}

	return data
}

// splitParts returns the parts a transfer of content at [fixturePartSize]
// covers, in file order.
//
// It applies the split rule directly instead of calling the plan package, so a
// test states its own expectation rather than borrowing the one the code under
// test works from.
func splitParts(t *testing.T, content []byte) []manifest.Part {
	t.Helper()

	size := int64(len(content))

	var parts []manifest.Part
	for offset := int64(0); ; offset += int64(fixturePartSize) {
		length := min(int64(fixturePartSize), size-offset)
		parts = append(parts, manifest.Part{
			Digest: digest.FromBytes(content[offset : offset+length]),
			Size:   length,
		})

		if offset+length >= size {
			return parts
		}
	}
}

// artifactFor returns the artifact a push of content at [fixturePartSize]
// writes, together with the canonical manifest bytes that artifact encodes to.
func artifactFor(t *testing.T, content []byte, title string) (manifest.Artifact, []byte) {
	t.Helper()

	artifact := manifest.Artifact{
		FileDigest: digest.FromBytes(content),
		FileSize:   int64(len(content)),
		PartSize:   fixturePartSize,
		Title:      title,
		Parts:      splitParts(t, content),
	}

	body, err := manifest.Encode(artifact)
	require.NoError(t, err)

	return artifact, body
}

// mockSource returns a [transfer.Source] double serving content from memory.
//
// ReadAt is answered by a fresh [bytes.Reader] per call, so the hash pass and
// every worker can read the same content at once. Both expectations are
// optional because a push of an empty file reads nothing and a push that fails
// its spec check reads nothing either.
func mockSource(t *testing.T, content []byte) *filemocks.MockSource {
	t.Helper()

	source := filemocks.NewMockSource(t)
	source.EXPECT().Size().Return(int64(len(content))).Maybe()
	source.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(
		func(p []byte, off int64) (int, error) {
			return bytes.NewReader(content).ReadAt(p, off)
		},
	).Maybe()

	return source
}

// upload is one blob a push handed to Blobs.Put: the length it declared and
// the bytes it streamed.
type upload struct {
	// size is the length the push declared for the blob.
	size int64
	// content is what the push actually streamed under that digest.
	content []byte
}

// uploads records the blobs a push wrote, keyed by digest, and the order the
// uploads arrived in. Workers call Put from several goroutines at once, so
// every field lives behind the mutex.
type uploads struct {
	// mu guards the two fields below.
	mu sync.Mutex
	// blobs holds what was uploaded under each digest.
	blobs map[digest.Digest]upload
	// order is the digests in the order Put was called with them.
	order []digest.Digest
}

// newUploads returns an empty recorder.
func newUploads() *uploads {
	return &uploads{blobs: make(map[digest.Digest]upload)}
}

// record drains r into the recorder under dgst.
//
// It reads the whole blob because a test compares the bytes a push streamed
// against the file it was given. The orchestrator itself never holds a part in
// memory; the mock does, because the fixtures are kilobytes.
func (u *uploads) record(dgst digest.Digest, size int64, r io.Reader) error {
	content, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	u.blobs[dgst] = upload{size: size, content: content}
	u.order = append(u.order, dgst)

	return nil
}

// blob returns what was uploaded under dgst, and the zero upload when nothing
// was.
func (u *uploads) blob(dgst digest.Digest) upload {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.blobs[dgst]
}

// digests returns the digests uploaded so far, in the order they were sent.
func (u *uploads) digests() []digest.Digest {
	u.mu.Lock()
	defer u.mu.Unlock()

	return slices.Clone(u.order)
}

// mockBlobs returns a [transfer.Blobs] double for a push: Exists answers from
// held, and Put records what it was handed.
//
// Both expectations are optional, because a push that fails early may reach
// neither.
func mockBlobs(t *testing.T, held map[digest.Digest]bool) (*ocimocks.MockBlobs, *uploads) {
	t.Helper()

	recorded := newUploads()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest) (bool, error) {
			return held[dgst], nil
		},
	).Maybe()
	blobs.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, size int64, r io.Reader) error {
			return recorded.record(dgst, size, r)
		},
	).Maybe()

	return blobs, recorded
}

// blobStore serves part bodies to a mocked Blobs.Get. It counts the bodies it
// handed out against the ones the puller closed, so a test can prove that no
// blob body was left open, including on the paths where a part is rejected.
type blobStore struct {
	// mu guards the three fields below.
	mu sync.Mutex
	// bodies is the content served for each digest. A test rewrites an entry
	// before the pull starts to serve corrupt, short, or long content.
	bodies map[digest.Digest][]byte
	// served counts the bodies handed out.
	served int
	// closed counts the bodies the puller closed.
	closed int
}

// newBlobStore returns a store serving each part's range of content.
func newBlobStore(parts []manifest.Part, content []byte) *blobStore {
	bodies := make(map[digest.Digest][]byte, len(parts))

	offset := int64(0)
	for _, part := range parts {
		bodies[part.Digest] = content[offset : offset+part.Size]
		offset += part.Size
	}

	return &blobStore{bodies: bodies}
}

// serve returns the body registered for dgst, which counts as open until the
// caller closes it.
func (s *blobStore) serve(dgst digest.Digest) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body, ok := s.bodies[dgst]
	if !ok {
		return nil, fmt.Errorf("no blob %s in the store", dgst)
	}
	s.served++

	return &blobBody{store: s, reader: bytes.NewReader(body)}, nil
}

// serveFlaky returns a body that serves prefix and then raises breaks,
// counted against the store's closes like any other body it handed out. It
// stands in for a registry that hangs up part way through a part.
func (s *blobStore) serveFlaky(prefix []byte, breaks error) io.ReadCloser {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.served++

	return &blobBody{store: s, reader: bytes.NewReader(prefix), fail: breaks}
}

// counts returns how many bodies were served and how many of those were
// closed.
func (s *blobStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.served, s.closed
}

// close records that one served body was closed.
func (s *blobStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed++
}

// blobBody is one blob body a [blobStore] handed out. It reports its own close
// back to the store.
type blobBody struct {
	// store is where the close is recorded.
	store *blobStore
	// reader holds the bytes still to be read.
	reader *bytes.Reader
	// fail is what the body raises once its bytes run out, nil for a body
	// that simply ends.
	fail error
}

// Read reads from the body, raising the failure it was built with in place of
// the end of its bytes.
func (b *blobBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if b.fail != nil && errors.Is(err, io.EOF) {
		return n, b.fail
	}

	return n, err
}

// Close records the close with the store. It never fails.
func (b *blobBody) Close() error {
	b.store.close()

	return nil
}

// mockBlobsServing returns a [transfer.Blobs] double whose Get is answered
// from store. The expectation is optional because a pull that stops at the
// manifest fetches nothing.
func mockBlobsServing(t *testing.T, store *blobStore) *ocimocks.MockBlobs {
	t.Helper()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, offset int64) (io.ReadCloser, int64, error) {
			assert.Zero(t, offset, "this phase fetches whole parts and never asks for a byte range")

			return wholeBlob(store.serve(dgst))
		},
	).Maybe()

	return blobs
}

// wholeBlob turns a body a [blobStore] handed out into the answer a mocked
// Blobs.Get gives: a stream starting at the blob's first byte, which is the
// only start this phase ever asks for or accepts.
func wholeBlob(body io.ReadCloser, err error) (io.ReadCloser, int64, error) {
	return body, 0, err
}

// memFile is the destination a mocked [transfer.Sink] assembles a pull into.
//
// It is storage, not an implementation of the port: the generated mock's hooks
// call these methods, and the mock is what the orchestrator sees. The mutex is
// here because a pull writes parts from several goroutines at once.
type memFile struct {
	// mu guards the two fields below.
	mu sync.Mutex
	// data is the file's content, sized by the pull's truncate.
	data []byte
	// commits counts how many times the pull published the file.
	commits int
}

// truncate sizes the file, which is what a pull does before it fetches
// anything.
func (f *memFile) truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.data = make([]byte, size)

	return nil
}

// writeAt copies p into the file at off, refusing a write that would fall
// outside the size the pull truncated to.
func (f *memFile) writeAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if off < 0 || off+int64(len(p)) > int64(len(f.data)) {
		return 0, fmt.Errorf("write of %d bytes at offset %d is outside the %d-byte file", len(p), off, len(f.data))
	}

	return copy(f.data[off:], p), nil
}

// commit records that the pull published the file.
func (f *memFile) commit() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.commits++

	return nil
}

// bytes returns a copy of the file's content.
func (f *memFile) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.data)
}

// commitCount returns how many times the file was committed.
func (f *memFile) commitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.commits
}

// mockSink returns a [transfer.Sink] double backed by file.
//
// Commit is deliberately left unwired: a test that expects a pull to succeed
// adds the expectation itself, and one that expects a failure leaves it off,
// so a pull that publishes a file it should not have fails loudly.
func mockSink(t *testing.T, file *memFile) *filemocks.MockSink {
	t.Helper()

	sink := filemocks.NewMockSink(t)
	sink.EXPECT().Truncate(mock.Anything).RunAndReturn(file.truncate).Maybe()
	sink.EXPECT().WriteAt(mock.Anything, mock.Anything).RunAndReturn(file.writeAt).Maybe()

	return sink
}
