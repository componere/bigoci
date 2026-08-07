package oci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/oci"
)

// registry is the host the references that never leave the process are
// written against.
const registry = "registry.example.com"

func TestNewRepository(t *testing.T) {
	t.Parallel()

	manifestDigest := digest.FromString(manifestBody).String()
	tests := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{name: "a tagged reference names a manifest", ref: registry + "/repo:" + tag},
		{name: "a nested repository path is a reference", ref: registry + "/team/nested/artifact:" + tag},
		{name: "a digest reference names a manifest", ref: registry + "/repo@" + manifestDigest},
		{name: "a reference may carry both a tag and a digest", ref: registry + "/repo:" + tag + "@" + manifestDigest},
		{name: "a registry may carry a port", ref: "localhost:5000/repo:" + tag},
		{name: "an empty reference is rejected", ref: "", wantErr: true},
		{name: "a reference that is not one is rejected", ref: "not a reference", wantErr: true},
		{name: "an uppercase repository is rejected", ref: registry + "/Repo:" + tag, wantErr: true},
		{name: "a reference without a registry is rejected", ref: "ubuntu:" + tag, wantErr: true},
		{name: "a reference with neither tag nor digest is rejected", ref: registry + "/repo", wantErr: true},
		{
			name:    "a digest reference that is not sha256 is rejected",
			ref:     registry + "/repo@" + sha512Digest(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, err := oci.NewRepository(tt.ref)

			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, repo)

				return
			}

			require.NoError(t, err)
			require.NotNil(t, repo)
			assert.NotNil(t, repo.Blobs())
			assert.NotNil(t, repo.Manifests())
		})
	}
}

func TestNewRepositoryAddressesTheReferencedRegistry(t *testing.T) {
	t.Parallel()

	var rec recorder
	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		w.WriteHeader(http.StatusOK)
	}), "team/nested/artifact:"+tag)

	dgst := digest.FromString(blobPayload)
	exists, err := repo.Blobs().Exists(t.Context(), dgst)
	require.NoError(t, err)
	assert.True(t, exists)

	request := rec.only(t)
	assert.Equal(t, "/v2/team/nested/artifact/blobs/"+dgst.String(), request.path)
	assert.NotEmpty(t, request.host, "the request must carry the registry the reference named")
}

func TestNewRepositoryDefaultsToHTTPS(t *testing.T) {
	t.Parallel()

	var rec recorder
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	repo, err := oci.NewRepository(hostOf(t, server)+"/"+repoName+":"+tag, oci.WithHTTPClient(server.Client()))
	require.NoError(t, err)

	exists, err := repo.Blobs().Exists(t.Context(), digest.FromString(blobPayload))

	require.NoError(t, err)
	assert.True(t, exists)
	assert.Len(t, rec.all(), 1, "a repository built without WithPlainHTTP must reach a TLS registry")
}

func TestStatusErrorCarriesTheCode(t *testing.T) {
	t.Parallel()

	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}), repoName+":"+tag)

	err := repo.Blobs().Put(
		t.Context(),
		digest.FromString(blobPayload),
		int64(len(blobPayload)),
		strings.NewReader(blobPayload),
	)

	var statusErr *oci.StatusError
	require.ErrorAs(t, err, &statusErr, "callers branch on the status through errors.As")
	assert.Equal(t, http.StatusRequestEntityTooLarge, statusErr.Status)
	assert.Equal(t, http.MethodPost, statusErr.Method, "the failed request here is the session open")
}

func TestRequestsHonorContextCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(ctx context.Context, repo *oci.Repository) error
	}{
		{
			name: "blob existence check",
			call: func(ctx context.Context, repo *oci.Repository) error {
				_, err := repo.Blobs().Exists(ctx, digest.FromString(blobPayload))

				return err
			},
		},
		{
			name: "blob read",
			call: func(ctx context.Context, repo *oci.Repository) error {
				_, err := repo.Blobs().Get(ctx, digest.FromString(blobPayload), 0)

				return err
			},
		},
		{
			name: "blob upload",
			call: func(ctx context.Context, repo *oci.Repository) error {
				payload := strings.NewReader(blobPayload)

				return repo.Blobs().Put(ctx, digest.FromString(blobPayload), int64(len(blobPayload)), payload)
			},
		},
		{
			name: "manifest read",
			call: func(ctx context.Context, repo *oci.Repository) error {
				_, _, err := repo.Manifests().Get(ctx)

				return err
			},
		},
		{
			name: "manifest write",
			call: func(ctx context.Context, repo *oci.Repository) error {
				_, err := repo.Manifests().Put(ctx, "application/vnd.oci.image.manifest.v1+json", []byte("{}"))

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}), repoName+":"+tag)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			require.ErrorIs(t, tt.call(ctx, repo), context.Canceled)
		})
	}
}

// sha512Digest returns a well-formed sha512 digest string, for the reference
// checks that insist on the algorithm the format pins.
func sha512Digest() string {
	return "sha512:" + strings.Repeat("ab", 64)
}

func TestWithHTTPClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		client  *http.Client
		wantErr bool
	}{
		{name: "a nil client leaves the default in place", client: nil},
		{name: "the given client carries the request", client: &http.Client{Timeout: time.Nanosecond}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)

			repo, err := oci.NewRepository(
				hostOf(t, server)+"/"+repoName+":"+tag,
				oci.WithPlainHTTP(),
				oci.WithHTTPClient(tt.client),
			)
			require.NoError(t, err)

			exists, err := repo.Blobs().Exists(t.Context(), digest.FromString(blobPayload))

			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.True(t, exists)
		})
	}
}
