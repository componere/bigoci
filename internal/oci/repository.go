package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/distribution/reference"
	ociblob "github.com/imgoci/go-oci-blob"
	digest "github.com/opencontainers/go-digest"

	"github.com/imgoci/bigoci/internal/retry"
)

// The URL schemes a repository talks. Registries are https; plain HTTP is
// opt-in because a registry that speaks it is a local one.
const (
	// schemeHTTPS is the scheme a repository uses unless [WithPlainHTTP] says
	// otherwise.
	schemeHTTPS = "https"
	// schemeHTTP is the scheme [WithPlainHTTP] selects.
	schemeHTTP = "http"
)

// apiPrefix is the path prefix every distribution-spec endpoint hangs off.
const apiPrefix = "/v2/"

// The two headers a credential travels in.
const (
	// headerAuthorization is the request header a credential rides in. Four
	// places set it, and no more: [Repository.newRequest], [Repository.replay],
	// the token exchange's SetBasicAuth — which is safe because the request it
	// authenticates is one this package built for the realm, never one that
	// leaves for the registry or anywhere a caller named — and the same-origin
	// arm of [Repository.nextHop], which carries it onto a re-issue only when
	// the location the registry named is the registry itself. That closed list
	// is what makes "where can a credential go" a reviewable question.
	headerAuthorization = "Authorization"
	// headerChallenge is the response header a registry states its
	// authentication requirement in.
	headerChallenge = "WWW-Authenticate"
)

// errorBodyLimit caps how many bytes of a response body this package reads
// when it is not the content the caller asked for: the detail retained on a
// [StatusError], and the drain that lets a connection go back in the pool.
// Registries answer with a short JSON document, so the cap only bites when
// something other than a registry is on the other end.
const errorBodyLimit = 4096

// ErrNotFound reports that the registry does not hold what a request named: a
// manifest, or a blob [Blobs.Get] tried to read. [Blobs.Exists] never returns
// it, because "the registry does not hold this blob" is the answer that call
// asks for rather than a failure. A 404's [StatusError] matches it through
// [errors.Is]; nothing wraps the sentinel in a second layer.
var ErrNotFound = errors.New("not found")

// ErrUnauthorized reports that the registry refused a request rather than
// answering it: it wants credentials the request did not carry, or the ones
// it carried do not reach what the request asked for. A 401 or 403 registry
// error matches it through [errors.Is], the same way a 404 matches
// [ErrNotFound]; nothing wraps the sentinel in a second layer.
//
// The sentinel means "your credentials" and nothing else, which is what makes
// it worth branching on: every failure it names is answered by logging in or
// by being granted access, and no further attempt changes either.
//
// Admitting the 403 beside the 401 is deliberate — a registry refuses a
// credential it could not read, or one that falls short of the access asked
// for, with either status. The cost is that a 403 from something standing in
// front of the registry, a proxy or a web application firewall, reports as
// unauthorized too. That is admitted the way [ErrTooLarge] admits a 413
// answering a manifest write: sniffing the body to tell the two apart would
// be the vendor table again in a different shape.
var ErrUnauthorized = ociblob.ErrUnauthorized

// ErrTooLarge reports that the registry refused a request as larger than it
// accepts. A 413 registry error matches it through [errors.Is], the same way a
// 404 matches [ErrNotFound].
//
// A 413 is how a registry states its layer cap. bigoci ships no table of
// vendor limits — the caps differ per registry, they move, and a stale table
// is worse than none — so the limit is discovered by being told about it,
// once, by the registry that enforces it.
//
// The mapping is the status and nothing else, which means a 413 answering a
// manifest write would match too. That is admitted rather than guarded
// against: a bigoci manifest is a few hundred kilobytes at the format's own
// part cap, no registry rejects one as too large, and sniffing the body to
// tell the two apart would be the vendor table again in a different shape.
var ErrTooLarge = ociblob.ErrTooLarge

// Option configures a [Repository] as it is built. The set is deliberately
// small: everything else about a repository comes from the reference it is
// built from.
type Option func(*settings)

// settings are the option-configurable parts of a repository, collected so
// [NewRepository] can apply every option before it builds the immutable
// [Repository].
type settings struct {
	// client sends every request the repository makes.
	client *http.Client
	// scheme is the URL scheme, https unless [WithPlainHTTP] changes it.
	scheme string
	// creds resolves the credential a challenge is answered with, nil when the
	// caller configured none.
	creds Credentials
	// now reads the clock token expiry is measured on, [time.Now] unless a
	// test replaced it.
	now func() time.Time
	// allowUnverifiedExternal permits cross-registry requests through a custom
	// dial hook, proxy, or opaque transport whose final destination bigoci
	// cannot observe.
	allowUnverifiedExternal bool
	// externalBase is the caller-derived transport pool shared by repositories
	// from one public client, nil for a standalone internal repository.
	externalBase *ExternalTransportBase
}

// WithHTTPClient sends the repository's requests with client instead of the
// default one. A nil client is ignored, so a caller may pass one through
// unconditionally. The public package's option of the same name carries the
// rationale.
func WithHTTPClient(client *http.Client) Option {
	return func(s *settings) {
		if client != nil {
			s.client = client
		}
	}
}

// WithUnverifiedExternalTransport authorizes registry-selected cross-host
// requests through a custom dial hook, proxy, or opaque transport whose final
// destination this adapter cannot verify. The public option of the same name
// documents the security boundary callers opt out of.
func WithUnverifiedExternalTransport() Option {
	return func(s *settings) {
		s.allowUnverifiedExternal = true
	}
}

// WithExternalTransportBase shares base with this repository's
// registry-selected external requests. The root package supplies it so one
// public Client keeps one external connection pool across transfers.
func WithExternalTransportBase(base *ExternalTransportBase) Option {
	return func(s *settings) {
		if base != nil {
			s.externalBase = base
		}
	}
}

// WithPlainHTTP talks http:// to the registry instead of https://, for local
// registries and test fixtures. Nothing else should use it.
func WithPlainHTTP() Option {
	return func(s *settings) {
		s.scheme = schemeHTTP
	}
}

// withClock reads the clock token expiry is measured on from now instead of
// [time.Now]. A nil function is ignored.
//
// It exists for the tests, which have to watch a token cross its expiry
// without waiting a minute for it, and it is unexported because a caller has
// no business moving the clock a live transfer authenticates against.
func withClock(now func() time.Time) Option {
	return func(s *settings) {
		if now != nil {
			s.now = now
		}
	}
}

// bound is the manifest reference a repository is fixed to for its lifetime.
type bound struct {
	// path is the tag or digest string the manifest endpoints address.
	path string
	// digest is the manifest digest the reference named, or empty when the
	// reference named only a tag. A fetch checks what it read against it.
	digest digest.Digest
}

// Repository is one repository on one registry: the endpoint prefix every
// request shares, plus the tag or digest the manifest endpoints are bound to.
//
// A Repository is immutable once [NewRepository] returns it and is safe to
// use from several goroutines at once, which is what lets one repository
// carry a transfer's parts in parallel.
type Repository struct {
	// client sends every request the repository makes to the registry itself.
	// It is the caller's client with redirect following turned off, so a 3xx
	// comes back to be decided on here rather than being followed by rules
	// this package did not write.
	client *http.Client
	// external sends requests to a host the registry named. It has no cookie
	// jar, retains the caller's original transport for the registry hostname,
	// and guards direct cross-host connections at the actual peer address before
	// HTTP request bytes leave.
	external *http.Client
	// scheme is the URL scheme, "https" or "http".
	scheme string
	// host is the registry, with a port when the reference named one.
	host string
	// name is the repository path under /v2/, such as "team/artifact".
	name string
	// manifest is the reference the manifest endpoints are bound to.
	manifest bound
	// auth is what the registry challenged with and the tokens acquired for
	// it. It is the one part of a repository that changes after construction,
	// and it guards itself.
	auth *authState
}

// NewRepository returns the repository ref names.
//
// ref is a reference in the registry/repo[:tag][@digest] grammar, parsed by
// github.com/distribution/reference, the canonical implementation of that
// grammar. The registry is required and the name must already be canonical:
// bigoci pushes and pulls where it is told, so a familiar Docker Hub short
// name such as "ubuntu:latest" is rejected rather than quietly expanded.
//
// The reference must carry a tag or a digest, because the manifest endpoints
// address one of the two and every push and pull names a manifest. A
// reference carrying both is bound to its digest: the digest names exactly
// one manifest, while the tag beside it is only a claim about where that tag
// pointed. A reference that names a digest also makes [Manifests.Get] verify
// what it fetched against it. Blob requests use neither.
//
// The repository talks https with a client built on
// [net/http.DefaultTransport]. [WithPlainHTTP] and [WithHTTPClient] change
// that.
//
// Two clients are derived from whichever one is in force, and the caller's own
// is never touched: [net/http.Client] is four exported fields and no hidden
// state, so copying the struct copies all of it. One turns redirect following
// off, which is how this package rather than [net/http] decides what a
// re-issued request may carry; the other drops the cookie jar as well and
// sends requests to token realms, redirect targets, and off-origin upload
// sessions. The external client binds the caller-derived guarded transport to
// this registry, preserving its TLS, dial, proxy, and pooling settings while
// enforcing the destination boundary at the actual connection. Repositories
// built by one public Client share that transport's external connection pool.
func NewRepository(ref string, opts ...Option) (*Repository, error) {
	named, err := reference.ParseNamed(ref)
	if err != nil {
		return nil, fmt.Errorf("parse reference %q: %w", ref, err)
	}

	manifest, err := boundManifest(named)
	if err != nil {
		return nil, fmt.Errorf("reference %q: %w", ref, err)
	}

	applied := settings{
		client: &http.Client{Transport: http.DefaultTransport},
		scheme: schemeHTTPS,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(&applied)
	}

	request := *applied.client
	request.CheckRedirect = refuseRedirect

	external := request
	external.Jar = nil
	host := reference.Domain(named)
	externalBase := applied.externalBase
	if externalBase == nil {
		externalBase = NewExternalTransportBase(request.Transport)
	}
	external.Transport = externalBase.forRegistry(host, applied.allowUnverifiedExternal)

	repo := &Repository{
		client:   &request,
		external: &external,
		scheme:   applied.scheme,
		host:     host,
		name:     reference.Path(named),
		manifest: manifest,
	}
	repo.auth = newAuthState(repo, applied.creds, applied.now)

	return repo, nil
}

// Blobs returns the adapter for this repository's blob endpoints.
func (r *Repository) Blobs() *Blobs {
	client := ociblob.New(
		ociblob.WithTransport(registryBlobTransport{repo: r}),
		ociblob.WithStorageTransport(storageBlobTransport{repo: r}),
		ociblob.WithPlainHTTP(r.scheme == schemeHTTP),
		ociblob.WithRetryPolicy(ociblob.RetryPolicy{}),
		ociblob.WithWriteRedirects(false),
	)

	return &Blobs{
		repo:   r,
		client: client,
		target: ociblob.Repository{
			Host: r.host,
			Name: r.name,
		},
	}
}

// Manifests returns the adapter for this repository's manifest endpoints,
// bound to the tag or digest the reference carried.
func (r *Repository) Manifests() *Manifests {
	return &Manifests{repo: r}
}

// endpoint returns the URL of one endpoint on this repository. path is the
// suffix under the repository's /v2/<name>/ prefix, such as
// "blobs/sha256:abc…" or "manifests/v1".
//
// The suffix goes into [net/url.URL.Path] raw. RFC 3986 allows a colon inside
// a path segment, so [net/url.URL.String] leaves a digest spelled the way the
// distribution spec spells it instead of escaping it into %3A.
func (r *Repository) endpoint(path string) *url.URL {
	return &url.URL{
		Scheme: r.scheme,
		Host:   r.host,
		Path:   apiPrefix + r.name + "/" + path,
	}
}

// newRequest builds a repository-native request to endpoint with the caller's
// context and the credential the registry has asked for.
//
// Authenticating is a pre-condition of a request rather than a recovery from
// one: what a request must carry is worked out before it is built, and the
// header is set here and nowhere else but re-issues the registry itself asks
// for. A registry that has not challenged asks for nothing, so nothing is set
// and nothing extra is sent.
func (r *Repository) newRequest(
	ctx context.Context,
	method string,
	endpoint *url.URL,
	body io.Reader,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build %s request for %s: %w", method, endpoint.Path, err)
	}

	if err := r.authorizeRequest(req); err != nil {
		return nil, err
	}

	return req, nil
}

// authorizeRequest attaches the credential req needs, resolving the registry's
// challenge first when req carries a body that cannot be replayed.
func (r *Repository) authorizeRequest(req *http.Request) error {
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		if resolved := r.auth.resolve(req.Context()); resolved != nil {
			return resolved
		}
	}

	header, err := r.auth.authorize(req.Context(), req.Method)
	if err != nil {
		return err
	}

	if header != "" {
		req.Header.Set(headerAuthorization, header)
	}

	return nil
}

// send sends req and reports the registry's verdict on it back to the auth
// state.
//
// A refusal is the registry demanding a different request, which is one of the
// two things that make this package send a second one. Everything else proves
// the credential the request carried and goes to [Repository.accepted], which
// hands back the response unless the registry named somewhere else to get it.
func (r *Repository) send(req *http.Request) (*http.Response, error) {
	return r.sendWith(req, r.do, r.accepted)
}

// requestSender performs one HTTP request and returns its raw response.
type requestSender func(req *http.Request) (*http.Response, error)

// responseAcceptance records a non-refusal response and applies the redirect
// behavior owned by the caller of [Repository.sendWith].
type responseAcceptance func(
	req *http.Request,
	resp *http.Response,
	presented string,
) (*http.Response, error)

// sendWith sends req through send, answers one authentication challenge, and
// delegates every accepted response to accept.
func (r *Repository) sendWith(
	req *http.Request,
	send requestSender,
	accept responseAcceptance,
) (*http.Response, error) {
	resp, err := send(req)
	if err != nil {
		return nil, err
	}

	if isRefusal(resp.StatusCode) {
		return r.answerWith(req, resp, send, accept)
	}

	return accept(req, resp, req.Header.Get(headerAuthorization))
}

// accepted records that the registry answered a request rather than refusing
// it, and follows the answer when the answer is a location instead of content.
//
// A redirect proves a credential exactly as a 200 does: a registry that did
// not like what the request carried would have said so rather than pointing
// somewhere. presented is the header the request that got this answer carried,
// which is what the auth state marks as working.
func (r *Repository) accepted(req *http.Request, resp *http.Response, presented string) (*http.Response, error) {
	r.auth.answered(presented)

	if isRedirect(resp.StatusCode) {
		followed, err := r.follow(originOf(req), req, resp)
		if err != nil {
			return nil, err
		}

		// A refusal that comes back from a chain is on the registry's own
		// origin by construction — anything off it classified through the
		// off-origin table inside follow — so it is the registry speaking,
		// and it gets the same answer a refusal of the first request gets.
		// The replay inside re-sends the original request, which a followed
		// method guarantees is bodyless, and the auth state bounds the loop
		// the way it bounds every refusal: denied is absorbing.
		if isRefusal(followed.StatusCode) {
			return r.answer(req, followed)
		}

		return followed, nil
	}

	return resp, nil
}

// acceptedDirect records an accepted credential and returns the response
// without following a redirect. The embedding blob client owns that policy.
func (r *Repository) acceptedDirect(
	_ *http.Request,
	resp *http.Response,
	presented string,
) (*http.Response, error) {
	r.auth.answered(presented)

	return resp, nil
}

// answer decides what a refusal is worth and acts on it.
//
// A 403 stating no requirement is a permission answer, or something standing
// in front of the registry — presenting the same identity again cannot change
// it, so it ends here. Everything else carries a challenge, and answering it
// either changes what the next request will present or it does not; the auth
// state is what knows which, and it says so by returning a header or an
// error.
//
// A request whose body can only be sent once is the case this cannot rescue.
// The credential is refreshed all the same, so the next attempt goes out with
// it, and the failure comes back marked worth repeating: the orchestrator
// opens the file again, which is the only place a second copy of those bytes
// exists.
func (r *Repository) answer(req *http.Request, resp *http.Response) (*http.Response, error) {
	return r.answerWith(req, resp, r.do, r.accepted)
}

// answerWith decides what a refusal is worth and replays an answerable request
// through send and accept.
func (r *Repository) answerWith(
	req *http.Request,
	resp *http.Response,
	send requestSender,
	accept responseAcceptance,
) (*http.Response, error) {
	defer resp.Body.Close()

	status := statusError(resp)

	raw := challengeHeader(resp)
	if raw == "" && resp.StatusCode == http.StatusForbidden {
		return nil, status
	}

	// A challenge nobody can answer still happened to a request the registry
	// answered, so the refusal it sent stays underneath: the status a caller
	// reaches through errors.As is the registry's, and the message says what
	// was wrong with the challenge after saying what the registry said.
	asked, err := parseChallenge(raw)
	if err != nil {
		return nil, &authError{cause: status, reason: err.Error()}
	}

	sent := req.Header.Get(headerAuthorization)

	header, err := r.auth.refused(req.Context(), req.Method, sent, asked, status)
	if err != nil {
		return nil, err
	}

	if !replayable(req) {
		return nil, retry.Transient(&authError{
			cause:  status,
			reason: "the credential was refreshed, but this request's body can only be sent once",
		}, 0)
	}

	return r.replayWith(req, header, send, accept)
}

// replayWith sends a refused request once with header and delegates an accepted
// response to accept.
func (r *Repository) replayWith(
	req *http.Request,
	header string,
	send requestSender,
	accept responseAcceptance,
) (*http.Response, error) {
	second := req.Clone(req.Context())
	second.Header.Set(headerAuthorization, header)

	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("%s %s: rewind the request body: %w", req.Method, req.URL.Path, err)
		}

		second.Body = body
	}

	resp, err := send(second)
	if err != nil {
		return nil, err
	}

	if isRefusal(resp.StatusCode) {
		defer resp.Body.Close()

		return nil, r.auth.deny(req.Method, header, statusError(resp))
	}

	return accept(second, resp, header)
}

// do sends req and reports a transport failure against the method and path it
// happened on. The response body comes back open: the caller either closes it
// or hands it to the caller of the port.
//
// A transport failure is transient. The adapter does not sort network
// failures by species — connection refused, DNS, TLS handshake, reset,
// timeout — because every one of them is worth one more attempt. Nor does it
// expose the underlying message: an HTTP parser can repeat a malformed
// registry header, including a credential the registry just received. The
// exception is a request the caller ended: that is the transfer stopping, not
// the network failing, and the check belongs here because this is the last
// place the two are still distinguishable.
func (r *Repository) do(req *http.Request) (*http.Response, error) {
	return r.doWith(r.client, req, originOf(req).String())
}

// doExternal sends req through the cookie-free client used for a token realm.
// It retains the caller-derived transport behavior, timeout, and redirect
// refusal while removing ambient authority from the request.
func (r *Repository) doExternal(req *http.Request) (*http.Response, error) {
	return r.doWith(r.external, req, "token exchange")
}

// doWith sends req through client and classifies a transport failure without
// admitting parser text, response bytes, or a caller transport's message to
// the public error. operation is chosen before the request and is safe to
// render.
func (r *Repository) doWith(
	client *http.Client,
	req *http.Request,
	operation string,
) (*http.Response, error) {
	// #nosec G704 -- repository URLs are validated at construction and external URLs at dial time.
	resp, err := client.Do(req)
	if err != nil {
		return nil, transportFailure(req, resp, err, operation)
	}

	return resp, nil
}

// transportFailure sanitizes and classifies one failed HTTP exchange.
func transportFailure(
	req *http.Request,
	resp *http.Response,
	err error,
	operation string,
) error {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if req.Context().Err() != nil {
		return fmt.Errorf("%s: %w", operation, req.Context().Err())
	}

	var targetErr *externalTargetError
	if errors.As(err, &targetErr) {
		return fmt.Errorf("%s: %w", operation, targetErr)
	}

	return retry.Transient(safeCause(operation+": transport failed", transportCause(err)), 0)
}

// isRefusal reports whether a status is the registry refusing a request
// rather than answering it.
//
// Both statuses point at the same thing: the credential the request carried,
// or the one it did not carry. A registry refuses a credential it could not
// read with either of them, and one that falls short of the access asked for
// with either of them, so both get the same chance to be answered and neither
// proves a token.
func isRefusal(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// replayable reports whether [net/http] can produce req's body a second time.
//
// It is a fact the standard library computes rather than a judgement this
// package makes: [net/http.NewRequestWithContext] fills in GetBody for the
// readers it can rewind and leaves it nil for everything else. The one
// request that must never be sent twice — a blob upload streaming a section
// of a file — is exactly the one it refuses to rewind.
func replayable(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

// boundManifest returns the manifest reference named addresses. A reference
// carrying both a tag and a digest resolves to its digest, which names one
// manifest exactly; a reference carrying neither cannot name a manifest at
// all and is rejected here rather than at the first manifest request.
func boundManifest(named reference.Named) (bound, error) {
	if digested, ok := named.(reference.Digested); ok {
		dgst := digested.Digest()
		if algorithm := dgst.Algorithm(); algorithm != digest.SHA256 {
			return bound{}, fmt.Errorf(
				"manifest digest algorithm is %q, want %q: the v1 format pins sha256",
				algorithm, digest.SHA256,
			)
		}

		return bound{path: dgst.String(), digest: dgst}, nil
	}

	if tagged, ok := named.(reference.Tagged); ok {
		return bound{path: tagged.Tag(), digest: ""}, nil
	}

	return bound{}, errors.New("must name a tag or a digest, for example repo:v1 or repo@sha256:<hex>")
}

// StatusError reports a response whose HTTP status the repository adapter did
// not expect. Internal callers that must react to a specific status read
// [StatusError.Status] through [errors.As] instead of parsing the message.
type StatusError struct {
	// Method is the HTTP method of the request that failed.
	Method string
	// Path is the safe structural request path on the registry. A blob read
	// replaces its manifest-selected digest with "<digest>". A token endpoint
	// failure uses the fixed label "token endpoint". A redirected request is
	// reported against the original registry path, never against the location
	// it was sent to.
	Path string
	// Status is the HTTP status code the registry answered with.
	Status int
	// RetryAfter is how long the registry asked the caller to wait before
	// trying again, read raw from the Retry-After header a 429 or a 503
	// carries. It is zero when the response sent no header or sent one that
	// cannot be read. Nothing is trimmed or dropped here: this field is what
	// the registry said, and deciding how much of it to obey belongs to the
	// retry policy, which bounds every wait by its own cap.
	RetryAfter time.Duration
	// Detail is the bounded start of the response body, when it carried one.
	// It is retained for programmatic diagnosis through [errors.As] but never
	// rendered by [StatusError.Error]: a registry can reflect the Authorization
	// header it just received, making this field live credential material.
	Detail string
}

// Error renders only the method, path, and status. [StatusError.Detail] is
// deliberately excluded because it is peer-controlled and can contain a
// reusable credential reflected from the request. A wait the registry asked
// for is also left out: it is bookkeeping for the retry loop, not something a
// person reading a failure needs.
func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: registry returned %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
}

// Is makes a 404 match [ErrNotFound], a 401 or a 403 match [ErrUnauthorized],
// and a 413 match [ErrTooLarge] under [errors.Is] without a second wrapping
// layer, so a not-found failure says "not found" once — in the status text
// the message already carries — instead of stacking the phrase at every
// boundary.
func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	case ErrTooLarge:
		return e.Status == http.StatusRequestEntityTooLarge
	default:
		return false
	}
}

// statusError reports a response whose status the adapter did not expect,
// against the request that produced it. It is for endpoints that never leave
// the registry. Everything a redirect can move reports through
// [statusErrorAt] instead.
func statusError(resp *http.Response) error {
	return statusErrorAt(originOf(resp.Request), resp)
}

// statusErrorAt reports a response whose status the adapter did not expect,
// against the registry request at names, classified for the retry policy on
// the way out. It reads the body for the error detail, which also drains it
// for the close that follows.
//
// The classification lives here because this is the one place every unexpected
// status from the registry passes through, which is what keeps the
// transient-or-terminal split a single table instead of a decision repeated at
// every endpoint. A transient status still gets its detail read into the
// structured error and the connection still goes back in the pool. The detail
// never reaches [StatusError.Error]. A redirect's body is not retained at all:
// see [statusDetail].
//
// The origin is threaded in rather than read off the response because the
// response may have come from somewhere else entirely: after a re-issue,
// resp.Request is the request that went to the location the registry named,
// and rendering its URL would print a signature. What at holds was fixed
// before anything was sent.
func statusErrorAt(at origin, resp *http.Response) error {
	err := &StatusError{
		Method:     at.method,
		Path:       at.path,
		Status:     resp.StatusCode,
		RetryAfter: retryAfter(resp),
		Detail:     statusDetail(resp),
	}

	if transientStatus(err.Status) {
		return retry.Transient(err, err.RetryAfter)
	}

	return err
}

// statusDetail reads a failed response's body for [StatusError.Detail] — unless
// the status is a redirect. A 3xx body is a server's courtesy rendering of its
// own Location header, which for a blob read is a presigned URL, so it is not
// retained even in the structured field. The body is drained instead, letting
// the connection go back in the pool.
func statusDetail(resp *http.Response) string {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		drain(resp.Body)

		return ""
	}

	return errorDetail(resp.Body)
}

// errorDetail reads the first errorBodyLimit bytes of a failed response body
// for structured diagnosis. A body it cannot read contributes nothing: the
// status already says what went wrong.
func errorDetail(body io.Reader) string {
	detail, err := io.ReadAll(io.LimitReader(body, errorBodyLimit))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(detail))
}

// drain reads and discards what is left of a response body the adapter does
// not need, up to errorBodyLimit bytes, so the connection goes back in the
// pool instead of being torn down. A body larger than that is not worth the
// read: giving up on it costs one connection, not correctness.
func drain(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, errorBodyLimit))
}
