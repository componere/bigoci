package bigoci

import (
	"fmt"
	"net/http"

	"github.com/componere/bigoci/internal/auth"
	"github.com/componere/bigoci/internal/oci"
)

// PartSize is the size in bytes of the parts a file is split into: the P of
// the format's split rule, where part i covers bytes
// [i*P, min((i+1)*P, size)). It is a distinct type so a part size cannot be
// transposed with a file size or a worker count.
type PartSize int64

// DefaultPartSize is the part size a push splits at when the caller names
// none.
//
// 512 MiB sits roughly 19 times under the lowest registry layer cap, makes a
// 5 GB file into ten parts, and keeps the per-part request overhead in the
// noise. The value is measured, not provisional: with eight workers it came
// within 1.6 percent of the best 16 GiB zot cell and one percent of the best
// CNCF Distribution cell. Against GHCR its push and pull medians were within
// 2.5 and 4.1 percent of 256 MiB parts, while smaller parts paid visible
// per-part overhead at local scale (bare-metal matrix, 2026-08; see
// https://componere.github.io/bigoci/reference/benchmarks/).
const DefaultPartSize PartSize = 512 << 20

// DefaultWorkers is how many parts a push or a pull moves at once when the
// caller names no worker count.
//
// With 512 MiB parts, moving from four to eight workers left aggregate push
// throughput effectively flat against zot, CNCF Distribution, and GHCR. It
// raised GHCR's aggregate pull median from 161.6 to 262.1 MB/s, while every
// same-site median changed by at most 1.3 percent. The corrected GHCR matrix
// kept all eight workers active and recorded no 429 or 503 response
// (bare-metal matrix, 2026-08; see
// https://componere.github.io/bigoci/reference/benchmarks/). Callers can
// override the count with [WithWorkers].
const DefaultWorkers = 8

// Option configures a [Client] as [New] builds it.
//
// The function signature names a type this package keeps to itself, which
// seals the set: the only Options that exist are the ones declared here, so
// [New] can never be handed a knob the client does not know how to honor.
type Option func(*clientSettings)

// PushOption configures one call to [Client.Push].
//
// The interface is sealed by an unexported method: the options are the ones
// this package ships, so a push cannot be handed a knob the library does not
// know how to honor.
type PushOption interface {
	// applyPush records the option on the settings one push runs with.
	applyPush(*pushSettings)
}

// PullOption configures one call to [Client.Pull]. It is sealed the same way
// [PushOption] is.
type PullOption interface {
	// applyPull records the option on the settings one pull runs with.
	applyPull(*pullSettings)
}

// TransferOption configures either direction. An option is one of these when
// what it sets means the same thing to a push and to a pull, which today is
// [WithWorkers] and [WithProgress]: how many parts move at once, and who
// watches them move. Part size and title describe how a file is stored and
// belong to the push that decides it.
type TransferOption interface {
	PushOption
	PullOption
}

// WithHTTPClient sends every registry request with client instead of the
// default one.
//
// This is the seam for timeouts, proxies, connection pool tuning, and a
// credential source bigoci does not have — an authenticating
// [net/http.RoundTripper] such as go-containerregistry's keychain. A nil
// client is ignored, so a caller may pass one through unconditionally.
//
// bigoci retries failed transfers itself, so a transport that also retries
// multiplies the schedule: the attempts a part gets become the product of the
// two counts, and the waits between them stack. A client Timeout bounds each
// request rather than the transfer or even one attempt of it — an attempt
// that authenticates and follows a redirect chain makes several requests,
// each with a fresh window — so the deadline a caller means to put on the
// whole transfer belongs on the context, not on the client.
//
// A transport that adds Authorization unconditionally will add it to the
// re-issue of a redirect too. bigoci strips the header on its way to a host
// the registry named, but a transport sits below that decision by
// construction: whatever it sets, it sets on the request bigoci already
// cleaned. Large registries answer a blob read with a redirect to object
// storage on a URL that is already signed, so a credential added there is a
// credential in somebody else's logs, on a request that would have worked
// without it. The fix is a host check in the transport — set the header only
// when req.URL.Host is the registry the transport was built for, comparing
// hosts and not domains. The authentication how-to shows it in full.
//
// bigoci copies this client rather than using it, and never writes to the
// original: a program that shares one client with the rest of its work gets it
// back exactly as it handed it over. The copies keep the transport and the
// timeout, and set a redirect policy of their own — bigoci decides what a
// re-issued request may carry, which is the whole of the paragraph above. The
// external copy carries no cookie jar. Ambient cookies therefore do not reach
// a token realm, an off-origin upload session, or a blob redirect target that
// the registry selected. Explicit protocol credentials remain unchanged.
//
// For registry-selected cross-host endpoints, bigoci clones a concrete
// [http.Transport] once and checks the actual direct connection's peer before
// HTTP request bytes leave. Its TLS roots and pool tuning are preserved in one
// separate cross-host pool; requests to the registry's own hostname keep the
// caller's original pool. A proxy, an opaque RoundTripper, a custom dial hook,
// or a caller-supplied TLSNextProto handler can hide that final destination, so
// cross-host requests through one fail closed unless
// [WithUnverifiedExternalTransport] explicitly authorizes that trust boundary.
// The standard dial hook inherited from
// [http.DefaultTransport] remains automatic.
func WithHTTPClient(client *http.Client) Option {
	return func(s *clientSettings) {
		if client != nil {
			s.httpClient = client
		}
	}
}

// WithUnverifiedExternalTransport authorizes registry-selected cross-host
// requests through a custom dial hook, proxy, or opaque [http.RoundTripper]
// whose final network destination bigoci cannot verify.
//
// This is a security escape hatch for callers whose transport boundary
// already enforces an equivalent destination policy. It is unnecessary for a
// direct [http.Transport] that uses the standard dial hook, including one with
// custom TLS roots. Any caller-supplied Dial, DialContext, DialTLS, or
// DialTLSContext hook requires this option for cross-host requests because the
// hook may return a tunnel whose RemoteAddr identifies its proxy rather than
// the registry-selected endpoint. A transport using
// [http.Transport.RegisterProtocol] also requires it: Go does not expose those
// handlers and [http.Transport.Clone] intentionally does not copy them.
//
// The option does not permit a realm or upload Location that directly names a
// private IP address. For every other cross-host target it uses the caller's
// original transport unchanged, preserving registered protocols, proxy and
// dial behavior, and connection pooling. The caller therefore owns the whole
// destination check behind this option; bigoci does not claim an actual-peer
// check on that explicitly trusted path.
func WithUnverifiedExternalTransport() Option {
	return func(s *clientSettings) {
		s.allowUnverifiedExternal = true
	}
}

// WithPlainHTTP talks http:// to the registry instead of https://.
//
// Everything a transfer sends rides unencrypted under it, credentials and
// token exchanges included, which is one more reason it is for local
// registries only. A token endpoint a plain-HTTP registry names is refused
// unless it is the registry's own host.
//
// Local registries — zot or CNCF Distribution in a container, a test fixture
// — serve plain HTTP. Nothing else should.
func WithPlainHTTP() Option {
	return func(s *clientSettings) {
		s.plainHTTP = true
	}
}

// WithDockerCredentials authenticates with the credentials `docker login`
// stores: the entries in the Docker configuration file, and whatever the
// credential helpers that file names print for a registry.
//
// It is opt-in, and the opt is the point. Reading a user's configuration is
// one thing; a configuration that names a credential helper makes a lookup
// into running someone else's program, and a library that did that because it
// was linked in would be a surprise with a security dimension. Naming this
// option is the consent for both.
//
// The file is $DOCKER_CONFIG/config.json where that variable is set, and
// .docker/config.json under the user's home otherwise. [New] reads it, so a
// file that cannot be parsed fails there rather than in the middle of a
// transfer, and a file that is not there is not a failure at all: that is a
// machine nobody has logged in on, and every registry resolves to the
// anonymous credential. Helpers are asked afresh at every lookup, but the file
// itself is read once, so a `docker login` run during a transfer does not
// reach it.
//
// bigoci only ever reads. No transfer writes a credential anywhere.
func WithDockerCredentials() Option {
	return func(s *clientSettings) {
		s.credentials = dockerCredentials
	}
}

// WithCredentials presents username and secret to whatever registry a transfer
// dials.
//
// It is the direct route, for a caller who already holds the secret: a CI job
// with a registry token in its environment, or a program that reads its own
// configuration. Nothing is looked up, no file is read, and no program is run.
// secret is a password, or — at most registries today — a personal access
// token.
//
// Every registry is the deliberate part. The credential goes to whatever host
// the reference names, so the caller, who chose both the secret and the
// reference, is the one deciding who sees it.
// [WithDockerCredentials] is the other shape: it answers only for the hosts a
// login was stored under.
//
// Naming both options leaves the last one named in effect.
func WithCredentials(username, secret string) Option {
	return func(s *clientSettings) {
		s.credentials = func() (oci.Credentials, error) {
			return auth.NewStatic(oci.Credential{Username: username, Password: secret}), nil
		}
	}
}

// WithPartSize splits the pushed file into parts of size bytes, in place of
// [DefaultPartSize]. The last part is whatever is left over.
//
// The part size is recorded in the manifest, so a pull never guesses it, and
// it is part of what the manifest digest describes: the same file at two part
// sizes is two artifacts. size must be positive, which [Client.Push] checks
// before it opens anything.
//
// Raising it trades parallelism for fewer, larger requests and has to stay
// under the registry's layer cap. A registry that refuses a part for being
// larger than it accepts reports [ErrPartTooLarge]; the answer is to push
// again with a smaller size. Lowering it makes a failed part cheaper to
// re-push; the format allows at most 4096 parts, so a very small part size
// puts a ceiling on the file.
func WithPartSize(size PartSize) PushOption {
	return partSizeOption(size)
}

// WithTitle records title as the artifact's file name annotation instead of
// the base name of the pushed file.
//
// The annotation is informational: it travels in the standard
// org.opencontainers.image.title key so registry UIs and generic tools can
// show what the artifact holds. An empty title writes no annotation at all.
func WithTitle(title string) PushOption {
	return titleOption(title)
}

// WithWorkers moves n parts at once, in place of [DefaultWorkers]. n must be
// positive, which [Client.Push] and [Client.Pull] check before they open
// anything.
//
// Each worker holds one connection to the registry for the length of a part,
// so this is the knob that trades connections for throughput.
func WithWorkers(n int) TransferOption {
	return workersOption(n)
}

// clientSettings are the option-configurable parts of a client, collected so
// [New] can apply every option before it builds the immutable [Client].
type clientSettings struct {
	// httpClient sends every registry request, nil when the caller named
	// none.
	httpClient *http.Client
	// plainHTTP talks http:// to the registry instead of https://.
	plainHTTP bool
	// allowUnverifiedExternal authorizes cross-registry requests through a
	// custom dial hook, proxy, or opaque transport whose final destination
	// bigoci cannot observe.
	allowUnverifiedExternal bool
	// credentials builds the source a transfer resolves credentials through,
	// nil when no option named one. It is a builder rather than a built source
	// because building one can fail — reading the Docker configuration is the
	// case — and [New] is where a caller can still be told about it.
	credentials func() (oci.Credentials, error)
}

// dockerCredentials builds the credential source [WithDockerCredentials]
// names: the Docker configuration file wherever this machine keeps it.
//
// A machine that cannot say where its configuration would be — no home
// directory and no $DOCKER_CONFIG, the shape of a scratch container — has no
// configuration, which is the same answer as a configuration file that does
// not exist: no source is installed and every registry resolves anonymously.
// The error [New] reports is reserved for a configuration that exists and
// cannot be read, because that is the one case where failing quietly would
// hide a credential the user meant to be used.
func dockerCredentials() (oci.Credentials, error) {
	path, err := auth.DefaultConfigPath()
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // no locatable configuration is the anonymous case, not a failure
	}

	return auth.NewStore(path)
}

// pushSettings are the knobs one push runs with, once the defaults and the
// caller's options have been applied.
type pushSettings struct {
	// partSize is the size the file is split at.
	partSize PartSize
	// title is the file name the manifest records, empty for no annotation.
	title string
	// workers is how many parts upload at once.
	workers int
	// progress receives the push's snapshots, nil when nobody is watching.
	progress ProgressFunc
}

// validate rejects settings that cannot describe a transfer. [Client.Push]
// calls it before it opens the file or builds a request, so a caller's typo
// costs nothing.
func (s pushSettings) validate() error {
	if s.partSize <= 0 {
		return fmt.Errorf("part size must be positive, got %d", s.partSize)
	}

	return validateWorkers(s.workers)
}

// pullSettings are the knobs one pull runs with. A pull reads the part size
// and the title from the manifest, so the worker count is all it has to be
// told.
type pullSettings struct {
	// workers is how many parts download at once.
	workers int
	// progress receives the pull's snapshots, nil when nobody is watching.
	progress ProgressFunc
}

// validate rejects settings that cannot describe a transfer. [Client.Pull]
// calls it before it creates the partial file or builds a request.
func (s pullSettings) validate() error {
	return validateWorkers(s.workers)
}

// partSizeOption carries [WithPartSize].
type partSizeOption PartSize

// applyPush records the part size the push splits at.
func (o partSizeOption) applyPush(s *pushSettings) {
	s.partSize = PartSize(o)
}

// titleOption carries [WithTitle].
type titleOption string

// applyPush records the title the manifest annotation carries.
func (o titleOption) applyPush(s *pushSettings) {
	s.title = string(o)
}

// workersOption carries [WithWorkers]. It implements both directions'
// interfaces, which is what makes it a [TransferOption].
type workersOption int

// applyPush records how many parts upload at once.
func (o workersOption) applyPush(s *pushSettings) {
	s.workers = int(o)
}

// applyPull records how many parts download at once.
func (o workersOption) applyPull(s *pullSettings) {
	s.workers = int(o)
}

// validateWorkers rejects a worker count that would move nothing. Zero is the
// value a caller lands on by forgetting to set one, and it would otherwise
// deadlock a transfer rather than fail it.
func validateWorkers(n int) error {
	if n <= 0 {
		return fmt.Errorf("worker count must be positive, got %d", n)
	}

	return nil
}
