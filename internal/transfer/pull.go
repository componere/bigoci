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
)

// PullSpec wires one pull: the registry end of the transfer, the file end,
// and the one knob that shapes it. Every field is required.
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
// Pull does not retry, and it does not resume: every part is fetched, even
// one an earlier attempt already wrote.
func Pull(ctx context.Context, spec PullSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}

	body, _, err := spec.Manifests.Get(ctx)
	if err != nil {
		return fmt.Errorf("fetch the manifest: %w", err)
	}

	artifact, err := manifest.Decode(body)
	if err != nil {
		return fmt.Errorf("decode the manifest: %w", err)
	}

	if err := spec.Sink.Truncate(artifact.FileSize); err != nil {
		return fmt.Errorf("size the destination to %d bytes: %w", artifact.FileSize, err)
	}

	if err := fetchParts(ctx, spec, artifact); err != nil {
		return err
	}

	if err := spec.Sink.Commit(); err != nil {
		return fmt.Errorf("commit the destination: %w", err)
	}

	return nil
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
// anything reads it. A worker leaves when the queue runs dry or when the
// group's context is cancelled, which is why a failed or cancelled pull
// cannot leave a worker behind.
func fetchParts(ctx context.Context, spec PullSpec, artifact manifest.Artifact) error {
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
			return fetchWorker(groupCtx, spec.Blobs, spec.Sink, jobs)
		})
	}

	return group.Wait()
}

// fetchWorker drains jobs until the channel closes, downloading and verifying
// each part it takes. Every worker builds its own fetcher, so the scratch a
// part streams through is never shared between goroutines.
func fetchWorker(ctx context.Context, blobs Blobs, sink Sink, jobs <-chan partJob) error {
	fetcher := newPartFetcher(blobs, sink)

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
}

// newPartFetcher builds the fetcher for one worker, allocating that worker's
// share of scratch once.
func newPartFetcher(blobs Blobs, sink Sink) *partFetcher {
	return &partFetcher{
		blobs:  blobs,
		sink:   sink,
		buf:    make([]byte, copyBufferSize),
		hasher: sha256.New(),
	}
}

// fetch downloads one part into its range of the sink and verifies it against
// the digest the manifest recorded.
//
// The reader is closed on every path out, including the ones where the part
// is rejected: a pull that gives up still has to hand the connection back.
func (f *partFetcher) fetch(ctx context.Context, job partJob) error {
	content, err := f.blobs.Get(ctx, job.dgst, 0)
	if err != nil {
		return fmt.Errorf("fetch part %d (%s): %w", job.part.Index, job.dgst, err)
	}
	defer content.Close()

	f.hasher.Reset()

	if err := f.stream(content, job.part); err != nil {
		return err
	}

	if got := digest.NewDigest(digest.SHA256, f.hasher); got != job.dgst {
		return fmt.Errorf(
			"%w: part %d hashes to %s, but the manifest names %s", ErrDigestMismatch, job.part.Index, got, job.dgst,
		)
	}

	return nil
}

// stream copies the part's bytes from content into its range of the sink,
// hashing them on the way, and checks that the blob is exactly as long as the
// manifest says.
//
// The copy runs through the worker's own buffer into an [io.OffsetWriter],
// which turns the sink's positional writes into a stream, so no part is ever
// held whole in memory and no worker writes outside its own range. The limit
// stops the copy at the declared length; a blob longer than that would
// otherwise go unnoticed, so [io.ReadFull] asks for one more byte afterwards,
// and getting it proves the registry served content the manifest does not
// describe.
//
// The blob reader travels through a tagging wrapper because [io.CopyBuffer]
// collapses reader and writer failures into one value: a registry that hangs
// up mid-part must be reported as a fetch failure, not a destination write
// failure — the retry phase classifies the two differently.
func (f *partFetcher) stream(content io.Reader, part plan.Part) error {
	into := io.MultiWriter(f.hasher, io.NewOffsetWriter(f.sink, part.Offset))

	written, err := io.CopyBuffer(into, io.LimitReader(tagReads{r: content}, part.Size), f.buf)
	if err != nil {
		var read *readError
		if errors.As(err, &read) {
			return fmt.Errorf("fetch part %d: %w", part.Index, read.err)
		}

		return fmt.Errorf("write part %d into the destination at offset %d: %w", part.Index, part.Offset, err)
	}
	if written != part.Size {
		return fmt.Errorf(
			"part %d ended after %d bytes, but the manifest declares %d", part.Index, written, part.Size,
		)
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
