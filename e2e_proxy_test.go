package bigoci_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci"
)

// The bounds the counting proxy puts on a kill it is delivering. Neither sits
// on a success path: each fires only when something that should have happened
// did not, and each fails the row loudly rather than letting it hang.
const (
	// killBackstop is the longest an armed proxy waits for the traffic that
	// proves the causal moment came. Reaching it means the damage never bit,
	// which [countProxy.assertTriggered] reports as a row that proved nothing.
	killBackstop = 45 * time.Second
	// killHoldBackstop is the longest the proxy holds its traffic waiting for a
	// signalled child to be gone. Reaching it means the signal did not end the
	// child, so the requests being held prove nothing about what landed.
	killHoldBackstop = 30 * time.Second
)

// manifestsSegment is the path segment every manifest endpoint carries, which
// is how the counting proxy tells a manifest read from a blob read.
const manifestsSegment = "/manifests/"

// The classes the counting proxy sorts traffic into. They are the vocabulary
// the rows assert in: "which parts did this rerun read", "which parts did it
// upload", "how many manifests did it fetch".
const (
	// classBlobGet is a read of one blob's content.
	classBlobGet = "blob-get"
	// classBlobHead is an existence check on one blob.
	classBlobHead = "blob-head"
	// classManifestGet is a manifest fetch.
	classManifestGet = "manifest-get"
	// classUploadOpen is the POST that opens a blob upload session.
	classUploadOpen = "upload-open"
	// classUploadComplete is the PUT that streams a blob into a session and
	// names the digest it completes into.
	classUploadComplete = "upload-complete"
	// classOther is everything else, which the rows never assert on.
	classOther = "other"
)

// errCutShort ends one blob read part way through its body. It is returned by
// the writer the proxy hands the reverse proxy, which turns it into an
// aborted response and a connection the client sees die mid-part.
var errCutShort = errors.New("the proxy cut this response short")

// proxyRecord is one request the counting proxy carried, recorded only once
// the whole response has been written. Recording on the way out rather than on
// the way in is what makes the byte count and the status true of a request that
// finished, so a row can say "this part was read once, whole" and mean it.
type proxyRecord struct {
	// method is the request method.
	method string
	// class is what the request was for, one of the class constants.
	class string
	// dgst is the blob the request names, empty for a request that names none.
	dgst digest.Digest
	// status is the status the answer carried.
	status int
	// ranged reports whether the client asked for a byte range. It is read
	// before any damage is applied, so it says what the client asked for and
	// not what the registry was shown.
	ranged bool
	// bytes is how many body bytes the proxy moved for this request.
	bytes int64
}

// proxyTraffic is what the counting proxy has seen so far, read as one
// snapshot so a trigger and the evidence for it agree.
type proxyTraffic struct {
	// blobGets is how many distinct blob digests have been asked for. Distinct
	// rather than total, because a part that was retried must not be able to
	// stand in for a part that was never reached.
	blobGets int
	// uploadOpens is how many blob upload sessions have been opened.
	uploadOpens int
	// downstream is how many blob body bytes have gone to the client.
	downstream int64
	// upstream is how many upload body bytes have come from it.
	upstream int64
}

// killTrigger reports whether the traffic so far is the causal event a row
// kills on. Every one of them is a statement about what the transfer has
// provably done, never about how long it has been running: a fault scheduled
// off a clock either lands after the transfer finished or never gets a clean
// window, and neither outcome is rare.
type killTrigger func(proxyTraffic) bool

// killPlan is what a row kills, and the traffic that proves the moment has
// come.
type killPlan struct {
	// target is the child that gets the signal.
	target *helper
	// sig is the signal it gets.
	sig syscall.Signal
	// when reports whether the traffic so far is the causal event.
	when killTrigger
}

// proxyDamage is what a counting proxy does to the traffic on its way past,
// beyond counting it.
type proxyDamage struct {
	// cutAfter closes the connection under one blob read once that read has
	// carried this many bytes, once for the whole proxy. Zero cuts nothing.
	cutAfter int64
	// stripRange deletes the Range header off every request, which makes the
	// registry answer a continued read with the whole blob and 200 — the
	// fallback the spec leaves every registry free to take.
	stripRange bool
}

// countProxy is a registry address that serves everything zot does, records
// every request that goes past, and — when a row arms it — kills the transfer
// making them at a moment the traffic itself decides.
//
// It is the shape [newCorruptingProxy] introduced: to the client it is simply
// a different registry host, and the registry behind it keeps answering
// correctly, so anything the row proves is a fact about bigoci rather than
// about a fixture pretending to be a registry. Bodies stream through counting
// readers and are never read into memory: the kill rows move eight megabytes a
// part at a time.
type countProxy struct {
	// at is the address rows aim their transfers at.
	at zot
	// proxy is the reverse proxy that does the forwarding.
	proxy *httputil.ReverseProxy
	// server is the listener rows aim at. Closing it is how a row waits out
	// the traffic a killed transfer left in flight.
	server *httptest.Server
	// damage is what this proxy does to the traffic beyond counting it. It is
	// fixed when the proxy is built and never changes.
	damage proxyDamage

	// mu guards everything below. The traffic is a few dozen requests, so one
	// lock beats reasoning about atomics racing the maps.
	mu sync.Mutex
	// records is every request that has completed.
	records []proxyRecord
	// seen is the arrival-time bookkeeping the triggers read.
	seen proxyTraffic
	// gets remembers which blob digests have been asked for, which is what
	// makes [proxyTraffic.blobGets] a count of distinct parts.
	gets map[digest.Digest]struct{}
	// cutTaken records that the one mid-part cut has been handed out.
	cutTaken bool
	// cutDone records that the handed-out cut actually bit: a response really
	// was ended part way through its body. Handing a cut to a body shorter
	// than the cut point would leave it unused, and a row that continued
	// nothing proves nothing, so the evidence is the bite and not the claim.
	cutDone bool
	// fired records that the kill has been triggered, after which every
	// request waits on killed before it is forwarded.
	fired bool
	// backstopped records that the backstop timer got there first, which means
	// the causal event never happened and the row proved nothing.
	backstopped bool
	// heldOut records that the signalled child never went away, which means
	// the requests being held prove nothing about what landed.
	heldOut bool
	// killErr is what signalling the child returned.
	killErr error
	// plan is what the proxy kills and when, nil when the row arms nothing.
	plan *killPlan

	// killed closes once the signal has provably been sent and the child
	// reaped. Every request that arrives after the trigger waits on it, so
	// nothing else can complete while the signal is landing.
	killed chan struct{}
	// once closes killed exactly once, whichever path gets there first.
	once sync.Once
}

// newCountProxy returns a registry address in front of upstream that records
// what goes past it and applies damage on the way.
func newCountProxy(t *testing.T, upstream string, damage proxyDamage) *countProxy {
	t.Helper()

	origin, err := url.Parse("http://" + upstream)
	require.NoError(t, err)

	p := &countProxy{
		proxy:  httputil.NewSingleHostReverseProxy(origin),
		damage: damage,
		gets:   make(map[digest.Digest]struct{}),
		killed: make(chan struct{}),
	}

	p.proxy.Transport = newTransport(t)
	// A client the row killed is gone by the time some held request is
	// forwarded, so its round trip fails; answering it quietly keeps the
	// deliberate wreckage out of the test output.
	p.proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		w.WriteHeader(http.StatusBadGateway)
	}

	p.server = httptest.NewServer(p)
	t.Cleanup(p.server.Close)

	p.at = zot{host: strings.TrimPrefix(p.server.URL, "http://")}
	t.Logf("a counting proxy on %s is serving %s in front of %s", p.at.host, apiPath, upstream)

	return p
}

// ServeHTTP forwards one request to the registry, counts what goes past, and
// applies whatever this row configured.
//
// The order is the contract. A request is classified and counted before it is
// forwarded, so the trigger fires on the request that proves the causal event
// rather than on the answer to it; it then waits out any kill in progress, so
// the registry never even sees a request the row means to have prevented; and
// only when the answer has been written whole is it recorded, so what a row
// asserts on is traffic that finished.
func (p *countProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	class, dgst := classifyRequest(req)
	rec := proxyRecord{method: req.Method, class: class, dgst: dgst, ranged: req.Header.Get("Range") != ""}

	if p.damage.stripRange {
		req.Header.Del("Range")
	}

	p.arrive(rec)
	p.hold()

	if rec.class == classUploadComplete && req.Body != nil {
		req.Body = &countedBody{rc: req.Body, count: p.addUpstream}
	}

	writer := &countedWriter{ResponseWriter: w}
	if rec.class == classBlobGet {
		writer.count = p.addDownstream
		writer.cutAt = p.claimCut()
		writer.onCut = p.markCut
	}

	p.proxy.ServeHTTP(writer, req)

	rec.status = writer.status
	rec.bytes = writer.written
	p.record(rec)
}

// taggedRef returns the reference of the artifact repo holds at the fixtures'
// tag, as reached through this proxy.
func (p *countProxy) taggedRef(repo string) bigoci.Reference {
	return p.at.taggedRef(repo, tag)
}

// arm points the proxy at the child it kills and the traffic that proves the
// moment has come.
//
// The backstop timer schedules nothing: it exists to turn a hang into a
// failure. A row whose causal event never arrives releases its held traffic and
// fails on [countProxy.assertTriggered] rather than waiting out the package
// timeout with no explanation.
func (p *countProxy) arm(t *testing.T, plan killPlan) {
	t.Helper()

	p.mu.Lock()
	p.plan = &plan
	p.mu.Unlock()

	timer := time.AfterFunc(killBackstop, func() {
		p.mu.Lock()
		already := p.fired
		if !already {
			p.fired = true
			p.backstopped = true
		}
		p.mu.Unlock()

		// A kill that is already under way owns the release: it holds the
		// traffic until the child is gone, which is the whole point of it.
		if !already {
			p.release()
		}
	})

	// Registered after the server's own cleanup, so it runs before it: a
	// server that is closing waits for its outstanding handlers, and a handler
	// still holding for a kill would never return.
	t.Cleanup(func() {
		timer.Stop()
		p.release()
	})
}

// assertTriggered checks that the row really did stop the transfer at the
// moment it meant to, and that everything the harness promised about that
// moment held.
//
// A row that fails here proved nothing, however green its outcome assertions
// are: the transfer either was never interrupted, or was interrupted while the
// registry was still answering it.
func (p *countProxy) assertTriggered(t *testing.T) {
	t.Helper()

	p.mu.Lock()
	defer p.mu.Unlock()

	require.True(t, p.fired, "the proxy never saw the traffic this row kills on")
	require.False(
		t, p.backstopped,
		"the backstop timer fired instead of the trigger: the transfer was never interrupted, "+
			"so this row proved nothing",
	)
	require.False(
		t, p.heldOut,
		"the signalled child was still running when the proxy gave up holding its traffic, "+
			"so what landed on disk proves nothing",
	)
	require.NoError(t, p.killErr, "signalling the helper child failed")
}

// settle shuts the proxy down and waits for every request it is still carrying
// to finish, which is what makes the state a killed transfer left behind a
// settled answer rather than a moving one.
//
// A killed transfer does not take its traffic with it. An upload whose body the
// proxy had already forwarded whole is still being committed by the registry
// when the process that asked for it is gone, and a request the child had
// already written into a socket is served after the child is a zombie. Closing
// the server is the barrier for both: it stops the listener and blocks until
// the last handler has returned, so what the registry says afterwards is what
// it will keep saying.
func (p *countProxy) settle(t *testing.T) {
	t.Helper()

	// A kill in progress is waited out exactly as a request would wait it out;
	// deliver bounds its own wait, so this cannot hang past the backstop.
	p.hold()
	p.release()
	p.server.Close()
}

// assertCut checks that the row's one mid-part cut really happened. A proxy
// that never cut anything answered every read whole, and a row that continues
// nothing proves nothing.
func (p *countProxy) assertCut(t *testing.T) {
	t.Helper()

	p.mu.Lock()
	defer p.mu.Unlock()

	require.NotZero(t, p.damage.cutAfter, "this proxy was never configured to cut a read short")
	require.True(t, p.cutDone, "no blob read was ever cut short, so nothing had to be continued")
}

// digestsOf returns how many requests of class the proxy carried for each
// blob digest.
func (p *countProxy) digestsOf(class string) map[digest.Digest]int {
	p.mu.Lock()
	defer p.mu.Unlock()

	counts := make(map[digest.Digest]int)

	for _, rec := range p.records {
		if rec.class == class {
			counts[rec.dgst]++
		}
	}

	return counts
}

// countOf returns how many requests of class the proxy carried.
func (p *countProxy) countOf(class string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	seen := 0

	for _, rec := range p.records {
		if rec.class == class {
			seen++
		}
	}

	return seen
}

// rangedRecords returns every completed request whose client asked for a byte
// range, which is what a continuation looks like from outside.
func (p *countProxy) rangedRecords() []proxyRecord {
	p.mu.Lock()
	defer p.mu.Unlock()

	var ranged []proxyRecord

	for _, rec := range p.records {
		if rec.ranged {
			ranged = append(ranged, rec)
		}
	}

	return ranged
}

// arrive records a request as it arrives and checks whether it is the one the
// row was waiting for.
func (p *countProxy) arrive(rec proxyRecord) {
	p.mu.Lock()

	switch rec.class {
	case classBlobGet:
		if _, seen := p.gets[rec.dgst]; !seen {
			p.gets[rec.dgst] = struct{}{}
			p.seen.blobGets++
		}
	case classUploadOpen:
		p.seen.uploadOpens++
	}

	p.mu.Unlock()
	p.check()
}

// addDownstream counts blob body bytes on their way to the client.
func (p *countProxy) addDownstream(n int64) {
	p.mu.Lock()
	p.seen.downstream += n
	p.mu.Unlock()
	p.check()
}

// addUpstream counts upload body bytes on their way to the registry.
func (p *countProxy) addUpstream(n int64) {
	p.mu.Lock()
	p.seen.upstream += n
	p.mu.Unlock()
	p.check()
}

// record files a request that has completed.
func (p *countProxy) record(rec proxyRecord) {

	p.mu.Lock()
	defer p.mu.Unlock()

	p.records = append(p.records, rec)
}

// claimCut hands the one mid-part cut to the first blob read that asks for it
// and gives every later one zero, so a row cuts exactly one part exactly once.
func (p *countProxy) claimCut() int64 {
	if p.damage.cutAfter == 0 {
		return 0
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cutTaken {
		return 0
	}
	p.cutTaken = true

	return p.damage.cutAfter
}

// markCut records that the handed-out cut really ended a body part way
// through, which is the fact [countProxy.assertCut] asserts on.
func (p *countProxy) markCut() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cutDone = true
}

// check fires the kill the moment the traffic proves the causal event
// happened, and never more than once.
func (p *countProxy) check() {
	p.mu.Lock()
	if p.plan == nil || p.fired || !p.plan.when(p.seen) {
		p.mu.Unlock()

		return
	}
	p.fired = true
	plan := p.plan
	p.mu.Unlock()

	go p.deliver(plan)
}

// deliver signals the child, waits for it to be gone, and only then lets the
// held traffic through.
//
// Waiting for the child to be reaped is what makes the moment exact. Between
// the signal being sent and the process being gone the registry could have
// answered another part, and a row that asserted "these parts and no others"
// against traffic from that window would be asserting a race.
func (p *countProxy) deliver(plan *killPlan) {
	err := plan.target.signal(plan.sig)

	p.mu.Lock()
	p.killErr = err
	p.mu.Unlock()

	timer := time.NewTimer(killHoldBackstop)
	defer timer.Stop()

	stuck := false

	select {
	case <-plan.target.exited:
	case <-timer.C:
		stuck = true
	}

	p.mu.Lock()
	p.heldOut = stuck
	p.mu.Unlock()

	p.release()
}

// hold blocks until a kill in progress has landed, so nothing the transfer
// asked for can complete while the signal is on its way.
func (p *countProxy) hold() {
	p.mu.Lock()
	fired := p.fired
	p.mu.Unlock()

	if fired {
		<-p.killed
	}
}

// release lets the held traffic through, whichever path got here first.
func (p *countProxy) release() {
	p.once.Do(func() { close(p.killed) })
}

// countedWriter is the [net/http.ResponseWriter] the counting proxy hands the
// reverse proxy: it records the status, counts the body bytes on their way to
// the client, and is where a row's one mid-part cut happens.
//
// Cutting from the writing side rather than the reading one is deliberate. A
// write that fails makes the reverse proxy abort the response, which closes
// the connection with the declared content length unsatisfied — exactly what a
// registry whose link dies mid-part looks like, and nothing a fixture had to
// pretend.
type countedWriter struct {
	// ResponseWriter is the writer the server handed the handler. Everything
	// this type does not define is answered by it.
	http.ResponseWriter

	// status is the status the answer carried.
	status int
	// written is how many body bytes have gone to the client.
	written int64
	// count reports each write onward, nil when this request's bytes are not
	// counted.
	count func(int64)
	// cutAt is the byte this response is cut short at, zero when this is not
	// the response that gets cut.
	cutAt int64
	// onCut reports the moment the cut actually bites, nil once it has. The
	// proxy's evidence that a body was really ended part way through is this
	// call, not the handing-out of cutAt.
	onCut func()
}

// WriteHeader records the status and passes it on.
func (c *countedWriter) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

// Write sends p to the client and counts it, unless this response has already
// carried everything the row means it to.
func (c *countedWriter) Write(p []byte) (int, error) {
	if c.cutAt > 0 && c.written >= c.cutAt {
		if c.onCut != nil {
			c.onCut()
			c.onCut = nil
		}

		return 0, errCutShort
	}

	n, err := c.ResponseWriter.Write(p)
	c.written += int64(n)

	if c.count != nil {
		c.count(int64(n))
	}

	return n, err
}

// Unwrap exposes the real writer, which is how
// [net/http.ResponseController] reaches the flushing the reverse proxy asks
// for.
func (c *countedWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// countedBody counts an upload's body bytes on their way to the registry. It
// wraps rather than buffers: a part of the kill fixture is a megabyte.
type countedBody struct {
	// rc is the body the client is sending.
	rc io.ReadCloser
	// count reports each read onward.
	count func(int64)
}

// Read reads from the client's body and counts what it got.
func (c *countedBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.count(int64(n))
	}

	return n, err
}

// Close closes the client's body.
func (c *countedBody) Close() error {
	return c.rc.Close()
}

// killOnDistinctGets returns the trigger an exact pull row rides on: the nth
// distinct part has just been asked for.
//
// With one worker this is proof rather than a guess. A pull worker takes its
// next job only after the previous part's fetch returned — after the last
// write and after the digest check — so the request for part n is made only
// once parts 0 to n-1 are whole on disk, and a completed positional write
// survives the death of the process that made it.
func killOnDistinctGets(n int) killTrigger {
	return func(seen proxyTraffic) bool { return seen.blobGets >= n }
}

// killOnUploadOpens returns the trigger an exact push row rides on: the nth
// upload session has just been opened. It is the push's mirror of
// [killOnDistinctGets] — an uploader takes its next job only after the
// previous part's upload returned.
func killOnUploadOpens(n int) killTrigger {
	return func(seen proxyTraffic) bool { return seen.uploadOpens >= n }
}

// killOnDownstreamBytes returns the trigger a messy pull row rides on: this
// many blob body bytes have reached the client. A messy row makes no claim
// about which parts that is — it reads the answer off the disk afterwards.
func killOnDownstreamBytes(n int64) killTrigger {
	return func(seen proxyTraffic) bool { return seen.downstream >= n }
}

// killOnUpstreamBytes returns the trigger a messy push row rides on: this many
// upload body bytes have left the client.
func killOnUpstreamBytes(n int64) killTrigger {
	return func(seen proxyTraffic) bool { return seen.upstream >= n }
}

// classifyRequest sorts one request into the class the rows count it under and
// names the blob it is about when it has one.
func classifyRequest(req *http.Request) (string, digest.Digest) {
	route := req.URL.Path

	switch {
	case strings.Contains(route, uploadsSegment):
		return uploadClass(req)
	case strings.Contains(route, blobsSegment):
		return blobClass(req.Method), digest.Digest(path.Base(route))
	case strings.Contains(route, manifestsSegment) && req.Method == http.MethodGet:
		return classManifestGet, ""
	default:
		return classOther, ""
	}
}

// uploadClass sorts one request against the upload endpoints. The digest an
// upload completes into rides in the query rather than the path, which is what
// lets a row say which parts a rerun really sent.
func uploadClass(req *http.Request) (string, digest.Digest) {
	switch req.Method {
	case http.MethodPost:
		return classUploadOpen, ""
	case http.MethodPut:
		return classUploadComplete, digest.Digest(req.URL.Query().Get(digestParam))
	default:
		return classOther, ""
	}
}

// blobClass sorts one request against the blob endpoints by its method.
func blobClass(method string) string {
	switch method {
	case http.MethodGet:
		return classBlobGet
	case http.MethodHead:
		return classBlobHead
	default:
		return classOther
	}
}

// newTransport returns a transport of this test's own: a clone of the default
// one, so a row never shares a connection pool with the rest of the suite and
// never mutates the process-wide transport to get what it needs.
func newTransport(t *testing.T) *http.Transport {
	t.Helper()

	shared, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok, "the default transport must be an *http.Transport to clone")

	own := shared.Clone()
	t.Cleanup(own.CloseIdleConnections)

	return own
}

// newHTTPClient returns an HTTP client on a transport of this test's own, for
// the checks and the reruns the parent makes itself.
func newHTTPClient(t *testing.T) *http.Client {
	t.Helper()

	return &http.Client{Transport: newTransport(t)}
}
