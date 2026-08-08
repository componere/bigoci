package bigoci_test

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	toxiproxy "github.com/Shopify/toxiproxy/v2/client"
	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/componere/bigoci"
	"github.com/componere/bigoci/internal/file"
	"github.com/componere/bigoci/internal/retry"
)

// The proxy this suite puts between the client and zot, so a row can break
// the link the way a real network breaks it.
const (
	// toxiproxyImage is the proxy image, pinned both for reproducibility and
	// to the version of the client package this file drives it with.
	toxiproxyImage = "ghcr.io/shopify/toxiproxy:2.12.0"
	// toxiproxyControlPort is the port the control API answers on inside the
	// container. Adding and removing damage goes through it.
	toxiproxyControlPort = "8474/tcp"
	// toxiproxyDataPort is the port the proxy in front of zot listens on
	// inside the container. A transfer that is meant to be damaged is aimed
	// at the address this port is published on.
	toxiproxyDataPort = "8666/tcp"
	// toxiproxyVersionPath is the control-API endpoint whose answer means the
	// proxy is up and ready to be configured.
	toxiproxyVersionPath = "/version"
	// toxiproxyAlias is the name the proxy answers to on the private network.
	toxiproxyAlias = "toxiproxy"
	// zotAlias is the name zot answers to on the private network, which is
	// what lets the proxy name its upstream without knowing an address.
	zotAlias = "zot"
	// proxyName is the name the one proxy is registered under.
	proxyName = "zot"
	// proxyListen is the address inside the toxiproxy container that proxy
	// listens on. It binds every interface, because the published port
	// arrives from outside the container.
	proxyListen = "0.0.0.0:8666"
	// proxyUpstream is where the proxy forwards to: zot's own port, reached
	// by name over the private network.
	proxyUpstream = "zot:5000"
)

// The damage the rows do, and the shape of each toxic that does it. Every one
// of these is deterministic per connection: nothing here depends on a clock,
// which is what keeps a fault-injection suite from flaking.
const (
	// toxicLimitData names the toxic that closes a connection once it has
	// carried a fixed number of bytes in one direction. It is the honest way
	// to kill a transfer mid-part: every connection dies at the same offset,
	// with no timing involved.
	toxicLimitData = "limit_data"
	// toxicResetPeer names the toxic that kills a connection with an RST
	// rather than a graceful close, which is a different path through Go's
	// transport and worth its own row.
	toxicResetPeer = "reset_peer"
	// streamUpstream names the client-to-registry direction, which is where
	// the bytes of a push are.
	streamUpstream = "upstream"
	// streamDownstream names the registry-to-client direction, which is where
	// the bytes of a pull are.
	streamDownstream = "downstream"
	// alwaysToxic is the toxicity every row uses: the damage applies to every
	// connection rather than to a random share of them. Probability belongs
	// in the manual gate, not in a suite that runs on every commit.
	alwaysToxic = 1.0
	// limitDataBytes is how many bytes a connection carries before limit_data
	// cuts it: about a third of a part, so the cut always lands inside a part
	// body and never at a boundary.
	limitDataBytes = int(multiPartSize) / 3
	// resetImmediately is the reset_peer delay in milliseconds. Zero kills
	// the connection as soon as data flows through it.
	resetImmediately = 0
)

// What the gate watches for, and what the retry budget allows. The budgets
// are written from [retry.DefaultAttempts] rather than from a number, so a
// change to the policy moves the bound with it.
const (
	// gateFailures is how many round-trip failures a push row lets happen
	// before the gate repairs the link. Two is a real retry with two attempts
	// still in hand, so no unit of work can exhaust its budget while the
	// damage is still on the wire.
	gateFailures = 2
	// gateBackstop is the longest a row leaves the damage on the wire no
	// matter what the traffic looks like. It exists so a row whose damage
	// never bites fails on its assertions instead of hanging.
	gateBackstop = 3 * time.Second
	// outageBackstop is the backstop of the row that takes the registry away
	// entirely. The design puts the registry back after about this long,
	// which is comfortably inside the one, two, and four second waits of a
	// default budget.
	outageBackstop = 1200 * time.Millisecond
	// pushUnits is how many blob uploads one push of the fixture makes: one
	// per part, plus the empty config blob. Each carries its own retry
	// budget.
	pushUnits = multiParts + 1
	// pushBudget is the most upload sessions a push of the fixture may open.
	// Holding a run to it is the automated half of "no infinite retry".
	pushBudget = pushUnits * retry.DefaultAttempts
	// pullBudget is the most blob reads a pull of the fixture may start, on
	// the same reasoning.
	pullBudget = multiParts * retry.DefaultAttempts
	// blobsSegment is the path segment every blob endpoint carries, which is
	// how the gate tells a blob read from a manifest read.
	blobsSegment = "/blobs/"
	// stampLength is how many characters of a row's own repository name are
	// written into each part of its fixture. Eight is far more than enough to
	// keep two rows' parts apart.
	stampLength = 8
)

// TestE2ETransfersRideThroughABrokenNetwork puts a real zot behind a
// toxiproxy and breaks the link under a transfer four different ways, to show
// that a push and a pull come out the other side with the file intact.
//
// Every row shares one pair of containers, because starting them is the
// expensive part, and every row drives its damage from a [gate] rather than
// from a clock, which is what makes a fault-injection suite safe to run on
// every commit.
func TestE2ETransfersRideThroughABrokenNetwork(t *testing.T) {
	reg := newFlakyRegistry(t)

	t.Run("a push rides through uploads cut short mid-part", func(t *testing.T) {
		t.Cleanup(func() { reg.reset(t) })

		repair := reg.breakWith(t, toxicLimitData, streamUpstream, toxiproxy.Attributes{"bytes": limitDataBytes})

		pushRidesThrough(t, reg, "flaky/cut-upload", gateBackstop, repair)
	})

	t.Run("a push rides through connections the peer resets", func(t *testing.T) {
		t.Cleanup(func() { reg.reset(t) })

		repair := reg.breakWith(t, toxicResetPeer, streamUpstream, toxiproxy.Attributes{"timeout": resetImmediately})

		pushRidesThrough(t, reg, "flaky/reset-upload", gateBackstop, repair)
	})

	t.Run("a push rides through the registry going away and coming back", func(t *testing.T) {
		t.Cleanup(func() { reg.reset(t) })

		require.NoError(t, reg.proxy.Disable(), "take the registry away")

		pushRidesThrough(t, reg, "flaky/outage", outageBackstop, reg.proxy.Enable)
	})

	t.Run("a pull rides through bodies that die mid-part", func(t *testing.T) {
		const repo = "flaky/dying-body"

		t.Cleanup(func() { reg.reset(t) })

		source := newRowFile(t, repo)
		want := fileDigest(t, source)
		dest := newPath(t, destName)

		_, err := newClient(t, bigoci.WithPlainHTTP()).Push(
			t.Context(), reg.direct.taggedRef(repo, tag), bigoci.FromFile(source),
			bigoci.WithPartSize(multiPartSize),
		)
		require.NoError(t, err, "the fixture is pushed over the address nothing is breaking")

		repair := reg.breakWith(t, toxicLimitData, streamDownstream, toxiproxy.Attributes{"bytes": limitDataBytes})
		watched := newGate(t, gateBackstop, hurtByRefetch(int64(multiParts)), repair)

		require.NoError(t, newClient(t, bigoci.WithPlainHTTP(), bigoci.WithHTTPClient(watched.client())).Pull(
			t.Context(), reg.through.taggedRef(repo, tag), bigoci.ToFile(dest),
		), "a pull must ride through bodies that die mid-part")

		watched.assertRepaired(t)

		seen := watched.counts()
		t.Logf("the pull started %d blob reads and saw %d round trips fail", seen.blobGets, seen.failures)
		assert.Greater(
			t, seen.blobGets, int64(multiParts),
			"a part must have been read twice, or the damage never cost the pull anything",
		)
		assert.LessOrEqual(t, seen.blobGets, int64(pullBudget), "a pull must not read a part past its retry budget")

		assert.Equal(t, want, fileDigest(t, dest), "the pulled file must be byte-identical to the pushed one")
		assert.NoFileExists(t, dest+file.PartialSuffix, "a pull that committed leaves no partial file")
	})
}

// pushRidesThrough pushes the fixture to repo through the proxy while the link
// is already broken, and checks that the artifact that landed is the file that
// was pushed.
//
// The caller breaks the link and hands over repair, which the gate calls as
// soon as the push has provably been hurt, or after backstop at the latest.
// Verification pulls back over the direct address: a row proves the bytes
// survived the damage, and a check that ran through the damage would be
// proving something else.
func pushRidesThrough(t *testing.T, reg flaky, repo string, backstop time.Duration, repair func() error) {
	t.Helper()

	source := newRowFile(t, repo)
	want := fileDigest(t, source)
	dest := newPath(t, destName)

	watched := newGate(t, backstop, hurtByFailures(gateFailures), repair)

	_, err := newClient(t, bigoci.WithPlainHTTP(), bigoci.WithHTTPClient(watched.client())).Push(
		t.Context(), reg.through.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
	)
	require.NoError(t, err, "a push must ride through a link this broken")

	watched.assertRepaired(t)

	seen := watched.counts()
	t.Logf("the push saw %d round trips fail and opened %d upload sessions", seen.failures, seen.uploads)
	assert.GreaterOrEqual(
		t, seen.failures, int64(gateFailures),
		"the push must have been hurt, or the damage never reached it",
	)
	assert.GreaterOrEqual(
		t, seen.uploads, int64(multiParts),
		"every part must have been uploaded through the broken link, not found already there",
	)
	assert.LessOrEqual(
		t, seen.uploads, int64(pushBudget),
		"a push must not open upload sessions past its retry budget",
	)

	assertRestored(t, reg.direct, repo, want, dest)
}

// newRowFile writes the fixture one row moves: the usual size and split, with
// parts no other row shares.
//
// The whole suite runs against one zot, and zot answers a blob HEAD from any
// repository once it holds the bytes anywhere. Two rows moving the same
// content would therefore find every part already uploaded and prove nothing,
// and [newRandomFile] seeds only from a size. Stamping the row's own
// repository name over the first bytes of every part changes every part
// digest while leaving the split, the part count, and the tail exactly as the
// shared fixture has them.
func newRowFile(t *testing.T, repo string) string {
	t.Helper()

	path := newRandomFile(t, multiSize)
	stamp := []byte(digest.FromString(repo).Encoded()[:stampLength])

	f, err := os.OpenFile(path, os.O_WRONLY, fixturePerm)
	require.NoError(t, err)

	defer func() { require.NoError(t, f.Close()) }()

	for part := range multiParts {
		_, err := f.WriteAt(stamp, int64(part)*int64(multiPartSize))
		require.NoError(t, err, "stamp %s into part %d", repo, part)
	}

	return path
}

// assertRestored pulls what repo holds back over the direct address and checks
// that it is the file the row pushed, with no partial left behind.
func assertRestored(t *testing.T, direct zot, repo string, want digest.Digest, dest string) {
	t.Helper()

	require.NoError(t, newClient(t, bigoci.WithPlainHTTP()).Pull(
		t.Context(), direct.taggedRef(repo, tag), bigoci.ToFile(dest),
	), "the artifact must be readable over the address nothing is breaking")

	assert.Equal(t, want, fileDigest(t, dest), "the pulled file must be byte-identical to the pushed one")
	assert.NoFileExists(t, dest+file.PartialSuffix, "a pull that committed leaves no partial file")
}

// flaky is one zot reachable twice over: through a link a row can break, and
// directly, so setup and verification never run through the damage under test.
type flaky struct {
	// through is zot as reached through the proxy. A transfer that is meant
	// to be damaged is aimed here.
	through zot
	// direct is zot's own published address, which nothing in this suite
	// breaks.
	direct zot
	// proxy is the control handle the damage is added to and taken off.
	proxy *toxiproxy.Proxy
}

// newFlakyRegistry starts a zot and a toxiproxy in front of it on one private
// network, and returns both addresses plus the proxy's control handle.
//
// One pair of containers serves the whole suite, because starting them is by
// far the most expensive thing it does; [flaky.reset] is what keeps one row
// from leaking damage into the next.
func newFlakyRegistry(t *testing.T) flaky {
	t.Helper()

	nw, err := network.New(t.Context())
	testcontainers.CleanupNetwork(t, nw)
	require.NoError(t, err, "create the private network the two containers share")

	direct := newZot(t, network.WithNetwork([]string{zotAlias}, nw))

	container, err := testcontainers.Run(t.Context(), toxiproxyImage,
		testcontainers.WithExposedPorts(toxiproxyControlPort, toxiproxyDataPort),
		network.WithNetwork([]string{toxiproxyAlias}, nw),
		testcontainers.WithWaitStrategy(wait.ForHTTP(toxiproxyVersionPath).WithPort(toxiproxyControlPort)),
	)
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err, "start %s", toxiproxyImage)

	control, err := container.PortEndpoint(t.Context(), toxiproxyControlPort, "http")
	require.NoError(t, err)

	through, err := container.PortEndpoint(t.Context(), toxiproxyDataPort, "")
	require.NoError(t, err)

	proxy, err := toxiproxy.NewClient(control).CreateProxy(proxyName, proxyListen, proxyUpstream)
	require.NoError(t, err, "create the proxy in front of %s", proxyUpstream)

	t.Logf("%s serves %s in front of %s, with its control API on %s", toxiproxyImage, through, proxyUpstream, control)

	return flaky{through: zot{host: through}, direct: direct, proxy: proxy}
}

// breakWith adds a toxic to the proxy and returns the function that takes it
// off again, which is what a [gate] repairs the link with.
//
// It checks that the proxy really is carrying the toxic before the row starts.
// A toxic the proxy rejected, or applied to nothing, would leave a row that
// passes every outcome assertion while proving nothing at all.
func (f flaky) breakWith(t *testing.T, kind, stream string, attrs toxiproxy.Attributes) func() error {
	t.Helper()

	added, err := f.proxy.AddToxic("", kind, stream, alwaysToxic, attrs)
	require.NoError(t, err, "add the %s toxic on the %s stream", kind, stream)

	active, err := f.proxy.Toxics()
	require.NoError(t, err)
	require.Len(t, active, 1, "the proxy must be carrying exactly the toxic this row added")

	return func() error { return f.proxy.RemoveToxic(added.Name) }
}

// reset takes every toxic off the proxy and switches it back on, so the row
// that runs next starts against an unbroken link.
func (f flaky) reset(t *testing.T) {
	t.Helper()

	require.NoError(t, f.proxy.Enable(), "put the registry back")

	active, err := f.proxy.Toxics()
	require.NoError(t, err)

	for _, toxic := range active {
		require.NoError(t, f.proxy.RemoveToxic(toxic.Name), "remove the %s toxic", toxic.Name)
	}
}

// counts is what a [gate] has seen so far, read as one snapshot so a decision
// and the evidence for it agree.
type counts struct {
	// failures is how many round trips came back with an error.
	failures int64
	// blobGets is how many blob reads were started.
	blobGets int64
	// uploads is how many blob upload sessions were opened.
	uploads int64
}

// gate is the [http.RoundTripper] every row hands its client, and the reason
// this suite is not flaky.
//
// Scheduling fault injection off a wall clock is what makes a toxiproxy test
// unreliable: the transfer either finishes before the damage window opens or
// never gets a clean window to finish in, and full jitter means neither
// outcome is rare. The gate makes the schedule causal instead. It watches the
// traffic and repairs the link the moment the counters prove a retry is under
// way — because the transfer was provably hurt, not because some number of
// milliseconds went by. The same counters are the evidence the row asserts on
// afterwards, which is a sharper claim than any proxy-side counter could make:
// it is the client's own view of its retries.
//
// A backstop timer repairs the link anyway if the counters never move, so a
// row whose damage does not bite fails on [gate.assertRepaired] rather than
// hanging.
type gate struct {
	// next is where the round trips actually go: a clone of
	// [http.DefaultTransport], so a row never mutates the process-wide one.
	next *http.Transport
	// hurt reports whether the counters so far prove the transfer needed a
	// retry. It is consulted every time a counter moves.
	hurt func(counts) bool
	// repair takes the damage off the link. It runs at most once.
	repair func() error
	// failures counts round trips that came back with an error. A push row
	// rides on this: a connection killed while a body is being written makes
	// the round trip itself fail, so the gate sees it directly.
	failures atomic.Int64
	// blobGets counts blob reads that were started. A pull row rides on this
	// instead: a body that dies mid-read has already returned a successful
	// round trip, so the failure is invisible here and the retry has to be
	// recognised as a second read of the same blob.
	blobGets atomic.Int64
	// uploads counts blob upload sessions that were opened, which is one per
	// upload attempt: the number a push row holds the retry budget to.
	uploads atomic.Int64
	// mu guards the repair bookkeeping below.
	mu sync.Mutex
	// repaired reports whether repair has run.
	repaired bool
	// backstopped reports whether it was the timer that ran it, which means
	// the damage never bit and the row proved nothing.
	backstopped bool
	// repairErr is what repair returned.
	repairErr error
}

// newGate returns a gate over a fresh clone of [http.DefaultTransport] that
// repairs the link as soon as hurt says the transfer needs a retry, and after
// backstop at the very latest.
func newGate(t *testing.T, backstop time.Duration, hurt func(counts) bool, repair func() error) *gate {
	t.Helper()

	shared, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok, "the default transport must be an *http.Transport to clone")

	g := &gate{next: shared.Clone(), hurt: hurt, repair: repair}

	timer := time.AfterFunc(backstop, func() { g.release(true) })
	t.Cleanup(func() { timer.Stop() })
	t.Cleanup(g.next.CloseIdleConnections)

	return g
}

// RoundTrip sends req through the cloned transport, counting what the row
// needs on the way in and on the way out.
//
// A blob read is counted before the request goes out, on purpose: a row that
// rides on read counts has to repair the link in time for the retry it just
// saw start to be the one that lands.
func (g *gate) RoundTrip(req *http.Request) (*http.Response, error) {
	if isBlobRead(req) {
		g.blobGets.Add(1)
		g.check()
	}

	if isUploadOpen(req) {
		g.uploads.Add(1)
	}

	resp, err := g.next.RoundTrip(req)
	if err != nil {
		g.failures.Add(1)
		g.check()
	}

	return resp, err
}

// client returns the HTTP client a row gives bigoci: this gate, and nothing
// else changed.
func (g *gate) client() *http.Client {
	return &http.Client{Transport: g}
}

// counts returns a snapshot of what the gate has seen.
func (g *gate) counts() counts {
	return counts{
		failures: g.failures.Load(),
		blobGets: g.blobGets.Load(),
		uploads:  g.uploads.Load(),
	}
}

// assertRepaired checks that the gate really did repair the link, and that it
// did so because the transfer was hurt rather than because the backstop timer
// went off.
//
// This is the assertion that makes a silent pass loud. A row whose damage
// never applied would push and pull perfectly well and prove nothing.
func (g *gate) assertRepaired(t *testing.T) {
	t.Helper()

	g.mu.Lock()
	defer g.mu.Unlock()

	require.True(t, g.repaired, "the gate never repaired the link")
	require.NoError(t, g.repairErr, "repairing the link failed")
	require.False(
		t, g.backstopped,
		"the backstop timer repaired the link: the damage never hurt the transfer, so this row proved nothing",
	)
}

// check repairs the link once the counters prove the transfer has been hurt.
func (g *gate) check() {
	if g.hurt(g.counts()) {
		g.release(false)
	}
}

// release runs repair once and records how it went. backstop says the timer
// got there first.
func (g *gate) release(backstop bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.repaired {
		return
	}

	g.repaired = true
	g.backstopped = backstop
	g.repairErr = g.repair()
}

// hurtByFailures returns the gate condition a push row rides on: the transfer
// is provably hurt once n round trips have come back with an error.
func hurtByFailures(n int64) func(counts) bool {
	return func(seen counts) bool { return seen.failures >= n }
}

// hurtByRefetch returns the gate condition a pull row rides on: with parts
// parts to read, a read past the parts-th one can only be a part being read
// again, which is a retry.
func hurtByRefetch(parts int64) func(counts) bool {
	return func(seen counts) bool { return seen.blobGets > parts }
}

// isBlobRead reports whether req reads a blob, which is the request a pull
// retries. Manifest reads share the method but not the path.
func isBlobRead(req *http.Request) bool {
	return req.Method == http.MethodGet && strings.Contains(req.URL.Path, blobsSegment)
}

// isUploadOpen reports whether req opens a blob upload session, which is the
// first request of every upload attempt that has something to upload.
func isUploadOpen(req *http.Request) bool {
	return req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, uploadsPath)
}
