package bigoci_test

import (
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

// The object store's vocabulary: where a location points, what makes one
// valid, and the header it will not take.
const (
	// storagePrefix is the path the object store serves blobs under, which is
	// not a path any registry would answer.
	storagePrefix = "/storage"
	// storageSigParam is the query a location carries. It stands in for the
	// signature every presigned URL is signed with, and the store honors each
	// one exactly once.
	storageSigParam = "sig"
	// storageHeaderAuth is the header the store refuses outright, because that
	// is what a signed request means: the URL is the authorization, and a
	// credential on top of it is a credential in somebody else's logs.
	storageHeaderAuth = "Authorization"
	// storageCut is how many bytes of one part's body the store sends before
	// the connection dies, which is what makes the next attempt a
	// continuation and forces it to ask the registry for a fresh location.
	storageCut = 64 << 10
)

// TestE2ERedirectMovesAWholeFileThroughSignedStorage is the gate for a
// registry that keeps its content somewhere else.
//
// Every blob read is answered with a location at an object store that refuses
// a credential outright, honors each signature once, and knows nothing about
// the registry in front of it. A pull that comes out byte-identical proves the
// mechanism works; the counts beside it prove why, which a working pull on its
// own does not — a real object store answers 200 to a leaked bearer token as
// happily as to a clean request.
func TestE2ERedirectMovesAWholeFileThroughSignedStorage(t *testing.T) {
	const repo = "e2e/redirect"

	reg := newZot(t)
	store := newSignedStorage(t, reg.host)
	front := newRedirectingRegistry(t, reg.host, store)
	client := newClient(t, bigoci.WithPlainHTTP())

	source := newRandomFile(t, multiSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), front.at.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
	)
	require.NoError(t, err, "a push is not redirected: only reads are")

	require.NoError(t, client.Pull(t.Context(), front.at.taggedRef(repo, tag), bigoci.ToFile(dest)))
	assert.Equal(t, fileDigest(t, source), fileDigest(t, dest), "the pulled file must be byte-identical")

	served := store.all()
	assert.GreaterOrEqual(t, len(served), multiParts, "every part came off the store")
	assert.Equal(t, len(served), front.issued(), "one location was signed for every request the store answered")

	for _, one := range served {
		assert.False(t, one.credentialed, "no request to signed storage may carry a credential")
	}

	assert.Len(t, distinctSignatures(served), len(served), "no signature was used twice")
}

// TestE2ERedirectAsksAgainWhenAPartBreaksMidBody is the row that proves no
// location is ever kept.
//
// The store cuts one part's body part way through, so the transfer continues
// that part from where it got to. A continuation that reused the location it
// already had would present a signature the store has already spent and be
// refused; what it does instead is ask the registry, take a fresh location,
// and read the rest of the part through it.
func TestE2ERedirectAsksAgainWhenAPartBreaksMidBody(t *testing.T) {
	const repo = "e2e/redirect-continue"

	reg := newZot(t)
	store := newSignedStorage(t, reg.host)
	store.cutAt = storageCut
	front := newRedirectingRegistry(t, reg.host, store)
	client := newClient(t, bigoci.WithPlainHTTP())

	source := newRandomFile(t, multiSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), front.at.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
	)
	require.NoError(t, err)

	require.NoError(t, client.Pull(
		t.Context(), front.at.taggedRef(repo, tag), bigoci.ToFile(dest), bigoci.WithWorkers(1),
	), "a part that broke mid-body must still finish")
	assert.Equal(t, fileDigest(t, source), fileDigest(t, dest), "the pulled file must be byte-identical")

	require.True(t, store.wasCut(), "no body was ever cut short: this row proved nothing")

	served := store.all()
	assert.Greater(t, len(served), multiParts, "the broken part was read twice")
	assert.Len(t, distinctSignatures(served), len(served), "and the second read carried a signature of its own")

	ranged, reread := 0, 0
	perBlob := make(map[string]int)

	for _, one := range served {
		assert.False(t, one.credentialed)
		perBlob[one.path]++

		if one.ranged {
			ranged++

			assert.Equal(t, http.StatusPartialContent, one.status, "a continuation asks for a range and gets one")
		}
	}

	for _, count := range perBlob {
		if count > 1 {
			reread++
		}
	}

	assert.Equal(t, 1, reread, "one part was read twice, and it is the one whose body was cut short")

	assert.Positive(t, ranged, "nothing continued from an offset, so nothing proved a location was not reused")
}

// TestE2ERedirectStorageThatWantsACredentialFailsThePull is the positive
// control for both rows above.
//
// They assert that no request to storage carried a credential. A store that
// was never reached would satisfy that perfectly, so the same fixture is
// flipped to serve only requests that authenticate: bigoci withholds the
// header by construction, the store refuses every read, and the pull fails.
// That failure is what proves the store's records are of traffic that happened.
func TestE2ERedirectStorageThatWantsACredentialFailsThePull(t *testing.T) {
	const repo = "e2e/redirect-control"

	reg := newZot(t)
	store := newSignedStorage(t, reg.host)
	front := newRedirectingRegistry(t, reg.host, store)
	client := newClient(t, bigoci.WithPlainHTTP())

	source := newRandomFile(t, multiSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), front.at.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
	)
	require.NoError(t, err)

	store.requires = true

	err = client.Pull(t.Context(), front.at.taggedRef(repo, tag), bigoci.ToFile(dest), bigoci.WithWorkers(1))

	require.Error(t, err, "the only pull that could succeed here is one that leaked the registry's credential")
	require.NotErrorIs(t, err, bigoci.ErrUnauthorized, "a signed URL that was refused is not the caller's credentials")
	assert.NotEmpty(t, store.all(), "and the store was asked, which is what the other rows rest on")
	assert.NoFileExists(t, dest, "a pull that could not read a part publishes nothing")
}

// storageRecord is one request the object store answered.
type storageRecord struct {
	// path is the path it was asked for, under the store's own prefix.
	path string
	// signature is the query the location carried, empty when it carried none.
	signature string
	// credentialed reports whether the request arrived with an Authorization
	// header, which is the whole question these rows exist to answer.
	credentialed bool
	// ranged reports whether it asked for a byte range, which is what a
	// continuation does.
	ranged bool
	// status is what the store answered with.
	status int
}

// signedStorage is the object store a registry hands its blob reads to.
//
// It is a stand-in for S3, GCS, or the storage behind GHCR, reduced to the
// three properties those share and a local registry has none of: the URL is
// the authorization, a credential on top of one is refused rather than
// ignored, and a signature is good for a single request. The last is what
// turns "bigoci never stores a location" from something a row can only fail to
// observe into something a row can make fail.
//
// It serves real content by forwarding an accepted request to the registry
// behind it, so the bytes a row compares are the bytes that were pushed.
type signedStorage struct {
	// server is the httptest server the store answers on.
	server *httptest.Server
	// proxy forwards an accepted request to the registry that holds the blob.
	proxy *httputil.ReverseProxy

	// requires flips the store into serving only requests that authenticate,
	// which is the positive control: with it set, the only pull that works is
	// one that leaked.
	requires bool
	// cutAt is how many body bytes one response is cut short after, zero for a
	// store that serves every response whole.
	cutAt int64

	// mu guards everything below.
	mu sync.Mutex
	// seen is what the store answered, in order.
	seen []storageRecord
	// spent are the signatures that have already been used.
	spent map[string]bool
	// handed records that the one cut has been given to a response.
	handed bool
	// cut records that it actually bit, which is the evidence a body really
	// ended part way through rather than merely being marked for it.
	cut bool
}

// newSignedStorage starts an object store in front of upstream and returns it.
func newSignedStorage(t *testing.T, upstream string) *signedStorage {
	t.Helper()

	origin, err := url.Parse("http://" + upstream)
	require.NoError(t, err)

	store := &signedStorage{
		proxy: httputil.NewSingleHostReverseProxy(origin),
		spent: make(map[string]bool),
	}
	store.proxy.Transport = newTransport(t)

	store.server = httptest.NewServer(store)
	t.Cleanup(store.server.Close)

	t.Logf("signed storage on %s is serving blobs out of %s", store.server.URL, upstream)

	return store
}

// ServeHTTP answers one request and records what it was and what it got.
//
// The record is filed from a deferred call because a cut body does not leave
// this handler through a return: the reverse proxy panics with
// [net/http.ErrAbortHandler] once a write fails, which is how it tears the
// connection down, and the server recovers it. A handler that recorded on the
// way out would lose exactly the request every row here is about.
func (s *signedStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	record := storageRecord{
		path:         r.URL.Path,
		signature:    r.URL.Query().Get(storageSigParam),
		credentialed: r.Header.Get(storageHeaderAuth) != "",
		ranged:       r.Header.Get("Range") != "",
	}
	writer := &countedWriter{ResponseWriter: w, status: http.StatusOK, cutAt: s.takeCut(), onCut: s.wasCutNow}

	defer func() {
		record.status = writer.status

		s.mu.Lock()
		s.seen = append(s.seen, record)
		s.mu.Unlock()
	}()

	s.answer(writer, r, record)
}

// all returns what the store answered, in arrival order.
func (s *signedStorage) all() []storageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]storageRecord(nil), s.seen...)
}

// wasCut reports whether the one cut this store was configured for really
// ended a body.
func (s *signedStorage) wasCut() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cut
}

// answer applies the store's three rules and forwards whatever survives them.
func (s *signedStorage) answer(w http.ResponseWriter, r *http.Request, record storageRecord) {
	if s.requires {
		if !record.credentialed {
			http.Error(w, "this storage serves only requests that authenticate", http.StatusForbidden)

			return
		}
	} else if record.credentialed {
		http.Error(w, "a presigned request carries no credential", http.StatusBadRequest)

		return
	}

	if !s.spend(record.signature) {
		http.Error(w, "that signature is not good for another request", http.StatusForbidden)

		return
	}

	r.URL.Path = strings.TrimPrefix(r.URL.Path, storagePrefix)
	r.URL.RawQuery = ""
	s.proxy.ServeHTTP(w, r)
}

// spend reports whether a signature is good for this request, and marks it
// used when it is. An absent signature is never good: an unsigned request is
// what a leaked or hand-built URL looks like.
func (s *signedStorage) spend(signature string) bool {
	if signature == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.spent[signature] {
		return false
	}
	s.spent[signature] = true

	return true
}

// takeCut hands the one cut to the first response that asks for it and to no
// other, so exactly one part of one transfer dies part way through.
func (s *signedStorage) takeCut() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cutAt == 0 || s.handed {
		return 0
	}
	s.handed = true

	return s.cutAt
}

// wasCutNow records that the cut reached a response body.
func (s *signedStorage) wasCutNow() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cut = true
}

// redirectingRegistry is a registry front end that answers every blob read
// with a signed location at an object store and forwards everything else to
// the registry behind it.
//
// It is the shape of every registry that offloads its content and of no local
// one, which is why it exists: without it, the redirect path would only ever
// run against a cloud registry by hand.
type redirectingRegistry struct {
	// at is the registry address rows aim their transfers at.
	at zot
	// log counts what went past, in the vocabulary the other e2e rows count in.
	log *authLog
	// proxy forwards everything that is not a blob read.
	proxy *httputil.ReverseProxy
	// store is where the blob reads are sent.
	store *signedStorage

	// mu guards signed.
	mu sync.Mutex
	// signed is how many locations have been handed out. It doubles as the
	// signature each one carries, so two reads of the same blob can never be
	// answered with the same location.
	signed int
}

// newRedirectingRegistry starts a redirecting front end for upstream and
// returns it.
func newRedirectingRegistry(t *testing.T, upstream string, store *signedStorage) *redirectingRegistry {
	t.Helper()

	origin, err := url.Parse("http://" + upstream)
	require.NoError(t, err)

	front := &redirectingRegistry{
		log:   &authLog{},
		proxy: httputil.NewSingleHostReverseProxy(origin),
		store: store,
	}
	front.proxy.Transport = newTransport(t)

	server := httptest.NewServer(front)
	t.Cleanup(server.Close)

	front.at = zot{host: strings.TrimPrefix(server.URL, "http://")}

	t.Logf("a redirecting registry on %s is serving %s in front of %s", front.at.host, apiPath, upstream)

	return front
}

// ServeHTTP answers one request: a location for a blob read, and a proxied
// request for everything else.
func (g *redirectingRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	g.log.record(req)

	class, _ := classifyRequest(req)
	if class != classBlobGet {
		g.proxy.ServeHTTP(w, req)

		return
	}

	w.Header().Set("Location", g.sign(req))
	w.WriteHeader(http.StatusTemporaryRedirect)
}

// issued returns how many locations the front end has signed.
func (g *redirectingRegistry) issued() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.signed
}

// sign returns the location a blob read is answered with: the store's address,
// the blob's own path under the store's prefix, and a signature no other
// location carries.
func (g *redirectingRegistry) sign(req *http.Request) string {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.signed++

	return g.store.server.URL + storagePrefix + req.URL.Path + "?" + storageSigParam + "=" + strconv.Itoa(g.signed)
}

// distinctSignatures returns how many different signatures a set of store
// records carried, which is how a row says no location was used twice.
func distinctSignatures(served []storageRecord) map[string]bool {
	seen := make(map[string]bool, len(served))

	for _, one := range served {
		seen[one.signature] = true
	}

	return seen
}
