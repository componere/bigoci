package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headerFieldPattern matches one header field of a log line: the bare dash that
// means the header was not there, or a quoted value.
//
// A legal header value holds spaces — "bytes 0-4/2048" is the ordinary case — so
// a pattern of non-space bytes would pass only for as long as no test served a
// realistic header. The quoting is what keeps one field one field, and this is
// the pattern that holds it to that.
const headerFieldPattern = `(?:-|"(?:[^"\\]|\\.)*")`

// logLines is one tap's output, grouped by the kind of line, with the provenance
// line the tap opens with kept aside.
type logLines struct {
	// provenance is the first line, which records which build wrote the log.
	provenance string
	// sent are the http> lines.
	sent []string
	// received are the http< lines.
	received []string
	// failed are the http! lines.
	failed []string
	// summary are the lines the tap wrote about itself rather than a request.
	summary []string
}

// splitLog groups everything a tap wrote so a test can count lines of one kind
// without matching the others by accident.
func splitLog(t *testing.T, raw string) logLines {
	t.Helper()

	var log logLines
	for line := range strings.Lines(strings.TrimSuffix(raw, "\n")) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "http> "):
			log.sent = append(log.sent, line)
		case strings.HasPrefix(line, "http< "):
			log.received = append(log.received, line)
		case strings.HasPrefix(line, "http! "):
			log.failed = append(log.failed, line)
		case strings.HasPrefix(line, "bigoci: debug:") && log.provenance == "":
			log.provenance = line
		default:
			log.summary = append(log.summary, line)
		}
	}

	return log
}

// isolatedTransport returns a transport private to one test.
//
// The tap tests drive real requests, and the default transport is shared
// process-wide: [net/http/httptest.Server.Close] closes the default
// transport's idle connections as a courtesy to tests that used it, so
// parallel tests sharing it can break each other's requests mid-flight —
// a HEAD through the tap failing with "CloseIdleConnections called" when a
// sibling test's server shut down. A per-test transport takes these tests
// out of that race in both directions. The nil-means-default fallback in
// newTap stays covered by every run-driven -debug test, which builds its
// tap the way the real CLI does.
func isolatedTransport(t *testing.T) *http.Transport {
	t.Helper()

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	return transport
}

// loopbackTransport dials the loopback interface whatever hostname it was given.
//
// That is what lets a test redirect across a hostname boundary — which is what
// makes the standard library strip the Authorization header — while both test
// servers really live on 127.0.0.1.
func loopbackTransport() *http.Transport {
	dialer := &net.Dialer{}

	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}
}

// portOf returns the port a test server is listening on.
func portOf(t *testing.T, rawURL string) string {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)

	return parsed.Port()
}

// get sends one request through client and drains the response, which is what a
// caller of the library would do.
func get(t *testing.T, client *http.Client, method, target string, header http.Header) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, target, nil)
	require.NoError(t, err)
	maps.Copy(req.Header, header)

	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return resp
}

// TestTapLogsOneRequestAsTwoLines is the shape of the whole format: one line when
// a request goes out, one when its response headers arrive, paired by sequence
// number, with a bare dash wherever a header was not there.
func TestTapLogsOneRequestAsTwoLines(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", "sha256:ab")
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	probe := newTap(&buf, isolatedTransport(t))
	get(t, &http.Client{Transport: probe}, http.MethodGet, srv.URL+"/v2/team/m/blobs/sha256:ab", nil)

	log := splitLog(t, buf.String())
	require.Len(t, log.sent, 1)
	require.Len(t, log.received, 1)
	assert.Empty(t, log.failed)
	assert.Contains(t, log.provenance, "bigoci reference CLI")

	assert.True(t, strings.HasPrefix(log.sent[0], "http> 0001 "), log.sent[0])
	assert.True(t, strings.HasPrefix(log.received[0], "http< 0001 "), log.received[0])

	assert.Contains(t, log.sent[0], "GET  "+srv.URL+"/v2/team/m/blobs/"+redactedDigestSegment)
	assert.NotContains(t, log.sent[0], "sha256:ab")
	assert.Contains(t, log.sent[0], "class=blob-read")
	assert.Contains(t, log.sent[0], "auth=none")
	assert.Contains(t, log.sent[0], "type=- range=- accept=-")

	assert.Contains(t, log.received[0], "status=200")
	assert.Contains(t, log.received[0], "dur=")
	assert.Contains(t, log.received[0], "clen=-2")
	assert.Contains(t, log.received[0], `ctype="present"`)
	assert.Contains(t, log.received[0], `ddigest="present"`)
	assert.Contains(t, log.received[0], "crange=- loc=-")
	assert.Contains(t, log.received[0], "retry-after=- challenge=-")
}

// TestTapExternalLayerSharesOneObserver proves bigoci can rebuild the CLI tap
// around a guarded base without copying its lock, sequence, or counters.
func TestTapExternalLayerSharesOneObserver(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	base := isolatedTransport(t)
	probe := newTap(&buf, base)
	assert.Same(t, base, probe.BigociExternalBase())

	layer := probe.BigociWrapExternal(base)
	get(t, &http.Client{Transport: layer}, http.MethodGet, srv.URL+"/token", nil)
	probe.writeSummary()

	log := splitLog(t, buf.String())
	require.Len(t, log.sent, 1)
	require.Len(t, log.received, 1)
	require.Len(t, log.summary, 1)
	assert.Contains(t, log.sent[0], "http> 0001")
	assert.Contains(t, log.summary[0], "requests=1")
}

// TestResponseContentLengthRevealsOnlyWhetherKnown checks that no exact peer
// response length survives while the clen field retains its numeric grammar.
func TestResponseContentLengthRevealsOnlyWhetherKnown(t *testing.T) {
	t.Parallel()

	assert.Equal(t, responseLengthUnknown, responseContentLength(-1))
	assert.Equal(t, responseLengthUnknown, responseContentLength(-99))
	assert.Equal(t, responseLengthKnown, responseContentLength(0))
	assert.Equal(t, responseLengthKnown, responseContentLength(731984620517284693))
}

// TestTapLogsRanges checks the two fields a resumed or partial read turns on, in
// both directions.
func TestTapLogsRanges(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-4/2048")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	probe := newTap(&buf, isolatedTransport(t))
	header := http.Header{"Range": []string{"bytes=0-4"}}
	get(t, &http.Client{Transport: probe}, http.MethodGet, srv.URL+"/v2/team/m/blobs/sha256:ab", header)

	log := splitLog(t, buf.String())
	require.Len(t, log.sent, 1)
	require.Len(t, log.received, 1)
	assert.Contains(t, log.sent[0], `range="bytes=0-4"`)
	assert.Contains(t, log.received[0], "status=206")
	assert.Contains(t, log.received[0], `crange="present"`)
}

// TestTapLogsATransportFailureAsOneLine checks the case a dead port produces: no
// response line, one failed line, and the error quoted so it cannot become two.
func TestTapLogsATransportFailureAsOneLine(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := srv.URL + "/v2/team/m/manifests/v1"
	srv.Close()

	var buf bytes.Buffer
	probe := newTap(&buf, isolatedTransport(t))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)

	_, err = (&http.Client{Transport: probe}).Do(req)
	require.Error(t, err)

	log := splitLog(t, buf.String())
	require.Len(t, log.sent, 1)
	assert.Empty(t, log.received)
	require.Len(t, log.failed, 1)

	assert.Contains(t, log.failed[0], "class=manifest-read")
	assert.Contains(t, log.failed[0], "dur=")
	assert.Contains(t, log.failed[0], `err="`+redactedTargetError+`"`)
}

// TestTapRedactsAnOffOriginTransportFailure checks the error half of the
// origin boundary. A dial failure can repeat the hostname it was handed, so
// replacing only the URL field would still disclose a credential-bearing host.
func TestTapRedactsAnOffOriginTransportFailure(t *testing.T) {
	t.Parallel()

	const (
		hostTicket  = "redeemable-failed-host-ticket"
		pathTicket  = "redeemable-failed-path-ticket"
		queryTicket = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.Contains(addr, hostTicket) {
				return nil, fmt.Errorf("dial %s: injected transport failure", addr)
			}

			return dialer.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)

	var buf bytes.Buffer
	probe := newTap(&buf, transport)
	client := &http.Client{Transport: probe}
	get(t, client, http.MethodGet, origin.URL+"/v2/team/m/blobs/sha256:ab", nil)

	target := "http://" + hostTicket + ".example:" + portOf(t, origin.URL) +
		"/v2/team/m/blobs/" + pathTicket + "?digest=" + queryTicket
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)

	_, err = client.Do(req)
	require.Error(t, err)

	log := splitLog(t, buf.String())
	require.Len(t, log.failed, 1)
	assert.Contains(t, log.failed[0], offOriginTarget)
	assert.Contains(t, log.failed[0], `err="`+redactedTargetError+`"`)
	for _, credential := range []string{hostTicket, pathTicket, queryTicket} {
		assert.NotContains(t, buf.String(), credential)
	}
}

// TestTapRedactsAMasqueradingTokenTransportFailure pins the same decision for a
// same-origin token realm whose path is deliberately classified as blob-read.
// The original transport detail is unsafe because it can repeat the full URL.
func TestTapRedactsAMasqueradingTokenTransportFailure(t *testing.T) {
	t.Parallel()

	const (
		pathTicket  = "redeemable-masquerading-path-ticket"
		queryTicket = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	)

	base, err := url.Parse("https://reg.example.com/v2/team/m/manifests/v1")
	require.NoError(t, err)
	target, err := url.Parse(
		"https://reg.example.com/v2/team/m/blobs/" + pathTicket +
			"?digest=" + queryTicket + "&scope=repository:team/m:pull",
	)
	require.NoError(t, err)

	var buf bytes.Buffer
	probe := newTap(&buf, nil)
	_ = probe.renderTargetURL(base, classManifestRead)
	req := requestFor(t, target.String(), nil)
	line := probe.errorLine(
		2,
		req,
		classBlobRead,
		0,
		fmt.Errorf("GET %s: injected transport failure", target),
	)

	assert.Contains(t, line, "https://reg.example.com"+redactedTargetPath)
	assert.Contains(t, line, "class=blob-read")
	assert.Contains(t, line, `err="`+redactedTargetError+`"`)
	for _, credential := range []string{pathTicket, queryTicket} {
		assert.NotContains(t, line, credential)
	}
}

// TestTapLogsBothHopsOfARedirect pins the tap's own sightline: a redirect to
// another origin is visible as a second request line, while credentials in the
// target host, path, and even a digest-shaped query remain unrepresentable. The
// client here is a bare [net/http.Client] on purpose — the tap must see hops
// whatever follows them, and the library no longer lets the standard library
// follow at all.
func TestTapLogsBothHopsOfARedirect(t *testing.T) {
	t.Parallel()

	const (
		hostTicket  = "redeemable-host-ticket"
		pathTicket  = "redeemable-path-ticket"
		queryTicket = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("hello"))
	}))
	defer target.Close()

	targetPort := portOf(t, target.URL)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(
			"Location",
			"http://"+hostTicket+".example:"+targetPort+"/v2/team/m/blobs/"+pathTicket+"?digest="+queryTicket,
		)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	var buf bytes.Buffer
	transport := loopbackTransport()
	t.Cleanup(transport.CloseIdleConnections)
	probe := newTap(&buf, transport)
	header := http.Header{"Authorization": []string{"Bearer " + secret}}
	get(t, &http.Client{Transport: probe}, http.MethodGet, origin.URL+"/v2/team/m/blobs/sha256:ab", header)

	log := splitLog(t, buf.String())
	require.Len(t, log.sent, 2)
	require.Len(t, log.received, 2)

	assert.Contains(t, log.sent[0], "auth=bearer")
	assert.Contains(t, log.sent[1], "auth=none", "a cross-host hop must show as auth=none, and the tap must see it")
	assert.Contains(t, log.sent[1], offOriginTarget)
	assert.Contains(t, log.received[0], "status=307")
	assert.Contains(t, log.received[0], `loc="`+offOriginTarget+`"`)
	assert.Contains(t, log.received[1], "status=200")
	assert.Contains(t, log.received[1], offOriginTarget)
	for _, credential := range []string{secret, hostTicket, pathTicket, queryTicket} {
		assert.NotContains(t, buf.String(), credential)
	}
}

// TestTapSummary checks the accounting the summary line reports, including the one
// case that is easy to get wrong: a blob check that answers 404 is a miss, which is
// the answer the question asked for and not a failure.
//
// The whole line is asserted, because its shape is the contract: every class is
// listed every time, in one order, so a gate can grep for the zero it expects
// rather than infer it from a field that is not there.
func TestTapSummary(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "sha256:missing"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "sha256:broken"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	var buf bytes.Buffer
	probe := newTap(&buf, isolatedTransport(t))
	client := &http.Client{Transport: probe}

	get(t, client, http.MethodHead, srv.URL+"/v2/team/m/blobs/sha256:missing", nil)
	get(t, client, http.MethodHead, srv.URL+"/v2/team/m/blobs/sha256:present", nil)
	get(t, client, http.MethodGet, srv.URL+"/v2/team/m/blobs/sha256:broken", nil)
	get(t, client, http.MethodPut, srv.URL+"/v2/team/m/manifests/v1", nil)
	probe.writeSummary()

	log := splitLog(t, buf.String())
	require.Len(t, log.summary, 1)

	assert.Equal(t,
		"bigoci: http requests=4 failed=1 blob-check=2 (1 hit, 1 miss) blob-write=0 upload-open=0 "+
			"blob-read=1 manifest-read=0 manifest-write=1 manifest-check=0 other=0",
		log.summary[0],
	)
}

// TestClassify checks the URL-shape table on its own, so a change to it is a
// change to one obvious list rather than to a log a test happened to read.
func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
		want   class
	}{
		{name: "open an upload", method: http.MethodPost, path: "/v2/t/m/blobs/uploads/", want: classUploadOpen},
		{name: "finish an upload", method: http.MethodPut, path: "/v2/t/m/blobs/uploads/9f", want: classBlobWrite},
		{name: "stream to an upload", method: http.MethodPatch, path: "/v2/t/m/blobs/uploads/9f", want: classBlobWrite},
		{name: "check a blob", method: http.MethodHead, path: "/v2/t/m/blobs/sha256:ab", want: classBlobCheck},
		{name: "read a blob", method: http.MethodGet, path: "/v2/t/m/blobs/sha256:ab", want: classBlobRead},
		{name: "read a manifest", method: http.MethodGet, path: "/v2/t/m/manifests/v1", want: classManifestRead},
		{name: "write a manifest", method: http.MethodPut, path: "/v2/t/m/manifests/v1", want: classManifestWrite},
		{name: "check a manifest", method: http.MethodHead, path: "/v2/t/m/manifests/v1", want: classManifestCheck},
		{name: "the version check", method: http.MethodGet, path: "/v2/", want: classOther},
		{name: "a token exchange", method: http.MethodGet, path: "/token", want: classOther},
		{name: "a method nobody expects", method: http.MethodDelete, path: "/v2/t/m/blobs/sha256:ab", want: classOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, classify(tt.method, tt.path))
		})
	}
}

// TestTapUnderConcurrency is the interleaving check. Every line must be whole and
// match the grammar, and every request must be paired, with as many workers running
// as a real transfer would have.
//
// The server answers with a Content-Range, which holds a space. That is what makes
// the grammar check mean something: a whole-line pattern only proves the quoting
// works if a value that would split a field without it is actually in the log.
func TestTapUnderConcurrency(t *testing.T) {
	t.Parallel()

	const workers, each = 8, 50

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 0-4/2048")
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	probe := newTap(&buf, isolatedTransport(t))
	client := &http.Client{Transport: probe}

	failures := make([]error, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			for i := range each {
				target := fmt.Sprintf("%s/v2/team/m/blobs/sha256:%02d%02d", srv.URL, worker, i)
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
				if err != nil {
					failures[worker] = err

					return
				}

				resp, err := client.Do(req)
				if err != nil {
					failures[worker] = err

					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				if closeErr := resp.Body.Close(); closeErr != nil {
					failures[worker] = closeErr

					return
				}
			}
		})
	}
	wg.Wait()

	for _, err := range failures {
		require.NoError(t, err)
	}

	log := splitLog(t, buf.String())
	require.Len(t, log.sent, workers*each)
	require.Len(t, log.received, workers*each)
	assert.Empty(t, log.failed)
	assert.Empty(t, log.summary)

	sentPattern := regexp.MustCompile(
		`^http> \d{4,} \+\d+\.\d{3}s [A-Z]+ +\S+ class=[a-z-]+ auth=(none|bearer|basic|other) clen=-?\d+ ` +
			`type=` + headerFieldPattern + ` range=` + headerFieldPattern + ` accept=` + headerFieldPattern + `$`,
	)
	receivedPattern := regexp.MustCompile(
		`^http< \d{4,} \+\d+\.\d{3}s [A-Z]+ +\S+ class=[a-z-]+ status=\d{3} dur=\S+ clen=-?\d+ ` +
			`ctype=` + headerFieldPattern + ` crange=` + headerFieldPattern + ` loc=` + headerFieldPattern +
			` ddigest=` + headerFieldPattern + ` retry-after=` + headerFieldPattern +
			` challenge=` + headerFieldPattern + `$`,
	)

	seqs := make(map[string]bool, workers*each)
	for _, line := range log.sent {
		require.Regexp(t, sentPattern, line)
		seqs[strings.Fields(line)[1]] = true
	}
	for _, line := range log.received {
		require.Regexp(t, receivedPattern, line)
		assert.Contains(t, line, `crange="present"`, "a peer response header must retain presence only")
	}
	assert.Len(t, seqs, workers*each, "every request must get its own sequence number")
}
