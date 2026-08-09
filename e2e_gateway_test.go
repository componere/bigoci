package bigoci_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci"
)

// The bearer gateway's own vocabulary: the endpoint it issues tokens at, the
// name it calls itself by, and the two headers the exchange rides in.
const (
	// gatewayTokenPath is the path the gateway's token endpoint answers on,
	// and the path its challenges name as the realm.
	gatewayTokenPath = "/token"
	// gatewayService is what the gateway calls itself in the service parameter
	// of a challenge. It is deliberately not the host bigoci dialed: a
	// credential must be looked up under the host, never under this.
	gatewayService = "bigoci-gateway"
	// gatewayHeaderAuth is the request header a credential or a token rides in.
	gatewayHeaderAuth = "Authorization"
	// gatewayHeaderChallenge is the response header a refusal states its
	// requirement in.
	gatewayHeaderChallenge = "WWW-Authenticate"
	// gatewayBearerPrefix is what a token is presented behind.
	gatewayBearerPrefix = "Bearer "
)

// The numbers the gateway rows are built from.
const (
	// gatewayTokenSeconds is the lifetime the gateway states on every token it
	// issues. It is long enough that no row can re-mint because the clock ran
	// out: the only expiry these rows exercise is the one they arrange
	// themselves, and a count that moved for a second reason would prove
	// nothing.
	gatewayTokenSeconds = 300
	// gatewayForever is the lifespan of a token the gateway never treats as
	// expired.
	gatewayForever = 0
	// gatewayLifespan is how many requests one token authenticates in the
	// expiry row.
	//
	// It is four for two reasons. A pull of this fixture makes exactly five
	// requests — one manifest and four parts — so the fifth is always refused
	// and the row cannot pass without exercising expiry. And it is above the
	// worker count, which is what keeps the row honest: a token refused before
	// any request carrying it was answered is a credential that never worked,
	// and bigoci is right to give up on one. Only a token that worked and then
	// stopped is an expiry.
	gatewayLifespan = 4
	// gatewayWorkers is how many parts the gateway rows move at once. Two is
	// enough to make the token a shared resource that several requests reach
	// for at once, and it stays under gatewayLifespan.
	gatewayWorkers = 2
	// tokensPerPush is how many tokens a push needs: one for the scope its
	// blob checks read under, and one for the scope its uploads and its
	// manifest write under. The scope is a function of the method, which is
	// what holds the count to two.
	tokensPerPush = 2
	// tokensPerPull is how many a pull needs. Everything it does is a read, so
	// one scope covers all of it.
	tokensPerPull = 1
	// maxAttempts is the library's per-part attempt budget. A part that needed
	// more than this would have failed the transfer, so a green row already
	// implies it; asserting it names the number the row is really about.
	maxAttempts = 4
)

// TestE2EBearerGatewayMovesFilesWithOneTokenPerScope runs a whole transfer
// through the bearer exchange the distribution spec defines.
//
// zot behind it authenticates nothing: every challenge, every token, and every
// refusal in this file is the gateway's, which is what lets a row state the
// exchange exactly — how many tokens a transfer costs, and what it does when
// one stops working. That exchange is otherwise only exercised against a cloud
// registry by hand, so this is the gate that runs on every commit.
func TestE2EBearerGatewayMovesFilesWithOneTokenPerScope(t *testing.T) {
	const repo = "e2e/gateway"

	reg := newZot(t)
	gate := newGateway(t, reg.host, gatewayForever)
	client := newClient(t, bigoci.WithPlainHTTP(), bigoci.WithCredentials(authUser, authPassword))

	source := newRandomFile(t, multiSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), gate.at.taggedRef(repo, tag), bigoci.FromFile(source),
		bigoci.WithPartSize(multiPartSize), bigoci.WithWorkers(gatewayWorkers),
	)
	require.NoError(t, err)
	assert.Equal(
		t, tokensPerPush, gate.tokens(),
		"a push asks for one token per scope: one to check blobs with, one to write them with",
	)

	require.NoError(t, client.Pull(
		t.Context(), gate.at.taggedRef(repo, tag), bigoci.ToFile(dest), bigoci.WithWorkers(gatewayWorkers),
	))
	assert.Equal(
		t, tokensPerPush+tokensPerPull, gate.tokens(),
		"a pull reads everything under one scope, and it holds none of the push's tokens: the cache is per transfer",
	)

	assert.Equal(t, fileDigest(t, source), fileDigest(t, dest), "the pulled file must be byte-identical")
	assert.Zero(t, gate.expired(), "nothing expired here; that is the other row")
}

// TestE2EBearerGatewaySurvivesTokensThatExpireMidTransfer is the row the whole
// gateway exists for.
//
// A token that stops working in the middle of a transfer is the one
// authentication failure that is not the caller's fault and must not end the
// transfer. Whether it costs anything depends on the request it happened to:
// one whose body can be sent again is re-issued on the spot, and one whose
// body is a section of a file on disk costs the part an attempt and is
// re-streamed by the orchestrator. Both paths run here, and what proves they
// ran is that a transfer which met several refusals still moved every byte.
func TestE2EBearerGatewaySurvivesTokensThatExpireMidTransfer(t *testing.T) {
	const repo = "e2e/gateway-expiry"

	reg := newZot(t)
	gate := newGateway(t, reg.host, gatewayLifespan)
	client := newClient(t, bigoci.WithPlainHTTP(), bigoci.WithCredentials(authUser, authPassword))

	source := newRandomFile(t, multiSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), gate.at.taggedRef(repo, tag), bigoci.FromFile(source),
		bigoci.WithPartSize(multiPartSize), bigoci.WithWorkers(gatewayWorkers),
	)
	require.NoError(t, err, "a push whose token expired part way through must still finish")

	require.NoError(t, client.Pull(
		t.Context(), gate.at.taggedRef(repo, tag), bigoci.ToFile(dest), bigoci.WithWorkers(gatewayWorkers),
	), "a pull whose token expired part way through must still finish")

	assert.Equal(t, fileDigest(t, source), fileDigest(t, dest), "the pulled file must be byte-identical")

	assert.Positive(t, gate.expired(), "no request was ever refused for a spent token: this row proved nothing")
	assert.Greater(
		t, gate.tokens(), tokensPerPush+tokensPerPull,
		"a transfer that re-minted nothing cost exactly the scopes' worth of tokens, so nothing expired",
	)
	assert.LessOrEqual(
		t, gate.log.most(classUploadComplete), maxAttempts,
		"a refused upload costs the part one attempt, and no part may spend more than its budget",
	)
	assert.LessOrEqual(t, gate.log.most(classBlobGet), maxAttempts, "no part may be read more often than its budget")
}

// gatewayAnswer is the token document the gateway's endpoint returns, in the
// shape the distribution spec's token flow defines.
type gatewayAnswer struct {
	// Token is the bearer token itself.
	Token string `json:"token"`
	// ExpiresIn is how many seconds the issuer says it is good for.
	ExpiresIn int `json:"expires_in"`
}

// gateway is an in-process registry that authenticates with bearer tokens and
// proxies everything it accepts to a real registry behind it.
//
// It is the fixture for the half of authentication no local registry speaks: a
// challenge naming a token endpoint, an exchange against that endpoint, and a
// token that stops working while a transfer is still running. Everything it
// answers, it answers itself; zot behind it never sees a credential and never
// refuses anything, so every refusal a row observes is this fixture's and its
// meaning is exact.
//
// It deliberately does not redirect blob reads to storage. That is the other
// half of a cloud registry's behavior, and it arrives with the phase that
// implements it; this type is shaped so that adding it is a branch in
// [gateway.ServeHTTP] and nothing else.
type gateway struct {
	// at is the registry address rows aim their transfers at.
	at zot
	// log counts every registry request that reached the gateway, refused or
	// not, which is what makes a per-part attempt count a count of attempts
	// rather than of successes.
	log *authLog
	// proxy forwards an authenticated request to the registry behind.
	proxy *httputil.ReverseProxy
	// realm is the absolute URL of the token endpoint, which every challenge
	// names.
	realm string
	// lifespan is how many requests one token authenticates before the gateway
	// treats it as expired, or gatewayForever for a token that never does.
	lifespan int

	// mu guards everything below.
	mu sync.Mutex
	// issued is how many tokens the endpoint has handed out.
	issued int
	// refused is how many requests were turned away for carrying a token the
	// gateway had stopped accepting.
	refused int
	// left is how many more requests each live token authenticates.
	left map[string]int
}

// newGateway starts a bearer gateway in front of upstream and returns it.
//
// lifespan is how many requests each token it issues authenticates;
// gatewayForever leaves them good for the whole run.
func newGateway(t *testing.T, upstream string, lifespan int) *gateway {
	t.Helper()

	origin, err := url.Parse("http://" + upstream)
	require.NoError(t, err)

	g := &gateway{
		log:      &authLog{},
		proxy:    httputil.NewSingleHostReverseProxy(origin),
		lifespan: lifespan,
		left:     make(map[string]int),
	}
	g.proxy.Transport = newTransport(t)

	server := httptest.NewServer(g)
	t.Cleanup(server.Close)

	g.at = zot{host: strings.TrimPrefix(server.URL, "http://")}
	g.realm = server.URL + gatewayTokenPath

	t.Logf("a bearer gateway on %s is serving %s in front of %s", g.at.host, apiPath, upstream)

	return g
}

// ServeHTTP answers one request: a token exchange, a challenge, or a proxied
// registry request.
func (g *gateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == gatewayTokenPath {
		g.issue(w, req)

		return
	}

	g.log.record(req)

	if !g.spend(strings.TrimPrefix(req.Header.Get(gatewayHeaderAuth), gatewayBearerPrefix)) {
		g.challenge(w, req)

		return
	}

	// The token has done its job and the registry behind has no use for it.
	// Stripping it here is what makes that registry's silence meaningful: it
	// never sees a credential, so nothing it does can depend on one.
	req.Header.Del(gatewayHeaderAuth)
	g.proxy.ServeHTTP(w, req)
}

// tokens returns how many tokens the endpoint has issued.
func (g *gateway) tokens() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.issued
}

// expired returns how many requests were refused for carrying a token the
// gateway had stopped accepting.
func (g *gateway) expired() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.refused
}

// issue answers a token request.
//
// The credential arrives in a Basic header on a GET, which is what the token
// flow defines and the only shape bigoci sends: a secret in a request body is
// a secret in somebody's access log. A request carrying the wrong one is
// refused with no challenge, because a token endpoint that challenged its own
// refusal would be asking the client to try the same thing again.
func (g *gateway) issue(w http.ResponseWriter, req *http.Request) {
	user, password, ok := req.BasicAuth()
	if !ok || user != authUser || password != authPassword {
		http.Error(w, "the gateway does not know that credential", http.StatusUnauthorized)

		return
	}

	g.mu.Lock()
	g.issued++
	token := "gateway-token-" + strconv.Itoa(g.issued)
	g.left[token] = g.lifespan
	g.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gatewayAnswer{Token: token, ExpiresIn: gatewayTokenSeconds})
}

// spend reports whether token authenticates one more request, and counts the
// use when it does.
//
// A token with no uses left is not deleted: it stays known and unusable, which
// is what an expired token is. Deleting it would make a second refusal
// indistinguishable from a token the gateway never issued.
func (g *gateway) spend(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	left, known := g.left[token]
	if !known {
		return false
	}

	if g.lifespan == gatewayForever {
		return true
	}

	if left <= 0 {
		g.refused++

		return false
	}

	g.left[token] = left - 1

	return true
}

// challenge refuses a request and states how to authenticate it.
//
// The scope names the access this very request needs, which is what a registry
// answering a repository request sends. The realm is the gateway's own token
// endpoint; the service is a name of the gateway's choosing, which bigoci
// echoes to the endpoint and must never use as a credential lookup key.
//
// The body is drained first. A refusal that arrives while the client is still
// streaming a part would otherwise race the write, and this fixture is here to
// test what bigoci does with the refusal, not what it does with a connection
// that died mid-body.
func (g *gateway) challenge(w http.ResponseWriter, req *http.Request) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}

	w.Header().Set(gatewayHeaderChallenge, strings.Join([]string{
		`Bearer realm="` + g.realm + `"`,
		`service="` + gatewayService + `"`,
		`scope="` + gatewayScope(req) + `"`,
	}, ","))
	http.Error(w, "the gateway requires a token", http.StatusUnauthorized)
}

// gatewayScope returns the access grant a request needs, spelled the way the
// distribution spec spells one.
func gatewayScope(req *http.Request) string {
	actions := "pull,push"
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		actions = "pull"
	}

	return "repository:" + gatewayRepository(req.URL.Path) + ":" + actions
}

// gatewayRepository returns the repository name a distribution-spec path
// addresses, which is everything between the API prefix and the endpoint
// segment.
func gatewayRepository(path string) string {
	trimmed := strings.TrimPrefix(path, apiPath)

	for _, segment := range []string{blobsSegment, manifestsSegment} {
		if name, _, found := strings.Cut(trimmed, segment); found {
			return name
		}
	}

	return trimmed
}
