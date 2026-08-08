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
)

// PushSpec wires one push: the file end of the transfer, the registry end,
// and the two knobs that shape it. Every field but Title is required.
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
// Push does not retry. The first failure cancels the rest of the transfer and
// surfaces, wrapped in context naming the part it came from.
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
	// goroutine returns at the next part boundary it reaches, and Wait returns
	// once the last of them is gone.
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

	claims := newClaimSet()
	// A worker beyond the part count would only ever take one closed-channel
	// receive and leave, so its goroutine is never started.
	for range min(spec.Workers, split.NumParts()) {
		group.Go(func() error {
			return uploadParts(groupCtx, spec.Blobs, spec.Source, claims, jobs)
		})
	}

	if err := group.Wait(); err != nil {
		return ocispec.Descriptor{}, err
	}

	if err := ensureEmptyConfig(ctx, spec.Blobs); err != nil {
		return ocispec.Descriptor{}, err
	}

	return writeManifest(ctx, spec.Manifests, manifest.Artifact{
		FileDigest: fileDigest,
		FileSize:   split.FileSize(),
		PartSize:   spec.PartSize,
		Title:      spec.Title,
		Parts:      parts,
	})
}

// validate checks the spec before the push touches anything. A missing port
// or a nonsensical knob is a programming error, and catching it here means no
// request is made and no file is read on the way to reporting it.
func (s PushSpec) validate() error {
	switch {
	case s.Source == nil:
		return errors.New("push spec has no source")
	case s.Blobs == nil:
		return errors.New("push spec has no blobs port")
	case s.Manifests == nil:
		return errors.New("push spec has no manifests port")
	case s.PartSize <= 0:
		return fmt.Errorf("part size must be positive, got %d", s.PartSize)
	case s.Workers <= 0:
		return fmt.Errorf("worker count must be positive, got %d", s.Workers)
	}

	return nil
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

// uploadParts drains jobs until the channel closes, uploading every part the
// repository does not already hold. Every worker runs it against the one
// channel, and any error it returns cancels the group the others run in.
func uploadParts(ctx context.Context, blobs Blobs, source Source, claims *claimSet, jobs <-chan partJob) error {
	for job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := uploadPart(ctx, blobs, source, claims, job); err != nil {
			return err
		}
	}

	return nil
}

// uploadPart uploads one part unless the repository already holds it or
// another worker of the same push is already uploading its digest.
//
// The claim comes first: two byte-identical parts share one blob, and without
// it two workers would both see Exists answer no and upload the same bytes
// twice. Skipping instead of waiting is sound because a failed upload fails
// the whole push — there is no path where a skipped part needs the blob and
// the claiming worker's upload quietly never happened.
//
// The upload reads from a section reader opened here and nowhere else: the
// reader the hash pass used is spent, and a blob upload consumes what it is
// given exactly once. The file is the transfer's buffer, so re-reading a
// range costs a read and no memory.
func uploadPart(ctx context.Context, blobs Blobs, source Source, claims *claimSet, job partJob) error {
	if !claims.claim(job.dgst) {
		return nil
	}

	exists, err := blobs.Exists(ctx, job.dgst)
	if err != nil {
		return fmt.Errorf("check whether part %d (%s) exists: %w", job.part.Index, job.dgst, err)
	}
	if exists {
		return nil
	}

	content := io.NewSectionReader(source, job.part.Offset, job.part.Size)
	if err := blobs.Put(ctx, job.dgst, job.part.Size, content); err != nil {
		return fmt.Errorf("upload part %d (%s): %w", job.part.Index, job.dgst, err)
	}

	return nil
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
func ensureEmptyConfig(ctx context.Context, blobs Blobs) error {
	descriptor, content := manifest.EmptyConfig()

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
}

// writeManifest encodes the artifact and writes it at the bound reference,
// returning the descriptor of what it wrote. The size in the descriptor is
// the length of the bytes that were sent, and the digest is the one the port
// computed over those same bytes.
func writeManifest(ctx context.Context, manifests Manifests, artifact manifest.Artifact) (ocispec.Descriptor, error) {
	body, err := manifest.Encode(artifact)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("encode the manifest: %w", err)
	}

	dgst, err := manifests.Put(ctx, ocispec.MediaTypeImageManifest, body)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("write the manifest: %w", err)
	}

	return ocispec.Descriptor{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: manifest.ArtifactType,
		Digest:       dgst,
		Size:         int64(len(body)),
	}, nil
}
