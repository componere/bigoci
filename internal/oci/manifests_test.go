package oci_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
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
