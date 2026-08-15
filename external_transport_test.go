package bigoci

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// observerState is shared by a wrapper and every layer BigociWrapExternal
// rebuilds from it, so construction-time wrapping and request-time
// observation are visible together after the transfer returns.
type observerState struct {
	// original is the concrete transport handed to [WithHTTPClient].
	original http.RoundTripper
	// wrapCalls counts [rebuiltObserver.BigociWrapExternal] invocations.
	wrapCalls atomic.Int64
	// mu guards wrappedNext and externalHosts.
	mu sync.Mutex
	// wrappedNext is the last transport BigociWrapExternal received.
	wrappedNext http.RoundTripper
	// externalHosts are hostnames observed only by rebuilt layers.
	externalHosts []string
}

// recordWrap notes that bigoci rebuilt the wrapper around next.
func (s *observerState) recordWrap(next http.RoundTripper) {
	s.wrapCalls.Add(1)
	s.mu.Lock()
	s.wrappedNext = next
	s.mu.Unlock()
}

// recordExternal notes one rebuilt-layer request host.
func (s *observerState) recordExternal(host string) {
	s.mu.Lock()
	s.externalHosts = append(s.externalHosts, host)
	s.mu.Unlock()
}

// wrapped returns the last transport BigociWrapExternal received.
func (s *observerState) wrapped() http.RoundTripper {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.wrappedNext
}

// hosts returns a copy of the rebuilt-layer hosts.
func (s *observerState) hosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.externalHosts...)
}

// rebuiltObserver is a pure observer wrapper over a concrete transport. It
// implements the public structural methods so bigoci can clone the base and
// rebuild this layer around the guarded external transport.
type rebuiltObserver struct {
	// next is the transport this layer forwards to.
	next http.RoundTripper
	// state is shared with every rebuilt layer.
	state *observerState
	// external reports that this layer was produced by BigociWrapExternal.
	external bool
}

// RoundTrip records an external host when this is a rebuilt layer, then
// forwards req unchanged.
func (o *rebuiltObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	if o.external {
		o.state.recordExternal(req.URL.Hostname())
	}

	return o.next.RoundTrip(req)
}

// BigociExternalBase returns the transport this layer forwards to.
func (o *rebuiltObserver) BigociExternalBase() http.RoundTripper {
	return o.next
}

// BigociWrapExternal rebuilds this observer around next.
func (o *rebuiltObserver) BigociWrapExternal(next http.RoundTripper) http.RoundTripper {
	o.state.recordWrap(next)

	return &rebuiltObserver{next: next, state: o.state, external: true}
}

// opaqueObserver forwards to a concrete transport without the structural
// methods, so bigoci must treat it as an opaque RoundTripper.
type opaqueObserver struct {
	// next is the transport this layer forwards to.
	next http.RoundTripper
}

// RoundTrip forwards req unchanged.
func (o *opaqueObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	return o.next.RoundTrip(req)
}

// TestExternalTransportWrapperContract locks the public structural seam:
// a compliant rebuilt wrapper stays in default verified mode, observes the
// external request, and still applies the destination guard, while the
// equivalent opaque wrapper fails closed.
func TestExternalTransportWrapperContract(t *testing.T) {
	t.Parallel()

	t.Run("compliant rebuilt wrapper", func(t *testing.T) {
		t.Parallel()

		base, registryHost, realmHost, realmCalls := loopbackTokenChallenge(t)
		state := &observerState{original: base}
		client, err := New(WithHTTPClient(&http.Client{
			Transport: &rebuiltObserver{next: base, state: state},
		}))
		require.NoError(t, err)
		repo, err := client.repository(Reference(registryHost + "/team/artifact:v1"))
		require.NoError(t, err)

		exists, err := repo.Blobs().Exists(t.Context(), digest.FromString("payload"))

		require.ErrorIs(t, err, oci.ErrUnauthorized)
		assert.False(t, exists)
		assert.EqualValues(t, 1, state.wrapCalls.Load(), "bigoci must rebuild a compliant wrapper")
		assert.NotSame(t, state.original, state.wrapped(), "the rebuilt layer must wrap the guarded clone")
		_, isTransport := state.wrapped().(*http.Transport)
		assert.True(t, isTransport, "the guarded base under the rebuilt wrapper must be a concrete transport")
		assert.Contains(t, state.hosts(), realmHost, "the rebuilt wrapper must observe the external request")
		assert.Contains(t, err.Error(), "local or private IP address", "the destination guard must still apply")
		assert.Zero(t, realmCalls.Load(), "HTTP request bytes must not reach the private peer")
	})

	t.Run("opaque wrapper fails closed", func(t *testing.T) {
		t.Parallel()

		base, registryHost, _, realmCalls := loopbackTokenChallenge(t)
		client, err := New(WithHTTPClient(&http.Client{
			Transport: &opaqueObserver{next: base},
		}))
		require.NoError(t, err)
		repo, err := client.repository(Reference(registryHost + "/team/artifact:v1"))
		require.NoError(t, err)

		exists, err := repo.Blobs().Exists(t.Context(), digest.FromString("payload"))

		require.ErrorIs(t, err, oci.ErrUnauthorized)
		assert.False(t, exists)
		assert.Contains(t, err.Error(), "opaque transport cannot prove")
		assert.Zero(t, realmCalls.Load(), "an opaque wrapper must not reach the token realm")
	})
}

// loopbackTokenChallenge starts a TLS registry that challenges to a second
// TLS server named as localhost, plus a concrete transport that can complete
// both handshakes. localhost is a different hostname from the registry's
// 127.0.0.1 address, so the token request is cross-host, but the actual peer
// is still loopback and the destination guard must refuse it.
func loopbackTokenChallenge(t *testing.T) (*http.Transport, string, string, *atomic.Int64) {
	t.Helper()

	var realmCalls atomic.Int64
	realm := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		realmCalls.Add(1)
	}))
	t.Cleanup(realm.Close)
	realmURL, err := url.Parse(realm.URL)
	require.NoError(t, err)
	const realmName = "localhost"
	realmURL.Host = realmName + ":" + realmURL.Port()
	realmURL.Path = "/token"

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realmURL.String()+`",service="fixture"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(registry.Close)
	registryURL, err := url.Parse(registry.URL)
	require.NoError(t, err)

	roots := x509.NewCertPool()
	roots.AddCert(registry.Certificate())
	roots.AddCert(realm.Certificate())
	base := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		// Both fixtures present a 127.0.0.1 certificate. The token request
		// uses the localhost name so it is cross-host; this ServerName keeps
		// the handshake from failing before the destination guard runs.
		ServerName: "127.0.0.1",
	}}
	t.Cleanup(base.CloseIdleConnections)

	return base, registryURL.Host, realmName, &realmCalls
}
