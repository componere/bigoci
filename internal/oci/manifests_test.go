package oci_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/oci"
	"github.com/imgoci/bigoci/internal/retry"
)

// manifestBody stands in for an encoded bigoci manifest. What it says does
// not matter: this adapter moves manifest bytes and never parses them.
const manifestBody = `{"schemaVersion":2,"artifactType":"application/vnd.bigoci.file.v1"}`

// otherBody is a manifest that is not manifestBody, for the fixtures where a
// registry answers with content the caller did not ask for.
const otherBody = `{"schemaVersion":2,"artifactType":"application/vnd.oci.image.index.v1+json"}`

// maxManifestSize mirrors the adapter's manifest size cap. Registries cap
// manifests around the same place, and the tests pin both sides of the limit.
const maxManifestSize = 4 << 20

// lyingDigest is what the fixtures put in the header registries report a
// manifest's digest in. It is deliberately wrong: the descriptor must
// describe the bytes that arrived, not the claim that came with them.
const lyingDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// reflectedDigestHeader is a reusable credential-shaped value a registry can
// place in Docker-Content-Digest. The adapter ignores that claim and never
// repeats it in a bound-manifest mismatch.
const reflectedDigestHeader = "reusable-digest-header-bearer-a8f4c2"

// headerAcceptEncoding is the identity-coding request header every manifest
// and blob GET must send.
const headerAcceptEncoding = "Accept-Encoding"

// codingIdentity is the only Accept-Encoding token those GETs may ask for.
const codingIdentity = "identity"

// reflectedCoding is a reusable credential-shaped Content-Encoding value. A
// peer controls this header, so a refusal must not repeat it.
const reflectedCoding = "reusable-encoding-bearer-a8f4c2"

// codedRefusal is the fixed diagnosis a coded response is refused with.
const codedRefusal = "the response is not identity coded"

func TestManifestsGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		body      string
		lie       string
		wantErrIs error
		wantErr   bool
	}{
		{
			name:   "returns the manifest with a descriptor computed from its bytes",
			status: http.StatusOK,
			body:   manifestBody,
		},
		{
			name:   "a content digest header the registry lies in is ignored",
			status: http.StatusOK,
			body:   manifestBody,
			lie:    lyingDigest,
		},
		{
			name:      "a manifest the registry does not have is not found",
			status:    http.StatusNotFound,
			wantErrIs: oci.ErrNotFound,
		},
		{
			name:    "a server failure is an error",
			status:  http.StatusInternalServerError,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(t, r)
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				if tt.lie != "" {
					w.Header().Set("Docker-Content-Digest", tt.lie)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}), repoName+":"+tag)

			body, desc, err := repo.Manifests().Get(t.Context())

			request := rec.only(t)
			assert.Equal(t, http.MethodGet, request.method)
			assert.Equal(t, "/v2/"+repoName+"/manifests/"+tag, request.path)
			assert.Equal(t, ocispec.MediaTypeImageManifest, request.header.Get("Accept"))
			assert.Equal(t, codingIdentity, request.header.Get(headerAcceptEncoding))

			if tt.wantErr || tt.wantErrIs != nil {
				require.Error(t, err)
				if tt.wantErrIs != nil {
					require.ErrorIs(t, err, tt.wantErrIs)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.body, string(body))
			assert.Equal(t, ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    digest.FromString(tt.body),
				Size:      int64(len(tt.body)),
			}, desc)
		})
	}
}

// TestManifestsGetRejectsACodedResponse proves a content coding on the
// manifest body is refused before the bytes are read, whether the status was
// a success or a failure, and that the body is closed either way.
func TestManifestsGetRejectsACodedResponse(t *testing.T) {
	t.Parallel()

	originPath := "/v2/" + repoName + "/manifests/" + tag
	tests := []struct {
		name     string
		status   int
		encoding string
		body     string
		wantErr  bool
	}{
		{name: "a gzipped success is refused", status: http.StatusOK, encoding: "gzip", wantErr: true},
		{name: "a gzipped not-found is refused", status: http.StatusNotFound, encoding: "gzip", wantErr: true},
		{
			name:     "a gzipped server failure is refused",
			status:   http.StatusInternalServerError,
			encoding: "gzip",
			wantErr:  true,
		},
		{
			name:     "a peer-selected coding is not reflected",
			status:   http.StatusOK,
			encoding: reflectedCoding,
			wantErr:  true,
		},
		{
			name:     "an identity-coded success is accepted",
			status:   http.StatusOK,
			encoding: codingIdentity,
			body:     manifestBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo, closed := newClosingRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(t, r)
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				w.Header().Set("Content-Encoding", tt.encoding)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, manifestBody)
			}), repoName+":"+tag)

			body, _, err := repo.Manifests().Get(t.Context())

			request := rec.only(t)
			assert.Equal(t, codingIdentity, request.header.Get(headerAcceptEncoding))

			if tt.wantErr {
				assertCodedRefusal(t, err, originPath, tt.encoding)
				assert.True(t, closed.Load(), "the coded response body must be closed")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.body, string(body))
			assert.True(t, closed.Load(), "a successful read still closes the response body")
		})
	}
}

func TestManifestsGetSizeLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "a manifest at the size limit is read", size: maxManifestSize},
		{name: "a manifest over the size limit is an error", size: maxManifestSize + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				_, _ = io.WriteString(w, strings.Repeat("a", tt.size))
			}), repoName+":"+tag)

			body, _, err := repo.Manifests().Get(t.Context())

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), strconv.Itoa(maxManifestSize))

				return
			}

			require.NoError(t, err)
			assert.Len(t, body, tt.size)
		})
	}
}

func TestManifestsGetTagsABodyThatBreaksMidRead(t *testing.T) {
	t.Parallel()

	// The registry promises a body far longer than it sends, so the read
	// fails part way through rather than ending early and cleanly.
	const declared = 1 << 16

	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		w.Header().Set("Content-Length", strconv.Itoa(declared))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("a", declared/2))
		resetConnection(t, w)
	}), repoName+":"+tag)

	_, _, err := repo.Manifests().Get(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read manifest response body")

	_, transient := retry.IsTransient(err)
	assert.True(t, transient, "a manifest whose connection broke mid-read is worth another attempt")
}

func TestManifestsGetVerifiesBoundDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "the manifest the digest names is returned", body: manifestBody},
		{name: "a manifest that is not the one the digest names is an error", body: otherBody, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wanted := digest.FromString(manifestBody)
			repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				w.Header().Set("Docker-Content-Digest", reflectedDigestHeader)
				_, _ = io.WriteString(w, tt.body)
			}), repoName+"@"+wanted.String())

			body, desc, err := repo.Manifests().Get(t.Context())

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "manifest content does not match the requested digest")
				assert.NotContains(t, err.Error(), wanted.String())
				assert.NotContains(t, err.Error(), reflectedDigestHeader)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.body, string(body))
			assert.Equal(t, wanted, desc.Digest)
		})
	}
}

func TestManifestsPut(t *testing.T) {
	t.Parallel()

	manifestDigest := digest.FromString(manifestBody).String()
	tests := []struct {
		name     string
		ref      string
		status   int
		wantPath string
		wantErr  bool
	}{
		{
			name:     "a tag reference writes to the tag",
			ref:      repoName + ":" + tag,
			status:   http.StatusCreated,
			wantPath: "/v2/" + repoName + "/manifests/" + tag,
		},
		{
			name:     "a digest reference writes to the digest",
			ref:      repoName + "@" + manifestDigest,
			status:   http.StatusCreated,
			wantPath: "/v2/" + repoName + "/manifests/" + manifestDigest,
		},
		{
			name:     "a reference with a tag and a digest writes to the digest",
			ref:      repoName + ":" + tag + "@" + manifestDigest,
			status:   http.StatusCreated,
			wantPath: "/v2/" + repoName + "/manifests/" + manifestDigest,
		},
		{
			name:     "a manifest the registry rejects is an error",
			ref:      repoName + ":" + tag,
			status:   http.StatusBadRequest,
			wantPath: "/v2/" + repoName + "/manifests/" + tag,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(t, r)
				w.WriteHeader(tt.status)
			}), tt.ref)

			got, err := repo.Manifests().Put(t.Context(), ocispec.MediaTypeImageManifest, []byte(manifestBody))

			request := rec.only(t)
			assert.Equal(t, http.MethodPut, request.method)
			assert.Equal(t, tt.wantPath, request.path)
			assert.Equal(t, ocispec.MediaTypeImageManifest, request.header.Get("Content-Type"))
			assert.Equal(
				t,
				digest.FromString(manifestBody),
				digest.FromBytes(request.body),
				"the manifest must reach the registry byte for byte",
			)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), strconv.Itoa(tt.status))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, digest.FromString(manifestBody), got)
		})
	}
}

// newDigestPushRegistry starts a fake registry served by handler and returns
// a repository that publishes manifests by digest on repoName. The server
// shuts down with the test.
func newDigestPushRegistry(t *testing.T, handler http.Handler) *oci.Repository {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	repo, err := oci.NewDigestPushRepository(hostOf(t, server)+"/"+repoName, oci.WithPlainHTTP())
	require.NoError(t, err)

	return repo
}

func TestManifestsPutPublishesByDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		status  int
		wantErr bool
	}{
		{
			name:   "writes to the digest of the body",
			body:   manifestBody,
			status: http.StatusCreated,
		},
		{
			name:   "a different body is a different digest path",
			body:   otherBody,
			status: http.StatusCreated,
		},
		{
			name:    "a manifest the registry rejects is an error",
			body:    manifestBody,
			status:  http.StatusBadRequest,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo := newDigestPushRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(t, r)
				w.WriteHeader(tt.status)
			}))

			got, err := repo.Manifests().Put(t.Context(), ocispec.MediaTypeImageManifest, []byte(tt.body))

			wantDigest := digest.FromString(tt.body)
			request := rec.only(t)
			assert.Equal(t, http.MethodPut, request.method)
			assert.Equal(t, "/v2/"+repoName+"/manifests/"+wantDigest.String(), request.path)
			assert.Equal(t, ocispec.MediaTypeImageManifest, request.header.Get("Content-Type"))
			assert.Equal(
				t,
				wantDigest,
				digest.FromBytes(request.body),
				"the manifest must reach the registry byte for byte",
			)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), strconv.Itoa(tt.status))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, wantDigest, got)
		})
	}
}

func TestManifestsGetRejectsDigestPush(t *testing.T) {
	t.Parallel()

	var rec recorder
	repo := newDigestPushRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		t.Error("digest-push Get must not talk to the registry")
		w.WriteHeader(http.StatusOK)
	}))

	body, desc, err := repo.Manifests().Get(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no bound manifest")
	require.NotErrorIs(t, err, oci.ErrNotFound)
	assert.Empty(t, body)
	assert.Equal(t, ocispec.Descriptor{}, desc)
	assert.Empty(t, rec.all())
}

// assertCodedRefusal checks that a coded manifest or blob response failed as
// a terminal, origin-safe identity-coding refusal and did not echo the peer
// header.
func assertCodedRefusal(t *testing.T, err error, originPath, encoding string) {
	t.Helper()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "GET "+originPath, "the original registry operation stays diagnostic")
	assert.Contains(t, err.Error(), codedRefusal)
	assert.NotContains(t, err.Error(), encoding, "a peer-selected coding is direct reflection material")
	require.NotErrorIs(t, err, oci.ErrNotFound)
	require.NotErrorIs(t, err, oci.ErrUnauthorized)

	_, transient := retry.IsTransient(err)
	assert.False(t, transient, "a coded response will arrive coded again")
}

// newClosingRegistry starts a fake registry and returns a repository bound to
// ref on it, together with a flag that becomes true when a response body is
// closed.
func newClosingRegistry(t *testing.T, handler http.Handler, ref string) (*oci.Repository, *atomic.Bool) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	closed := &atomic.Bool{}
	client := &http.Client{Transport: &bodyCloseTransport{closed: closed}}
	repo, err := oci.NewRepository(hostOf(t, server)+"/"+ref, oci.WithPlainHTTP(), oci.WithHTTPClient(client))
	require.NoError(t, err)

	return repo, closed
}

// bodyCloseTransport records whether a response body was closed, so a coded
// refusal can prove it released the connection.
type bodyCloseTransport struct {
	// closed is set when any response body this transport wrapped is closed.
	closed *atomic.Bool
}

// RoundTrip sends req with the default transport and wraps the body.
func (t *bodyCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	resp.Body = &bodyCloseTracker{ReadCloser: resp.Body, closed: t.closed}

	return resp, nil
}

// bodyCloseTracker is a response body that records its Close.
type bodyCloseTracker struct {
	io.ReadCloser

	// closed is set when Close is called.
	closed *atomic.Bool
}

// Close closes the wrapped body and records that it happened.
func (b *bodyCloseTracker) Close() error {
	b.closed.Store(true)

	return b.ReadCloser.Close()
}
