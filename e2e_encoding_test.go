package bigoci_test

import (
	"bytes"
	"compress/gzip"
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

	"github.com/imgoci/bigoci"
	"github.com/imgoci/bigoci/internal/file"
)

// TestE2EPullRejectsAGzippedManifest is a complete pull whose only fault is
// that a middlebox gzipped the manifest. The parts behind it are identity-
// coded and untouched; the transfer must stop at the manifest, match no
// public sentinel, and leave nothing published.
func TestE2EPullRejectsAGzippedManifest(t *testing.T) {
	const repo = "e2e/gzip-manifest"

	reg := newZot(t)
	client := newClient(t, bigoci.WithPlainHTTP())
	source := newRandomFile(t, multiSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), reg.taggedRef(repo, tag), bigoci.FromFile(source),
		bigoci.WithPartSize(multiPartSize), bigoci.WithWorkers(1),
	)
	require.NoError(t, err)

	front := newGzippingProxy(t, reg.host, classManifestGet)

	err = client.Pull(
		t.Context(), front.at.taggedRef(repo, tag), bigoci.ToFile(dest), bigoci.WithWorkers(1),
	)

	assertCodedPullFailure(t, err, dest, "fetch the manifest", "GET /v2/"+repo+"/manifests/"+tag)
	assert.NoFileExists(
		t, dest+file.PartialSuffix,
		"a pull that never left the manifest must not litter a partial file",
	)
	assert.Equal(t, 1, front.log.count(classManifestGet), "a coded manifest is not worth another attempt")
	assert.Zero(t, front.log.count(classBlobGet), "a pull that refused the manifest must read no part")
	assert.Equal(t, 1, front.compressed(), "this row is vacuous unless the fixture gzipped the manifest")
}

// TestE2EPullRejectsAGzippedBlob is the matching complete pull whose manifest
// is identity-coded and whose fault is a gzipped part. The two rows are
// separate so a single coded-response helper cannot stand in for both paths.
func TestE2EPullRejectsAGzippedBlob(t *testing.T) {
	const repo = "e2e/gzip-blob"

	reg := newZot(t)
	client := newClient(t, bigoci.WithPlainHTTP())
	source := newRandomFile(t, multiSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), reg.taggedRef(repo, tag), bigoci.FromFile(source),
		bigoci.WithPartSize(multiPartSize), bigoci.WithWorkers(1),
	)
	require.NoError(t, err)

	front := newGzippingProxy(t, reg.host, classBlobGet)

	err = client.Pull(
		t.Context(), front.at.taggedRef(repo, tag), bigoci.ToFile(dest), bigoci.WithWorkers(1),
	)

	assertCodedPullFailure(t, err, dest, "fetch part 0", "GET /v2/"+repo+"/blobs/<digest>")
	assert.FileExists(
		t, dest+file.PartialSuffix,
		"a pull that failed on a part keeps the partial file a later resume hashes",
	)
	assert.Equal(t, 1, front.log.count(classManifestGet), "the manifest itself must arrive identity-coded")
	assert.Equal(t, 1, front.log.most(classBlobGet), "a coded part is not fetched again")
	assert.Positive(t, front.compressed(), "this row is vacuous unless the fixture gzipped a blob")
}

// gzippingProxy is a registry address that serves everything upstream does,
// except that successful responses of one class come back gzipped no matter
// what the client asked for.
//
// Compressing after the registry has answered, and against an identity
// request, is what makes the fixture a middlebox rather than a registry that
// negotiated gzip. To the client it is simply a different registry host.
type gzippingProxy struct {
	// at is the address rows aim their transfers at.
	at zot
	// log counts every request that went past, gzipped or not.
	log *authLog
	// mu guards gzipped, which ModifyResponse writes from whichever
	// goroutine is serving a response.
	mu sync.Mutex
	// gzipped is how many responses this fixture compressed.
	gzipped int
}

// compressed returns how many responses this fixture gzipped.
func (p *gzippingProxy) compressed() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.gzipped
}

// newGzippingProxy returns a registry address in front of upstream that
// forwards every request, asks the registry for identity bytes, and gzips
// every successful response of class.
func newGzippingProxy(t *testing.T, upstream, class string) *gzippingProxy {
	t.Helper()

	origin, err := url.Parse("http://" + upstream)
	require.NoError(t, err)

	front := &gzippingProxy{log: &authLog{}}
	proxy := httputil.NewSingleHostReverseProxy(origin)
	proxy.Transport = newTransport(t)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		// The registry must answer in identity so the coding this fixture
		// adds is the only one on the response. A middlebox that compresses
		// anyway is the whole of the row.
		req.Header.Set("Accept-Encoding", "identity")
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		got, _ := classifyRequest(resp.Request)
		if got != class || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil
		}

		if err := gzipResponse(resp); err != nil {
			return err
		}

		front.mu.Lock()
		front.gzipped++
		front.mu.Unlock()

		return nil
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		front.log.record(req)
		proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)

	front.at = zot{host: strings.TrimPrefix(server.URL, "http://")}
	t.Logf("a gzipping proxy on %s is serving %s in front of %s, compressing %s", front.at.host, apiPath, upstream, class)

	return front
}

// gzipResponse replaces resp's body with a gzipped copy and marks the
// Content-Encoding that implies. The original body is closed. Peer-selected
// encoding values never reach the caller of this helper as error text.
func gzipResponse(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()

		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(body); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	resp.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	resp.ContentLength = int64(buf.Len())
	resp.Uncompressed = false
	resp.Header.Set("Content-Encoding", "gzip")
	resp.Header.Set("Content-Length", strconv.Itoa(buf.Len()))

	return nil
}

// assertCodedPullFailure checks that a pull refused a non-identity content
// coding: no public sentinel, no retry budget, no published destination, the
// operation and the safe origin in the message, and none of the peer-chosen
// encoding.
func assertCodedPullFailure(t *testing.T, err error, dest, operation, origin string) {
	t.Helper()

	require.Error(t, err)
	require.NotErrorIs(t, err, bigoci.ErrNotFound)
	require.NotErrorIs(t, err, bigoci.ErrUnauthorized)
	require.NotErrorIs(t, err, bigoci.ErrNotBigociArtifact)
	require.NotErrorIs(t, err, bigoci.ErrDigestMismatch)
	require.NotErrorIs(t, err, bigoci.ErrPartTooLarge)
	require.NotContains(t, err.Error(), "after 4 attempts", "a coded response is terminal")
	require.ErrorContains(t, err, operation)
	require.ErrorContains(t, err, origin)
	require.ErrorContains(t, err, "the response is not identity coded")
	assert.NotContains(t, err.Error(), "gzip", "the peer-selected encoding is not safe log context")
	assert.NotContains(t, err.Error(), "Content-Encoding")
	assert.NoFileExists(t, dest, "a pull that refused a coded response publishes nothing")
}
