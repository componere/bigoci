package oci

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/componere/bigoci/internal/retry"
)

// maxRedirectHops is how many times one request may be re-issued at a location
// a peer named.
//
// Three is generous for what registries do — one hop from the registry to
// signed storage, sometimes a second inside the storage provider — and it is a
// bound rather than a budget. Every hop past the first is another party
// learning what is being fetched, and a chain that has not arrived by the
// third is a loop or a misconfiguration rather than a route.
const maxRedirectHops = 3

// headerLocation is the response header a redirect names its target in.
const headerLocation = "Location"

// The port each scheme implies, so a URL that spells its port out and one that
// leaves it to the scheme still compare as the same origin.
const (
	// portHTTPS is the port an https URL means when it names none.
	portHTTPS = "443"
	// portHTTP is the port an http URL means when it names none.
	portHTTP = "80"
)

// origin names the registry request a failure is reported against: the method
// it used and the path it addressed on the registry.
//
// It is taken before anything is sent and carried through every re-issue,
// which is what keeps a signed storage URL out of every error this package
// builds. A redirect moves where a request goes; it does not move what the
// request was for, and what it was for is the only thing a person reading a
// failure can act on.
type origin struct {
	// method is the HTTP method of the registry request.
	method string
	// path is the path that request addressed on the registry. It is a path
	// and never a URL: a path carries no query, and a query is where a
	// signature lives.
	path string
}

// originOf returns the registry request req is.
func originOf(req *http.Request) origin {
	return origin{method: req.Method, path: req.URL.Path}
}

// String renders the "METHOD /path" prefix the messages in this package open
// with.
func (o origin) String() string {
	return o.method + " " + o.path
}

// refuseRedirect is the redirect policy every client this package derives
// carries: hand the 3xx back rather than follow it.
//
// Following is off for every request, not only the blob reads that meet a
// redirect in practice. [net/http] forwards an Authorization header to any
// target whose host is the request's own domain or a subdomain of it, which
// would put a registry credential on a request to a CDN the registry named
// under its own domain (net/http/client.go, shouldCopyHeaderOnRedirect). That
// rule is not wrong; it is simply not the rule bigoci needs, and the only way
// to state a different one is to do the following here.
//
// [net/http.ErrUseLastResponse] returns the 3xx with its body still open, so
// everything that acts on one drains and closes it.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// follow re-issues req at the location resp named, and at each location after
// that, until something answers with a status that is not another redirect.
//
// Every hop is a request built from nothing: a fresh
// [net/http.NewRequestWithContext] carrying the two headers [copyAllowed]
// permits and whatever the same-origin rule allows. Nothing is inherited,
// which is what makes "can a credential reach signed storage" a question with
// a written answer rather than a matter of which headers happened to be set.
//
// Authorization rides a hop only when the target's scheme, host, and port all
// equal those of the request that received the redirect. A registry that
// redirects inside itself keeps authenticating; a registry that redirects to
// storage — or to a CDN on a subdomain of its own — sends a request carrying
// nothing at all. The re-issue goes through a client with no cookie jar, so
// nothing the registry set in one reaches the target either.
//
// The location is never stored. It lives in this frame, for this call, and a
// later attempt at the same blob asks the registry again and follows whatever
// fresh redirect it sends — which is what makes a signature that expires
// between attempts a non-event.
func (r *Repository) follow(at origin, req *http.Request, resp *http.Response) (*http.Response, error) {
	current := req
	away := false

	for hops := 0; ; hops++ {
		next, err := r.nextHop(at, current, resp, hops, away)

		drain(resp.Body)
		_ = resp.Body.Close()

		if err != nil {
			return nil, err
		}

		away = away || !sameOrigin(r.registryURL(), next.URL)

		resp, err = r.hop(at, next)
		if err != nil {
			return nil, err
		}
		current = next

		if isRedirect(resp.StatusCode) {
			continue
		}

		if away {
			return offOriginResponse(at, next.URL.Host, resp)
		}

		return resp, nil
	}
}

// nextHop returns the request that follows one redirect, or the failure that
// ends the chain.
//
// It may read the failing response's body for an error detail; it never closes
// it. The caller drains and closes whatever is left either way, because a body
// left open costs a connection whether the hop happens or not.
func (r *Repository) nextHop(
	at origin,
	current *http.Request,
	resp *http.Response,
	hops int,
	away bool,
) (*http.Request, error) {
	if hops >= maxRedirectHops {
		return nil, fmt.Errorf("%s: redirected more than %d times without reaching the content", at, maxRedirectHops)
	}

	method, ok := followMethod(current.Method, resp.StatusCode)
	if !ok {
		// Off the registry's origin the refusal is about something a storage
		// host said, so it is reported as one — a [StatusError] would say
		// "registry returned" about a verdict the registry never gave.
		if away {
			return nil, &redirectError{at: at, host: current.URL.Host, status: resp.StatusCode}
		}

		return nil, statusErrorAt(at, resp)
	}

	target, err := r.redirectTarget(at, current, resp)
	if err != nil {
		return nil, err
	}

	hop, err := http.NewRequestWithContext(current.Context(), method, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build the request %s redirected to: %w", at, target.Host, scrub(err))
	}

	copyAllowed(hop.Header, current.Header)

	if sameOrigin(current.URL, target) {
		if presented := current.Header.Get(headerAuthorization); presented != "" {
			hop.Header.Set(headerAuthorization, presented)
		}
	}

	return hop, nil
}

// redirectTarget resolves the location a redirect named against the request
// that received it, and refuses the ones bigoci will not go to.
//
// A relative location resolves against the current request, which is what the
// spec says and what registries rely on when they redirect inside themselves.
// Every refusal below is a peer choosing something for bigoci: where to go
// when it named nowhere, what a URL is, a scheme a blob cannot be read over,
// a downgrade to plain http the registry does not get to make, and — the one
// that matters most — which credential to present, because [net/http] turns
// userinfo in a URL into a Basic header (net/http/client.go, in the request
// builder).
//
// No message here quotes the location. It is the one string in this exchange
// that carries a signature, and an error naming it would hand that signature
// to a terminal, a log file, and whatever collects both. The host is named
// instead, which is what says who to go ask.
//
// One refusal below is unreachable through [net/http.Client.Do] and kept
// anyway. The standard library resolves the Location itself before it consults
// the redirect policy (net/http/client.go, at the top of the client's redirect
// loop), so a location that is not a URL fails there, quoting it, and this
// package never sees the response. That costs nothing worth closing: a
// presigned URL always parses, because [net/url.Parse] does not validate a
// query at all — it is [net/url.URL.Query] that reads one — so the location a
// signature rides in cannot be the location that fails to parse. The branch
// stays as the guard for a request this package sends some other way and for a
// standard library that reorders the two steps.
func (r *Repository) redirectTarget(at origin, current *http.Request, resp *http.Response) (*url.URL, error) {
	location := resp.Header.Get(headerLocation)
	if location == "" {
		return nil, fmt.Errorf("%s: redirected without a Location header", at)
	}

	parsed, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("%s: redirected to a Location that is not a URL: %w", at, scrub(err))
	}

	target := current.URL.ResolveReference(parsed)

	switch {
	case target.Scheme != schemeHTTPS && target.Scheme != schemeHTTP:
		return nil, fmt.Errorf("%s: redirected to a %q location, which is not http", at, target.Scheme)
	case target.Host == "":
		return nil, fmt.Errorf("%s: redirected to a location naming no host", at)
	case target.Scheme == schemeHTTP && r.scheme == schemeHTTPS:
		return nil, fmt.Errorf("%s: %s redirected an https request to plain http", at, target.Host)
	case target.User != nil:
		return nil, fmt.Errorf("%s: the location for %s carries a user name and password", at, target.Host)
	}

	return target, nil
}

// checkSession refuses an upload-session URL this package will not write to.
//
// The Location that opens an upload is registry-chosen, exactly as a
// redirect's is, and the request that goes to it carries a part's bytes — so
// it gets the refusals a redirect target gets: a scheme a blob cannot ride, a
// plain-http downgrade of an https registry, and a URL choosing a credential
// through userinfo. The message never quotes the URL, because a session URL
// carries registry-minted state in its query — the same class of string as a
// signature.
func (r *Repository) checkSession(session *url.URL) error {
	switch {
	case session.Scheme != schemeHTTPS && session.Scheme != schemeHTTP:
		return fmt.Errorf("the registry opened an upload at a %q location, which is not http", session.Scheme)
	case session.Host == "":
		return errors.New("the registry opened an upload at a location naming no host")
	case session.Scheme == schemeHTTP && r.scheme == schemeHTTPS:
		return fmt.Errorf("the registry opened an upload at plain http on %s, a downgrade from https", session.Host)
	case session.User != nil:
		return fmt.Errorf("the registry's upload location at %s carries a user name and password", session.Host)
	default:
		return nil
	}
}

// hop sends one request through the repository's cookie-free external client
// and reports a transport failure against the registry operation that named
// the target.
//
// The failure is scrubbed on the way out. Everything else is the verdict
// [Repository.do] reaches for the same reasons: a network that failed once is
// worth another attempt, and a context the caller ended is the transfer
// stopping rather than the network failing.
func (r *Repository) hop(at origin, req *http.Request) (*http.Response, error) {
	resp, err := r.external.Do(req)
	if err != nil {
		failure := fmt.Errorf("%s: the request to %s failed: %w", at, req.URL.Host, scrub(err))
		if req.Context().Err() != nil {
			return nil, failure
		}

		return nil, retry.Transient(failure, 0)
	}

	return resp, nil
}

// registryURL returns the origin every request of this repository starts at,
// which is what "the registry" means when a chain of redirects has left it.
func (r *Repository) registryURL() *url.URL {
	return &url.URL{Scheme: r.scheme, Host: r.host}
}

// copyAllowed copies the two request headers a re-issue may carry over from
// the request that was redirected, and nothing else.
//
// The list is the whole of it. There is no branch that could add a third
// header and no way to add one at run time, mirroring the allow-lists the
// reference CLI's log renders: a Content-Type, a cookie, a private header,
// anything a later phase starts sending, has no path to a host the registry
// named. Authorization is absent here on purpose — the same-origin rule is the
// only thing that sets it, and it is stated where that decision is made.
//
// Carrying Range has a second effect worth naming. [net/http] asks for gzip on
// its own only when a request carries no Range header
// (net/http/transport.go, where the transport adds Accept-Encoding), so a
// ranged re-issue can never come back as decompressed bytes under a compressed
// Content-Length — the one shape that would make a part's length disagree with
// what the manifest says about it.
func copyAllowed(dst, src http.Header) {
	for _, name := range []string{"Range", "Accept"} {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}

// followMethod returns the method a re-issue uses, and whether the redirect
// may be followed at all.
//
// Only a read is ever re-issued. A 303 says to fetch the result of what was
// just done with a GET, which is what a GET already is and what a HEAD is not
// — a HEAD turned into a request for a body nobody asked for is not something
// bigoci has any use for. Everything else is a write, and re-issuing a write
// means sending its body a second time: the body bigoci sends is a section of
// a file that [net/http] refuses to rewind, and a registry redirecting an
// upload is stating a protocol this package does not speak rather than a place
// it can go.
func followMethod(method string, status int) (string, bool) {
	switch method {
	case http.MethodGet:
		return http.MethodGet, true
	case http.MethodHead:
		if status == http.StatusSeeOther {
			return "", false
		}

		return http.MethodHead, true
	default:
		return "", false
	}
}

// isRedirect reports whether a status is one of the redirects this package
// follows.
//
// The set is named rather than taken as the whole 3xx class. A 304 answers a
// conditional request bigoci never makes, and a 300 offers a choice with no
// location to take, so neither is a redirect to follow — both fall through to
// the caller, which reports them as the unexpected statuses they are.
func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

// sameOrigin reports whether two URLs name the same origin: the same scheme,
// the same host, and the same port, with the port a scheme implies counted as
// present.
//
// It is the whole of the rule that decides whether a re-issue carries a
// credential. Host names compare case-insensitively, as DNS names do; scheme
// case is already normalized by [net/url.Parse]. The rule is deliberately
// stricter than the standard library's,
// which forwards to any domain-or-subdomain target. A CDN on a subdomain of
// the registry is a different party and gets nothing; storage on the same host
// behind a different port is a different party too. A registry redirecting
// inside itself is not, and that single case is the reason the header is kept
// at all rather than dropped always.
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && portOf(a) == portOf(b)
}

// portOf returns the port a URL is on, filling in the one its scheme implies
// when it names none.
func portOf(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}

	switch u.Scheme {
	case schemeHTTPS:
		return portHTTPS
	case schemeHTTP:
		return portHTTP
	default:
		return ""
	}
}

// offOriginResponse decides what a response from beyond the registry means.
//
// Content comes back untouched: a 200 is the whole blob and a 206 is the range
// that was asked for, and both are checked against what the request asked for
// by the same code that checks a registry's own answer. Everything else fails
// here rather than travelling on, because the statuses that reach this point
// mean something different than they do from a registry and there is no way to
// tell them apart further up.
func offOriginResponse(at origin, host string, resp *http.Response) (*http.Response, error) {
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		return resp, nil
	}

	return nil, offOriginFailure(at, host, resp)
}

// offOriginUploadResponse decides what a response from an upload session
// beyond the registry means. Only Created completes the write; every other
// status is classified as an external failure without interpreting a
// challenge or exposing the session response.
func offOriginUploadResponse(at origin, host string, resp *http.Response) (*http.Response, error) {
	if resp.StatusCode == http.StatusCreated {
		return resp, nil
	}

	return nil, offOriginFailure(at, host, resp)
}

// offOriginFailure closes an unsuccessful response from a host the registry
// named and reports only the original registry operation, the host, and its
// status. Its body and URL may contain a live signed capability and never
// become part of the returned error.
func offOriginFailure(at origin, host string, resp *http.Response) error {
	drain(resp.Body)
	_ = resp.Body.Close()

	failure := &redirectError{
		at:         at,
		host:       host,
		status:     resp.StatusCode,
		retryAfter: retryAfter(resp),
	}

	if offOriginTransient(failure.status) {
		return retry.Transient(failure, failure.retryAfter)
	}

	return failure
}

// offOriginTransient reports whether a status from beyond the registry is
// worth another attempt.
//
// The four statuses this adds to the registry's own table are the ones a
// signed URL answers once its signature is no longer good: expired, revoked,
// or naming an object the store has since moved. The window on one is minutes
// and a single backoff can be tens of seconds, so meeting an expired signature
// is routine rather than exceptional — and the answer is always the same, ask
// the registry again and follow the fresh redirect it sends, which is exactly
// what another attempt does.
func offOriginTransient(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	default:
		return transientStatus(status)
	}
}

// scrub drops the URL a [net/url.Error] renders, keeping the failure
// underneath.
//
// [net/http.Client.Do] wraps everything it returns in one, and that error's
// message prints the URL whole — [net/http] blanks a password in userinfo and
// nothing else (net/http/client.go, stripPassword), so a signed query rides
// straight into whatever reads the error, and from there into a terminal and a
// support ticket. The cause is what says what went wrong; the URL is the part
// that must not be said.
//
// The tap the reference CLI installs is safe without this and stays safe, and
// that is a fact about where the wrapping happens rather than a hope. The
// [net/url.Error] is built by [net/http.Client.Do] itself, above the transport
// (net/http/client.go, the uerr helper the client wraps every send with), so
// what a [net/http.RoundTripper] returns — which is all the tap ever sees and
// all its err= field ever holds — is the cause on its own, with no URL
// rendered around it.
func scrub(err error) error {
	var wrapped *url.Error
	if errors.As(err, &wrapped) {
		return wrapped.Err
	}

	return err
}

// redirectError reports a failure the target of a redirect answered with.
//
// It is deliberately not a [StatusError]. A [StatusError] carries the
// registry's own verdict, where a 401 or a 403 means "your credentials" and a
// 404 means "the registry does not hold this" — statements only the registry
// is in a position to make. A signed URL whose signature has expired says
// neither, and a caller that read it as either would go fix a password that
// was never wrong or give up on an artifact that is sitting right there. So
// nothing here matches [ErrUnauthorized], [ErrNotFound], or [ErrTooLarge], and
// the type stays unexported because there is nothing to branch on: whether the
// failure is worth another attempt is the whole answer.
type redirectError struct {
	// at is the registry request the redirect started from. It is the only
	// method and path this error will ever name.
	at origin
	// host is the host that answered — no scheme, no path, no query. Enough to
	// say who refused, and not enough to repeat what was asked.
	host string
	// status is the HTTP status that host answered with.
	status int
	// retryAfter is the wait it asked for, read the same way a registry's is.
	retryAfter time.Duration
}

// Error names the registry request, the host that answered it after the
// redirect, and the status — and nothing the target sent. The location never
// appears, and neither does the response body: an object store's error
// document usually carries only a request id, but at least one real store
// echoes the signed request it is complaining about, which is a credential.
// The -debug log is where diagnosis happens, and it never renders bodies
// either.
func (e *redirectError) Error() string {
	return fmt.Sprintf(
		"%s: %s answered the redirected request with %d %s",
		e.at, e.host, e.status, http.StatusText(e.status),
	)
}
