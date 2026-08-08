package bigoci

import (
	"fmt"
	"net/http"
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
// noise. It is a provisional value: the benchmark harness sets the measured
// one before v1.
const DefaultPartSize PartSize = 512 << 20

// DefaultWorkers is how many parts a push or a pull moves at once when the
// caller names no worker count.
//
// One worker holds one connection, and four saturate a 2 to 3 Gbit/s path
// against the throughput a single registry connection sustains. Callers with
// a bigger pipe raise it with [WithWorkers].
const DefaultWorkers = 4

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
// [WithWorkers] alone: part size and title describe how a file is stored and
// belong to the push that decides it.
type TransferOption interface {
	PushOption
	PullOption
}

// WithHTTPClient sends every registry request with client instead of the
// default one.
//
// This is the seam for timeouts, proxies, connection pool tuning, and — once
// bigoci grows authentication — an authenticating
// [net/http.RoundTripper]. A nil client is ignored, so a caller may pass one
// through unconditionally.
func WithHTTPClient(client *http.Client) Option {
	return func(s *clientSettings) {
		if client != nil {
			s.httpClient = client
		}
	}
}

// WithPlainHTTP talks http:// to the registry instead of https://.
//
// Local registries — zot or CNCF Distribution in a container, a test fixture
// — serve plain HTTP. Nothing else should.
func WithPlainHTTP() Option {
	return func(s *clientSettings) {
		s.plainHTTP = true
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
// under the registry's layer cap. Lowering it makes a failed part cheaper to
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
