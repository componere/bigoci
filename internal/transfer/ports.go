package transfer

import (
	"context"
	"io"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// WireProgress records upload bytes consumed by the transport. The adapter
// calls it with positive deltas and stops before [Blobs.Put] returns.
type WireProgress func(delta int64)

// Blobs is the distribution-spec blob surface of one repository: the three
// blob operations a transfer performs, and nothing else the spec offers.
//
// An implementation is bound to one repository when it is constructed, so no
// method names one. Every method must be safe for concurrent use, because
// both directions of a transfer run several parts at once against a single
// Blobs.
//
// Errors report what happened and leave what to do about it to the caller.
// An implementation must not retry: the orchestrator attempts every operation
// under one policy, and an adapter that retried underneath it would multiply
// the attempt count and the waits between attempts out of anyone's sight.
//
// What an implementation does owe is a verdict. A failure that repeating the
// request could fix is returned tagged with
// [github.com/imgoci/bigoci/internal/retry.Transient], carrying whatever
// wait the far end asked for; everything else is returned untagged and ends
// the transfer at once. The tag is what lets the orchestrator decide without
// knowing how the implementation talks to a registry, so an implementation is
// the only thing that can supply it — nothing above this port can tell a
// dropped connection from a refused request.

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

	// Get opens the blob dgst names for reading and reports the offset the
	// stream it returns actually starts at.
	//
	// off is where the caller wants to begin: zero for the whole blob, a
	// positive value for a resume, where the caller already holds every byte
	// before it. It is never negative — the caller computes it from bytes it
	// already holds, so a negative off is a bug above this port, not a case an
	// implementation handles.
	//
	// Serving a byte range is optional in the distribution spec, so what a
	// caller gets back is data rather than a promise. The reported offset is
	// either off, meaning the range was honored, or 0, meaning the far end
	// ignored it and is serving the whole blob from its first byte. No other
	// value is ever reported: an implementation served a range that starts
	// somewhere else fails the call, because a stream at a position nobody
	// asked for would silently corrupt the file the caller is assembling. A
	// call that fails reports no reader and a zero offset.
	//
	// Reporting the offset rather than refusing a whole-blob answer is what
	// makes the fallback free. The body of such an answer is a perfectly good
	// blob read; a caller that wanted a range writes the part again from its
	// first byte instead, over the bytes it already holds, at the cost of no
	// extra request and no extra attempt.
	//
	// The caller owns the returned reader and must close it. Get returns an
	// error when the blob does not exist; ask with [Blobs.Exists] instead
	// when absence is an expected answer.
	//
	// Every call is a fresh request that resolves the blob again, so a caller
	// fetching a part a second time never rides on whatever a previous call
	// followed — a redirect to object storage expires, and a stale one would
	// fail a retry for a reason that has nothing to do with the first failure.
	//
	// The reader's failures are classified the same way its opening was: an
	// implementation whose stream can break part way through — because it is
	// arriving over a connection — tags those read failures too, since by the
	// time they surface the caller holds nothing but an [io.Reader] and cannot
	// tell a broken connection from a finished one.
	Get(ctx context.Context, dgst digest.Digest, off int64) (io.ReadCloser, int64, error)

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
	// fresh reader over the same bytes. An implementation that receives a
	// spent reader has been handed a bug, not a retry.
	Put(ctx context.Context, dgst digest.Digest, size int64, r io.Reader, wire WireProgress) error
}

// Manifests is the manifest surface of one repository. Addressing is fixed
// at construction, in one of two modes: bound to a tag or a digest, or
// digest-publication, where Put writes at the digest of the body and Get is
// unsupported.
//
// Fixing the address on the adapter instead of passing a reference per call
// keeps reference grammar out of the core. Nothing here parses, validates, or
// renders registry/repository:tag@digest, and the adapter that has to speak
// that grammar is the only code that knows it exists.
//
// A transfer reads or writes the manifest once, at the very start of a pull
// or the very end of a push, so an implementation need not be fast — but it
// must be safe for concurrent use, like every other port here.
//
// Failures are classified exactly as [Blobs] describes: transient ones
// tagged, everything else not. In bound mode both methods are safe to
// repeat — a Get is a read, and a Put of identical bytes at the same
// reference reaches the same state. In digest-publication mode Put of
// identical bytes is the same, and Get is unused. The orchestrator retries
// a manifest operation under the same policy it retries a part under.
type Manifests interface {
	// Get fetches the manifest the bound tag or digest resolves to and
	// returns its raw bytes together with a descriptor for them.
	//
	// Get is unsupported in digest-publication mode: that construction has
	// no bound tag or digest to fetch. An implementation fails the call
	// without talking to a registry.
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

	// Put writes body as the manifest under the given media type and returns
	// the digest of body.
	//
	// In bound mode the write addresses the tag or digest the adapter was
	// constructed with. In digest-publication mode it addresses the digest
	// of body, which is not known until this call.
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
//
// A Source never classifies its failures. A local read that fails is not a
// thing repeating fixes, so it is always terminal and ends the transfer at
// once: the orchestrator retries the registry, never the disk.
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
//
// A Sink never classifies its failures either. A write the destination
// refuses is terminal, whatever was happening on the registry side of the
// same copy: a full or unwritable filesystem is not a peer having a bad
// minute, and hammering it three more times only delays the report.
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
	//
	// A pull reads it once, before it truncates. The order is part of the
	// contract rather than an implementation detail: [Sink.Truncate] is what
	// makes a leftover partial the right length, so afterwards nothing can
	// tell a file an earlier run filled from one this pull just sized.
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
