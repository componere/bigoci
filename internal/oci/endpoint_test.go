package oci

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/retry"
)

// dialMode selects the concrete [http.Transport] hook a regression exercises.
type dialMode string

const (
	// dialWithContext routes a raw connection through DialContext.
	dialWithContext dialMode = "DialContext"
	// dialTLSWithContext routes an already handshaken TLS connection through
	// DialTLSContext.
	dialTLSWithContext dialMode = "DialTLSContext"
)

// mappedTLSTransport builds a caller transport that maps two public DNS names
// onto TLS fixtures. targetDials counts connection setup to the target; an
// HTTP handler count separately proves whether request bytes followed it.
func mappedTLSTransport(
	t *testing.T,
	registry *httptest.Server,
	target *httptest.Server,
	mode dialMode,
	targetDials *atomic.Int64,
) *http.Transport {
	t.Helper()

	roots := x509.NewCertPool()
	roots.AddCert(registry.Certificate())
	roots.AddCert(target.Certificate())
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "example.com",
	}
	dialer := &net.Dialer{}
	mapAddress := func(address string) string {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return address
		}

		switch host {
		case "registry.example.com":
			return registry.Listener.Addr().String()
		case "internal.example.com", "auth.example.com":
			targetDials.Add(1)

			return target.Listener.Addr().String()
		default:
			return address
		}
	}

	transport := &http.Transport{TLSClientConfig: tlsConfig}
	switch mode {
	case dialWithContext:
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, mapAddress(address))
		}
	case dialTLSWithContext:
		transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, mapAddress(address))
			if err != nil {
				return nil, err
			}

			secured := tls.Client(conn, tlsConfig.Clone())
			if err := secured.HandshakeContext(ctx); err != nil {
				_ = conn.Close()

				return nil, err
			}

			return secured, nil
		}
	default:
		t.Fatalf("unknown dial mode %q", mode)
	}
	t.Cleanup(transport.CloseIdleConnections)

	return transport
}

// TestCrossRegistryTokenRealmCannotResolveToAPrivatePeer ports the DNS
// rebinding proof against both concrete custom dial seams. The secure default
// refuses each opaque dial path before connection setup. The explicit escape
// hatch delegates the destination boundary to the caller and therefore reaches
// the mapped realm.
func TestCrossRegistryTokenRealmCannotResolveToAPrivatePeer(t *testing.T) {
	t.Parallel()

	for _, mode := range []dialMode{dialWithContext, dialTLSWithContext} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			const (
				pathSecret  = "redeemable-path-ticket"
				querySecret = "redeemable-query-ticket"
			)

			var realmCalls atomic.Int64
			realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				realmCalls.Add(1)
				_, _ = io.WriteString(w, `{"token":"must-not-arrive"}`)
			}))
			t.Cleanup(realm.Close)

			registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				port := realm.Listener.Addr().(*net.TCPAddr).Port
				w.Header().Set(headerChallenge, fmt.Sprintf(
					`Bearer realm="https://internal.example.com:%d/%s?ticket=%s",service="fixture"`,
					port,
					pathSecret,
					querySecret,
				))
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(registry.Close)

			var targetDials atomic.Int64
			transport := mappedTLSTransport(t, registry, realm, mode, &targetDials)
			creds := WithCredentials(&staticCredentials{
				cred: Credential{Username: "audit", Password: "secret"},
			})
			defaultRepo, err := NewRepository(
				"registry.example.com/"+authRepo+":"+authTag,
				WithHTTPClient(&http.Client{Transport: transport}),
				creds,
			)
			require.NoError(t, err)

			_, err = defaultRepo.Blobs().Exists(t.Context(), authDigest())

			require.ErrorIs(t, err, ErrUnauthorized)
			_, transient := retry.IsTransient(err)
			assert.False(t, transient)
			assert.Zero(t, targetDials.Load(), "an unverified caller dial hook fails before it can tunnel")
			assert.Zero(t, realmCalls.Load())
			assert.NotContains(t, err.Error(), pathSecret)
			assert.NotContains(t, err.Error(), querySecret)

			repo, err := NewRepository(
				"registry.example.com/"+authRepo+":"+authTag,
				WithHTTPClient(&http.Client{Transport: transport}),
				creds,
				WithUnverifiedExternalTransport(),
			)
			require.NoError(t, err)

			_, err = repo.Blobs().Exists(t.Context(), authDigest())

			require.ErrorIs(t, err, ErrUnauthorized)
			_, transient = retry.IsTransient(err)
			assert.False(t, transient)
			assert.Positive(t, targetDials.Load(), "the explicit option delegates to the caller's dial hook")
			assert.Positive(t, realmCalls.Load(), "the caller-owned boundary receives the token request")
			assert.NotContains(t, err.Error(), pathSecret)
			assert.NotContains(t, err.Error(), querySecret)
		})
	}
}

// TestCrossRegistryUploadLocationCannotResolveToAPrivatePeer ports the upload
// DNS proof through the same guarded external transport used by token realms.
func TestCrossRegistryUploadLocationCannotResolveToAPrivatePeer(t *testing.T) {
	t.Parallel()

	const (
		payload     = "caller-file-bytes-sent-to-loopback"
		pathSecret  = "private-admin-path"
		querySecret = "signed-upload-state"
	)

	var uploadCalls atomic.Int64
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		uploadCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(store.Close)

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/blobs/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
			port := store.Listener.Addr().(*net.TCPAddr).Port
			w.Header().Set("Location", fmt.Sprintf(
				"https://internal.example.com:%d/%s?state=%s",
				port,
				pathSecret,
				querySecret,
			))
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(registry.Close)

	var targetDials atomic.Int64
	transport := mappedTLSTransport(t, registry, store, dialWithContext, &targetDials)
	defaultRepo, err := NewRepository(
		"registry.example.com/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
	)
	require.NoError(t, err)

	err = defaultRepo.Blobs().Put(t.Context(),
		digest.FromString(payload),
		int64(len(payload)),
		strings.NewReader(payload), nil)

	require.Error(t, err)
	_, transient := retry.IsTransient(err)
	assert.False(t, transient)
	assert.Zero(t, targetDials.Load(), "an unverified caller dial hook fails before it can tunnel")
	assert.Zero(t, uploadCalls.Load())
	assert.NotContains(t, err.Error(), payload)
	assert.NotContains(t, err.Error(), pathSecret)
	assert.NotContains(t, err.Error(), querySecret)

	repo, err := NewRepository(
		"registry.example.com/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
		WithUnverifiedExternalTransport(),
	)
	require.NoError(t, err)

	err = repo.Blobs().
		Put(t.Context(), digest.FromString(payload), int64(len(payload)), strings.NewReader(payload), nil)

	require.NoError(t, err)
	assert.Positive(t, targetDials.Load(), "the explicit option delegates to the caller's dial hook")
	assert.Positive(t, uploadCalls.Load(), "the caller-owned boundary receives the upload")
}

// reportedAddrConn lets a loopback fixture model a direct connection whose
// actual peer is public without changing the bytes carried by the connection.
type reportedAddrConn struct {
	net.Conn

	// remote is the address the transport guard observes.
	remote net.Addr
}

// RemoteAddr returns the modeled public peer.
func (c *reportedAddrConn) RemoteAddr() net.Addr {
	return c.remote
}

// stringAddr is a non-TCP network address whose String method carries an IP
// endpoint. It exercises the fallback parser used for wrapped connections.
type stringAddr string

// Network names the fixture address family.
func (a stringAddr) Network() string {
	return "fixture"
}

// String renders the fixture endpoint.
func (a stringAddr) String() string {
	return string(a)
}

// TestPrivatePeerRefusalDoesNotRenderAddress proves both address-parsing paths
// refuse a private peer without repeating peer-controlled address text in the
// public policy error.
func TestPrivatePeerRefusalDoesNotRenderAddress(t *testing.T) {
	t.Parallel()

	const reflectedToken = "127.0.0.1"
	tests := []struct {
		name string
		addr net.Addr
	}{
		{name: "TCP address", addr: &net.TCPAddr{IP: net.ParseIP(reflectedToken), Port: 443}},
		{name: "fallback address", addr: stringAddr(reflectedToken + ":443")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, server := net.Pipe()
			t.Cleanup(func() { _ = server.Close() })
			check := &connectionCheck{}
			check.gotConn(httptrace.GotConnInfo{Conn: &reportedAddrConn{Conn: client, remote: tt.addr}})

			err := check.failure()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "local or private IP address")
			assert.NotContains(t, err.Error(), reflectedToken)
		})
	}
}

// TestAPublicCrossRegistryTokenRealmStillWorks proves the boundary permits
// the distribution protocol's normal public cross-host token exchange.
func TestAPublicCrossRegistryTokenRealmStillWorks(t *testing.T) {
	t.Parallel()

	const token = "public-realm-token"

	var realmCalls atomic.Int64
	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		realmCalls.Add(1)
		_, _ = fmt.Fprintf(w, `{"token":%q}`, token)
	}))
	t.Cleanup(realm.Close)

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerAuthorization) == bearerHeader(token) {
			w.WriteHeader(http.StatusOK)

			return
		}

		port := realm.Listener.Addr().(*net.TCPAddr).Port
		w.Header().Set(headerChallenge, fmt.Sprintf(
			`Bearer realm="https://auth.example.com:%d/token",service="fixture"`,
			port,
		))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(registry.Close)

	var targetDials atomic.Int64
	transport := mappedTLSTransport(t, registry, realm, dialWithContext, &targetDials)
	baseDial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := baseDial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(address, "auth.example.com:") {
			return &reportedAddrConn{
				Conn:   conn,
				remote: &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 443},
			}, nil
		}

		return conn, nil
	}

	repo, err := NewRepository(
		"registry.example.com/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
		WithUnverifiedExternalTransport(),
	)
	require.NoError(t, err)

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.NoError(t, err)
	assert.True(t, exists)
	assert.EqualValues(t, 1, realmCalls.Load())
}

// TestExternalTransportKeepsOneConnectionPool proves the explicit escape hatch
// keeps the caller's original pool rather than rebuilding a transport for each
// registry-selected request.
func TestExternalTransportKeepsOneConnectionPool(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)

	var dials atomic.Int64
	dialer := &net.Dialer{}
	base := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		conn, err := dialer.DialContext(ctx, network, server.Listener.Addr().String())
		if err != nil {
			return nil, err
		}

		return &reportedAddrConn{
			Conn:   conn,
			remote: &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 80},
		}, nil
	}}
	t.Cleanup(base.CloseIdleConnections)
	guard := newExternalTransport(base, "registry.example.com", true)

	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://auth.example.com/token", nil)
		require.NoError(t, err)
		resp, err := guard.RoundTrip(req)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	assert.EqualValues(t, 1, dials.Load(), "the second request reuses the guarded transport's connection")
}

// TestTheRegistrysOwnPrivateHostStillWorksOnAnotherPort proves the exception
// is bound to the registry hostname, not to one port or to public addressing.
func TestTheRegistrysOwnPrivateHostStillWorksOnAnotherPort(t *testing.T) {
	t.Parallel()

	const token = "same-registry-token"

	var realmCalls atomic.Int64
	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		realmCalls.Add(1)
		_, _ = fmt.Fprintf(w, `{"token":%q}`, token)
	}))
	t.Cleanup(realm.Close)

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerAuthorization) == bearerHeader(token) {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.Header().Set(headerChallenge, fmt.Sprintf(
			`Bearer realm=%q,service="fixture"`, realm.URL+"/token",
		))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(registry.Close)

	roots := x509.NewCertPool()
	roots.AddCert(registry.Certificate())
	roots.AddCert(realm.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	}}
	t.Cleanup(transport.CloseIdleConnections)

	registryURL, err := url.Parse(registry.URL)
	require.NoError(t, err)
	repo, err := NewRepository(
		registryURL.Host+"/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
	)
	require.NoError(t, err)

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.NoError(t, err)
	assert.True(t, exists)
	assert.EqualValues(t, 1, realmCalls.Load())
}

// TestSameEndpointHostComparesHostIdentity pins case, a terminal DNS dot,
// ports, and equivalent IP spellings without broadening the exception to a
// sibling or merely similar hostname.
func TestSameEndpointHostComparesHostIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		target   string
		registry string
		want     bool
	}{
		{name: "DNS case and port", target: "REGISTRY.example.", registry: "registry.example:5000", want: true},
		{name: "IPv6 spelling and port", target: "::1", registry: "[0:0:0:0:0:0:0:1]:5000", want: true},
		{name: "mapped IPv4 spelling", target: "::ffff:127.0.0.1", registry: "127.0.0.1:5000", want: true},
		{name: "sibling hostname", target: "auth.registry.example", registry: "registry.example:5000"},
		{name: "domain suffix", target: "evilregistry.example", registry: "registry.example:5000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, sameEndpointHost(tt.target, tt.registry))
		})
	}
}

// TestRedeemableRealmTicketNeverEntersAnError ports the consequence proof: a
// registry-selected ticket remains usable at the realm after bigoci refuses
// the malformed challenge, while the public error reveals none of it.
func TestRedeemableRealmTicketNeverEntersAnError(t *testing.T) {
	t.Parallel()

	const (
		pathSecret  = "redeemable-realm-path"
		querySecret = "redeemable-realm-ticket"
		token       = "ticket-holder-token"
	)

	var realmCalls atomic.Int64
	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realmCalls.Add(1)
		if r.URL.Query().Get("ticket") != querySecret {
			w.WriteHeader(http.StatusForbidden)

			return
		}

		_, _ = fmt.Fprintf(w, `{"token":%q}`, token)
	}))
	t.Cleanup(realm.Close)

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerChallenge, fmt.Sprintf(
			`Bearer realm=%q,service="fixture"`,
			realm.URL+"/"+pathSecret+"?ticket="+querySecret+"#private-fragment",
		))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(registry.Close)

	roots := x509.NewCertPool()
	roots.AddCert(registry.Certificate())
	roots.AddCert(realm.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	}}
	t.Cleanup(transport.CloseIdleConnections)
	registryURL, err := url.Parse(registry.URL)
	require.NoError(t, err)
	repo, err := NewRepository(
		registryURL.Host+"/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
	)
	require.NoError(t, err)

	_, err = repo.Blobs().Exists(t.Context(), authDigest())

	require.ErrorIs(t, err, ErrUnauthorized)
	assert.Zero(t, realmCalls.Load(), "a fragmented realm is refused before its ticket is redeemed")
	assert.NotContains(t, err.Error(), pathSecret)
	assert.NotContains(t, err.Error(), querySecret)
	assert.NotContains(t, err.Error(), "private-fragment")

	resp, redeemErr := realm.Client().Get(realm.URL + "/" + pathSecret + "?ticket=" + querySecret)
	require.NoError(t, redeemErr)
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), token, "the omitted ticket had a real bearer-token consequence")
}

// TestTokenEndpointFailureDoesNotExposePathQueryOrBody proves successful
// realm validation does not make endpoint-controlled error material safe to
// render. Status sentinels, retry classification, and Retry-After survive.
func TestTokenEndpointFailureDoesNotExposePathQueryOrBody(t *testing.T) {
	t.Parallel()

	const (
		pathSecret  = "token-path-secret"
		querySecret = "token-query-secret"
		bodySecret  = "token-body-secret"
	)

	tests := []struct {
		name          string
		status        int
		retryAfter    string
		wantTransient bool
		wantAfter     time.Duration
		wantRefusal   bool
	}{
		{
			name:          "transient status",
			status:        http.StatusServiceUnavailable,
			retryAfter:    "2",
			wantTransient: true,
			wantAfter:     2 * time.Second,
		},
		{name: "credential refusal", status: http.StatusUnauthorized, wantRefusal: true},
		{name: "terminal status", status: http.StatusBadRequest},
		{name: "successful status with malformed body", status: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, bodySecret)
			}))
			t.Cleanup(realm.Close)

			registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(headerChallenge, fmt.Sprintf(
					`Bearer realm=%q,service="fixture"`,
					realm.URL+"/"+pathSecret+"?ticket="+querySecret,
				))
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(registry.Close)

			roots := x509.NewCertPool()
			roots.AddCert(registry.Certificate())
			roots.AddCert(realm.Certificate())
			transport := &http.Transport{TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    roots,
			}}
			t.Cleanup(transport.CloseIdleConnections)
			registryURL, err := url.Parse(registry.URL)
			require.NoError(t, err)
			repo, err := NewRepository(
				registryURL.Host+"/"+authRepo+":"+authTag,
				WithHTTPClient(&http.Client{Transport: transport}),
			)
			require.NoError(t, err)

			_, err = repo.Blobs().Exists(t.Context(), authDigest())

			require.Error(t, err)
			after, transient := retry.IsTransient(err)
			assert.Equal(t, tt.wantTransient, transient)
			assert.Equal(t, tt.wantAfter, after)
			assert.Equal(t, tt.wantRefusal, errors.Is(err, ErrUnauthorized))
			assert.NotContains(t, err.Error(), pathSecret)
			assert.NotContains(t, err.Error(), querySecret)
			assert.NotContains(t, err.Error(), bodySecret)
			if tt.status != http.StatusOK {
				var status *StatusError
				require.ErrorAs(t, err, &status)
				assert.Equal(t, "token endpoint", status.Path)
				assert.Empty(t, status.Detail)
			}
		})
	}
}

// roundTripFunc adapts a function into a RoundTripper for boundary tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// okResponse returns an empty successful response for req.
func okResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Header:     make(http.Header),
		Request:    req,
	}
}

// TestOpaqueExternalTransportRequiresExplicitAuthorization proves the secure
// default, same-registry exception, and public escape hatch for a transport
// whose final connection bigoci cannot inspect.
func TestOpaqueExternalTransportRequiresExplicitAuthorization(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	opaque := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)

		return okResponse(req), nil
	})

	cross, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://auth.example.com/token", nil)
	require.NoError(t, err)
	same, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://registry.example.com:5443/token", nil)
	require.NoError(t, err)

	_, err = newExternalTransport(opaque, "registry.example.com:443", false).RoundTrip(cross)
	var targetErr *externalTargetError
	require.ErrorAs(t, err, &targetErr)
	assert.Zero(t, calls.Load())

	resp, err := newExternalTransport(opaque, "registry.example.com:443", false).RoundTrip(same)
	require.NoError(t, err)
	resp.Body.Close()
	assert.EqualValues(t, 1, calls.Load(), "the registry's own host stays authorized")

	resp, err = newExternalTransport(opaque, "registry.example.com:443", true).RoundTrip(cross)
	require.NoError(t, err)
	resp.Body.Close()
	assert.EqualValues(t, 2, calls.Load(), "the explicit option delegates the hidden destination policy")
}

// TestSameRegistryUsesTheOriginalConcreteTransport proves the hostname
// exception keeps caller behavior that [http.Transport.Clone] cannot carry,
// as well as the caller's original pool.
func TestSameRegistryUsesTheOriginalConcreteTransport(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	transport := &http.Transport{}
	transport.RegisterProtocol("https", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)

		return okResponse(req), nil
	}))
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"https://registry.example.com:5443/token",
		nil,
	)
	require.NoError(t, err)

	resp, err := newExternalTransport(transport, "registry.example.com:443", false).RoundTrip(req)

	require.NoError(t, err)
	resp.Body.Close()
	assert.EqualValues(t, 1, calls.Load())
}

// TestCustomTLSProtocolHandlerIsOpaque proves a concrete [http.Transport] is
// not overclaimed when the caller replaces net/http's trace-aware protocol
// implementation after the TLS handshake.
func TestCustomTLSProtocolHandlerIsOpaque(t *testing.T) {
	t.Parallel()

	var protocolCalls atomic.Int64
	transport := &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{
		"custom": func(string, *tls.Conn) http.RoundTripper {
			protocolCalls.Add(1)

			return roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return okResponse(req), nil
			})
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://auth.example.com/token", nil)
	require.NoError(t, err)

	_, err = newExternalTransport(transport, "registry.example.com", false).RoundTrip(req)

	var targetErr *externalTargetError
	require.ErrorAs(t, err, &targetErr)
	assert.Zero(t, protocolCalls.Load())
}

// TestAStandardTransportStaysInspectableAfterHTTP2Use proves net/http's
// automatically populated TLSNextProto hooks remain distinguishable from a
// caller-supplied opaque handler after an unrelated HTTPS request used them.
func TestAStandardTransportStaysInspectableAfterHTTP2Use(t *testing.T) {
	t.Parallel()

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "warm")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	}
	t.Cleanup(transport.CloseIdleConnections)
	resp, err := (&http.Client{Transport: transport}).Get(server.URL)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 2, resp.ProtoMajor, "the warm-up really installed and used standard HTTP/2")
	assert.NotEmpty(t, transport.TLSNextProto, "the source transport now has auto-populated hooks")

	repo, err := NewRepository(
		"registry.example.com/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
	)
	require.NoError(t, err)
	guard, ok := repo.external.Transport.(*externalTransport)
	require.True(t, ok)
	assert.True(t, guard.inspectable)
	assert.False(t, guard.unverifiedDial, "DefaultTransport's inherited dialer is direct, not an opaque tunnel")
	_, ok = guard.next.(*http.Transport)
	assert.True(t, ok, "standard HTTP/2 remains on the guarded concrete transport path")
}

// TestUnverifiedDialHooksDistinguishesStandardDialersFromCallerHooks pins the
// default boundary: net/http's ordinary dial path remains seamless, while a
// hook whose behavior cannot be inferred from its type requires authorization.
func TestUnverifiedDialHooksDistinguishesStandardDialersFromCallerHooks(t *testing.T) {
	t.Parallel()

	dialer := &net.Dialer{}
	dialError := errors.New("dial must not run")
	tests := []struct {
		name      string
		transport *http.Transport
		want      bool
	}{
		{
			name:      "zero transport uses net http default dialer",
			transport: &http.Transport{},
		},
		{
			name:      "default transport clone keeps the standard dial hook",
			transport: http.DefaultTransport.(*http.Transport).Clone(),
		},
		{
			name:      "explicit net Dialer method is a direct standard hook",
			transport: &http.Transport{DialContext: dialer.DialContext},
		},
		{
			name: "custom DialContext closure",
			transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, dialError
			}},
			want: true,
		},
		{
			name: "legacy Dial hook",
			transport: &http.Transport{Dial: func(string, string) (net.Conn, error) {
				return nil, dialError
			}},
			want: true,
		},
		{
			name: "DialTLS hook",
			transport: &http.Transport{DialTLS: func(string, string) (net.Conn, error) {
				return nil, dialError
			}},
			want: true,
		},
		{
			name: "DialTLSContext hook",
			transport: &http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, dialError
			}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, unverifiedDialHooks(tt.transport))
		})
	}
}

// TestUnverifiedDialHooksDoesNotTrustAMutatedDefaultTransport proves the
// standard-hook identity was snapshotted before callers could replace the
// mutable global transport with a tunnel. This test is intentionally serial
// because it temporarily changes a process global.
func TestUnverifiedDialHooksDoesNotTrustAMutatedDefaultTransport(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport)
	original := transport.DialContext
	t.Cleanup(func() {
		transport.DialContext = original
	})

	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("custom tunnel must not run")
	}

	assert.True(t, unverifiedDialHooks(transport))
}

// TestProxyExternalTransportRequiresExplicitAuthorization proves bigoci does
// not claim a direct-peer check when a proxy owns the destination connection.
func TestProxyExternalTransportRequiresExplicitAuthorization(t *testing.T) {
	t.Parallel()

	var proxyCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)

	base := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	t.Cleanup(base.CloseIdleConnections)
	cross, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://auth.example.com/token", nil)
	require.NoError(t, err)
	same, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://registry.example.com:5000/token", nil)
	require.NoError(t, err)

	_, err = newExternalTransport(base, "registry.example.com:443", false).RoundTrip(cross)
	var targetErr *externalTargetError
	require.ErrorAs(t, err, &targetErr)
	assert.Zero(t, proxyCalls.Load())

	resp, err := newExternalTransport(base, "registry.example.com:443", false).RoundTrip(same)
	require.NoError(t, err)
	resp.Body.Close()
	assert.EqualValues(t, 1, proxyCalls.Load(), "same-registry endpoints need no escape hatch")

	resp, err = newExternalTransport(base, "registry.example.com:443", true).RoundTrip(cross)
	require.NoError(t, err)
	resp.Body.Close()
	assert.EqualValues(t, 2, proxyCalls.Load())
}

// TestExternalTargetPolicyErrorIsNotTransient pins the terminal classification
// used by upload locations and redirect targets.
func TestExternalTargetPolicyErrorIsNotTransient(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://auth.example.com/token", nil)
	require.NoError(t, err)
	client := &http.Client{Transport: newExternalTransport(
		roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("must not run")
		}),
		"registry.example.com",
		false,
	)}

	_, err = client.Do(req)
	require.Error(t, err)
	_, transient := retry.IsTransient(err)
	assert.False(t, transient)
}
