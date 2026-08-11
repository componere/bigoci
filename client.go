package bigoci

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/imgoci/bigoci/internal/file"
	"github.com/imgoci/bigoci/internal/oci"
	"github.com/imgoci/bigoci/internal/plan"
	"github.com/imgoci/bigoci/internal/transfer"
)

// Reference names one artifact: a registry, a repository on it, and a tag or
// a digest, written registry/repo[:tag][@digest].
//
// The grammar is the one container tooling uses, parsed by
// github.com/distribution/reference. Four rules follow from it. The registry
// is required, so "team/model:v1" is not a reference and no short name is
// quietly expanded to Docker Hub. The name must be canonical, which means
// lowercase. A reference must carry a tag or a digest, because every
// transfer names one manifest. And a digest must be sha256 — the only
// algorithm the v1 format uses — so a reference naming another one is
// refused before a request is made.
//
// A reference that carries both is bound to its digest: the digest names one
// manifest exactly, while the tag beside it is a claim about where that tag
// pointed. Pulling by digest also makes bigoci check the manifest it fetched
// against it.
type Reference string

// Client pushes and pulls bigoci artifacts.
//
// A Client holds transfer-wide settings, its credential resolver, and one
// lazily prepared external connection pool — no state from one repository or
// transfer. A single client therefore serves any number of concurrent pushes
// and pulls against any number of repositories. [New] builds one; the zero
// value is usable and behaves as if built with no options.
type Client struct {
	// settings are the transport settings the options collected; the fields
	// are documented on clientSettings, their one home.
	settings clientSettings
	// creds resolves what a transfer presents to a registry that asks, nil
	// when no option named a source. Built once, by [New], and shared by every
	// transfer the client carries.
	creds oci.Credentials
	// external owns the lazily prepared external connection pool. It is a
	// pointer so copying a Client keeps the copy-safe semantics it had before
	// this state existed: both values share the concurrency-safe pool.
	external *externalTransportState
}

// externalTransportState lazily prepares one caller-derived transport pool.
// It lives behind a pointer in Client so the public value does not acquire a
// no-copy-after-use contract from [sync.Once].
type externalTransportState struct {
	// once serializes preparation across concurrent repositories.
	once sync.Once
	// base owns the shared caller-derived connection pool after preparation.
	base *oci.ExternalTransportBase
}

// zeroClientExternal is the shared default state used without mutating a
// zero-value Client, including when several zero-value clients start at once.
var zeroClientExternal externalTransportState //nolint:gochecknoglobals // Zero Client values need shared copy-safe lazy state.

// New returns a client configured by opts.
//
// It reports an error when an option cannot be applied, which today is one
// case: [WithDockerCredentials] records the intent to use the credentials
// `docker login` stores, and this is where they are read. A configuration file
// that is not there — or a machine that cannot even name where one would be,
// with no home directory and no $DOCKER_CONFIG — is not an error: that is a
// machine nobody has logged in on, and every registry resolves to the
// anonymous credential. A file that exists but cannot be read as a
// configuration is, because a caller who asked bigoci to use their
// credentials would otherwise watch it transfer without them and fail
// somewhere less obvious.
//
// A client built with no credential option is not a client with
// authentication turned off. A registry that asks for a token still gets the
// full exchange, made anonymously, which is what registries that hand out
// public-read tokens expect. It only means bigoci has no user name or secret
// to offer when the exchange asks for one.
func New(opts ...Option) (*Client, error) {
	var applied clientSettings
	for _, opt := range opts {
		opt(&applied)
	}

	client := &Client{settings: applied, external: &externalTransportState{}}

	if applied.credentials != nil {
		creds, err := applied.credentials()
		if err != nil {
			return nil, err
		}

		client.creds = creds
	}

	return client, nil
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
// 5xx, a connection that dropped — costs up to four attempts per part in
// total, with an exponentially growing jittered wait that never falls short
// of a Retry-After the registry sent. Everything else fails at once. A push that
// gives up leaves whichever parts had already landed in the repository,
// unreferenced, and a later push finds and skips them.
//
// A cancelled ctx stops the workers, cuts short any wait in progress, and
// returns its error. The errors a caller branches on are [ErrNotFound],
// [ErrUnauthorized], and [ErrPartTooLarge].
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
// connection that dropped mid-part — is fetched again, up to four attempts in
// total, with an exponentially growing jittered wait that never falls short
// of a Retry-After the registry sent. Those attempts carry on from the byte the
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
// [ErrUnauthorized], [ErrNotBigociArtifact], and [ErrDigestMismatch].
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
		Progress:  progressReport(DirectionPush, settings.progress),
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
		Progress:  progressReport(DirectionPull, settings.progress),
	}); err != nil {
		discardEmptyPartial(sink)

		return fmt.Errorf("pull %s to %s: %w", ref, dest.path, err)
	}

	return nil
}

// progressReport adapts the orchestrator's snapshots to the [Progress] a
// caller asked for, stamping in the direction the core does not carry: a
// transfer moves a file between the same two ends whichever way it is going,
// and only the call knows which way that is.
//
// It returns nil when no option named a callback, which is what keeps an
// unwatched transfer free of the accounting rather than merely quiet about
// it.
func progressReport(direction Direction, fn ProgressFunc) transfer.Report {
	if fn == nil {
		return nil
	}

	return func(s transfer.Snapshot) {
		fn(Progress{
			Direction:      direction,
			Phase:          publicPhase(s.Phase),
			TotalBytes:     s.TotalBytes,
			TotalParts:     s.TotalParts,
			CompletedBytes: s.CompletedBytes,
			CompletedParts: s.CompletedParts,
			SkippedParts:   s.SkippedParts,
			WireBytes:      s.WireBytes,
			HashedBytes:    s.HashedBytes,
			Retries:        s.Retries,
		})
	}
}

// publicPhase maps a core phase onto the one this package exports.
//
// The two enums are declared independently — one is a public contract, the
// other is an implementation detail free to gain a phase — so this is a
// switch and never a numeric conversion. A phase added on either side shows
// up here as a case that has to be answered, instead of silently renumbering
// the other side's constants.
func publicPhase(p transfer.Phase) Phase {
	switch p {
	case transfer.PhaseResolving:
		return PhaseResolving
	case transfer.PhaseTransferring:
		return PhaseTransferring
	case transfer.PhaseFinalizing:
		return PhaseFinalizing
	case transfer.PhaseDone:
		return PhaseDone
	case transfer.PhaseFailed:
		return PhaseFailed
	default:
		return 0
	}
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
	options := []oci.Option{
		oci.WithHTTPClient(c.settings.httpClient),
		oci.WithCredentials(c.creds),
		oci.WithExternalTransportBase(c.externalTransportBase()),
	}
	if c.settings.plainHTTP {
		options = append(options, oci.WithPlainHTTP())
	}
	if c.settings.allowUnverifiedExternal {
		options = append(options, oci.WithUnverifiedExternalTransport())
	}

	return oci.NewRepository(string(ref), options...)
}

// externalTransportBase returns the one caller-derived external transport
// pool shared by every repository this client builds.
func (c *Client) externalTransportBase() *oci.ExternalTransportBase {
	state := c.external
	if state == nil {
		state = &zeroClientExternal
	}

	state.once.Do(func() {
		if c.settings.httpClient == nil {
			state.base = oci.NewExternalTransportBase(nil)

			return
		}

		state.base = oci.NewExternalTransportBase(c.settings.httpClient.Transport)
	})

	return state.base
}
