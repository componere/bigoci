package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// fakeRepo is the repository the fake registry serves. The path has two
	// components so a nested repository name is proven to survive the whole way
	// from a command line to the endpoint URL.
	fakeRepo = "team/model"
	// fakeTag is the tag the end-to-end pushes bind their manifests to.
	fakeTag = "v1"
	// fixtureName is the file name the fixture is written under, which is also
	// the title a push records when "-title" is left alone.
	fixtureName = "model.bin"
	// fixtureSize is the fixture's length in bytes: three whole parts of
	// fixturePartSize and a short tail, so the split has a remainder the way a
	// real file's does.
	fixtureSize = 200 * kib
	// fixturePartSize is the "-part-size" the end-to-end runs ask for, spelled
	// the way a caller types it.
	fixturePartSize = "64KiB"
	// wholePartSize is a part size above the fixture's own length, so a push
	// that asks for it moves the file as a single part. The retry test uses
	// it to keep the run to one backoff, which it really waits out.
	wholePartSize = "256KiB"
	// fixtureBlobs is how many blobs a cold push of the fixture writes: its four
	// parts, and the empty config blob every OCI manifest carries. A pull reads
	// the four parts and not the config, which is why its counts are one lower.
	fixtureBlobs = 5
	// fixturePeriod is the period of the fixture's byte pattern. It is prime and
	// shares no factor with the part size, so no two parts hold the same bytes
	// and a part written at the wrong offset fails its digest check.
	fixturePeriod = 251
	// titleAnnotation is the standard annotation key a push records the file name
	// under.
	titleAnnotation = "org.opencontainers.image.title"
)

// storedManifest is one manifest the fake registry holds: the bytes a push wrote
// and the media type it declared them under.
type storedManifest struct {
	// body is the manifest exactly as it arrived.
	body []byte
	// ctype is the Content-Type the push sent, served back unchanged. A pull that
	// works is the proof it round-tripped.
	ctype string
}

// fakeRegistry is an in-process OCI registry: the six distribution API endpoints
// a bigoci transfer uses, backed by two maps.
//
// It is written by hand rather than generated because it is a fake HTTP peer, not
// an adapter behind a port — there is no interface here to generate against, and
// the library's own tests fake a registry the same way. It stores what it is
// given, refuses an upload whose bytes do not hash to the digest its session
// names, and serves a manifest back under both the tag it was written to and its
// own digest, the way a real registry resolves one. Everything else the
// distribution spec defines is a 404 from the mux.
type fakeRegistry struct {
	// mu guards every map and counter below. Handlers run on the server's
	// goroutines and a transfer moves several parts at once.
	mu sync.Mutex
	// blobs holds the uploaded blobs by digest.
	blobs map[string][]byte
	// manifests holds the stored manifests, keyed by tag and by digest.
	manifests map[string]storedManifest
	// sessions numbers the upload sessions, so each PUT lands on its own URL.
	sessions int
	// refusing answers every completing upload with 413, for a registry whose
	// layer cap the part is over.
	refusing bool
	// failing is how many completing uploads answer 500 before the registry
	// starts storing what it is sent, for a registry having a bad minute.
	failing int
	// server is the HTTP server the registry is served by.
	server *httptest.Server
	// host is the address the server listens on: the registry half of a
	// reference.
	host string
}

// newFakeRegistry starts a fake registry that serves fakeRepo over plain HTTP.
// The server shuts down with the test.
func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()

	reg := &fakeRegistry{
		blobs:     make(map[string][]byte),
		manifests: make(map[string]storedManifest),
	}

	prefix := "/v2/" + fakeRepo
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+prefix+"/blobs/uploads/", reg.openUpload)
	mux.HandleFunc("PUT "+prefix+"/blobs/uploads/{session}", reg.completeUpload)
	mux.HandleFunc("HEAD "+prefix+"/blobs/{digest}", reg.headBlob)
	mux.HandleFunc("GET "+prefix+"/blobs/{digest}", reg.getBlob)
	mux.HandleFunc("GET "+prefix+"/manifests/{reference}", reg.getManifest)
	mux.HandleFunc("PUT "+prefix+"/manifests/{reference}", reg.putManifest)

	reg.server = httptest.NewServer(mux)
	t.Cleanup(reg.server.Close)

	parsed, err := url.Parse(reg.server.URL)
	require.NoError(t, err)
	reg.host = parsed.Host

	return reg
}

// digestOf renders the sha256 digest of body the way the distribution API writes
// one.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)

	return "sha256:" + hex.EncodeToString(sum[:])
}

// fixture writes the end-to-end fixture file into a fresh temporary directory and
// returns the path it wrote and the bytes it holds.
func fixture(t *testing.T) (string, []byte) {
	t.Helper()

	content := make([]byte, fixtureSize)
	for i := range content {
		content[i] = byte(i % fixturePeriod)
	}

	path := filepath.Join(t.TempDir(), fixtureName)
	require.NoError(t, os.WriteFile(path, content, 0o600))

	return path, content
}

// taggedRef returns the reference of the artifact this registry holds at tag.
func (r *fakeRegistry) taggedRef(tag string) string {
	return r.host + "/" + fakeRepo + ":" + tag
}

// digestRef returns the reference that names one manifest of this registry
// exactly, which is what the line a push writes to stdout is for.
func (r *fakeRegistry) digestRef(dgst string) string {
	return r.host + "/" + fakeRepo + "@" + dgst
}

// manifestAt returns the manifest the registry holds at reference, which is a tag
// or a digest.
func (r *fakeRegistry) manifestAt(t *testing.T, reference string) storedManifest {
	t.Helper()

	r.mu.Lock()
	stored, ok := r.manifests[reference]
	r.mu.Unlock()

	require.True(t, ok, "the registry holds no manifest at %s", reference)

	return stored
}

// blobCount is how many blobs the registry holds, which is what every blob check
// of a warm push has to hit.
func (r *fakeRegistry) blobCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.blobs)
}

// refuseUploads makes every completing upload answer 413, which is how a
// registry says a part is larger than it accepts.
func (r *fakeRegistry) refuseUploads() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.refusing = true
}

// failUploads makes the next n completing uploads answer 500 and the ones
// after them succeed, which is the shape of a failure worth another attempt.
func (r *fakeRegistry) failUploads(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.failing = n
}

// refusal reports the status a completing upload is answered with instead of
// storing the blob, and whether a hook named one at all. A scripted failure
// is spent as it is read, so the attempt after it is answered normally.
func (r *fakeRegistry) refusal() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case r.refusing:
		return http.StatusRequestEntityTooLarge, true
	case r.failing > 0:
		r.failing--

		return http.StatusInternalServerError, true
	default:
		return 0, false
	}
}

// openUpload answers with the session URL the blob's content belongs at.
func (r *fakeRegistry) openUpload(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.sessions++
	session := r.sessions
	r.mu.Unlock()

	w.Header().Set("Location", req.URL.Path+strconv.Itoa(session))
	w.WriteHeader(http.StatusAccepted)
}

// completeUpload stores the uploaded blob under the digest its session names,
// after checking that the bytes that arrived really hash to it.
//
// The body is read before any hook answers, the way a registry that has to
// weigh a part reads it: an answer sent over an unread request body is a
// broken pipe on the client's side rather than the status the hook meant.
func (r *fakeRegistry) completeUpload(w http.ResponseWriter, req *http.Request) {
	body, err := readWholeBody(w, req)
	if err != nil {
		return
	}

	if status, refused := r.refusal(); refused {
		http.Error(w, "the registry refused the upload", status)

		return
	}

	claimed := req.URL.Query().Get("digest")
	if got := digestOf(body); got != claimed {
		http.Error(w, "uploaded bytes are "+got+", not "+claimed, http.StatusBadRequest)

		return
	}

	r.mu.Lock()
	r.blobs[claimed] = body
	r.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", claimed)
	w.WriteHeader(http.StatusCreated)
}

// headBlob answers whether the registry already holds a blob, which is the
// question that lets a push skip an upload.
func (r *fakeRegistry) headBlob(w http.ResponseWriter, req *http.Request) {
	blob, ok := r.blob(req.PathValue("digest"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	w.WriteHeader(http.StatusOK)
}

// getBlob serves a blob's bytes.
func (r *fakeRegistry) getBlob(w http.ResponseWriter, req *http.Request) {
	blob, ok := r.blob(req.PathValue("digest"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	_, _ = w.Write(blob)
}

// getManifest serves a stored manifest back under the media type it was written
// with.
func (r *fakeRegistry) getManifest(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	stored, ok := r.manifests[req.PathValue("reference")]
	r.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Type", stored.ctype)
	w.Header().Set("Docker-Content-Digest", digestOf(stored.body))
	_, _ = w.Write(stored.body)
}

// putManifest stores a manifest under the reference it was written to and under
// its own digest, which is how a real registry resolves one a later request asks
// for by digest.
func (r *fakeRegistry) putManifest(w http.ResponseWriter, req *http.Request) {
	body, err := readWholeBody(w, req)
	if err != nil {
		return
	}

	stored := storedManifest{body: body, ctype: req.Header.Get("Content-Type")}
	dgst := digestOf(body)

	r.mu.Lock()
	r.manifests[req.PathValue("reference")] = stored
	r.manifests[dgst] = stored
	r.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst)
	w.WriteHeader(http.StatusCreated)
}

// blob returns the stored blob dgst names, where dgst is the digest as it
// appeared in the request path.
func (r *fakeRegistry) blob(dgst string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	blob, ok := r.blobs[dgst]

	return blob, ok
}

// readWholeBody reads a request body and answers the request itself when it
// cannot, so a handler that needs the bytes has one line to write.
func readWholeBody(w http.ResponseWriter, req *http.Request) ([]byte, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return nil, err
	}

	return body, nil
}

// pushFixture pushes src to reg's fakeTag with the request log on, plus whatever
// extra flags the case wants, and returns what the run produced.
func pushFixture(t *testing.T, reg *fakeRegistry, src string, extra ...string) result {
	t.Helper()

	args := []string{cmdPush, "-plain-http", "-part-size", fixturePartSize, "-debug"}
	args = append(args, extra...)
	args = append(args, src, reg.taggedRef(fakeTag))

	return runCLI(t, args...)
}

// TestEndToEndPushAndPull drives the whole program against a registry, in
// process, and is the one thing every other test in this package leaves out:
// argument parsing, the library call, the request log, both streams, and a
// transfer that really moves bytes, on one path from a command line to stored
// blobs and back.
//
// The three stages share one registry and run in order on purpose. A warm
// re-push means nothing without a cold one before it, and the pull is what
// proves the cold push stored the file rather than only reporting that it had.
func TestEndToEndPushAndPull(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	src, content := fixture(t)

	cold := pushFixture(t, reg, src)
	require.Equal(t, exitOK, cold.code, cold.stderr)

	dgst := strings.TrimSuffix(cold.stdout, "\n")
	assert.Equal(t, dgst+"\n", cold.stdout, "a push writes exactly one line to stdout")
	assert.True(t, isDigest(dgst), "the one line a push writes must be a manifest digest, got %q", dgst)

	assert.Contains(t, cold.stderr, fmt.Sprintf(
		"bigoci: push %s (%d bytes) -> %s (part-size=%s, workers=4, plain-http)\n",
		src, fixtureSize, reg.taggedRef(fakeTag), fixturePartSize,
	))
	assert.Contains(t, cold.stderr,
		"bigoci: http requests=16 failed=0 blob-check=5 (0 hit, 5 miss) blob-write=5 upload-open=5 "+
			"blob-read=0 manifest-read=0 manifest-write=1 manifest-check=0 other=0\n",
	)
	assert.Contains(t, cold.stderr, "bigoci: pushed "+dgst+" in ")
	assert.Equal(t, fixtureBlobs, reg.blobCount())

	warm := pushFixture(t, reg, src)
	require.Equal(t, exitOK, warm.code, warm.stderr)

	assert.Equal(t, cold.stdout, warm.stdout, "the digest is a pure function of the bytes, size, and title")
	assert.Contains(t, warm.stderr,
		"bigoci: http requests=6 failed=0 blob-check=5 (5 hit, 0 miss) blob-write=0 upload-open=0 "+
			"blob-read=0 manifest-read=0 manifest-write=1 manifest-check=0 other=0\n",
		"every blob check must hit and no blob bytes may be sent again",
	)
	assert.Equal(t, fixtureBlobs, reg.blobCount(), "a warm push writes no new blob")

	dest := filepath.Join(t.TempDir(), "out.bin")
	pull := runCLI(t, cmdPull, "-plain-http", "-debug", reg.digestRef(dgst), dest)
	require.Equal(t, exitOK, pull.code, pull.stderr)

	assert.Empty(t, pull.stdout, "a pull writes nothing to stdout either way")
	assert.Contains(t, pull.stderr, fmt.Sprintf(
		"bigoci: pull %s -> %s (workers=4, plain-http)\n", reg.digestRef(dgst), dest,
	))
	assert.Contains(t, pull.stderr,
		"bigoci: http requests=5 failed=0 blob-check=0 (0 hit, 0 miss) blob-write=0 upload-open=0 "+
			"blob-read=4 manifest-read=1 manifest-write=0 manifest-check=0 other=0\n",
	)
	assert.Contains(t, pull.stderr, fmt.Sprintf("bigoci: pulled %d bytes in ", fixtureSize))

	pulled, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, content, pulled, "a round trip must be byte-identical")
}

// TestEndToEndTitleReachesTheWire pins the unset-means-absent rule at the far end
// of the wire: a flag left alone and a flag set to its zero value produce two
// different artifacts, and the difference is visible in the stored manifest.
//
// It reads the annotation key and value out of the manifest bytes as substrings
// rather than decoding them, because the CLI knows nothing about the artifact
// format and this test is not the place for it to start.
func TestEndToEndTitleReachesTheWire(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		extra     []string
		wantTitle bool
	}{
		{name: "left alone, so the library names the artifact after the file", wantTitle: true},
		{name: "cleared on purpose", extra: []string{"-title", ""}, wantTitle: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := newFakeRegistry(t)
			src, _ := fixture(t)

			got := pushFixture(t, reg, src, tt.extra...)
			require.Equal(t, exitOK, got.code, got.stderr)

			manifest := string(reg.manifestAt(t, fakeTag).body)
			if tt.wantTitle {
				assert.Contains(t, manifest, titleAnnotation)
				assert.Contains(t, manifest, fixtureName)

				return
			}

			assert.NotContains(t, manifest, titleAnnotation, `-title "" must write no annotation at all`)
			assert.NotContains(t, manifest, fixtureName)
		})
	}
}

// TestEndToEndAPartTheRegistryRefusesIsExitSeven activates the code that has
// been reserved since the CLI was written. The 413 is terminal, so the run
// costs no backoff at all: the library asks once and reports what it was told.
func TestEndToEndAPartTheRegistryRefusesIsExitSeven(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	reg.refuseUploads()
	src, _ := fixture(t)

	got := pushFixture(t, reg, src)

	assert.Equal(t, exitPartTooLarge, got.code, got.stderr)
	assert.Empty(t, got.stdout, "a push that failed writes no digest")
	assert.Contains(t, got.stderr, "registry returned 413 Request Entity Too Large")
	assert.Contains(t, got.stderr, "bigoci: matched sentinel bigoci.ErrPartTooLarge (exit 7)\n")
	assert.Zero(t, reg.blobCount(), "a refused part is not stored")
}

// TestEndToEndPushRidesThroughAServerError is the only test in this tree that
// runs on the library's real clock, and it is deliberate: everything else
// injects a sleep, and something has to prove the shipped policy works end to
// end through the default HTTP client rather than only under a fixture.
//
// The fixture is a single part, so the whole cost is one draw from the first
// backoff window — half a second on average, under one at worst.
func TestEndToEndPushRidesThroughAServerError(t *testing.T) {
	t.Parallel()

	reg := newFakeRegistry(t)
	reg.failUploads(1)
	src, content := fixture(t)

	got := runCLI(t, cmdPush, "-plain-http", "-part-size", wholePartSize, src, reg.taggedRef(fakeTag))
	require.Equal(t, exitOK, got.code, got.stderr)

	assert.Equal(t, 2, reg.blobCount(), "the part that was refused once, and the empty config blob")

	stored, held := reg.blob(digestOf(content))
	require.True(t, held, "a single part is the whole file, stored under the file's own digest")
	assert.Equal(t, content, stored, "the second attempt must stream the part again in full")
}
