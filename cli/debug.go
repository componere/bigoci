package main

import (
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// durPrecision is how finely a request's duration is reported: fine enough
	// to see a stalled request, coarse enough to keep the column steady.
	durPrecision = 100 * time.Microsecond
	// revisionWidth is how much of a commit hash the provenance line shows.
	revisionWidth = 7
)

// class names one kind of registry request, inferred from the shape of the URL
// and the method and nothing else.
//
// Inferring it from the URL is what keeps the CLI ignorant of the artifact
// format: these are the shapes of the OCI distribution API, which every registry
// client speaks, not anything bigoci decided.
type class string

const (
	// classUploadOpen is a POST that starts a blob upload.
	classUploadOpen class = "upload-open"
	// classBlobWrite is a PUT or PATCH that sends blob bytes.
	classBlobWrite class = "blob-write"
	// classBlobCheck is a HEAD that asks whether a blob is already there.
	classBlobCheck class = "blob-check"
	// classBlobRead is a GET that reads blob bytes.
	classBlobRead class = "blob-read"
	// classManifestRead is a GET that reads a manifest.
	classManifestRead class = "manifest-read"
	// classManifestWrite is a PUT that writes a manifest.
	classManifestWrite class = "manifest-write"
	// classManifestCheck is a HEAD that asks whether a manifest is there.
	classManifestCheck class = "manifest-check"
	// classOther is every request whose URL matches none of the shapes above,
	// including the version check and a token exchange.
	classOther class = "other"
)

// counters tallies what a tap saw, for the one summary line it writes after a
// transfer.
//
// Every number here is inference from HTTP traffic rather than something the
// library reported. That is the point — it is what an observer outside the
// library can honestly say — and it is also the limit: a count that disagrees
// with the library is a question about the instrument, not proof about the
// library.
type counters struct {
	// byClass counts the requests of each class.
	byClass map[class]int
	// total is every request the tap forwarded.
	total int
	// failed is every request that came back a transport error, or a status of
	// 400 or more that was not a blob check answering "no".
	failed int
	// hits are the blob checks that found the blob already in the registry.
	hits int
	// misses are the blob checks that did not.
	misses int
}

// summary renders the one-line tally a tap writes after a transfer.
//
// Every class is listed every time, zero or not, so the line always has the
// same shape and a gate can grep for the zero it expects: a warm push is proved
// by reading "blob-write=0", not by noticing a field is missing.
func (c counters) summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bigoci: http requests=%d failed=%d", c.total, c.failed)

	for _, name := range classOrder() {
		fmt.Fprintf(&b, " %s=%d", name, c.byClass[name])
		if name == classBlobCheck {
			fmt.Fprintf(&b, " (%d hit, %d miss)", c.hits, c.misses)
		}
	}
	b.WriteString("\n")

	return b.String()
}

// tap is a [net/http.RoundTripper] that records every registry request and the
// response or error that answered it, and forwards both untouched.
//
// It is a pure observer. It never changes a request, never wraps or reads a body
// in either direction, never sets a timeout, and never sets a redirect policy.
// Sizes come from Content-Length alone. No body is ever logged, not even in part,
// which is what keeps a credential exchange out of the log by construction
// rather than by care.
type tap struct {
	// out receives the log lines, one Write call per whole line.
	out io.Writer
	// next carries the request onward; the tap only watches it go.
	next http.RoundTripper
	// start is when the tap was built, the zero of the elapsed clock every line
	// is stamped with.
	start time.Time
	// seq numbers requests so a sent line can be paired with the response or the
	// error that answered it, however many workers are running.
	seq atomic.Uint64
	// mu serializes the writes and the counter updates.
	mu sync.Mutex
	// counts is the tally the summary line reports.
	counts counters
}

// newTap builds a tap that writes to out and forwards to next, or to
// [net/http.DefaultTransport] when next is nil.
//
// It writes its first line here, before any request can go out, recording which
// build of the CLI produced the log that follows. A log that cannot be traced to
// a build is worth much less when the question is whether behavior changed.
func newTap(out io.Writer, next http.RoundTripper) *tap {
	if next == nil {
		next = http.DefaultTransport
	}

	t := &tap{
		out:    out,
		next:   next,
		start:  time.Now(),
		counts: counters{byClass: make(map[class]int)},
	}
	t.writeLine("bigoci: debug: " + provenance() + "\n")

	return t
}

// RoundTrip logs the request, forwards it, and logs whatever came back.
//
// The sent line is written before the request leaves, so a hang or a dead port is
// visible while it is happening rather than only once it is over. The duration on
// the received line is time to response headers, which on a large GET is time to
// first byte and says nothing about throughput.
func (t *tap) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.roundTrip(req, t.next)
}

// BigociExternalBase exposes the transport the observer forwards to so
// bigoci can preserve this tap around its guarded external transport.
func (t *tap) BigociExternalBase() http.RoundTripper {
	return t.next
}

// BigociWrapExternal rebuilds this observer around next while sharing its
// counters, clock, sequence, and output lock with registry requests.
func (t *tap) BigociWrapExternal(next http.RoundTripper) http.RoundTripper {
	return &tapLayer{tap: t, next: next}
}

// tapLayer forwards one external request through a guarded base while
// recording it in its parent tap's single request stream.
type tapLayer struct {
	// tap owns the observer state shared with registry requests.
	tap *tap
	// next is the guarded external base transport.
	next http.RoundTripper
}

// RoundTrip records req through the parent tap and forwards it to next.
func (t *tapLayer) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.tap.roundTrip(req, t.next)
}

// roundTrip logs the request, forwards it through next, and logs the answer.
func (t *tap) roundTrip(req *http.Request, next http.RoundTripper) (*http.Response, error) {
	seq := t.seq.Add(1)
	kind := classify(req.Method, req.URL.Path)
	t.writeLine(t.requestLine(seq, req, kind))

	started := time.Now()
	resp, err := next.RoundTrip(req)
	took := time.Since(started).Round(durPrecision)

	if err != nil {
		t.record(kind, 0, true)
		t.writeLine(t.errorLine(seq, req, kind, took, err))

		return nil, err
	}

	t.record(kind, resp.StatusCode, false)
	t.writeLine(t.responseLine(seq, req, resp, kind, took))

	return resp, nil
}

// requestLine renders the sent line: what is about to go out, and when.
func (t *tap) requestLine(seq uint64, req *http.Request, kind class) string {
	return fmt.Sprintf(
		"http> %04d %s %-4s %s class=%s auth=%s clen=%d %s\n",
		seq, t.elapsed(), req.Method, redactURL(req.URL), kind,
		authScheme(req.Header), req.ContentLength, requestHeaderFields(req.Header),
	)
}

// responseLine renders the received line, stamped with the time the response
// headers arrived.
//
// A clen of -1 means the response did not say how long it was. On a blob upload
// that would be a regression of the library's explicit Content-Length invariant,
// which is exactly the kind of thing this line exists to make visible.
func (t *tap) responseLine(seq uint64, req *http.Request, resp *http.Response, kind class, took time.Duration) string {
	return fmt.Sprintf(
		"http< %04d %s %-4s %s class=%s status=%d dur=%s clen=%d %s\n",
		seq, t.elapsed(), req.Method, redactURL(req.URL), kind,
		resp.StatusCode, took, resp.ContentLength, responseHeaderFields(req.URL, resp.Header),
	)
}

// errorLine renders the failed line for a request that never got a response. The
// error is quoted, so however many lines it would have printed it stays one.
func (t *tap) errorLine(seq uint64, req *http.Request, kind class, took time.Duration, err error) string {
	return fmt.Sprintf(
		"http! %04d %s %-4s %s class=%s dur=%s err=%s\n",
		seq, t.elapsed(), req.Method, redactURL(req.URL), kind, took, strconv.Quote(err.Error()),
	)
}

// writeSummary writes the tally of everything the tap saw. It runs once, after
// the transfer and before the line that says how the transfer ended.
func (t *tap) writeSummary() {
	t.mu.Lock()
	line := t.counts.summary()
	t.mu.Unlock()

	t.writeLine(line)
}

// record tallies one finished request.
//
// A blob check that answers 404 is a miss, not a failure: it is the answer the
// question asked for, and it is the request that lets a push skip an upload. That
// distinction is the whole reason the summary line splits blob checks into hits
// and misses instead of counting them.
func (t *tap) record(kind class, status int, transportErr bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.counts.total++
	t.counts.byClass[kind]++

	switch {
	case transportErr:
		t.counts.failed++
	case kind == classBlobCheck && status == http.StatusOK:
		t.counts.hits++
	case kind == classBlobCheck && status == http.StatusNotFound:
		t.counts.misses++
	case status >= http.StatusBadRequest:
		t.counts.failed++
	}
}

// writeLine writes one whole line with one Write under the lock, so lines from
// concurrent workers never interleave.
func (t *tap) writeLine(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	_, _ = io.WriteString(t.out, line)
}

// elapsed renders how long the tap has been watching, the clock every line
// carries. A backoff gap or a stalled request reads straight off the page.
func (t *tap) elapsed() string {
	return fmt.Sprintf("+%.3fs", time.Since(t.start).Seconds())
}

// classOrder is the order the summary line lists the classes in: the push path
// first, then the pull path, then the manifest and everything else.
func classOrder() []class {
	return []class{
		classBlobCheck, classBlobWrite, classUploadOpen, classBlobRead,
		classManifestRead, classManifestWrite, classManifestCheck, classOther,
	}
}

// classify names the kind of request a method and a URL path describe. The first
// shape that matches wins.
func classify(method, path string) class {
	switch {
	case strings.Contains(path, "/blobs/uploads/"):
		return uploadClass(method)
	case strings.Contains(path, "/blobs/"):
		return blobClass(method)
	case strings.Contains(path, "/manifests/"):
		return manifestClass(method)
	default:
		return classOther
	}
}

// uploadClass names a request against a blob upload session.
func uploadClass(method string) class {
	switch method {
	case http.MethodPost:
		return classUploadOpen
	case http.MethodPut, http.MethodPatch:
		return classBlobWrite
	default:
		return classOther
	}
}

// blobClass names a request against a blob that is already named by digest.
func blobClass(method string) class {
	switch method {
	case http.MethodHead:
		return classBlobCheck
	case http.MethodGet:
		return classBlobRead
	default:
		return classOther
	}
}

// manifestClass names a request against a manifest.
func manifestClass(method string) class {
	switch method {
	case http.MethodGet:
		return classManifestRead
	case http.MethodPut:
		return classManifestWrite
	case http.MethodHead:
		return classManifestCheck
	default:
		return classOther
	}
}

// provenance renders which build of the CLI wrote the log, read from the build
// information the toolchain embedded.
//
// Fields that are not there are left out rather than guessed at, which is what a
// binary built outside a checkout looks like. See [debug.ReadBuildInfo].
func provenance() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "bigoci reference CLI"
	}

	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}

	line := "bigoci reference CLI"
	if revision != "" {
		line += ", vcs=" + shortRevision(revision)
		if modified == "true" {
			line += " (modified)"
		}
	}
	if info.GoVersion != "" {
		line += ", " + info.GoVersion
	}

	return line
}

// shortRevision trims a commit hash to the length a human reads out loud.
func shortRevision(revision string) string {
	if len(revision) <= revisionWidth {
		return revision
	}

	return revision[:revisionWidth]
}
