package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"

	digest "github.com/opencontainers/go-digest"
	"golang.org/x/sync/errgroup"

	"github.com/componere/bigoci/internal/manifest"
	"github.com/componere/bigoci/internal/plan"
	"github.com/componere/bigoci/internal/retry"
)

// PullSpec wires one pull: the registry end of the transfer, the file end,
// and the one knob that shapes it. Every field but Retry is required.
type PullSpec struct {
	// Sink is the destination the file is assembled in. Pull sizes it once,
	// writes every part into it at that part's offset, and commits it when
	// the last part verifies.
	Sink Sink
	// Blobs is the blob surface of the repository the parts come from.
	Blobs Blobs
	// Manifests is the manifest surface of that repository, already bound to
	// the reference being pulled.
	Manifests Manifests
	// Workers is how many parts download at once. It must be positive.
	Workers int
	// Retry is the policy every registry operation of the pull runs under:
	// the manifest fetch and each part, each with its own budget. The zero
	// value is the library's default policy, so a caller that has nothing to
	// say about retries leaves it out.
	Retry retry.Policy
}

// Pull downloads the artifact the manifests port is bound to into the sink.
//
// The manifest comes first, because it says how large the file is and which
// blobs make it up. Sizing the sink to that length up front turns the
// download into a set of independent writes at fixed offsets, so workers
// finish in any order and no part waits on the one before it. Each part is
// hashed as it streams into place and checked against the digest the manifest
// records for it, which is the whole verification chain: the caller trusts a
// manifest digest, the manifest names every part.
//
// Nothing is published until every part verifies. On any failure the sink is
// left uncommitted, so the destination keeps whatever it held before and the
// partial content stays behind for a later attempt.
//
// A part whose fetch breaks in a way some layer marked worth repeating — a
// dropped connection, a 429, a 5xx, a body that ended before the manifest
// said it would — is fetched again under [PullSpec.Retry]: four times by
// default, with a jittered wait that grows between attempts and never falls
// short of one a registry asked for. The manifest fetch has a budget of its
// own. A part that arrives whole and hashes wrong does not: that is the
// registry serving content the artifact does not describe, and asking again
// gives the same answer. Neither does a destination that will not take the
// bytes — a local disk is not a peer having a bad minute.
//
// An attempt that follows a broken one carries on where it stopped. It asks
// the blob port for the rest of the part and hashes what arrives onto what the
// earlier attempts hashed, so a stream that died after most of a part costs
// only the bytes that never landed. A registry that will not serve the range
// says so by answering with the whole blob, which the port reports as a stream
// starting at byte zero: the attempt then writes the part again from its first
// byte, over what it already held, inside the same attempt and without a
// second request.
//
// Pull also resumes into a partial file an earlier run left behind. When the
// destination is already exactly as long as the manifest declares, each part
// is hashed out of the file before anything is fetched, and a part that
// already matches its digest costs no request at all. Bytes no run ever wrote
// read back as zeros and fail that check, so nothing has to be recorded
// between runs: the file on disk is the whole of the state. A partial of any
// other length belongs to some other artifact, and every part is fetched.
//
// A cancelled ctx stops the workers, cuts short any wait in progress, and
// interrupts a resume hash at its next bounded file read.
func Pull(ctx context.Context, spec PullSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}

	body, err := fetchManifest(ctx, spec)
	if err != nil {
		return err
	}

	artifact, err := manifest.Decode(body)
	if err != nil {
		return fmt.Errorf("decode the manifest: %w", err)
	}

	// Measuring comes first because the truncate below destroys the evidence:
	// it is what makes a leftover partial the right length or cuts it to fit,
	// and afterwards nothing can tell the two apart.
	existing, err := spec.Sink.Size()
	if err != nil {
		return fmt.Errorf("measure the destination: %w", err)
	}

	resume := resumable(existing, artifact)

	if err := spec.Sink.Truncate(artifact.FileSize); err != nil {
		return fmt.Errorf("size the destination to %d bytes: %w", artifact.FileSize, err)
	}

	if err := fetchParts(ctx, spec, artifact, resume); err != nil {
		return err
	}

	if err := spec.Sink.Commit(); err != nil {
		return fmt.Errorf("commit the destination: %w", err)
	}

	return nil
}

// fetchManifest reads the manifest the pull is bound to, under a budget of
// its own. A fetch is a plain read, so repeating one asks the same question
// and nothing about the transfer has started yet: a 503 on the first request
// of a pull should not be the whole answer.
//
// The descriptor the port also returns is dropped here. It describes the
// bytes, which is what the caller decodes, and the port has already checked
// it against a digest reference.
func fetchManifest(ctx context.Context, spec PullSpec) ([]byte, error) {
	var body []byte

	if err := retry.Do(ctx, spec.Retry, func(ctx context.Context) error {
		fetched, _, err := spec.Manifests.Get(ctx)
		if err != nil {
			return fmt.Errorf("fetch the manifest: %w", err)
		}
		body = fetched

		return nil
	}); err != nil {
		return nil, err
	}

	return body, nil
}

// resumable reports whether what the sink already holds is worth hashing
// before anything is fetched.
//
// The only evidence a resume rests on is the length: a partial file exactly as
// long as the manifest declares was written by a pull of an artifact this size,
// and every part range in it either hashes to the digest the manifest names or
// does not. Any other length belongs to some other artifact, and hashing it
// would only be a slow way of fetching everything.
//
// The size must be positive as well as equal. A zero-byte artifact makes a
// zero-byte destination look complete before a single request has gone out,
// and its one empty part would be verified out of a file nobody wrote — so an
// empty artifact is always fetched, exactly as it is on a first run.
func resumable(existing int64, artifact manifest.Artifact) bool {
	return existing == artifact.FileSize && artifact.FileSize > 0
}

// validate checks the spec before the pull touches anything. A missing port
// or a nonsensical knob is a programming error, and catching it here means no
// request is made and no file is created on the way to reporting it.
func (s PullSpec) validate() error {
	if s.Sink == nil {
		return errors.New("pull spec has no sink")
	}

	return validateRegistryPorts(s.Blobs, s.Manifests, s.Workers)
}

// fetchParts downloads and verifies every part of the artifact into the sink.
//
// The offsets come from the split the artifact's file size and part size
// imply, which [manifest.Decode] has already checked the parts against, so
// the plan and the manifest cannot disagree about how many parts there are.
//
// Every job is queued before the first worker starts, into a channel with
// room for all of them, so no send can block and the channel is closed before
// anything reads it. A worker leaves when the queue runs dry, or at the next
// bounded read or part boundary it reaches once the group's context is
// cancelled, or as soon as a backoff sleep is interrupted, which is why a
// failed or cancelled pull cannot leave a worker behind.
//
// resume says the sink holds a partial file worth hashing, and it travels to
// the workers rather than being acted on here: verifying a part is the first
// step of that part's job, so the hashing runs at the pull's own parallelism
// and overlaps the fetches of the parts that failed it.
func fetchParts(ctx context.Context, spec PullSpec, artifact manifest.Artifact, resume bool) error {
	split, err := plan.New(artifact.FileSize, artifact.PartSize)
	if err != nil {
		return fmt.Errorf("plan the split: %w", err)
	}

	jobs := make(chan partJob, split.NumParts())
	for part := range split.Parts() {
		jobs <- partJob{part: part, dgst: artifact.Parts[part.Index].Digest}
	}
	close(jobs)

	group, groupCtx := errgroup.WithContext(ctx)
	// A worker beyond the part count would only ever take one closed-channel
	// receive and leave, so its goroutine is never started.
	for range min(spec.Workers, split.NumParts()) {
		group.Go(func() error {
			return fetchWorker(groupCtx, spec, jobs, resume)
		})
	}

	return group.Wait()
}

// fetchWorker drains jobs until the channel closes, downloading and verifying
// each part it takes. Every worker builds its own fetcher, so the scratch a
// part streams through is never shared between goroutines.
func fetchWorker(ctx context.Context, spec PullSpec, jobs <-chan partJob, resume bool) error {
	fetcher := newPartFetcher(spec.Blobs, spec.Sink, spec.Retry, resume)

	for job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := fetcher.fetch(ctx, job); err != nil {
			return err
		}
	}

	return nil
}

// partFetcher is one pull worker: the two ports it moves bytes between, and
// the scratch it reuses for every part it handles. One worker owns one
// fetcher, so neither the buffer nor the hasher is ever touched by two
// goroutines.
type partFetcher struct {
	// blobs is where parts are read from.
	blobs Blobs
	// sink is where they are written.
	sink Sink
	// buf is the buffer every part streams through, reused across parts. It
	// bounds how much of a part is in memory at once.
	buf []byte
	// hasher computes the digest of the part being fetched. It is reset
	// between parts rather than allocated again.
	hasher hash.Hash
	// policy is how often and how patiently a part is attempted.
	policy retry.Policy
	// resume says the sink already holds a partial file of the right length,
	// so every part is hashed out of it before it is fetched.
	resume bool
}

// newPartFetcher builds the fetcher for one worker, allocating that worker's
// share of scratch once.
func newPartFetcher(blobs Blobs, sink Sink, policy retry.Policy, resume bool) *partFetcher {
	return &partFetcher{
		blobs:  blobs,
		sink:   sink,
		buf:    make([]byte, copyBufferSize),
		hasher: sha256.New(),
		policy: policy,
		resume: resume,
	}
}

// fetch puts one part into its range of the sink and verifies it: out of the
// partial file when a resume finds it already there, and off the registry
// otherwise, attempted again while the policy says a failure is worth
// repeating.
//
// The verify runs outside the retry budget, and deliberately. A part that is
// already on disk costs no attempt because it makes no request, and a partial
// file that cannot be read is a local failure, which is terminal here for the
// same reason a local write failure is: the orchestrator retries the registry,
// never the disk.
//
// How much of the part has arrived lives in done, in this frame, for the whole
// run of attempts. The hasher lives just as long, holding the bytes those
// attempts moved off the wire and nothing else, which is what lets a broken
// part be continued rather than started over and still be checked against one
// digest at the end.
func (f *partFetcher) fetch(ctx context.Context, job partJob) error {
	if f.resume {
		matched, err := f.verify(ctx, job)
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
	}

	var done int64

	return retry.Do(ctx, f.policy, func(ctx context.Context) error {
		return f.attempt(ctx, job, &done)
	})
}

// verify hashes one part's range out of the sink and reports whether what is
// there is already the part the manifest names.
//
// A mismatch is not a failure and never becomes an error value: bytes an
// earlier run never wrote read back as zeros, and a range that hashes wrong is
// simply a part still to fetch. Only the reading can fail. That separation is
// what keeps [ErrDigestMismatch] meaning one thing — a registry served content
// the artifact does not describe — instead of also meaning a stale file on the
// local disk.
//
// The hash runs through the worker's own buffer and hasher, both idle at this
// point in the job, so a resume costs one read pass over the file and not a
// byte of scratch beyond what the fetch would have used anyway.
//
// Cancellation is checked around each bounded read. A disk read already in
// progress is allowed to return, but at most one buffer of its bytes is hashed
// before the context error stops the pull.
func (f *partFetcher) verify(ctx context.Context, job partJob) (bool, error) {
	f.hasher.Reset()

	section := io.NewSectionReader(f.sink, job.part.Offset, job.part.Size)
	read, err := io.CopyBuffer(f.hasher, contextReader{ctx: ctx, reader: section}, f.buf)
	if err != nil {
		return false, fmt.Errorf("read part %d of the existing file: %w", job.part.Index, err)
	}
	if read != job.part.Size {
		return false, fmt.Errorf(
			"read part %d of the existing file: got %d bytes, but the manifest declares %d",
			job.part.Index, read, job.part.Size,
		)
	}

	return digest.NewDigest(digest.SHA256, f.hasher) == job.dgst, nil
}

// contextReader checks whether a transfer ended around each bounded read from
// reader. It lets [io.CopyBuffer] retain its standard copy and error semantics
// while ensuring a resume hash cannot outlive the caller's context by more
// than one read into its fixed-size buffer.
type contextReader struct {
	// ctx is the transfer context that bounds the read pass.
	ctx context.Context
	// reader is the part range being hashed out of the partial file.
	reader io.Reader
}

// Read returns the context's error before starting another read and after a
// read that overlapped cancellation. Returning bytes with that error lets
// [io.CopyBuffer] hash the bytes the disk already produced before it stops.
func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := r.reader.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, contextErr
	}

	return n, err
}

// attempt is one try at the rest of one part: open the blob where the part
// left off, stream what arrives into the sink's range while hashing it, and
// check the part against the manifest once all of it is there.
//
// Every attempt opens the blob again, which re-resolves whatever redirect the
// registry points at, so no expired presigned URL is ever reused. What it asks
// for is the part from done, and done is the bytes earlier attempts provably
// moved.
//
// A part that already has all its bytes is asked for whole instead. The last
// chunk of a body can arrive together with the failure that ends it, which
// leaves nothing to continue from: a range starting at the end of the part is
// a range no registry can satisfy, so the attempt starts the part over rather
// than earning a 416.
//
// The port reports where the stream it hands back really begins, and the
// attempt writes nothing until that offset is one it can place. Zero means the
// registry ignored the range and is sending the whole blob, which this attempt
// consumes as the fetch it would otherwise have had to ask for — no error, no
// second request, and no attempt spent. Any other start is a stream that would
// be copied into bytes of the file it does not belong to, so it ends the
// attempt terminally instead of being worked around.
//
// The reader is closed on every path out, including the ones where the part
// is rejected: a pull that gives up still has to hand the connection back,
// and an attempt that held its body open would keep every earlier attempt's
// body open with it.
func (f *partFetcher) attempt(ctx context.Context, job partJob, done *int64) error {
	if *done == job.part.Size {
		*done = 0
	}

	content, start, err := f.blobs.Get(ctx, job.dgst, *done)
	if err != nil {
		return fmt.Errorf("fetch part %d (%s): %w", job.part.Index, job.dgst, err)
	}
	defer content.Close()

	// The port contracts a start of either the offset asked for or zero, and
	// nothing else is placeable: one continues the part, the other restarts it.
	// A port that reports a third number has broken its contract, which is why
	// the error names the port and not the registry: a conformant adapter has
	// already refused any answer that starts elsewhere.
	if start != 0 && start != *done {
		return fmt.Errorf(
			"fetch part %d (%s): the blob port's stream starts at byte %d, which this attempt cannot write from",
			job.part.Index, job.dgst, start,
		)
	}

	// A stream that starts at zero is the whole blob: either this attempt asked
	// for it, or the registry ignored the range and sent it anyway. From here
	// the two are one thing — the part is written again from its first byte,
	// over whatever it already held.
	if start == 0 {
		*done = 0
	}

	if *done == 0 {
		f.hasher.Reset()
	}

	if err := f.stream(content, job.part, done); err != nil {
		return err
	}

	if got := digest.NewDigest(digest.SHA256, f.hasher); got != job.dgst {
		return fmt.Errorf(
			"%w: part %d hashes to %s, but the manifest names %s", ErrDigestMismatch, job.part.Index, got, job.dgst,
		)
	}

	return nil
}

// stream copies the rest of the part from content into its range of the sink,
// hashing it on the way, and checks that the blob is exactly as long as the
// manifest says.
//
// The copy runs through the worker's own buffer into an [io.OffsetWriter],
// which turns the sink's positional writes into a stream, so no part is ever
// held whole in memory and no worker writes outside its own range. done says
// where in the part this copy begins: it is both what the limit is measured
// against and what the writer is offset by, so a continued attempt writes past
// the bytes an earlier one left and hashes onto them.
//
// The limit stops the copy at the declared length; a blob longer than that
// would otherwise go unnoticed, so [io.ReadFull] asks for one more byte
// afterwards, and getting it proves the registry served content the manifest
// does not describe.
//
// The blob reader travels through a tagging wrapper because [io.CopyBuffer]
// collapses reader and writer failures into one value: a registry that hangs
// up mid-part is reported as a fetch failure, not a destination write failure
// — and the retry policy treats the two differently, because a broken
// connection is worth another attempt and a disk that will not take bytes is
// not.
//
// The distinction decides what is recorded, too. [io.CopyBuffer] writes and
// counts each chunk before it looks at the read error that came with it, and
// the hasher is the first writer of the pair, so after a read failure the
// hasher, the sink, and the count agree to the byte and the next attempt can
// carry on from there. A write failure carries no such promise — the hasher
// took the bytes the sink refused — so it returns before anything is recorded,
// which is safe because a destination that will not take bytes is never
// attempted again.
func (f *partFetcher) stream(content io.Reader, part plan.Part, done *int64) error {
	from, remaining := *done, part.Size-*done
	into := io.MultiWriter(f.hasher, io.NewOffsetWriter(f.sink, part.Offset+from))

	written, err := io.CopyBuffer(into, io.LimitReader(tagReads{r: content}, remaining), f.buf)
	if err != nil {
		var read *readError
		if errors.As(err, &read) {
			*done += written

			return fmt.Errorf("fetch part %d: %w", part.Index, read.err)
		}

		return fmt.Errorf("write part %d into the destination at offset %d: %w", part.Index, part.Offset+from, err)
	}
	*done += written

	if written != remaining {
		// The one failure the orchestrator diagnoses itself: a body that ended
		// cleanly before the manifest's byte count is a truncated transfer, and
		// the accounting that says so is the plan's, not the registry's. It is
		// stated against the part rather than against this attempt, because
		// what a reader needs to know is how much of the part is missing, not
		// how much of it this particular stream carried. The sibling case below
		// stays terminal — extra bytes are content the manifest does not
		// describe, and a second fetch serves them again.
		return retry.Transient(fmt.Errorf(
			"part %d ended after %d bytes, but the manifest declares %d", part.Index, *done, part.Size,
		), 0)
	}

	if _, err := io.ReadFull(content, f.buf[:1]); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("read the end of part %d: %w", part.Index, err)
		}

		return fmt.Errorf("part %d is longer than the %d bytes the manifest declares", part.Index, part.Size)
	}

	return nil
}

// tagReads wraps a pull's blob reader so an error it raises stays
// recognizable as a read failure after [io.CopyBuffer] has mixed it with
// write failures. [io.EOF] passes through untouched — wrapping it would break
// every copy primitive that treats it as the end of the stream.
type tagReads struct {
	// r is the blob body being read.
	r io.Reader
}

// Read reads from the wrapped reader and tags every failure except EOF.
func (t tagReads) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, &readError{err: err}
	}

	return n, err
}

// readError marks a failure as coming from the reading side of a copy.
type readError struct {
	// err is the underlying read failure.
	err error
}

// Error renders the underlying failure unchanged.
func (e *readError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the underlying failure to [errors.Is] and [errors.As].
func (e *readError) Unwrap() error {
	return e.err
}
