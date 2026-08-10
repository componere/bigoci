package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci"
)

// TestDebugLogDoesNotExposeRedeemableRealmTickets proves that bearer realm
// credentials in a path or digest-shaped query remain absent from the complete
// debug log, even when the realm masquerades as a distribution path and the
// library successfully redeems it.
func TestDebugLogDoesNotExposeRedeemableRealmTickets(t *testing.T) {
	t.Parallel()

	const (
		pathTicket   = "debug-log-readable-realm-path-ticket"
		digestTicket = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		issuedToken  = "token-issued-to-ticket-holder"
	)

	tests := []struct {
		name      string
		tickets   []string
		wantClass class
		realm     func(string) string
		redeems   func(*http.Request) bool
	}{
		{
			name:      "ticket in the realm path",
			tickets:   []string{pathTicket},
			wantClass: classOther,
			realm:     func(base string) string { return base + "/token/" + pathTicket },
			redeems: func(r *http.Request) bool {
				return r.URL.Path == "/token/"+pathTicket
			},
		},
		{
			name:      "digest-shaped ticket in the realm query",
			tickets:   []string{digestTicket},
			wantClass: classOther,
			realm:     func(base string) string { return base + "/token?digest=" + digestTicket },
			redeems: func(r *http.Request) bool {
				return r.URL.Path == "/token" && r.URL.Query().Get("digest") == digestTicket
			},
		},
		{
			name:      "distribution-shaped realm with path and digest tickets",
			tickets:   []string{pathTicket, digestTicket},
			wantClass: classBlobRead,
			realm: func(base string) string {
				return base + "/v2/team/artifact/blobs/" + pathTicket + "?digest=" + digestTicket
			},
			redeems: func(r *http.Request) bool {
				return r.URL.Path == "/v2/team/artifact/blobs/"+pathTicket &&
					r.URL.Query().Get("digest") == digestTicket
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var redemptions, authenticated atomic.Int64
			var registry *httptest.Server
			registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.redeems(r) {
					redemptions.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"token":%q,"expires_in":3600}`, issuedToken)

					return
				}

				if r.Header.Get("Authorization") == "Bearer "+issuedToken {
					authenticated.Add(1)
					w.WriteHeader(http.StatusNotFound)

					return
				}

				w.Header().Set(
					"WWW-Authenticate",
					fmt.Sprintf(`Bearer realm=%q,service="registry"`, tt.realm(registry.URL)),
				)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(registry.Close)

			var debug bytes.Buffer
			probe := newTap(&debug, isolatedTransport(t))
			client, err := bigoci.New(
				bigoci.WithPlainHTTP(),
				bigoci.WithHTTPClient(&http.Client{Transport: probe}),
			)
			require.NoError(t, err)

			ref := bigoci.Reference(strings.TrimPrefix(registry.URL, "http://") + "/team/artifact:v1")
			err = client.Pull(t.Context(), ref, bigoci.ToFile(filepath.Join(t.TempDir(), "out.bin")))
			require.ErrorIs(t, err, bigoci.ErrNotFound)
			assert.EqualValues(t, 1, redemptions.Load(), "the library did not redeem the challenge ticket")
			assert.EqualValues(t, 1, authenticated.Load(), "the bearer did not authenticate a registry request")

			// A second client proves the realm value remains independently
			// redeemable. The protection is that a log reader never receives it.
			resp, err := registry.Client().Get(tt.realm(registry.URL))
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })
			stolen, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, string(stolen), issuedToken)
			assert.EqualValues(t, 2, redemptions.Load())

			log := debug.String()
			assert.Contains(t, log, `challenge="present"`)
			assert.Contains(t, log, registry.URL+redactedTargetPath+" class="+string(tt.wantClass))
			for _, ticket := range tt.tickets {
				assert.NotContains(t, log, ticket)
			}
			assert.NotContains(t, log, issuedToken)
		})
	}
}

// TestDebugLogDoesNotExposeADigestShapedBearerFromAManifest proves that a
// syntactically valid layer digest is not automatically public. The registry
// issues that exact value as a reusable bearer, names it in an authenticated
// manifest, and receives a real blob request without the value reaching debug.
func TestDebugLogDoesNotExposeADigestShapedBearerFromAManifest(t *testing.T) {
	t.Parallel()

	const token = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	backend := newFakeRegistry(t)
	source, _ := fixture(t)
	setupClient, err := bigoci.New(
		bigoci.WithPlainHTTP(),
		bigoci.WithHTTPClient(&http.Client{Transport: isolatedTransport(t)}),
	)
	require.NoError(t, err)
	_, err = setupClient.Push(
		t.Context(), bigoci.Reference(backend.taggedRef(fakeTag)), bigoci.FromFile(source),
		bigoci.WithPartSize(1<<20), bigoci.WithWorkers(1),
	)
	require.NoError(t, err)
	stored := backend.manifestAt(t, fakeTag)

	var document map[string]any
	require.NoError(t, json.Unmarshal(stored.body, &document))
	layers, ok := document["layers"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, layers)
	first, ok := layers[0].(map[string]any)
	require.True(t, ok)
	originalDigest, ok := first["digest"].(string)
	require.True(t, ok)
	first["digest"] = token
	manifestBody, err := json.Marshal(document)
	require.NoError(t, err)
	blob, ok := backend.blob(originalDigest)
	require.True(t, ok)

	var authenticated, replayed atomic.Int64
	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/v2/" + fakeRepo
		switch {
		case r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":%q,"expires_in":3600}`, token)
		case r.URL.Path == "/redeem":
			if r.Header.Get("Authorization") == "Bearer "+token {
				replayed.Add(1)
				w.WriteHeader(http.StatusOK)

				return
			}
			w.WriteHeader(http.StatusForbidden)
		case r.Header.Get("Authorization") == "":
			w.Header().Set(
				"WWW-Authenticate",
				fmt.Sprintf(`Bearer realm=%q,service="registry"`, registry.URL+"/token"),
			)
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasPrefix(r.URL.Path, prefix+"/manifests/"):
			authenticated.Add(1)
			w.Header().Set("Content-Type", stored.ctype)
			_, _ = w.Write(manifestBody)
		case r.URL.Path == prefix+"/blobs/"+token:
			authenticated.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(registry.Close)

	var debug bytes.Buffer
	probe := newTap(&debug, isolatedTransport(t))
	client, err := bigoci.New(
		bigoci.WithPlainHTTP(),
		bigoci.WithHTTPClient(&http.Client{Transport: probe}),
	)
	require.NoError(t, err)

	ref := bigoci.Reference(strings.TrimPrefix(registry.URL, "http://") + "/" + fakeRepo + ":" + fakeTag)
	err = client.Pull(
		t.Context(), ref, bigoci.ToFile(filepath.Join(t.TempDir(), "out.bin")), bigoci.WithWorkers(1),
	)
	require.Error(t, err)
	require.GreaterOrEqual(
		t,
		authenticated.Load(),
		int64(2),
		"the registry never received an authenticated blob request",
	)

	log := debug.String()
	assert.Contains(t, log, registry.URL+"/v2/"+fakeRepo+"/manifests/"+redactedReferenceSegment)
	assert.Contains(t, log, registry.URL+"/v2/"+fakeRepo+"/blobs/"+redactedDigestSegment+
		" class=blob-read auth=bearer")
	assert.NotContains(t, log, token)

	replay, err := http.NewRequestWithContext(t.Context(), http.MethodGet, registry.URL+"/redeem", nil)
	require.NoError(t, err)
	replay.Header.Set("Authorization", "Bearer "+token)
	replayResponse, err := registry.Client().Do(replay)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replayResponse.Body.Close() })
	assert.Equal(t, http.StatusOK, replayResponse.StatusCode)
	assert.EqualValues(t, 1, replayed.Load(), "the separately copied bearer was not reusable")
}

// TestDebugLogDoesNotExposeAReflectedBearer proves that a registry which has
// authenticated a request cannot copy that bearer into either raw transport
// detail or an otherwise allow-listed response header in the debug log.
func TestDebugLogDoesNotExposeAReflectedBearer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		token   string
		want    string
		respond func(*testing.T, http.ResponseWriter, *http.Request)
	}{
		{
			name:  "malformed response transport error",
			token: "registry-minted-bearer-reflected-into-malformed-header",
			want:  `http! `,
			respond: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()

				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Error("server response writer cannot hijack")

					return
				}
				conn, rw, err := hijacker.Hijack()
				if err != nil {
					t.Errorf("hijack response: %v", err)

					return
				}
				defer conn.Close()

				_, _ = rw.WriteString(
					"HTTP/1.1 200 OK\r\n" + r.Header.Get("Authorization") + "\r\nConnection: close\r\n\r\n",
				)
				_ = rw.Flush()
			},
		},
		{
			name:  "ordinary response header",
			token: "registry-minted-bearer-reflected-into-content-type",
			want:  `ctype="present"`,
			respond: func(_ *testing.T, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name:  "numeric response content length",
			token: "731984620517284693",
			want:  `clen=-2`,
			respond: func(_ *testing.T, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "731984620517284693")
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var authenticated atomic.Int64
			var registry *httptest.Server
			registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/token":
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"token":%q,"expires_in":3600}`, tt.token)
				case r.Header.Get("Authorization") == "":
					w.Header().Set(
						"WWW-Authenticate",
						fmt.Sprintf(`Bearer realm=%q,service="registry"`, registry.URL+"/token"),
					)
					w.WriteHeader(http.StatusUnauthorized)
				case r.Header.Get("Authorization") == "Bearer "+tt.token:
					authenticated.Add(1)
					tt.respond(t, w, r)
				default:
					w.WriteHeader(http.StatusForbidden)
				}
			}))
			t.Cleanup(registry.Close)

			var debug bytes.Buffer
			probe := newTap(&debug, isolatedTransport(t))
			client, err := bigoci.New(
				bigoci.WithPlainHTTP(),
				bigoci.WithHTTPClient(&http.Client{Transport: probe}),
			)
			require.NoError(t, err)

			ref := bigoci.Reference(strings.TrimPrefix(registry.URL, "http://") + "/team/artifact:v1")
			err = client.Pull(t.Context(), ref, bigoci.ToFile(filepath.Join(t.TempDir(), "out.bin")))
			require.Error(t, err)
			require.Positive(t, authenticated.Load(), "the registry never received the issued bearer")

			log := debug.String()
			assert.Contains(t, log, tt.want)
			assert.NotContains(t, log, tt.token)
			if strings.Contains(log, "http! ") {
				assert.Contains(t, log, `err="`+redactedTargetError+`"`)
			}
		})
	}
}

// TestDebugLogDoesNotExposeRedeemableUploadSession proves that a same-origin
// upload Location remains absent from loc= and the completing PUT even though a
// separate client can replay the capability and make the server accept bytes.
func TestDebugLogDoesNotExposeRedeemableUploadSession(t *testing.T) {
	t.Parallel()

	const (
		sessionTicket = "unguessable-redeemable-upload-session"
		queryTicket   = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	)

	prefix := "/v2/team/artifact"
	sessionPath := prefix + "/blobs/uploads/" + sessionTicket
	var accepted atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("HEAD "+prefix+"/blobs/{digest}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST "+prefix+"/blobs/uploads/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", sessionPath+"?digest="+queryTicket)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("PUT "+sessionPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		accepted.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("PUT "+prefix+"/manifests/{reference}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	})

	registry := httptest.NewServer(mux)
	t.Cleanup(registry.Close)
	source, _ := fixture(t)

	var debug bytes.Buffer
	probe := newTap(&debug, isolatedTransport(t))
	client, err := bigoci.New(
		bigoci.WithPlainHTTP(),
		bigoci.WithHTTPClient(&http.Client{Transport: probe}),
	)
	require.NoError(t, err)

	ref := bigoci.Reference(strings.TrimPrefix(registry.URL, "http://") + "/team/artifact:v1")
	_, err = client.Push(
		t.Context(), ref, bigoci.FromFile(source),
		bigoci.WithPartSize(1<<20), bigoci.WithWorkers(1),
	)
	require.NoError(t, err)
	acceptedBefore := accepted.Load()
	require.Positive(t, acceptedBefore, "the real push never redeemed the upload session")

	log := debug.String()
	assert.Contains(t, log, registry.URL+prefix+"/blobs/uploads/ class=upload-open")
	assert.Contains(t, log, `loc="`+registry.URL+redactedTargetPath+`"`)
	assert.Contains(t, log, registry.URL+redactedTargetPath+" class=blob-write")
	assert.Contains(t, log, registry.URL+prefix+"/blobs/"+redactedDigestSegment,
		"the blob endpoint and repository stay useful")
	assert.Contains(t, log, registry.URL+prefix+"/manifests/"+redactedReferenceSegment,
		"the manifest endpoint and repository stay useful")
	assert.NotContains(t, log, sessionTicket)
	assert.NotContains(t, log, queryTicket)

	replay, err := http.NewRequestWithContext(
		t.Context(), http.MethodPut, registry.URL+sessionPath+"?digest="+queryTicket,
		strings.NewReader("attacker-controlled upload"),
	)
	require.NoError(t, err)
	replayResponse, err := registry.Client().Do(replay)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replayResponse.Body.Close() })
	assert.Equal(t, http.StatusCreated, replayResponse.StatusCode)
	assert.Equal(t, acceptedBefore+1, accepted.Load(), "the separately replayed session was not accepted")
}

// TestDebugLogDoesNotExposeRedeemableSameOriginRedirect proves the same rule
// for pulls: a same-origin blob-shaped Location remains absent from loc= and
// the followed GET while a separate client can still replay it successfully.
func TestDebugLogDoesNotExposeRedeemableSameOriginRedirect(t *testing.T) {
	t.Parallel()

	const (
		locationTicket = "unguessable-redeemable-pull-location"
		queryTicket    = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	)

	setup := newFakeRegistry(t)
	source, content := fixture(t)
	setupClient, err := bigoci.New(
		bigoci.WithPlainHTTP(),
		bigoci.WithHTTPClient(&http.Client{Transport: isolatedTransport(t)}),
	)
	require.NoError(t, err)
	_, err = setupClient.Push(
		t.Context(), bigoci.Reference(setup.taggedRef(fakeTag)), bigoci.FromFile(source),
		bigoci.WithPartSize(1<<20), bigoci.WithWorkers(1),
	)
	require.NoError(t, err)
	manifest := setup.manifestAt(t, fakeTag)

	prefix := "/v2/" + fakeRepo
	locationPath := prefix + "/blobs/" + locationTicket
	var accepted atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"/manifests/{reference}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", manifest.ctype)
		_, _ = w.Write(manifest.body)
	})
	mux.HandleFunc("GET "+prefix+"/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("digest") == locationTicket {
			accepted.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)

			return
		}

		w.Header().Set("Location", locationPath+"?digest="+queryTicket)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})

	registry := httptest.NewServer(mux)
	t.Cleanup(registry.Close)
	var debug bytes.Buffer
	probe := newTap(&debug, isolatedTransport(t))
	client, err := bigoci.New(
		bigoci.WithPlainHTTP(),
		bigoci.WithHTTPClient(&http.Client{Transport: probe}),
	)
	require.NoError(t, err)

	destination := filepath.Join(t.TempDir(), "pulled.bin")
	ref := bigoci.Reference(strings.TrimPrefix(registry.URL, "http://") + "/" + fakeRepo + ":" + fakeTag)
	err = client.Pull(t.Context(), ref, bigoci.ToFile(destination), bigoci.WithWorkers(1))
	require.NoError(t, err)
	pulled, err := os.ReadFile(destination)
	require.NoError(t, err)
	assert.Equal(t, content, pulled)
	acceptedBefore := accepted.Load()
	require.EqualValues(t, 1, acceptedBefore, "the real pull never redeemed the Location")

	log := debug.String()
	assert.Contains(t, log, registry.URL+prefix+"/blobs/"+redactedDigestSegment,
		"the direct blob endpoint and repository stay useful")
	assert.NotContains(t, log, digestOf(content))
	assert.Contains(t, log, `loc="`+registry.URL+redactedTargetPath+`"`)
	assert.Contains(t, log, registry.URL+redactedTargetPath+" class=blob-read")
	assert.NotContains(t, log, locationTicket)
	assert.NotContains(t, log, queryTicket)

	replayResponse, err := registry.Client().Get(registry.URL + locationPath + "?digest=" + queryTicket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replayResponse.Body.Close() })
	replayed, err := io.ReadAll(replayResponse.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, replayResponse.StatusCode)
	assert.Equal(t, content, replayed)
	assert.Equal(t, acceptedBefore+1, accepted.Load(), "the separately replayed Location was not accepted")
}

// TestPreflightOperandsCannotForgeARecordOrControlATerminal proves that every
// raw push/pull operand is made terminal-safe before a rejected transfer is
// described, including invalid UTF-8 that can occur in an argument byte string.
func TestPreflightOperandsCannotForgeARecordOrControlATerminal(t *testing.T) {
	t.Parallel()

	const hostile = "\nhttp> 9999 +0.000s GET forged class=blob-read auth=bearer" +
		"\r\x1b]52;c;YXVkaXQtY2xpcGJvYXJkLXBvaXNvbg==\x07\u202e"
	hostileRef := "reg.example/team/model:v1" + hostile + string([]byte{0xff})
	source := filepath.Join(t.TempDir(), "source.bin")
	require.NoError(t, os.WriteFile(source, []byte("source"), 0o600))
	destination := filepath.Join(t.TempDir(), "destination"+hostile+string([]byte{0xfe}))

	tests := []struct {
		name string
		args []string
	}{
		{name: "push", args: []string{cmdPush, "-debug", source, hostileRef}},
		{name: "pull", args: []string{cmdPull, "-debug", hostileRef, destination}},
	}
	if runtime.GOOS != "windows" {
		// Windows cannot represent control bytes in a filename, so only Unix can
		// exercise a hostile source operand through the real os.Stat preflight.
		hostileSource := filepath.Join(t.TempDir(), "source"+hostile)
		require.NoError(t, os.WriteFile(hostileSource, []byte("source"), 0o600))
		tests = append(tests, struct {
			name string
			args []string
		}{name: "push source", args: []string{cmdPush, "-debug", hostileSource, "not a valid reference"}})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := runCLI(t, tt.args...)
			assert.Equal(t, exitFailure, got.code)
			assert.Empty(t, got.stdout)
			assert.Contains(t, got.stderr,
				`\nhttp> 9999 +0.000s GET forged class=blob-read auth=bearer\r`+
					`\x1b]52;c;YXVkaXQtY2xpcGJvYXJkLXBvaXNvbg==\a\u202e`,
			)
			assert.Zero(t, strings.Count(got.stderr, "\nhttp> "), "the operand forged a physical request record")
			for _, control := range []string{
				"\r", "\x1b", "\x07", "\u202e", string([]byte{0xff}), string([]byte{0xfe}),
			} {
				assert.NotContains(t, got.stderr, control)
			}
		})
	}
}

// TestPeerErrorCannotForgeADebugRecordOrControlATerminal proves that a hostile
// registry body is omitted from the public error and cannot emit a record or
// OSC 52. It stays serial because runCLI uses the real CLI's default transport.
func TestPeerErrorCannotForgeADebugRecordOrControlATerminal(t *testing.T) {
	const hostile = "registry failure\nhttp< 9999 +0.000s GET forged class=blob-read status=200" +
		"\rreturn\x00\t\x1b]52;c;YXVkaXQtY2xpcGJvYXJkLXBvaXNvbg==\x07\x7f" +
		"\u0085\u009b\u2028\u2029\u200btail"

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, hostile)
	}))
	t.Cleanup(registry.Close)

	ref := strings.TrimPrefix(registry.URL, "http://") + "/team/artifact:v1"
	got := runCLI(t, "pull", "-debug", "-plain-http", ref, filepath.Join(t.TempDir(), "out.bin"))

	assert.Equal(t, exitFailure, got.code)
	assert.Empty(t, got.stdout)
	assert.NotContains(t, got.stderr, "registry failure")
	assert.NotContains(t, got.stderr, "YXVkaXQtY2xpcGJvYXJkLXBvaXNvbg==")
	assert.NotContains(t, got.stderr, "tail")
	assert.NotContains(t, got.stderr, "\nhttp< 9999")
	assert.Equal(t, 1, strings.Count(got.stderr, "\nhttp< "), "the peer forged another received record")

	for _, control := range []string{
		"\r", "\x00", "\t", "\x1b", "\x07", "\x7f", "\u0085", "\u009b", "\u2028", "\u2029", "\u200b",
	} {
		assert.NotContains(t, got.stderr, control)
	}
}

// TestTerminalSafeLineEscapesOnlyNonGraphicRunes fixes the presentation
// contract for printable text and each control family a peer could supply.
func TestTerminalSafeLineEscapesOnlyNonGraphicRunes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "printable text", value: "registry says café — retry", want: "registry says café — retry"},
		{name: "line controls", value: "a\r\nb", want: `a\r\nb`},
		{name: "C0 and delete", value: "\x00\t\x07\x1b\x7f", want: `\x00\t\a\x1b\x7f`},
		{name: "C1 controls", value: "\u0085\u009b", want: `\u0085\u009b`},
		{name: "C1 OSC", value: "\u009d52;c;payload\u009c", want: `\u009d52;c;payload\u009c`},
		{name: "Unicode separators", value: "\u2028\u2029", want: `\u2028\u2029`},
		{
			name:  "bidi and format controls",
			value: "\u202eabc\u2066def\u2069\u200b",
			want:  `\u202eabc\u2066def\u2069\u200b`,
		},
		{name: "invalid UTF-8", value: string([]byte{'a', 0xff, 0xfe, 'b'}), want: `a\xff\xfeb`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, terminalSafeLine(tt.value))
		})
	}
}

// TestReportErrorSanitizesPresentationWithoutChangingClassification proves the
// renderer does not replace the original error used for sentinel matching.
func TestReportErrorSanitizesPresentationWithoutChangingClassification(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("peer says\nforged\x1b: %w", bigoci.ErrUnauthorized)
	var stderr bytes.Buffer
	code := reportError(env{stderr: &stderr}, err, nil)

	assert.Equal(t, exitUnauthorized, code)
	assert.Contains(t, stderr.String(), `bigoci: peer says\nforged\x1b: unauthorized`)
	assert.Contains(t, stderr.String(), "matched sentinel bigoci.ErrUnauthorized (exit 6)")
	assert.NotContains(t, stderr.String(), "\nforged")
}
