package oci_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/oci"
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

// deadAddress returns the host and port of a listener that has been closed.
// Nothing answers there, so a connection to it is refused at once instead of
// hanging, which is the transport failure a test can produce without a
// network.
func deadAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	return address
}

// resetConnection sends whatever the handler has already written and then
// tears its connection down without a FIN handshake, cutting a response short
// part way through the body it declared a length for.
//
// A body cut short is the case that matters most: the request has already
// succeeded by then, so the failure surfaces from a Read long after anything
// could have wrapped it, and only the adapter's own body wrapper can name it.
func resetConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	// The handler runs on the server's goroutine, where a require call's
	// FailNow is not allowed, so a failure is reported rather than fatal.
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Error("the fake registry's response writer cannot flush")

		return
	}
	flusher.Flush()

	tearDownConnection(t, w)
}

// dropConnection tears the handler's connection down before it has answered
// anything at all, which is what a registry behind a load balancer that went
// away looks like from the client: the request went out and nothing came
// back.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	tearDownConnection(t, w)
}

// tearDownConnection takes the handler's connection over and closes it with a
// reset rather than a graceful shutdown, so the client sees the connection
// die mid-exchange instead of ending tidily.
func tearDownConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("the fake registry's response writer cannot be hijacked")

		return
	}

	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("the fake registry could not take over its connection: %v", err)

		return
	}

	// Lingering for no time at all is what turns the close into a reset.
	if tcp, isTCP := conn.(*net.TCPConn); isTCP {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
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
