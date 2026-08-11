package bigoci_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci"
	"github.com/imgoci/bigoci/internal/manifest"
)

// The repository the fake registries in these tests serve. The path has two
// components so a nested repository name is proven to survive the whole way
// from a [bigoci.Reference] to the endpoint URL.
const (
	// repoName is the repository path under /v2/.
	repoName = "team/artifact"
	// tag is the tag the fixtures bind their manifests to.
	tag = "v1"
)

// The fixture file the transfer tests move.
const (
	// payloadSize is the length of the fixture file in bytes. It is not a
	// whole multiple of testPartSize, so the split has a short final part.
	payloadSize = 5000
	// testPartSize splits payloadSize into eight parts. Everything the part
	// machinery does at gigabyte scale it also does here, which is the point
	// of part size being an option.
	testPartSize bigoci.PartSize = 700
	// payloadPeriod is the period of the fixture's byte pattern. It is prime
	// and shares no factor with testPartSize, so no two parts of the fixture
	// hold the same bytes and a part written at the wrong offset fails its
	// digest check.
	payloadPeriod = 251
	// payloadParts is how many parts payloadSize splits into at testPartSize:
	// seven full parts and a short tail.
	payloadParts = 8
	// shortFile is a fixture length under one part, which splits into a
	// single short part.
	shortFile = int(testPartSize) - 1
	// wholeParts is a fixture length that divides exactly into parts, so the
	// split has no short tail.
	wholeParts = 3 * int(testPartSize)
	// sourceName is the file name the fixtures are written under, which is
	// also the title a push records by default.
	sourceName = "source.bin"
	// destName is the file name the pull fixtures write their result to.
	destName = "pulled.bin"
)

// fixturePerm is the mode the tests write their own fixture files with. It
// only has to be readable by the test process.
const fixturePerm os.FileMode = 0o600

// schemaVersion is the schema version an OCI image manifest carries, fixed at
// 2 by the image spec. The fixtures write it by hand because the manifest
// package keeps its own copy unexported.
const schemaVersion = 2

// registry is a fake OCI registry: the six distribution-spec endpoints a
// bigoci transfer uses, backed by two maps.
//
// It is minimal but honest. It stores what it is given, it refuses an upload
// whose bytes do not hash to the digest the session names, and it serves a
// manifest back under both the tag it was written to and its own digest, the
// way a real registry resolves one. What it does not do is the rest of the
// spec: no catalog, no referrers, no ranges, no authentication.
type registry struct {
	// mu guards every field below. Handlers run on the server's goroutines
	// and a transfer moves several parts at once, so all of this is shared.
	mu sync.Mutex
	// blobs holds the uploaded blobs by digest.
	blobs map[digest.Digest][]byte
	// manifests holds the stored manifests, keyed by tag and by digest.
	manifests map[string][]byte
	// sessions numbers the upload sessions, so each PUT lands on its own URL.
	sessions int
	// uploads counts the completed blob uploads, which is how a test tells a
	// deduplicated push from a fresh one.
	uploads int
	// requests counts every request that arrived, which is how a test proves
	// a failure happened before the client touched the network.
	requests int
	// active is how many blob transfers are in flight right now.
	active int
	// peak is the most blob transfers that were ever in flight at once, which
	// is how a test proves the worker count is honored.
	peak int
	// server is the HTTP server the registry is served by.
	server *httptest.Server
	// host is the address the server listens on: the registry half of a
	// reference.
	host string
}

// newRegistry starts a fake registry that serves repoName over plain HTTP.
// The server shuts down with the test.
func newRegistry(t *testing.T) *registry {
	t.Helper()

	return startRegistry(t, httptest.NewServer)
}

// newTLSRegistry starts a fake registry that serves repoName over TLS, for
// the tests that exercise the https default. Its certificate is trusted only
// by the client [registry.client] returns.
func newTLSRegistry(t *testing.T) *registry {
	t.Helper()

	return startRegistry(t, httptest.NewTLSServer)
}

// startRegistry builds a fake registry, serves it with newServer, and binds it
// to the address the server listens on.
func startRegistry(t *testing.T, newServer func(http.Handler) *httptest.Server) *registry {
	t.Helper()

	reg := &registry{
		blobs:     make(map[digest.Digest][]byte),
		manifests: make(map[string][]byte),
	}

	reg.server = newServer(reg.counted(reg.routes()))
	t.Cleanup(reg.server.Close)

	parsed, err := url.Parse(reg.server.URL)
	require.NoError(t, err)
	reg.host = parsed.Host

	return reg
}

// taggedRef returns the reference of the artifact this registry holds at
// target, which is a tag.
func (r *registry) taggedRef(target string) bigoci.Reference {
	return bigoci.Reference(r.host + "/" + repoName + ":" + target)
}

// digestRef returns the reference of the manifest dgst names in this
// registry.
func (r *registry) digestRef(dgst digest.Digest) bigoci.Reference {
	return bigoci.Reference(r.host + "/" + repoName + "@" + dgst.String())
}

// client returns an HTTP client that trusts the registry's certificate. Only
// the TLS fixtures need one.
func (r *registry) client() *http.Client {
	return r.server.Client()
}

// manifestBody returns the raw manifest the registry holds at tag, which is
// where every fixture writes one.
func (r *registry) manifestBody(t *testing.T) []byte {
	t.Helper()

	r.mu.Lock()
	body, ok := r.manifests[tag]
	r.mu.Unlock()

	require.True(t, ok, "the registry holds no manifest at %s", tag)

	return body
}

// artifact decodes the manifest the registry holds at tag.
func (r *registry) artifact(t *testing.T) manifest.Artifact {
	t.Helper()

	artifact, err := manifest.Decode(r.manifestBody(t))
	require.NoError(t, err)

	return artifact
}

// storeManifest puts body at tag without a push, which is how a test serves a
// manifest bigoci would never write.
func (r *registry) storeManifest(body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.manifests[tag] = body
}

// corruptBlob changes one byte of a stored blob without changing its length,
// so the registry serves content that no longer hashes to the digest the
// manifest records for it.
func (r *registry) corruptBlob(t *testing.T, dgst digest.Digest) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	blob, ok := r.blobs[dgst]
	require.True(t, ok, "the registry holds no blob %s", dgst)
	require.NotEmpty(t, blob, "an empty blob cannot be corrupted without changing its length")

	blob[0]++
}

// dropBlob forgets a stored blob, which is how a test serves an artifact
// whose manifest still names a part the registry no longer holds.
func (r *registry) dropBlob(t *testing.T, dgst digest.Digest) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	_, ok := r.blobs[dgst]
	require.True(t, ok, "the registry holds no blob %s", dgst)

	delete(r.blobs, dgst)
}

// counts returns how many requests arrived and how many blob uploads
// completed.
func (r *registry) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.requests, r.uploads
}

// peakTransfers returns the most blob transfers that were ever in flight at
// once. A transfer never runs more parts than its worker count, so this is
// the ceiling a test holds the worker options to.
func (r *registry) peakTransfers() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.peak
}

// enterTransfer records that a blob transfer started and returns the function
// that records its end.
func (r *registry) enterTransfer() func() {
	r.mu.Lock()
	r.active++
	r.peak = max(r.peak, r.active)
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}
}

// routes maps the endpoints a transfer uses onto their handlers. Everything
// else the distribution spec defines is a 404 from the mux.
func (r *registry) routes() http.Handler {
	prefix := "/v2/" + repoName

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+prefix+"/blobs/uploads/", r.openUpload)
	mux.HandleFunc("PUT "+prefix+"/blobs/uploads/{session}", r.completeUpload)
	mux.HandleFunc("HEAD "+prefix+"/blobs/{digest}", r.headBlob)
	mux.HandleFunc("GET "+prefix+"/blobs/{digest}", r.getBlob)
	mux.HandleFunc("GET "+prefix+"/manifests/{reference}", r.getManifest)
	mux.HandleFunc("PUT "+prefix+"/manifests/{reference}", r.putManifest)

	return mux
}

// counted wraps next in the request counter.
func (r *registry) counted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests++
		r.mu.Unlock()

		next.ServeHTTP(w, req)
	})
}

// openUpload answers with the session URL the content belongs at.
func (r *registry) openUpload(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.sessions++
	session := r.sessions
	r.mu.Unlock()

	w.Header().Set("Location", req.URL.Path+strconv.Itoa(session))
	w.WriteHeader(http.StatusAccepted)
}

// completeUpload stores the uploaded blob under the digest the session names,
// after checking that the bytes that arrived hash to it.
func (r *registry) completeUpload(w http.ResponseWriter, req *http.Request) {
	done := r.enterTransfer()
	defer done()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	claimed := digest.Digest(req.URL.Query().Get("digest"))
	if got := digest.FromBytes(body); got != claimed {
		http.Error(w, "uploaded bytes are "+got.String()+", not "+claimed.String(), http.StatusBadRequest)

		return
	}

	r.mu.Lock()
	r.blobs[claimed] = body
	r.uploads++
	r.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", claimed.String())
	w.WriteHeader(http.StatusCreated)
}

// headBlob answers whether the registry holds a blob.
func (r *registry) headBlob(w http.ResponseWriter, req *http.Request) {
	blob, ok := r.blob(req.PathValue("digest"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	w.WriteHeader(http.StatusOK)
}

// getBlob serves a blob's bytes.
func (r *registry) getBlob(w http.ResponseWriter, req *http.Request) {
	done := r.enterTransfer()
	defer done()

	blob, ok := r.blob(req.PathValue("digest"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	_, _ = w.Write(blob)
}

// getManifest serves a stored manifest under the media type the format uses.
func (r *registry) getManifest(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	body, ok := r.manifests[req.PathValue("reference")]
	r.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
	w.Header().Set("Docker-Content-Digest", digest.FromBytes(body).String())
	_, _ = w.Write(body)
}

// putManifest stores a manifest under the reference it was written to and
// under its own digest, which is how a real registry resolves a manifest a
// later request asks for by digest.
func (r *registry) putManifest(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	dgst := digest.FromBytes(body)

	r.mu.Lock()
	r.manifests[req.PathValue("reference")] = body
	r.manifests[dgst.String()] = body
	r.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.WriteHeader(http.StatusCreated)
}

// blob returns the stored blob ref names, where ref is a digest string as it
// appeared in the request path.
func (r *registry) blob(ref string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	blob, ok := r.blobs[digest.Digest(ref)]

	return blob, ok
}

// newClient builds a client from opts and fails the test when it cannot.
func newClient(t *testing.T, opts ...bigoci.Option) *bigoci.Client {
	t.Helper()

	client, err := bigoci.New(opts...)
	require.NoError(t, err)
	require.NotNil(t, client)

	return client
}

// newFile writes content to a fresh temporary directory under sourceName and
// returns the path it wrote. The name is fixed because a push records it, and
// the title assertions read it back.
func newFile(t *testing.T, content []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), sourceName)
	require.NoError(t, os.WriteFile(path, content, fixturePerm))

	return path
}

// newPath returns a path in a fresh temporary directory that nothing has
// written yet: where a pull is told to put its result.
func newPath(t *testing.T, name string) string {
	t.Helper()

	return filepath.Join(t.TempDir(), name)
}

// payload returns the fixture file's first size bytes.
func payload(size int) []byte {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % payloadPeriod)
	}

	return content
}

// seedArtifact pushes the fixture file to reg at tag with the given options
// on top of testPartSize, and returns the descriptor of the manifest the push
// wrote. The pull tests start from it.
func seedArtifact(t *testing.T, reg *registry, opts ...bigoci.PushOption) ocispec.Descriptor {
	t.Helper()

	desc, err := newClient(t, bigoci.WithPlainHTTP()).Push(
		t.Context(),
		reg.taggedRef(tag),
		bigoci.FromFile(newFile(t, payload(payloadSize))),
		append([]bigoci.PushOption{bigoci.WithPartSize(testPartSize)}, opts...)...,
	)
	require.NoError(t, err)

	return desc
}

// otherArtifact returns a manifest for an artifact of some other kind: valid
// OCI, but not the artifactType bigoci defines.
func otherArtifact(t *testing.T) []byte {
	t.Helper()

	body, err := json.Marshal(ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: schemaVersion},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example.other.v1",
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       []ocispec.Descriptor{},
	})
	require.NoError(t, err)

	return body
}
