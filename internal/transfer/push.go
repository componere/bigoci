package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"

	"github.com/componere/bigoci/internal/manifest"
	"github.com/componere/bigoci/internal/plan"
	"github.com/componere/bigoci/internal/retry"
)

// PushSpec wires one push: the file end of the transfer, the registry end,
// and the two knobs that shape it. Every field but Title and Retry is
// required.
type PushSpec struct {
	// Source is the file being uploaded. Push reads every byte of it once to
	// hash it, and each part a second time to upload it.
	Source Source
	// Blobs is the blob surface of the repository the parts go into.
	Blobs Blobs
	// Manifests is the manifest surface of that repository, already bound to
	// the reference the artifact is written at.
	Manifests Manifests
	// PartSize is the size the file splits at, in bytes. It must be positive.
	// The manifest records it, so a pull never has to guess it.
	PartSize plan.PartSize
	// Workers is how many parts upload at once. It must be positive.
	Workers int
	// Title is the file name the manifest records. It is informational and
	// may be empty.
	Title string
	// Retry is the policy every registry operation of the push runs under:
	// each part, the empty config blob, and the manifest, each with its own
	// budget. The zero value is the library's default policy, so a caller
	// that has nothing to say about retries leaves it out.
	Retry retry.Policy
}

// Push uploads the source as a bigoci artifact and returns the descriptor of
// the manifest it wrote.
//
// The push is one sequential pass over the file feeding a pool of uploaders.
// The pass hashes each part and the whole file at the same time and hands a
// part to the pool the moment its digest is known, so hashing never waits on
// an upload and the file is read front to back exactly once. A worker skips
// any part the registry already holds, which is what makes re-pushing an
// unchanged file nearly free and lets an interrupted push resume for the
// price of a re-hash. Nothing buffers a part: every upload streams straight
// out of the file at the part's offset.
//
// The manifest goes last, once every part and the empty config blob exist in
// the repository. A push that fails or is cancelled therefore leaves no
// artifact behind, only unreferenced blobs the registry collects.
//
// A failure some layer marked worth repeating is attempted again under
// [PushSpec.Retry]: four times by default, with a jittered wait that grows
// between attempts and never falls short of one a registry asked for. The
// budget is per unit of work. A part's existence check and its upload share
// one, so an upload whose bytes landed and whose answer was lost costs the
// next attempt a check instead of the whole part again; the empty config blob
// and the manifest each have their own. A failure nobody marked ends its unit
// on the first attempt, and so does a range the source will not read: the
// registry is what gets another attempt, never the disk.
//
// The first failure that survives its budget cancels the rest of the transfer
// and surfaces, wrapped in context naming the part it came from. A cancelled
// ctx does the same and cuts short any wait in progress, so a worker between
// attempts leaves as promptly as one between parts.
func Push(ctx context.Context, spec PushSpec) (ocispec.Descriptor, error) {
	if err := spec.validate(); err != nil {
		return ocispec.Descriptor{}, err
	}

	split, err := plan.New(spec.Source.Size(), spec.PartSize)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("plan the split: %w", err)
	}

	parts := make([]manifest.Part, split.NumParts())

	var fileDigest digest.Digest

	group, groupCtx := errgroup.WithContext(ctx)

	// The channel holds every part the plan has, so the hash pass never blocks
	// on a send even when every worker has already stopped, and it always
	// closes the channel on its way out, so no worker ever blocks on a
	// receive. Cancellation therefore unwinds without a rendezvous: each
	// goroutine returns at the next part boundary it reaches, or as soon as a
	// backoff sleep is interrupted, and Wait returns once the last of them is
	// gone.
	jobs := make(chan partJob, split.NumParts())

	group.Go(func() error {
		defer close(jobs)

		hashed, hashErr := hashParts(groupCtx, spec.Source, split, parts, jobs)
		if hashErr != nil {
			return hashErr
		}
		fileDigest = hashed

		return nil
	})

	// One uploader serves every worker: it holds no per-worker scratch, and
	// the claim set it carries is the thing the workers have to share.
	up := &uploader{blobs: spec.Blobs, source: spec.Source, claims: newClaimSet(), policy: spec.Retry}
	// A worker beyond the part count would only ever take one closed-channel
	// receive and leave, so its goroutine is never started.
	for range min(spec.Workers, split.NumParts()) {
		group.Go(func() error {
			return up.drain(groupCtx, jobs)
		})
	}

	if err := group.Wait(); err != nil {
		return ocispec.Descriptor{}, err
	}

	if err := ensureEmptyConfig(ctx, spec.Blobs, spec.Retry); err != nil {
		return ocispec.Descriptor{}, err
	}

	return writeManifest(ctx, spec.Manifests, spec.Retry, manifest.Artifact{
		FileDigest: fileDigest,
		FileSize:   split.FileSize(),
		PartSize:   spec.PartSize,
		Title:      spec.Title,
		Parts:      parts,
	})
}

// validate checks the spec before the push touches anything. A missing port
// or a nonsensical knob is a programming error, and catching it here means no
// request is made and no file is read on the way to reporting it. The part
// size is not checked here: plan.New is the split rule's single gate and
// rejects a non-positive one before anything else happens.
func (s PushSpec) validate() error {
	if s.Source == nil {
		return errors.New("push spec has no source")
	}

	return validateRegistryPorts(s.Blobs, s.Manifests, s.Workers)
}

// hashParts makes the push's single sequential pass over the source. It fills
// parts in file order, queues each part for upload as soon as that part's
// digest is known, and returns the digest of the whole file, which falls out
// of the same pass.
//
// The order of parts comes from the plan and never from the order uploads
// finish: a worker only ever reads the job it was handed, so what the
// manifest records is the file's own order.
//
// The pass checks for cancellation between parts rather than inside one, so a
// cancelled push stops after at most one more part is hashed.
func hashParts(
	ctx context.Context,
	source Source,
	split plan.Plan,
	parts []manifest.Part,
	jobs chan<- partJob,
) (digest.Digest, error) {
	fileHasher := sha256.New()
	partHasher := sha256.New()
	// Both hashers see every byte, and resetting the part hasher between parts
	// does not disturb the writer, so one multi-writer serves the whole pass.
	hashers := io.MultiWriter(partHasher, fileHasher)
	buf := make([]byte, copyBufferSize)

	for part := range split.Parts() {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		partHasher.Reset()

		content := io.NewSectionReader(source, part.Offset, part.Size)

		read, err := io.CopyBuffer(hashers, content, buf)
		if err != nil {
			return "", fmt.Errorf("hash part %d at offset %d: %w", part.Index, part.Offset, err)
		}
		if read != part.Size {
			return "", fmt.Errorf(
				"part %d is %d bytes, but the plan expects %d: the source changed while the push read it",
				part.Index, read, part.Size,
			)
		}

		dgst := digest.NewDigest(digest.SHA256, partHasher)
		parts[part.Index] = manifest.Part{Digest: dgst, Size: part.Size}
		jobs <- partJob{part: part, dgst: dgst}
	}

	return digest.NewDigest(digest.SHA256, fileHasher), nil
}

// uploader is the upload side of one push: the ports it moves bytes between,
// the claims its workers share, and the policy every attempt runs under. It
// holds no per-worker scratch, so one value serves every worker.
type uploader struct {
	// blobs is where parts are written.
	blobs Blobs
	// source is the file parts are read out of.
	source Source
	// claims is the push-wide record of which digests are spoken for.
	claims *claimSet
	// policy is how often and how patiently a part is attempted.
	policy retry.Policy
}

// drain takes jobs until the channel closes, uploading every part the
// repository does not already hold. Every worker runs it against the one
// channel, and any error it returns cancels the group the others run in.
func (u *uploader) drain(ctx context.Context, jobs <-chan partJob) error {
	for job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := u.upload(ctx, job); err != nil {
			return err
		}
	}

	return nil
}

// upload moves one part into the repository, attempting it again while the
// policy says a failure is worth repeating, unless another worker of the same
// push is already uploading its digest.
//
// The claim comes first: two byte-identical parts share one blob, and without
// it two workers would both see Exists answer no and upload the same bytes
// twice. It is taken once, outside the attempts, because it is bookkeeping
// about this push rather than about the registry: a retry is the same worker
// carrying on with a digest it already owns, and it never re-claims. Skipping
// instead of waiting is sound because a part that exhausts its attempts fails
// the whole push — there is no path where a skipped part needs the blob and
// the claiming worker's upload quietly never happened.
func (u *uploader) upload(ctx context.Context, job partJob) error {
	if !u.claims.claim(job.dgst) {
		return nil
	}

	return retry.Do(ctx, u.policy, func(ctx context.Context) error {
		return u.attempt(ctx, job)
	})
}

// attempt makes one try at getting the part into the repository: ask whether
// the registry holds it, and upload it when it does not.
//
// The existence check sits inside the attempt rather than before the retry,
// which makes an attempt idempotent for free. An upload whose bytes landed
// and whose response was lost on the way back is an ordinary outcome of a
// broken connection, and the next attempt finds the part already there
// instead of sending it a second time.
//
// The upload reads from a section reader opened here and nowhere else: the
// reader the hash pass used is spent, and a blob upload consumes what it is
// given exactly once, so every attempt needs a fresh one over the same range.
// The file is the transfer's buffer, so re-reading a range costs a read and
// no memory, which is why a push takes a path and not a stream.
//
// The reader travels through a tagging wrapper because a range the source
// will not read fails from inside the upload, where the adapter cannot tell
// it from a connection that stopped carrying and marks it worth repeating.
// The orchestrator knows better: it unwraps its own tag and reports the disk
// failure plain, so a source that went away ends the push at once instead of
// costing three more reads of a range that will not read.
func (u *uploader) attempt(ctx context.Context, job partJob) error {
	exists, err := u.blobs.Exists(ctx, job.dgst)
	if err != nil {
		return fmt.Errorf("check whether part %d (%s) exists: %w", job.part.Index, job.dgst, err)
	}
	if exists {
		return nil
	}

	content := tagSourceReads{r: io.NewSectionReader(u.source, job.part.Offset, job.part.Size)}
	if err := u.blobs.Put(ctx, job.dgst, job.part.Size, content); err != nil {
		var src *sourceError
		if errors.As(err, &src) {
			return fmt.Errorf(
				"read part %d of the source at offset %d: %w", job.part.Index, job.part.Offset, src.err,
			)
		}

		return fmt.Errorf("upload part %d (%s): %w", job.part.Index, job.dgst, err)
	}

	return nil
}

// tagSourceReads wraps the section reader an upload streams from, so a
// failure the source raises stays recognizable after the adapter has wrapped
// it as a failed request. [io.EOF] passes through untouched — the transport
// reads it as the end of the body.
type tagSourceReads struct {
	// r is the range of the file being uploaded.
	r io.Reader
}

// Read reads from the source's range and tags every failure except EOF.
func (t tagSourceReads) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, &sourceError{err: err}
	}

	return n, err
}

// sourceError marks a failure as coming from the source the upload was
// reading, which is what lets the orchestrator keep a local disk failure
// terminal after the adapter — which cannot tell it from a broken connection
// — has marked the request worth repeating.
type sourceError struct {
	// err is the underlying read failure.
	err error
}

// Error renders the underlying failure unchanged.
func (e *sourceError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the underlying failure to [errors.Is] and [errors.As].
func (e *sourceError) Unwrap() error {
	return e.err
}

// claimSet tracks the digests some worker of this push already owns, so a
// digest that appears in several parts is uploaded exactly once.
type claimSet struct {
	// mu guards claimed: workers claim concurrently.
	mu sync.Mutex
	// claimed holds every digest a worker has taken responsibility for.
	claimed map[digest.Digest]struct{}
}

// newClaimSet returns an empty claim set for one push.
func newClaimSet() *claimSet {
	return &claimSet{claimed: make(map[digest.Digest]struct{})}
}

// claim reports whether the caller is the first worker to take dgst. Only the
// first caller uploads; every later one skips the part.
func (c *claimSet) claim(dgst digest.Digest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, taken := c.claimed[dgst]; taken {
		return false
	}
	c.claimed[dgst] = struct{}{}

	return true
}

// ensureEmptyConfig uploads the empty config blob the manifest references
// unless the repository already holds it. Registries reject a manifest naming
// a blob they do not have, and after the first push of any artifact to a
// repository this is a single existence check.
//
// The pair runs under its own budget, and the reader over the two bytes is
// built inside the attempt: a reader is consumed exactly once, and a second
// attempt handed the first one's would send nothing under a Content-Length
// that promises two bytes.
func ensureEmptyConfig(ctx context.Context, blobs Blobs, policy retry.Policy) error {
	descriptor, content := manifest.EmptyConfig()

	return retry.Do(ctx, policy, func(ctx context.Context) error {
		exists, err := blobs.Exists(ctx, descriptor.Digest)
		if err != nil {
			return fmt.Errorf("check whether the empty config blob exists: %w", err)
		}
		if exists {
			return nil
		}

		if err := blobs.Put(ctx, descriptor.Digest, descriptor.Size, bytes.NewReader(content)); err != nil {
			return fmt.Errorf("upload the empty config blob: %w", err)
		}

		return nil
	})
}

// writeManifest encodes the artifact and writes it at the bound reference,
// returning the descriptor of what it wrote. The size in the descriptor is
// the length of the bytes that were sent, and the digest is the one the port
// computed over those same bytes.
//
// The write has a budget of its own. It is the last request of a transfer
// that may have moved gigabytes, over a document of a few kilobytes, and
// every attempt sends the same bytes to the same reference — so repeating it
// reaches the same state, and dropping the whole push on a 503 here would be
// indefensible.
func writeManifest(
	ctx context.Context,
	manifests Manifests,
	policy retry.Policy,
	artifact manifest.Artifact,
) (ocispec.Descriptor, error) {
	body, err := manifest.Encode(artifact)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("encode the manifest: %w", err)
	}

	var dgst digest.Digest

	if err := retry.Do(ctx, policy, func(ctx context.Context) error {
		written, putErr := manifests.Put(ctx, ocispec.MediaTypeImageManifest, body)
		if putErr != nil {
			return fmt.Errorf("write the manifest: %w", putErr)
		}
		dgst = written

		return nil
	}); err != nil {
		return ocispec.Descriptor{}, err
	}

	return ocispec.Descriptor{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: manifest.ArtifactType,
		Digest:       dgst,
		Size:         int64(len(body)),
	}, nil
}
