package oci_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/oci"
)

// repoName is the repository path the fake registries in these tests serve.
// It has two components so the tests prove a nested path survives into the
// endpoint URL.
const repoName = "team/artifact"

// tag is the tag the fixtures bind their manifests to.
const tag = "v1"

// newRegistry starts a fake registry served by handler and returns a
// repository bound to ref on it. ref is the part of the reference after the
// registry, such as "team/artifact:v1". The server shuts down with the test.
func newRegistry(t *testing.T, handler http.Handler, ref string) *oci.Repository {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	repo, err := oci.NewRepository(hostOf(t, server)+"/"+ref, oci.WithPlainHTTP())
	require.NoError(t, err)

	return repo
}

// hostOf returns the host and port a test server listens on, which is the
// registry half of a reference.
func hostOf(t *testing.T, server *httptest.Server) string {
	t.Helper()

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)

	return parsed.Host
}

// recorded is one request a fake registry received, captured before the
// handler answered it.
type recorded struct {
	// method is the HTTP method the fake registry saw.
	method string
	// host is the Host header the request carried.
	host string
	// path is the URL path the request addressed.
	path string
	// query is the parsed query string.
	query url.Values
	// header is a copy of the request headers.
	header http.Header
	// contentLength is the length the request declared, -1 when unknown.
	contentLength int64
	// transferEncoding lists the transfer encodings; a chunked upload shows
	// up here, which is what the Content-Length assertions rule out.
	transferEncoding []string
	// body is the request body, read to its end.
	body []byte
}

// recorder collects the requests a fake registry received, so a test can
// assert on them after the call under test returns.
type recorder struct {
	// mu guards requests. It is load bearing: handlers run on the server's
	// goroutines.
	mu sync.Mutex
	// requests are the captured requests, in arrival order.
	requests []recorded
}

// record captures req, reading its body to the end.
func (r *recorder) record(t *testing.T, req *http.Request) {
	t.Helper()

	// The handler runs on the server's goroutine, where a require call's
	// FailNow is not allowed, so a failure is reported rather than fatal.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Errorf("fake registry could not read the request body: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = append(r.requests, recorded{
		method:           req.Method,
		host:             req.Host,
		path:             req.URL.Path,
		query:            req.URL.Query(),
		header:           req.Header.Clone(),
		contentLength:    req.ContentLength,
		transferEncoding: slices.Clone(req.TransferEncoding),
		body:             body,
	})
}

// all returns the requests recorded so far, in the order they arrived.
func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.requests)
}

// only returns the single request the recorder captured, failing the test
// when the fake registry saw a different number.
func (r *recorder) only(t *testing.T) recorded {
	t.Helper()

	requests := r.all()
	require.Len(t, requests, 1)

	return requests[0]
}
