package oci

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The parts of a presigned location the redirect rows are about.
const (
	// storagePrefix is the path the object store serves blobs under. It is not
	// a registry path, so an error quoting it instead of the registry's would
	// be obvious.
	storagePrefix = "/storage"
	// signatureParam is the query a location carries, standing in for the
	// signature a presigned URL is signed with. No error may quote it back.
	signatureParam = "sig"
	// signatureValue is what that query holds.
	signatureValue = "deadbeef"
	// storageDetail is the body the object store refuses with. It is shaped
	// like the real thing — S3's SignatureDoesNotMatch echoes the signed
	// request it is complaining about — so every row asserting an error
	// carries no "?" and no signature is exercising the channel that would
	// carry one, not a fixture too polite to leak.
	storageDetail = "<Error><Resource>/storage/blobs/sha256:abc?sig=deadbeef&X-Amz-Signature=CAFEBABE</Resource></Error>"
)

// storageSecret is a value planted where a secret would be — in a cookie, in a
// location's userinfo — so a row can assert it never came out the other side.
const storageSecret = "not-in-any-message"

// storeRecord is one request the object store received, captured before it was
// answered.
type storeRecord struct {
	// method is the HTTP method the store saw, which says what a redirect did
	// to the method it started as.
	method string
	// path is the path the store was asked for.
	path string
	// query is the parsed query, which is where the signature rides.
	query url.Values
	// header is a copy of the request headers, because what this fixture
	// exists to answer is what the second request carried.
	header http.Header
}

// blobStore stands in for the presigned object storage a registry hands its
// blob reads to.
//
// It listens on the same loopback host as the registry fixture and a different
// port, which is deliberately the shape the standard library's own redirect
// rule reads as a single party: it compares domain names and ignores ports, so
// an automatic follow forwards a credential from one to the other.
type blobStore struct {
	// server is the httptest server the store runs on.
	server *httptest.Server

	// serveAs replaces what the store answers with, for the rows that need a
	// refusal, a redirect of the store's own, or the whole blob where a range
	// was asked for.
	serveAs http.HandlerFunc
	// requires makes the store serve only a request carrying an Authorization
	// header. It is the positive control: with it set, a read that leaks is
	// the only read that succeeds.
	requires bool

	// mu guards requests.
	mu sync.Mutex
	// requests are what arrived, in order.
	requests []storeRecord
}

// newBlobStore starts an object store that serves the fixture's payload. It
// shuts down with the test.
func newBlobStore(t *testing.T) *blobStore {
	t.Helper()

	store := &blobStore{}
	store.server = httptest.NewServer(store)
	t.Cleanup(store.server.Close)

	return store
}

// ServeHTTP records the request and answers it.
func (s *blobStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, storeRecord{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
		header: r.Header.Clone(),
	})
	s.mu.Unlock()

	if s.requires && r.Header.Get(headerAuthorization) == "" {
		http.Error(w, storageDetail, http.StatusForbidden)

		return
	}

	if s.serveAs != nil {
		s.serveAs(w, r)

		return
	}

	// ServeContent is what answers a Range header the way an object store
	// does, 206 and Content-Range included, without this fixture reimplementing
	// the arithmetic it would then be asserting against.
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(authPayload))
}

// all returns every request the store received, in arrival order.
func (s *blobStore) all() []storeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.requests)
}

// host returns the host and port the store listens on, which is the only part
// of it an error may name.
func (s *blobStore) host(t *testing.T) string {
	t.Helper()

	parsed, err := url.Parse(s.server.URL)
	require.NoError(t, err)

	return parsed.Host
}

// location returns the presigned URL a registry redirects r to: the store's
// address, the blob's path under its own prefix, and a query standing in for a
// signature.
func (s *blobStore) location(r *http.Request) string {
	return s.server.URL + storagePrefix + r.URL.Path + "?" + signatureParam + "=" + signatureValue
}

// redirectBlobReads returns the registry behavior every redirect row starts
// from: answer a blob read with a location at the store, and everything else
// the way the fixture always does.
func redirectBlobReads(store *blobStore, status int) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !isBlobRead(r) {
			return false
		}

		w.Header().Set(headerLocation, store.location(r))
		w.WriteHeader(status)

		return true
	}
}

// isBlobRead reports whether a request is the one a registry offloads: a GET
// or a HEAD of a blob by digest.
func isBlobRead(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	return strings.Contains(r.URL.Path, "/blobs/") && !strings.Contains(r.URL.Path, "/blobs/uploads/")
}

// deadStorage returns the address of a listener that has been closed, so a hop
// to it fails to connect at once instead of hanging.
func deadStorage(t *testing.T) string {
	t.Helper()

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()

	return address
}

// assertNamesNoPeerValue checks the property every off-registry failure has to
// have: it names only the original registry operation and fixed status, never
// a target-selected host, path, query, or response body.
func assertNamesNoPeerValue(t *testing.T, err error, host string) {
	t.Helper()

	message := err.Error()

	assert.Contains(t, message, "/v2/"+authRepo+"/blobs/", "the original registry operation stays diagnostic")
	assert.NotContains(t, message, host, "the target host is peer-selected direct reflection material")
	assert.NotContains(t, message, "?", "a query is where a signature lives")
	assert.NotContains(t, message, signatureParam+"=", "and this is what one looks like")
	assert.NotContains(t, message, storagePrefix, "the location's path is not the registry's path")
}
