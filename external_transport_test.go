package bigoci

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/oci"
)

// opaqueTransportFunc adapts a function into an opaque caller RoundTripper.
type opaqueTransportFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f opaqueTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// scriptedResponse builds one in-memory response to req.
func scriptedResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// TestWithUnverifiedExternalTransportAuthorizesAnOpaqueBoundary proves the
// public escape hatch reaches repositories while the secure default does not.
func TestWithUnverifiedExternalTransportAuthorizesAnOpaqueBoundary(t *testing.T) {
	t.Parallel()

	const token = "opaque-boundary-token"

	tests := []struct {
		name       string
		authorize  bool
		wantCalls  int64
		wantExists bool
	}{
		{name: "secure default", wantCalls: 0},
		{name: "explicit authorization", authorize: true, wantCalls: 1, wantExists: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var realmCalls atomic.Int64
			transport := opaqueTransportFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "auth.example.com" {
					realmCalls.Add(1)

					return scriptedResponse(req, http.StatusOK, make(http.Header), `{"token":"`+token+`"}`), nil
				}

				if req.Header.Get("Authorization") == "Bearer "+token {
					return scriptedResponse(req, http.StatusOK, make(http.Header), ""), nil
				}

				header := http.Header{}
				header.Set("WWW-Authenticate", `Bearer realm="https://auth.example.com/token",service="fixture"`)

				return scriptedResponse(req, http.StatusUnauthorized, header, ""), nil
			})
			opts := []Option{WithHTTPClient(&http.Client{Transport: transport})}
			if tt.authorize {
				opts = append(opts, WithUnverifiedExternalTransport())
			}

			client, err := New(opts...)
			require.NoError(t, err)
			repo, err := client.repository(Reference("registry.example.com/team/artifact:v1"))
			require.NoError(t, err)

			exists, err := repo.Blobs().Exists(t.Context(), digest.FromString("payload"))

			assert.Equal(t, tt.wantCalls, realmCalls.Load())
			assert.Equal(t, tt.wantExists, exists)
			if tt.authorize {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, oci.ErrUnauthorized)
		})
	}
}

// TestWithUnverifiedExternalTransportPreservesRegisteredProtocols proves the
// escape hatch uses the caller's original transport. Transport.Clone omits
// handlers installed through RegisterProtocol, so a clone would never reach
// the cross-host realm in this complete authentication flow.
func TestWithUnverifiedExternalTransportPreservesRegisteredProtocols(t *testing.T) {
	t.Parallel()

	const token = "registered-protocol-token"
	var realmCalls atomic.Int64
	transport := &http.Transport{}
	transport.RegisterProtocol("https", opaqueTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "auth.example.com" {
			realmCalls.Add(1)

			return scriptedResponse(req, http.StatusOK, make(http.Header), `{"token":"`+token+`"}`), nil
		}
		if req.Header.Get("Authorization") == "Bearer "+token {
			return scriptedResponse(req, http.StatusOK, make(http.Header), ""), nil
		}

		header := make(http.Header)
		header.Set("WWW-Authenticate", `Bearer realm="https://auth.example.com/token",service="fixture"`)

		return scriptedResponse(req, http.StatusUnauthorized, header, ""), nil
	}))
	t.Cleanup(transport.CloseIdleConnections)

	client, err := New(
		WithHTTPClient(&http.Client{Transport: transport}),
		WithUnverifiedExternalTransport(),
	)
	require.NoError(t, err)
	repo, err := client.repository(Reference("registry.example.com/team/artifact:v1"))
	require.NoError(t, err)

	exists, err := repo.Blobs().Exists(t.Context(), digest.FromString("payload"))

	require.NoError(t, err)
	assert.True(t, exists)
	assert.EqualValues(t, 1, realmCalls.Load())
}

// TestClientSharesOneExternalTransportBase proves concurrent repositories do
// not create a transport or connection pool per transfer. Copying a Client
// also keeps its earlier safe value semantics by sharing pointer state rather
// than copying a used synchronization primitive.
func TestClientSharesOneExternalTransportBase(t *testing.T) {
	t.Parallel()

	client := &Client{}
	bases := make([]*oci.ExternalTransportBase, 16)

	var group sync.WaitGroup
	for i := range bases {
		group.Go(func() {
			bases[i] = client.externalTransportBase()
		})
	}
	group.Wait()

	require.NotNil(t, bases[0])
	for _, base := range bases {
		assert.Same(t, bases[0], base)
	}

	built, err := New()
	require.NoError(t, err)
	copied := *built
	assert.Same(t, built.externalTransportBase(), copied.externalTransportBase())
}
