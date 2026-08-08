package bigoci

import (
	"context"
	"fmt"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/componere/bigoci/internal/file"
	"github.com/componere/bigoci/internal/oci"
	"github.com/componere/bigoci/internal/plan"
	"github.com/componere/bigoci/internal/transfer"
)

// Reference names one artifact: a registry, a repository on it, and a tag or
// a digest, written registry/repo[:tag][@digest].
//
// The grammar is the one container tooling uses, parsed by
// github.com/distribution/reference. Three rules follow from it. The registry
// is required, so "team/model:v1" is not a reference and no short name is
// quietly expanded to Docker Hub. The name must be canonical, which means
// lowercase. And a reference must carry a tag or a digest, because every
// transfer names one manifest.
//
// A reference that carries both is bound to its digest: the digest names one
// manifest exactly, while the tag beside it is a claim about where that tag
// pointed. Pulling by digest also makes bigoci check the manifest it fetched
// against it.
type Reference string

// Client pushes and pulls bigoci artifacts.
//
// A Client holds the transport settings and nothing else — no connection to
// one registry, no state from one transfer — so a single client serves any
// number of concurrent pushes and pulls against any number of repositories.
// [New] builds one; the zero value is usable and behaves as if built with no
// options.
type Client struct {
	// settings are the transport settings the options collected; the fields
	// are documented on clientSettings, their one home.
	settings clientSettings
}

// New returns a client configured by opts.
//
// It reports an error when an option cannot be applied. None can today; the
// error is in the signature because the options that will are known — loading
// credentials from the Docker configuration is the case the design names —
// and adding it later would break every caller.
func New(opts ...Option) (*Client, error) {
	var applied clientSettings
	for _, opt := range opts {
		opt(&applied)
	}

	return &Client{settings: applied}, nil
}

// Push uploads the file src names to ref and returns the descriptor of the
// manifest it wrote.
//
// The file is split into parts of [DefaultPartSize] bytes, or of whatever
// [WithPartSize] asks for. One sequential read hashes the file and hands each
// part to the workers as its digest completes, so uploading overlaps hashing.
// Parts the registry already holds are not uploaded again, which makes a
// re-push of an unchanged file nearly free and an interrupted push resume
// without bookkeeping. The manifest is written last, once every blob it
// references exists, so a push that dies leaves no artifact behind — only
// unreferenced blobs the registry collects.
//
// The returned descriptor names the manifest by digest. That digest is a pure
// function of the file bytes, the part size, and the title, so pushing the
// same file twice reproduces it, and anything bound to it — a signature, an
// index entry — survives a re-push.
//
// Push reads the file once, from start to end, and re-reads a part only to
// upload it. Nothing buffers a part in memory whatever the part size is. The
// file must not change while the push runs.
//
// A failure the registry or the network says is worth repeating — a 429, a
// 5xx, a connection that dropped — is retried up to four times per part, with
// an exponentially growing jittered wait that never falls short of a
// Retry-After the registry sent. Everything else fails at once. A push that
// gives up leaves whichever parts had already landed in the repository,
// unreferenced, and a later push finds and skips them.
//
// A cancelled ctx stops the workers, cuts short any wait in progress, and
// returns its error. The errors a caller branches on are [ErrNotFound] and
// [ErrPartTooLarge].
func (c *Client) Push(
	ctx context.Context,
	ref Reference,
	src FileSource,
	opts ...PushOption,
) (ocispec.Descriptor, error) {
	desc, err := c.push(ctx, ref, src, opts)
	if err != nil {
		return ocispec.Descriptor{}, classify(err)
	}

	return desc, nil
}

// Pull downloads the artifact ref names into the file dest names.
//
// It fetches the manifest, checks that it is a bigoci artifact, and reads the
// part size and the part digests out of it, so nothing about the transfer has
// to be told twice. Workers then fetch parts in parallel and stream them
// straight into their byte ranges of a partial file, hashing as they write.
// Every part is verified against the digest the manifest gives it, and the
// destination is published with one atomic rename once all of them pass.
//
// The destination is therefore never observed half written: a pull that
// fails, is cancelled, or is killed leaves the destination absent or holding
// its previous content, and leaves the partial file beside it.
//
// Those bytes are what the next pull resumes from. When the partial file is
// already the length the manifest declares, Pull hashes each part out of it
// and asks the registry only for the parts that do not match — a rerun after
// an interrupted pull moves what is missing and
// nothing else, and one that finds everything intact moves no blob bytes at
// all. Nothing is recorded between runs: a range no run ever wrote reads back
// as zeros and fails its check, and a partial file of any other length belongs
// to some other artifact and is refilled from the start.
//
// A part whose fetch breaks in a way worth repeating — a 429, a 5xx, a
// connection that dropped mid-part — is fetched again, up to four times, with
// an exponentially growing jittered wait that never falls short of a
// Retry-After the registry sent. Those attempts carry on from the byte the
// broken one reached, unless the registry will not serve a byte range, in
// which case it answers with the whole blob and the part is simply written
// again from its first byte. A part that arrives whole and hashes wrong is not
// retried: that is the registry serving content the artifact does not
// describe, and asking again gives the same answer.
//
// Verification rests on the manifest digest, exactly as pulling a container
// image does. A caller who names a digest reference gets that chain checked
// end to end: the manifest is verified against the digest it was asked for,
// and every part against the manifest.
//
// A cancelled ctx stops the workers, cuts short any wait in progress, and
// returns its error. The errors a caller branches on are [ErrNotFound],
// [ErrNotBigociArtifact], and [ErrDigestMismatch].
func (c *Client) Pull(ctx context.Context, ref Reference, dest FileDest, opts ...PullOption) error {
	if err := c.pull(ctx, ref, dest, opts); err != nil {
		return classify(err)
	}

	return nil
}

// push runs one push and returns the raw error, which [Client.Push] maps onto
// the public sentinels. Splitting the two keeps that mapping in one place
// instead of at every return.
func (c *Client) push(
	ctx context.Context,
	ref Reference,
	src FileSource,
	opts []PushOption,
) (ocispec.Descriptor, error) {
	// The title defaults from the source before the options run, so
	// [WithTitle] overrides it and WithTitle("") clears it.
	settings := pushSettings{
		partSize: DefaultPartSize,
		title:    filepath.Base(src.path),
		workers:  DefaultWorkers,
	}
	for _, opt := range opts {
		opt.applyPush(&settings)
	}
	if err := settings.validate(); err != nil {
		return ocispec.Descriptor{}, err
	}

	repo, err := c.repository(ref)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	source, err := file.OpenSource(src.path)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer func() { _ = source.Close() }()

	desc, err := transfer.Push(ctx, transfer.PushSpec{
		Source:    source,
		Blobs:     repo.Blobs(),
		Manifests: repo.Manifests(),
		PartSize:  plan.PartSize(settings.partSize),
		Workers:   settings.workers,
		Title:     settings.title,
	})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push %s to %s: %w", src.path, ref, err)
	}

	return desc, nil
}

// pull runs one pull and returns the raw error, which [Client.Pull] maps onto
// the public sentinels.
func (c *Client) pull(ctx context.Context, ref Reference, dest FileDest, opts []PullOption) error {
	settings := pullSettings{workers: DefaultWorkers}
	for _, opt := range opts {
		opt.applyPull(&settings)
	}
	if err := settings.validate(); err != nil {
		return err
	}

	repo, err := c.repository(ref)
	if err != nil {
		return err
	}

	sink, err := file.CreateSink(dest.path)
	if err != nil {
		return err
	}
	// Close is idempotent and does nothing after the transfer committed, so
	// this releases the handle of a failed pull without touching the file a
	// successful one published — and leaves the partial behind either way.
	defer func() { _ = sink.Close() }()

	if err := transfer.Pull(ctx, transfer.PullSpec{
		Sink:      sink,
		Blobs:     repo.Blobs(),
		Manifests: repo.Manifests(),
		Workers:   settings.workers,
	}); err != nil {
		discardEmptyPartial(sink)

		return fmt.Errorf("pull %s to %s: %w", ref, dest.path, err)
	}

	return nil
}

// discardEmptyPartial removes the partial file a failed pull created when it
// holds nothing: a reference that resolved to no artifact, or to something
// that is not one, should not litter the destination directory on its way to
// an error. A partial with any content stays where it is — those bytes are
// what a later pull resumes from.
func discardEmptyPartial(sink *file.Sink) {
	size, err := sink.Size()
	if err != nil || size != 0 {
		return
	}

	_ = sink.Discard()
}

// repository builds the registry adapters for ref under the client's
// transport settings. Reference grammar lives in the adapter, so this is
// where a malformed reference is reported.
func (c *Client) repository(ref Reference) (*oci.Repository, error) {
	options := []oci.Option{oci.WithHTTPClient(c.settings.httpClient)}
	if c.settings.plainHTTP {
		options = append(options, oci.WithPlainHTTP())
	}

	return oci.NewRepository(string(ref), options...)
}
