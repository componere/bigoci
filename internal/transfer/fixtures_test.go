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
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	filemocks "github.com/imgoci/bigoci/internal/file/mocks"
	"github.com/imgoci/bigoci/internal/manifest"
	ocimocks "github.com/imgoci/bigoci/internal/oci/mocks"
	"github.com/imgoci/bigoci/internal/plan"
	"github.com/imgoci/bigoci/internal/transfer"
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
func splitParts(t testing.TB, content []byte) []manifest.Part {
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
func artifactFor(t testing.TB, content []byte, title string) (manifest.Artifact, []byte) {
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
func mockSource(t testing.TB, content []byte) *filemocks.MockSource {
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

// readUpload drains r and reports the bytes the mock transport consumed.
func readUpload(r io.Reader, wire transfer.WireProgress) ([]byte, error) {
	content, err := io.ReadAll(r)
	if wire != nil {
		wire(int64(len(content)))
	}

	return content, err
}

// readDeclared consumes exactly size bytes from r, the way a Content-Length
// transport reads an upload body, and never asks for EOF.
func readDeclared(r io.Reader, size int64, wire transfer.WireProgress) ([]byte, error) {
	content := make([]byte, size)
	n, err := io.ReadFull(r, content)
	if wire != nil && n > 0 {
		wire(int64(n))
	}
	if err != nil {
		return content[:n], err
	}

	return content, nil
}

// record drains r into the recorder under dgst.
//
// It reads the whole blob because a test compares the bytes a push streamed
// against the file it was given. The orchestrator itself never holds a part in
// memory; the mock does, because the fixtures are kilobytes.
func (u *uploads) record(
	dgst digest.Digest,
	size int64,
	r io.Reader,
	wire transfer.WireProgress,
) error {
	content, err := readUpload(r, wire)
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
func mockBlobs(t testing.TB, held map[digest.Digest]bool) (*ocimocks.MockBlobs, *uploads) {
	t.Helper()

	recorded := newUploads()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Exists(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest) (bool, error) {
			return held[dgst], nil
		},
	).Maybe()
	blobs.EXPECT().
		Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, dgst digest.Digest, size int64, r io.Reader, wire transfer.WireProgress) error {
			return recorded.record(dgst, size, r, wire)
		}).
		Maybe()

	return blobs, recorded
}

// blobStore serves part bodies to a mocked Blobs.Get. It counts the bodies it
// handed out against the ones the puller closed, so a test can prove that no
// blob body was left open, including on the paths where a part is rejected,
// and it counts the bytes it put into them, so a test can prove that a
// continued part moved only the bytes it was missing.
type blobStore struct {
	// mu guards the four fields below.
	mu sync.Mutex
	// bodies is the content served for each digest. A test rewrites an entry
	// before the pull starts to serve corrupt, short, or long content.
	bodies map[digest.Digest][]byte
	// served counts the bodies handed out.
	served int
	// closed counts the bodies the puller closed.
	closed int
	// handedOut totals the bytes the store put into the bodies it served.
	handedOut int64
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

// serve answers a blob read of dgst from off the way a registry that honors
// byte ranges answers one: the rest of the blob, and the byte the stream
// starts at. The body counts as open until the caller closes it, and an off of
// zero is the whole blob, which is also the only answer such a registry and a
// range-ignoring one agree on.
//
// An off with no byte left to serve — at or past the end of a nonempty blob —
// is refused, which is what a registry does with a range it cannot satisfy.
// No pull should ever ask for one, so a fixture that does says so loudly
// instead of serving nothing.
func (s *blobStore) serve(dgst digest.Digest, off int64) (io.ReadCloser, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body, ok := s.bodies[dgst]
	if !ok {
		return nil, 0, fmt.Errorf("no blob %s in the store", dgst)
	}
	if off < 0 || (off > 0 && off >= int64(len(body))) {
		return nil, 0, fmt.Errorf("blob %s has no byte %d to start at", dgst, off)
	}

	return s.body(body[off:], nil), off, nil
}

// serveFlaky returns a body that serves prefix and then raises breaks,
// counted against the store's closes like any other body it handed out. It
// stands in for a registry that hangs up part way through a part.
func (s *blobStore) serveFlaky(prefix []byte, breaks error) io.ReadCloser {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.body(prefix, breaks)
}

// body records one body of content and returns it. The caller holds the lock.
func (s *blobStore) body(content []byte, fail error) io.ReadCloser {
	s.served++
	s.handedOut += int64(len(content))

	return &blobBody{store: s, reader: bytes.NewReader(content), fail: fail}
}

// counts returns how many bodies were served and how many of those were
// closed.
func (s *blobStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.served, s.closed
}

// bytesServed returns how many bytes the store put into the bodies it handed
// out, which is what a whole-part refetch costs more of than a continuation.
func (s *blobStore) bytesServed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.handedOut
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
// from store by a registry that honors every byte range it is given. The
// expectation is optional because a pull that stops at the manifest fetches
// nothing.
func mockBlobsServing(t testing.TB, store *blobStore) *ocimocks.MockBlobs {
	t.Helper()

	blobs := ocimocks.NewMockBlobs(t)
	blobs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, dgst digest.Digest, offset int64) (io.ReadCloser, int64, error) {
			return store.serve(dgst, offset)
		},
	).Maybe()

	return blobs
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

// newMemFile returns a file holding seed, which is the partial file a pull is
// resuming into. A nil seed is the empty file a first pull starts from.
func newMemFile(seed []byte) *memFile {
	return &memFile{data: slices.Clone(seed)}
}

// size reports the file's current length, which is the only evidence a pull
// has that an earlier run left something behind.
func (f *memFile) size() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return int64(len(f.data)), nil
}

// truncate sizes the file, which is what a pull does before it fetches
// anything. It keeps whatever content still fits, the way a real truncate
// does: a resume reads the bytes that survive it.
func (f *memFile) truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	sized := make([]byte, size)
	copy(sized, f.data)
	f.data = sized

	return nil
}

// readAt copies the file's content at off into p, with the os-like semantics a
// resume's hash pass reads under: the bytes that exist come back with
// [io.EOF] when there are not enough of them.
func (f *memFile) readAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if off < 0 {
		return 0, fmt.Errorf("read at negative offset %d", off)
	}

	return readAt(f.data, p, off)
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
// Size and ReadAt are wired because every pull measures the destination before
// it sizes it, and a pull that finds a partial file of the right length reads
// every part range back out of it.
//
// Commit is deliberately left unwired: a test that expects a pull to succeed
// adds the expectation itself, and one that expects a failure leaves it off,
// so a pull that publishes a file it should not have fails loudly.
func mockSink(t testing.TB, file *memFile) *filemocks.MockSink {
	t.Helper()

	sink := filemocks.NewMockSink(t)
	sink.EXPECT().Size().RunAndReturn(file.size).Maybe()
	sink.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(file.readAt).Maybe()
	sink.EXPECT().Truncate(mock.Anything).RunAndReturn(file.truncate).Maybe()
	sink.EXPECT().WriteAt(mock.Anything, mock.Anything).RunAndReturn(file.writeAt).Maybe()

	return sink
}
