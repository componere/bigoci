package bigoci_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci"
	"github.com/imgoci/bigoci/internal/manifest"
	"github.com/imgoci/bigoci/internal/oci"
)

// The credential a reflected-error fixture gives the client. Its value is
// intentionally ordinary; the reusable protocol credential is the complete
// Authorization header the registry reflects after accepting it.
const (
	reflectedUsername = "reflection-user"
	reflectedPassword = "reflection-password"
)

// TestPublicPullStatusErrorDoesNotRenderAReflectedBearer proves the complete
// consequence boundary: the registry reflects the reusable Bearer header it
// received into a 400 body, the library retains that body for structured
// diagnosis, but the public error string cannot be used to replay the token.
func TestPublicPullStatusErrorDoesNotRenderAReflectedBearer(t *testing.T) {
	proveReflectedAuthorizationIsNotRendered(t, "bearer")
}

// TestPublicPullStatusErrorDoesNotRenderAReflectedBasicCredential runs the
// same proof for the direct Basic header built from caller credentials.
func TestPublicPullStatusErrorDoesNotRenderAReflectedBasicCredential(t *testing.T) {
	proveReflectedAuthorizationIsNotRendered(t, "basic")
}

// proveReflectedAuthorizationIsNotRendered drives one public pull through an
// authentication challenge, a reflected 400 body, and a successful replay by
// a separate log reader.
func proveReflectedAuthorizationIsNotRendered(t *testing.T, kind string) {
	t.Helper()

	const bearerToken = "redeemable-reflected-bearer-a8f4c2"

	expected := "Bearer " + bearerToken
	if kind == "basic" {
		basic := base64.StdEncoding.EncodeToString([]byte(reflectedUsername + ":" + reflectedPassword))
		expected = "Basic " + basic
	}

	presented := make(chan string, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			user, password, ok := r.BasicAuth()
			if !ok || user != reflectedUsername || password != reflectedPassword {
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			writeToken(w, bearerToken)

			return
		case "/redeem":
			if r.Header.Get("Authorization") == expected {
				w.WriteHeader(http.StatusOK)

				return
			}

			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		authorization := r.Header.Get("Authorization")
		if authorization == "" {
			challenge := `Basic realm="fixture"`
			if kind == "bearer" {
				challenge = `Bearer realm="` + server.URL + `/token",service="fixture"`
			}
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		presented <- authorization
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, authorization)
	}))
	t.Cleanup(server.Close)

	ref := bigoci.Reference(strings.TrimPrefix(server.URL, "http://") + "/" + repoName + ":" + tag)
	client := newClient(
		t,
		bigoci.WithPlainHTTP(),
		bigoci.WithCredentials(reflectedUsername, reflectedPassword),
	)

	err := client.Pull(t.Context(), ref, bigoci.ToFile(newPath(t, destName)), bigoci.WithWorkers(1))
	require.Error(t, err)

	var got string
	select {
	case got = <-presented:
	default:
		require.FailNow(t, "the registry never received a reusable credential")
	}
	assert.Equal(t, expected, got, "the registry received the reusable credential before reflecting it")

	var status *oci.StatusError
	require.ErrorAs(t, err, &status)
	assert.Equal(t, expected, status.Detail, "structured diagnosis remains available for compatible callers")
	assert.NotContains(t, err.Error(), expected, "the public error string is safe to log")

	assertAuthorizationReplays(t, server, expected)
}

// TestPublicPullErrorDoesNotRenderAManifestLayerDigestBearer proves that a
// valid digest selected by an authenticated manifest stays out of the blob
// request's public operation path and the final integrity error. A separate
// client can still redeem the exact value as the Bearer token.
func TestPublicPullErrorDoesNotRenderAManifestLayerDigestBearer(t *testing.T) {
	token := "sha256:" + strings.Repeat("a", 64)
	body := peerManifest(t, token, 1, "1")
	server, ref, blobPath := newBearerManifestRegistry(t, token, body, "x")

	client := newClient(
		t,
		bigoci.WithPlainHTTP(),
		bigoci.WithCredentials(reflectedUsername, reflectedPassword),
	)
	err := client.Pull(t.Context(), ref, bigoci.ToFile(newPath(t, destName)), bigoci.WithWorkers(1))

	require.ErrorIs(t, err, bigoci.ErrDigestMismatch)
	assert.Contains(t, <-blobPath, token, "the manifest-selected digest was used for the actual blob request")
	assert.NotContains(t, err.Error(), token, "neither the blob path nor integrity error reflects the bearer")
	assertAuthorizationReplays(t, server, "Bearer "+token)
}

// TestPublicPullErrorDoesNotRenderAManifestSizeBearer proves the corresponding
// numeric channel. The registry issues a decimal Bearer and places the same
// digits in the part size fields; structural validation rejects the mismatch
// without copying those digits into the public error.
func TestPublicPullErrorDoesNotRenderAManifestSizeBearer(t *testing.T) {
	const token = "7319458260184726931"

	body := peerManifest(t, digest.FromString("one part").String(), mustInt64(t, token), token)
	server, ref, _ := newBearerManifestRegistry(t, token, body, "")

	client := newClient(
		t,
		bigoci.WithPlainHTTP(),
		bigoci.WithCredentials(reflectedUsername, reflectedPassword),
	)
	err := client.Pull(t.Context(), ref, bigoci.ToFile(newPath(t, destName)), bigoci.WithWorkers(1))

	require.Error(t, err)
	assert.NotContains(t, err.Error(), token, "manifest-selected decimal fields are never rendered")
	assertAuthorizationReplays(t, server, "Bearer "+token)
}

// TestPublicPullBoundManifestMismatchDoesNotRenderBearerDigest proves the
// digest-bound manifest check itself is structural. The token is the digest
// of the registry's response body, so rendering the computed digest would
// disclose a still-redeemable credential.
func TestPublicPullBoundManifestMismatchDoesNotRenderBearerDigest(t *testing.T) {
	responseBody := []byte("different manifest body")
	token := digest.FromBytes(responseBody).String()
	wanted := digest.FromString("the requested manifest body").String()
	server, _, _ := newBearerManifestRegistry(t, token, responseBody, "")
	ref := bigoci.Reference(
		strings.TrimPrefix(server.URL, "http://") + "/" + repoName + "@" + wanted,
	)

	client := newClient(
		t,
		bigoci.WithPlainHTTP(),
		bigoci.WithCredentials(reflectedUsername, reflectedPassword),
	)
	err := client.Pull(t.Context(), ref, bigoci.ToFile(newPath(t, destName)), bigoci.WithWorkers(1))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest content does not match the requested digest")
	assert.NotContains(t, err.Error(), token)
	assertAuthorizationReplays(t, server, "Bearer "+token)
}

// peerManifest returns a one-layer bigoci manifest with the peer-controlled
// digest and size fields the consequence tests need.
func peerManifest(t *testing.T, partDigest string, partSize int64, splitSize string) []byte {
	t.Helper()

	document := map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageManifest,
		"artifactType":  manifest.ArtifactType,
		"config": map[string]any{
			"mediaType": ocispec.DescriptorEmptyJSON.MediaType,
			"digest":    ocispec.DescriptorEmptyJSON.Digest.String(),
			"size":      ocispec.DescriptorEmptyJSON.Size,
		},
		"layers": []any{map[string]any{
			"mediaType": manifest.MediaTypePart,
			"digest":    partDigest,
			"size":      partSize,
		}},
		"annotations": map[string]any{
			manifest.AnnotationFileDigest: digest.FromString("one byte file").String(),
			manifest.AnnotationFileSize:   "1",
			manifest.AnnotationPartSize:   splitSize,
		},
	}

	body, err := json.Marshal(document)
	require.NoError(t, err)

	return body
}

// newBearerManifestRegistry serves an authenticated manifest and optional
// blob, plus a redeem endpoint that proves the issued token remains live.
func newBearerManifestRegistry(
	t *testing.T,
	token string,
	manifestBody []byte,
	blobBody string,
) (*httptest.Server, bigoci.Reference, <-chan string) {
	t.Helper()

	expected := "Bearer " + token
	blobPath := make(chan string, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			writeToken(w, token)
		case r.URL.Path == "/redeem":
			if r.Header.Get("Authorization") == expected {
				w.WriteHeader(http.StatusOK)

				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case r.Header.Get("Authorization") != expected:
			w.Header().Set(
				"WWW-Authenticate",
				`Bearer realm="`+server.URL+`/token",service="fixture"`,
			)
			w.WriteHeader(http.StatusUnauthorized)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			_, _ = w.Write(manifestBody)
		case strings.Contains(r.URL.Path, "/blobs/"):
			blobPath <- r.URL.Path
			_, _ = io.WriteString(w, blobBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	ref := bigoci.Reference(strings.TrimPrefix(server.URL, "http://") + "/" + repoName + ":" + tag)

	return server, ref, blobPath
}

// writeToken writes the token response a Bearer challenge exchange expects.
func writeToken(w http.ResponseWriter, token string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      token,
		"expires_in": 300,
	})
}

// assertAuthorizationReplays models a separate log reader replaying the exact
// header it recovered. HTTP 200 is the real-world consequence the error
// boundary prevents.
func assertAuthorizationReplays(t *testing.T, server *httptest.Server, authorization string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/redeem", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", authorization)

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "the reflected credential remains redeemable")
}

// mustInt64 parses one decimal fixture value and fails the test when it does
// not fit the manifest's signed 64-bit size fields.
func mustInt64(t *testing.T, value string) int64 {
	t.Helper()

	n, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err)

	return n
}
