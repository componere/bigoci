package oci

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/retry"
)

// TestMalformedRedirectTicketDoesNotEnterATransportError proves net/http's
// eager Location parsing cannot turn a still-redeemable registry capability
// into public error material.
func TestMalformedRedirectTicketDoesNotEnterATransportError(t *testing.T) {
	t.Parallel()

	const (
		ticket     = "redeemable-malformed-location-ticket"
		redemption = "capability-redeemed"
	)

	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ticket") != ticket {
			w.WriteHeader(http.StatusForbidden)

			return
		}

		_, _ = io.WriteString(w, redemption)
	}))
	t.Cleanup(store.Close)

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerLocation, store.URL+"/bad%zz?ticket="+ticket)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(registry.Close)
	control := *registry.Client()
	control.CheckRedirect = refuseRedirect
	controlResp, controlErr := control.Get(registry.URL + "/control")
	if controlResp != nil && controlResp.Body != nil {
		_ = controlResp.Body.Close()
	}
	require.Error(t, controlErr)
	assert.Contains(t, controlErr.Error(), ticket,
		"the positive control proves net/http repeats the malformed Location")

	registryURL, err := url.Parse(registry.URL)
	require.NoError(t, err)
	repo, err := NewRepository(
		registryURL.Host+"/"+authRepo+":"+authTag,
		WithHTTPClient(registry.Client()),
	)
	require.NoError(t, err)

	body, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)

	require.Error(t, err)
	assert.Nil(t, body)
	_, transient := retry.IsTransient(err)
	assert.True(t, transient)
	assert.Contains(t, err.Error(), "GET "+repo.endpoint("blobs/<digest>").Path)
	assert.NotContains(t, err.Error(), authDigest().String())
	assert.NotContains(t, err.Error(), ticket)
	assert.NotContains(t, err.Error(), "bad%zz")
	assert.NotContains(t, err.Error(), store.URL)

	resp, redeemErr := store.Client().Get(store.URL + "/redeem?ticket=" + ticket)
	require.NoError(t, redeemErr)
	defer resp.Body.Close()
	redeemed, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, redemption, string(redeemed), "the omitted ticket has a real redeemable consequence")
}

// TestReflectedBearerDoesNotEnterAResponseParserError proves a registry cannot
// copy the bearer it just received into a malformed response header and make
// net/http repeat that live credential through bigoci's public error.
func TestReflectedBearerDoesNotEnterAResponseParserError(t *testing.T) {
	t.Parallel()

	const (
		token      = "redeemable-reflected-bearer-token"
		redemption = "bearer-redeemed"
	)

	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"token":%q}`, token)
	}))
	t.Cleanup(realm.Close)

	reflected := make(chan string, 2)
	writeFailures := make(chan error, 2)
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get(headerAuthorization)
		if r.URL.Path == "/redeem" {
			if authorization != bearerHeader(token) {
				w.WriteHeader(http.StatusForbidden)

				return
			}

			_, _ = io.WriteString(w, redemption)

			return
		}

		if authorization == "" {
			w.Header().Set(headerChallenge, fmt.Sprintf(
				`Bearer realm=%q,service="fixture"`, realm.URL+"/token",
			))
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		reflected <- authorization
		if err := writeReflectedMalformedHeader(w, authorization); err != nil {
			writeFailures <- err
		}
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
	control, err := http.NewRequestWithContext(t.Context(), http.MethodHead, registry.URL+"/control", nil)
	require.NoError(t, err)
	control.Header.Set(headerAuthorization, bearerHeader(token))
	controlResp, controlErr := (&http.Client{Transport: transport}).Do(control)
	if controlResp != nil && controlResp.Body != nil {
		_ = controlResp.Body.Close()
	}
	require.Error(t, controlErr)
	assert.Contains(t, controlErr.Error(), token,
		"the positive control proves net/http repeats the reflected header line")
	requireReflected(t, reflected, bearerHeader(token))

	registryURL, err := url.Parse(registry.URL)
	require.NoError(t, err)
	repo, err := NewRepository(
		registryURL.Host+"/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
		WithCredentials(&staticCredentials{cred: Credential{Username: "audit", Password: "secret"}}),
	)
	require.NoError(t, err)

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.Error(t, err)
	assert.False(t, exists)
	_, transient := retry.IsTransient(err)
	assert.True(t, transient)
	assert.Contains(t, err.Error(), "HEAD "+repo.endpoint(blobPath(authDigest())).Path)
	assert.NotContains(t, err.Error(), token)
	assert.NotContains(t, err.Error(), "X-Reflected")
	requireReflected(t, reflected, bearerHeader(token))
	select {
	case writeErr := <-writeFailures:
		require.NoError(t, writeErr)
	default:
	}

	redeem, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		registry.URL+"/redeem",
		nil,
	)
	require.NoError(t, err)
	redeem.Header.Set(headerAuthorization, bearerHeader(token))
	resp, err := (&http.Client{Transport: transport}).Do(redeem)
	require.NoError(t, err)
	defer resp.Body.Close()
	redeemed, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, redemption, string(redeemed), "the omitted bearer remains usable authority")
}

// requireReflected reads one value a completed request's handler already
// recorded, failing immediately rather than letting a broken proof hang.
func requireReflected(t *testing.T, reflected <-chan string, want string) {
	t.Helper()

	select {
	case got := <-reflected:
		require.Equal(t, want, got, "the positive control proves the live bearer was reflected")
	default:
		require.Fail(t, "the registry did not observe a bearer to reflect")
	}
}

// writeReflectedMalformedHeader takes over an HTTP/1 connection and writes a
// NUL into a response header value. net/http's parser error repeats the entire
// malformed header line, including reflected when its caller does not redact
// the transport failure.
func writeReflectedMalformedHeader(w http.ResponseWriter, reflected string) error {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return fmt.Errorf("response writer %T cannot hijack HTTP/1", w)
	}

	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return fmt.Errorf("hijack response: %w", err)
	}
	defer conn.Close()

	_, err = buffered.WriteString(
		"HTTP/1.1 200 OK\r\nX-Reflected: " + reflected + "\x00\r\nContent-Length: 0\r\n\r\n",
	)
	if err != nil {
		return fmt.Errorf("write malformed response: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("flush malformed response: %w", err)
	}

	return nil
}

// TestTransportFailureDoesNotRepeatAnArbitraryCallerError pins the default
// boundary directly: even a caller transport that quotes the request URL and
// fabricated response material cannot put those strings into a public error.
func TestTransportFailureDoesNotRepeatAnArbitraryCallerError(t *testing.T) {
	t.Parallel()

	const secret = "caller-transport-secret"
	cause := errors.New("typed caller transport failure containing " + secret)
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("failed %s: %w", req.URL.String(), cause)
	})
	repo, err := NewRepository(
		"registry.example.com/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
	)
	require.NoError(t, err)

	_, err = repo.Blobs().Exists(t.Context(), authDigest())

	require.Error(t, err)
	require.ErrorIs(t, err, cause)
	_, transient := retry.IsTransient(err)
	assert.True(t, transient)
	assert.True(t, strings.HasPrefix(err.Error(), "HEAD "+repo.endpoint(blobPath(authDigest())).Path))
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), "registry.example.com")
}

// TestExternalTransportFailuresKeepOnlySafeOperationLabels pins both external
// request classifiers against a caller transport that repeats the selected URL
// and fabricated peer material in its error.
func TestExternalTransportFailuresKeepOnlySafeOperationLabels(t *testing.T) {
	t.Parallel()

	const secret = "external-caller-transport-secret"
	cause := errors.New("typed external transport failure containing " + secret)
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("failed %s: %w", req.URL.String(), cause)
	})
	repo, err := NewRepository(
		"registry.example.com/"+authRepo+":"+authTag,
		WithHTTPClient(&http.Client{Transport: transport}),
		WithUnverifiedExternalTransport(),
	)
	require.NoError(t, err)

	tokenReq, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"https://auth.example.com/private-token-path?ticket="+secret,
		nil,
	)
	require.NoError(t, err)
	_, tokenErr := repo.doExternal(tokenReq)
	require.Error(t, tokenErr)
	require.ErrorIs(t, tokenErr, cause)
	_, transient := retry.IsTransient(tokenErr)
	assert.True(t, transient)
	assert.Equal(t, "token exchange: transport failed", tokenErr.Error())

	at := origin{method: http.MethodGet, path: repo.endpoint(blobPath(authDigest())).Path}
	storageReq, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"https://storage.example.com/private-blob-path?ticket="+secret,
		nil,
	)
	require.NoError(t, err)
	_, storageErr := repo.hop(at, storageReq)
	require.Error(t, storageErr)
	require.ErrorIs(t, storageErr, cause)
	_, transient = retry.IsTransient(storageErr)
	assert.True(t, transient)
	assert.Equal(t, at.String()+": external transport failed", storageErr.Error())

	for _, publicErr := range []error{tokenErr, storageErr} {
		assert.NotContains(t, publicErr.Error(), secret)
		assert.NotContains(t, publicErr.Error(), "auth.example.com")
		assert.NotContains(t, publicErr.Error(), "storage.example.com")
	}
}
