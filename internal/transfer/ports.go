package transfer

import (
	"context"
	"io"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Blobs is the distribution-spec blob surface of one repository: the three
// blob operations a transfer performs, and nothing else the spec offers.
//
// An implementation is bound to one repository when it is constructed, so no
// method names one. Every method must be safe for concurrent use, because
// both directions of a transfer run several parts at once against a single
// Blobs.
//
// Errors report what happened and leave what to do about it to the caller.
// An implementation may retry a request it knows is safe to repeat, but a
// failure it gives up on has to surface: the orchestrator owns the retry
// policy and the accounting that goes with it.
type Blobs interface {
	// Exists reports whether the repository holds the blob with digest dgst.
	//
	// A registry answering that it does not hold the blob is a normal result,
	// not a failure: Exists returns (false, nil). It returns an error only
	// when the question could not be answered, because the request failed or
	// the registry refused it.
	//
	// Push calls Exists once per part. A part the registry already holds is
	// skipped, which is what makes re-pushing an unchanged file nearly free
	// and makes an interrupted push resume without extra bookkeeping.
	Exists(ctx context.Context, dgst digest.Digest) (bool, error)

	// Get opens the blob with digest dgst for reading, starting offset bytes
	// into it. An offset of 0 reads the whole blob.
	//
	// A nonzero offset is a resume: the caller already holds the bytes before
	// it and wants only the remainder. Serving a byte range is optional in
	// the distribution spec, so an implementation that asks for one must
	// confirm it got one and return an error when the registry answered with
	// the whole blob instead. Handing back a stream that starts at 0 for a
	// nonzero offset would silently corrupt the file the caller is
	// assembling, so a registry that will not honor the range has to be
	// reported rather than worked around.
	//
	// The caller owns the returned reader and must close it. Get returns an
	// error when the blob does not exist; ask with [Blobs.Exists] instead
	// when absence is an expected answer.
	Get(ctx context.Context, dgst digest.Digest, offset int64) (io.ReadCloser, error)

	// Put uploads the blob with digest dgst, size bytes long, reading its
	// content from r.
	//
	// The upload is monolithic: one request carrying the whole blob, which is
	// the single push primitive every registry implements correctly. Nothing
	// buffers the content, so a part streams straight from the file it lives
	// in however large it is.
	//
	// r is consumed exactly once and never rewound. The implementation must
	// transmit exactly size bytes and declare them in an explicit
	// Content-Length header; without one Go falls back to chunked transfer
	// encoding, which some registries and proxies reject. A short or long
	// read from r is a failure, not something to paper over, and so is a
	// digest the registry computes differently from dgst.
	//
	// Retrying is the caller's job, because only the caller can produce a
	// fresh reader over the same bytes.
	Put(ctx context.Context, dgst digest.Digest, size int64, r io.Reader) error
}

// Manifests is the manifest surface of one repository, bound at construction
// to one reference: a tag or a digest.
//
// Binding the reference to the adapter instead of passing one per call keeps
// reference grammar out of the core. Nothing here parses, validates, or
// renders registry/repository:tag@digest, and the adapter that has to speak
// that grammar is the only code that knows it exists.
//
// A transfer reads or writes the manifest once, at the very start of a pull
// or the very end of a push, so an implementation need not be fast — but it
// must be safe for concurrent use, like every other port here.
type Manifests interface {
	// Get fetches the manifest the bound reference resolves to and returns
	// its raw bytes together with a descriptor for them.
	//
	// The descriptor's digest is computed from the returned bytes and never
	// taken from a header the registry sent. The bytes are what the caller
	// decodes and what the caller's trust chain rests on, so the digest has
	// to describe them and not what the registry claims they are. An
	// implementation that was constructed with a digest reference checks the
	// two agree and fails the fetch when they do not.
	//
	// The bytes come back raw because decoding a bigoci manifest belongs to
	// the manifest package, not to this port. The port's job ends at
	// delivering verified bytes.
	Get(ctx context.Context) ([]byte, ocispec.Descriptor, error)

	// Put writes body as the manifest at the bound reference, under the given
	// media type, and returns the digest of body.
	//
	// body is sent byte for byte. The bigoci manifest encoding is canonical,
	// and the manifest digest only stays reproducible when what reaches the
	// registry is exactly what was encoded, so an implementation must not
	// re-encode, reindent, or otherwise touch it.
	//
	// The returned digest is computed from body, which is how a push that
	// wrote by tag still learns the artifact's digest. Push calls Put last,
	// once every blob the manifest references exists in the repository: a
	// registry rejects a manifest that names blobs it does not hold, and
	// writing the manifest last means an interrupted push leaves no broken
	// artifact behind, only unreferenced blobs the registry collects.
	Put(ctx context.Context, mediaType string, body []byte) (digest.Digest, error)
}

// Source is the readable end of a push: the file being uploaded.
//
// A push reads parts concurrently and out of order, and re-reads a part
// whenever an upload has to be retried, which is why the port reads at
// offsets instead of streaming. The file itself is the transfer's buffer;
// no part is ever held in memory.
//
// Both methods must be safe for concurrent use. The size is fixed for the
// life of a Source: an implementation reads it once when it opens the file.
// A file that changes underneath a running push is a caller error, not a
// case this port handles.
type Source interface {
	// ReaderAt reads one byte range of the file. Workers call it
	// concurrently, at unrelated offsets.
	io.ReaderAt

	// Size returns the total length of the source in bytes. It is the file
	// size the split plan divides and the manifest records.
	Size() int64
}

// Sink is the writable end of a pull: the file being downloaded, plus the
// reads a resume needs.
//
// The lifecycle is three steps. A pull sizes the sink with [Sink.Truncate],
// so workers can write their parts at their offsets in any order. It writes
// parts as they arrive and verifies each one. When every part verifies, it
// calls [Sink.Commit], which publishes the result.
//
// A Sink also reads, because a pull that finds an existing partial file
// resumes into it: it hashes each part range and fetches only the parts that
// do not match the manifest. Ranges no earlier attempt reached read as zeros
// and fail their check, so nothing has to be recorded between attempts.
//
// Every method must be safe for concurrent use except [Sink.Commit], which
// the pull calls once, alone, after the last write.
type Sink interface {
	// ReaderAt reads one byte range back, so a resume can hash the ranges an
	// earlier attempt already wrote.
	io.ReaderAt
	// WriterAt writes one part at its offset in the file. Workers call it
	// concurrently, and the ranges they write never overlap.
	io.WriterAt

	// Size returns the current length of the sink in bytes.
	//
	// Unlike [Source.Size] it can fail, because a sink's length changes and
	// an implementation may have to ask the filesystem for it. A pull
	// compares the answer against the file size in the manifest to decide
	// whether an existing partial file is worth resuming into.
	Size() (int64, error)

	// Truncate sets the sink's length to size bytes, extending it with zeros
	// or cutting it short as needed.
	//
	// A pull calls it once, up front, with the file size from the manifest.
	// Doing so turns the download into a set of independent writes at fixed
	// offsets: workers finish in any order and no part waits on the one
	// before it.
	Truncate(size int64) error

	// Commit atomically publishes the fully verified content at the
	// destination.
	//
	// The destination must never exist in a partial state. A pull that is
	// killed, fails, or is abandoned leaves the destination absent or holding
	// its previous content — never a half-written file that looks complete.
	// An implementation therefore writes somewhere else and moves the result
	// into place with a single atomic operation.
	//
	// A pull calls Commit once, after the last part verifies. Anything the
	// Sink still holds open is released, so it is not usable afterwards.
	Commit() error
}
