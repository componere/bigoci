package oci

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

// The repository the authenticating fixtures serve, and what it holds.
const (
	// authRepo is the repository path, with two components so a nested name
	// is proved to survive into a scope.
	authRepo = "team/artifact"
	// authTag is the tag the fixture's manifest is bound to.
	authTag = "v1"
	// authPayload is the blob the fixture serves and accepts.
	authPayload = "bigoci authenticated blob payload"
	// authManifest is the manifest document the fixture serves.
	authManifest = `{"schemaVersion":2,"artifactType":"application/vnd.bigoci.file.v1"}`
	// authService is the name the fixture's challenge gives itself, which is
	// deliberately not its host: nothing may look a credential up by it.
	authService = "fixture-registry"
)

// tokenPath is the path the fixture's own token endpoint lives at.
const tokenPath = "/token"

// authRecord is one request the fixture received, captured before it was
// answered.
type authRecord struct {
	// method is the HTTP method.
	method string
	// path is the URL path.
	path string
	// query is the parsed query string, which is where a token request's
	// service and scope parameters are read from.
	query url.Values
	// authorization is the Authorization header the request carried, empty
	// when it carried none.
	authorization string
	// cookie is the Cookie header the request carried, which is what says a
	// caller's jar reached the registry before a redirect took the request
	// somewhere the jar must not follow.
	cookie string
	// bodyLen is how many bytes of body arrived, which is what proves an
	// upload's body was sent exactly once.
	bodyLen int
}

// authRegistry is a fake registry that authenticates: it refuses requests
// carrying a credential it does not know, states a bearer challenge naming
// its own token endpoint, and issues tokens from it.
//
// Every knob a test scripts it with is a field, set before the call under
// test and left alone afterwards. What the fixture saw is read back through
// its recording methods.
type authRegistry struct {
	// t is the test the fixture reports its own failures to. Handlers run on
	// the server's goroutines, where a fatal assertion is not allowed.
	t *testing.T
	// server is the httptest server the fixture runs on.
	server *httptest.Server

	// refusalStatus is the status an unrecognized credential is refused with.
	refusalStatus int
	// omitChallenge drops the WWW-Authenticate header from a refusal, which is
	// how a permission answer differs from a demand for credentials.
	omitChallenge bool
	// challengeScope is the scope parameter the challenge carries, empty when
	// it carries none.
	challengeScope string
	// challengeScheme is the scheme the refusal names. It is Bearer unless a
	// test sets it to Basic, which is the registry that takes a user name and
	// password straight off the request and runs no exchange at all.
	challengeScheme string
	// challengeAs replaces the whole WWW-Authenticate header, for the rows
	// that need a challenge nothing can be made of.
	challengeAs string
	// expiresIn is the lifetime the token endpoint states, or -1 to leave the
	// field out of the document entirely.
	expiresIn int
	// wantUser and wantPass are what the token endpoint requires in a Basic
	// header. An empty wantUser accepts anonymous requests.
	wantUser string
	wantPass string
	// serveTokenAs takes the token endpoint over when set, so a test can
	// answer it with anything at all.
	serveTokenAs http.HandlerFunc
	// accepts reports whether a repository request is served rather than
	// refused. It defaults to accepting every request carrying a token the
	// fixture minted and has not retired, and takes the whole request so a
	// test can refuse one endpoint while serving the rest.
	accepts func(r *http.Request) bool
	// answerAs takes an authenticated repository request over when set and
	// reports whether it answered it, so a test can redirect one endpoint and
	// leave every other one serving as it always did.
	answerAs func(w http.ResponseWriter, r *http.Request) bool

	// mu guards everything below.
	mu sync.Mutex
	// requests are what arrived, in order.
	requests []authRecord
	// minted counts the tokens the token endpoint issued.
	minted int
	// live are the tokens the repository endpoints still accept.
	live map[string]bool
}

// newAuthRegistry starts a fake authenticating registry. It shuts down with
// the test.
func newAuthRegistry(t *testing.T) *authRegistry {
	t.Helper()

	fake := &authRegistry{
		t:             t,
		refusalStatus: http.StatusUnauthorized,
		expiresIn:     -1,
		live:          make(map[string]bool),
	}
	fake.accepts = func(r *http.Request) bool {
		return fake.isLive(r.Header.Get(headerAuthorization))
	}

	fake.server = httptest.NewServer(fake)
	t.Cleanup(fake.server.Close)

	return fake
}

// ServeHTTP records the request and routes it to the token endpoint or to the
// repository.
func (a *authRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		a.t.Errorf("the fake registry could not read a request body: %v", err)
	}

	a.mu.Lock()
	a.requests = append(a.requests, authRecord{
		method:        r.Method,
		path:          r.URL.Path,
		query:         r.URL.Query(),
		authorization: r.Header.Get(headerAuthorization),
		cookie:        r.Header.Get("Cookie"),
		bodyLen:       len(body),
	})
	a.mu.Unlock()

	if r.URL.Path == tokenPath {
		a.serveToken(w, r)

		return
	}

	a.serveRepository(w, r)
}

// repository returns a repository bound to the fixture, built with opts on top
// of the plain-HTTP and client options every fixture repository needs.
func (a *authRegistry) repository(t *testing.T, opts ...Option) *Repository {
	t.Helper()

	base := []Option{WithPlainHTTP(), WithHTTPClient(a.server.Client())}

	repo, err := NewRepository(a.host(t)+"/"+authRepo+":"+authTag, append(base, opts...)...)
	require.NoError(t, err)

	return repo
}

// host returns the host and port the fixture listens on, which is the
// registry half of a reference.
func (a *authRegistry) host(t *testing.T) string {
	t.Helper()

	parsed, err := url.Parse(a.server.URL)
	require.NoError(t, err)

	return parsed.Host
}

// grant makes the repository endpoints accept a token, for tests that answer
// the token endpoint themselves instead of letting the fixture mint.
func (a *authRegistry) grant(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.live[token] = true
}

// retire stops the repository endpoints accepting a token, which is how a
// test makes a credential that was working stop working.
func (a *authRegistry) retire(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.live, token)
}

// all returns every request the fixture received, in arrival order.
func (a *authRegistry) all() []authRecord {
	a.mu.Lock()
	defer a.mu.Unlock()

	return slices.Clone(a.requests)
}

// tokenRequests returns the requests the token endpoint received.
func (a *authRegistry) tokenRequests() []authRecord {
	var asked []authRecord

	for _, one := range a.all() {
		if one.path == tokenPath {
			asked = append(asked, one)
		}
	}

	return asked
}

// repositoryRequests returns the requests that were not token exchanges.
func (a *authRegistry) repositoryRequests() []authRecord {
	var made []authRecord

	for _, one := range a.all() {
		if one.path != tokenPath {
			made = append(made, one)
		}
	}

	return made
}

// isLive reports whether a header carries a token the fixture minted and has
// not retired.
func (a *authRegistry) isLive(header string) bool {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	return a.live[token]
}

// serveToken answers the fixture's own token endpoint.
func (a *authRegistry) serveToken(w http.ResponseWriter, r *http.Request) {
	if a.serveTokenAs != nil {
		a.serveTokenAs(w, r)

		return
	}

	if !a.credentialed(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		a.write(w, `{"errors":[{"code":"DENIED","message":"the credentials are not valid"}]}`)

		return
	}

	a.mu.Lock()
	a.minted++
	token := fmt.Sprintf("token-%d", a.minted)
	a.live[token] = true
	a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	a.write(w, a.tokenDocument(token))
}

// credentialed reports whether a token request carried what the fixture
// requires. A fixture that requires nothing accepts an anonymous request,
// which is what a registry handing out public-access tokens does.
func (a *authRegistry) credentialed(r *http.Request) bool {
	if a.wantUser == "" {
		return true
	}

	user, pass, ok := r.BasicAuth()

	return ok && user == a.wantUser && pass == a.wantPass
}

// tokenDocument renders the token endpoint's answer, leaving expires_in out
// when the fixture states no lifetime.
func (a *authRegistry) tokenDocument(token string) string {
	if a.expiresIn < 0 {
		return `{"token":"` + token + `"}`
	}

	return fmt.Sprintf(`{"token":%q,"expires_in":%d}`, token, a.expiresIn)
}

// serveRepository answers a repository request, refusing it first when the
// credential it carried is not one the fixture accepts.
func (a *authRegistry) serveRepository(w http.ResponseWriter, r *http.Request) {
	if !a.accepts(r) {
		a.refuse(w)

		return
	}

	a.serveEndpoint(w, r)
}

// refuse answers with the fixture's challenge, or with a bare refusal when
// the fixture states no requirement.
func (a *authRegistry) refuse(w http.ResponseWriter) {
	if !a.omitChallenge {
		w.Header().Set(headerChallenge, a.challenge())
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.refusalStatus)
	a.write(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
}

// challenge renders the WWW-Authenticate header the fixture refuses with.
func (a *authRegistry) challenge() string {
	if a.challengeAs != "" {
		return a.challengeAs
	}

	if a.challengeScheme == schemeBasic {
		return `Basic realm="fixture"`
	}

	asked := fmt.Sprintf("Bearer realm=%q,service=%q", a.server.URL+tokenPath, authService)
	if scope := a.scopeStated(); scope != "" {
		asked += fmt.Sprintf(",scope=%q", scope)
	}

	return asked
}

// statesScope changes the scope the fixture's challenges name, for a test that
// has already made requests through it.
//
// A registry states the scope of the request it refused, so one serving a push
// alternates between a read's grant and a write's. Imitating that means writing
// this field while the server's goroutines may still be finishing the last
// request, which is what the lock is for; [authRegistry.scopeStated] is the
// read side of it.
func (a *authRegistry) statesScope(scope string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.challengeScope = scope
}

// scopeStated returns the scope the next challenge will name.
func (a *authRegistry) scopeStated() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.challengeScope
}

// serveEndpoint answers the distribution-spec endpoints these tests exercise,
// after whatever a test scripted in place of one of them has had its say.
func (a *authRegistry) serveEndpoint(w http.ResponseWriter, r *http.Request) {
	if a.answerAs != nil && a.answerAs(w, r) {
		return
	}

	switch {
	case r.URL.Path == apiPrefix:
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost:
		w.Header().Set("Location", apiPrefix+authRepo+"/blobs/uploads/session-1")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut:
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(r.URL.Path, "/manifests/"):
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		a.write(w, authManifest)
	default:
		a.write(w, authPayload)
	}
}

// write sends a body from a handler, reporting a failure rather than hiding
// it.
func (a *authRegistry) write(w http.ResponseWriter, body string) {
	if _, err := io.WriteString(w, body); err != nil {
		a.t.Errorf("the fake registry could not write a response: %v", err)
	}
}

// authDigest is the digest of the payload the fixture serves, which is what a
// blob request addresses.
func authDigest() digest.Digest {
	return digest.FromString(authPayload)
}

// countingTransport counts every request that crossed it, so a test can prove
// that the caller's own transport carried the challenged request, the token
// exchange, and the re-issue alike.
type countingTransport struct {
	// next is the transport that actually sends.
	next http.RoundTripper

	// mu guards paths.
	mu sync.Mutex
	// paths are the request paths that crossed it, in order.
	paths []string
}

// RoundTrip records the request's path and sends it on.
func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.paths = append(c.paths, req.URL.Path)
	c.mu.Unlock()

	return c.next.RoundTrip(req)
}

// crossed returns the paths that crossed the transport, in order.
func (c *countingTransport) crossed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.paths)
}
