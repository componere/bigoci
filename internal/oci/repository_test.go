package oci_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ociblob "github.com/imgoci/go-oci-blob"
	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/oci"
	"github.com/imgoci/bigoci/internal/retry"
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

func TestBlobPutPreservesStatusCode(t *testing.T) {
	t.Parallel()

	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}), repoName+":"+tag)

	err := repo.Blobs().Put(t.Context(),
		digest.FromString(blobPayload),
		int64(len(blobPayload)),
		strings.NewReader(blobPayload), nil)

	code, ok := ociblob.StatusCode(err)
	require.True(t, ok, "callers can inspect the upstream response status")
	assert.Equal(t, http.StatusRequestEntityTooLarge, code)
	require.ErrorIs(t, err, oci.ErrTooLarge)
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
				_, _, err := repo.Blobs().Get(ctx, digest.FromString(blobPayload), 0)

				return err
			},
		},
		{
			name: "blob upload",
			call: func(ctx context.Context, repo *oci.Repository) error {
				payload := strings.NewReader(blobPayload)

				return repo.Blobs().Put(ctx, digest.FromString(blobPayload), int64(len(blobPayload)), payload, nil)
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

func TestStatusErrorMatchesTheSentinelItsStatusStandsFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		target error
		want   bool
	}{
		{
			name:   "a missing manifest is not found",
			status: http.StatusNotFound,
			target: oci.ErrNotFound,
			want:   true,
		},
		{
			name:   "a missing manifest is not too large",
			status: http.StatusNotFound,
			target: oci.ErrTooLarge,
		},
		{
			name:   "a part the registry refuses is too large",
			status: http.StatusRequestEntityTooLarge,
			target: oci.ErrTooLarge,
			want:   true,
		},
		{
			name:   "a part the registry refuses is not missing",
			status: http.StatusRequestEntityTooLarge,
			target: oci.ErrNotFound,
		},
		{
			name:   "a server failure is neither",
			status: http.StatusServiceUnavailable,
			target: oci.ErrNotFound,
		},
		{
			name:   "a server failure is not too large either",
			status: http.StatusServiceUnavailable,
			target: oci.ErrTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := &oci.StatusError{
				Method: http.MethodPut,
				Path:   "/v2/" + repoName + "/blobs/uploads/session-1",
				Status: tt.status,
			}

			if tt.want {
				assert.ErrorIs(t, err, tt.target)

				return
			}

			assert.NotErrorIs(t, err, tt.target)
		})
	}
}

// TestStatusErrorMatchesTheUnauthorizedSentinelOnBothRefusals pins the two
// statuses a registry refuses a request with onto one sentinel, and pins what
// they are not. The negative rows are the point of the table: a sentinel that
// tells a caller to go and log in is only useful while nothing else reaches
// it, and a status that means something else must keep meaning it.
func TestStatusErrorMatchesTheUnauthorizedSentinelOnBothRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		target error
		want   bool
	}{
		{
			name:   "a registry asking for credentials is unauthorized",
			status: http.StatusUnauthorized,
			target: oci.ErrUnauthorized,
			want:   true,
		},
		{
			name:   "a registry refusing the credentials it got is unauthorized too",
			status: http.StatusForbidden,
			target: oci.ErrUnauthorized,
			want:   true,
		},
		{
			name:   "a refused request is not missing",
			status: http.StatusUnauthorized,
			target: oci.ErrNotFound,
		},
		{
			name:   "a refused request is not too large",
			status: http.StatusUnauthorized,
			target: oci.ErrTooLarge,
		},
		{
			name:   "a forbidden request is not missing either",
			status: http.StatusForbidden,
			target: oci.ErrNotFound,
		},
		{
			name:   "a forbidden request is not too large either",
			status: http.StatusForbidden,
			target: oci.ErrTooLarge,
		},
		{
			name:   "a missing manifest is not a refusal",
			status: http.StatusNotFound,
			target: oci.ErrUnauthorized,
		},
		{
			name:   "a part the registry says is too large is not a refusal",
			status: http.StatusRequestEntityTooLarge,
			target: oci.ErrUnauthorized,
		},
		{
			name:   "a server failure is not a refusal",
			status: http.StatusServiceUnavailable,
			target: oci.ErrUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := &oci.StatusError{
				Method: http.MethodGet,
				Path:   "/v2/" + repoName + "/manifests/" + tag,
				Status: tt.status,
			}

			if tt.want {
				assert.ErrorIs(t, err, tt.target)

				return
			}

			assert.NotErrorIs(t, err, tt.target)
		})
	}
}

// TestStatusErrorAnswersOnlyForTheSentinelsItStandsFor guards the default of
// the match: a status error answers for this package's sentinels and reports
// nothing about any other target, so adding the refusal rows widened what a
// 401 matches and nothing else.
func TestStatusErrorAnswersOnlyForTheSentinelsItStandsFor(t *testing.T) {
	t.Parallel()

	unrelated := errors.New("some other failure entirely")
	err := &oci.StatusError{
		Method: http.MethodGet,
		Path:   "/v2/" + repoName + "/manifests/" + tag,
		Status: http.StatusUnauthorized,
	}

	require.NotErrorIs(t, err, unrelated)
	require.NotErrorIs(t, err, io.EOF)
	require.NotErrorIs(t, err, context.Canceled)
}

func TestUnexpectedStatusesAreClassifiedForTheRetryPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		wantTransient bool
	}{
		{name: "a rate limited request is worth repeating", status: http.StatusTooManyRequests, wantTransient: true},
		{
			name:          "an unavailable registry is worth repeating",
			status:        http.StatusServiceUnavailable,
			wantTransient: true,
		},
		{name: "a refused request is not", status: http.StatusUnauthorized},
		{name: "a part the registry says is too large is not", status: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}), repoName+":"+tag)

			_, err := repo.Blobs().Exists(t.Context(), digest.FromString(blobPayload))

			require.Error(t, err)

			after, transient := retry.IsTransient(err)
			assert.Equal(t, tt.wantTransient, transient)
			assert.Zero(t, after, "no Retry-After header means no wait rides on the tag")

			var statusErr *oci.StatusError

			require.ErrorAs(t, err, &statusErr, "the status stays reachable through the tag")
			assert.Equal(t, tt.status, statusErr.Status)
		})
	}
}

func TestStatusErrorReadsTheSameThroughTheTransientTag(t *testing.T) {
	t.Parallel()

	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "the registry is restarting")
	}), repoName+":"+tag)

	_, _, err := repo.Manifests().Get(t.Context())

	require.Error(t, err)
	assert.Equal(
		t,
		"GET /v2/"+repoName+"/manifests/"+tag+
			": registry returned 503 Service Unavailable",
		err.Error(),
		"neither peer detail nor the wait the registry asked for reaches the message a caller reads",
	)

	after, transient := retry.IsTransient(err)
	assert.True(t, transient)
	assert.Equal(t, 5*time.Second, after, "the wait rides on the tag, for the retry policy to bound")

	var statusErr *oci.StatusError

	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, 5*time.Second, statusErr.RetryAfter, "the raw value the registry asked for")
	assert.Equal(t, "the registry is restarting", statusErr.Detail,
		"bounded detail remains available for explicit programmatic diagnosis")
}

func TestARefusedConnectionIsWorthRepeating(t *testing.T) {
	t.Parallel()

	repo, err := oci.NewRepository(deadAddress(t)+"/"+repoName+":"+tag, oci.WithPlainHTTP())
	require.NoError(t, err)

	dgst := digest.FromString(blobPayload)
	_, err = repo.Blobs().Exists(t.Context(), dgst)

	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.True(t, transient, "a connection nobody answered is worth one more attempt")
	assert.Contains(
		t,
		err.Error(),
		"HEAD /v2/"+repoName+"/blobs/"+dgst.String()+": ",
		"the tag leaves the message the adapter has always reported",
	)
}

func TestACancelledRequestIsNotWorthRepeating(t *testing.T) {
	t.Parallel()

	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), repoName+":"+tag)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := repo.Blobs().Exists(ctx, digest.FromString(blobPayload))

	require.ErrorIs(t, err, context.Canceled)

	_, transient := retry.IsTransient(err)
	assert.False(t, transient, "the transfer ended; the network did not fail")
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

				// The caller's context is alive, so the client's own timeout is
				// a transport failure like any other and stays worth repeating —
				// each new attempt gets a fresh timeout window.
				_, transient := retry.IsTransient(err)
				assert.True(t, transient, "a client timeout with a live transfer context is retryable")

				return
			}

			require.NoError(t, err)
			assert.True(t, exists)
		})
	}
}
