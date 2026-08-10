package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secret is the credential every redaction test hides. It is long enough that a
// prefix of it would still be a useful thing to leak, which is what the negative
// assertions are checking for.
const secret = "eyJhbGciOiJSUzI1NiJ9.aVeryLongOpaqueRegistryToken.4uNeAqYIe0uP"

// requestFor builds a request against a blob URL, which is the shape most of the
// traffic a transfer produces has.
func requestFor(t *testing.T, target string, header http.Header) *http.Request {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)
	if header != nil {
		req.Header = header
	}

	return req
}

// renderedRequest returns the log line a request would produce, so a test can
// assert on what a reader would actually see.
func renderedRequest(t *testing.T, req *http.Request) string {
	t.Helper()

	probe := newTap(&bytes.Buffer{}, nil)

	return probe.requestLine(1, req, classify(req.Method, req.URL.Path))
}

// TestAuthSchemeKeepsCredentialsUnrepresentable checks both halves of the auth
// field: the scheme is named exactly, and nothing of the credential reaches the
// line — not the whole thing, not a prefix of it.
func TestAuthSchemeKeepsCredentialsUnrepresentable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "no header at all", header: "", want: "none"},
		{name: "bearer token", header: "Bearer " + secret, want: "bearer"},
		{name: "basic credentials", header: "Basic " + secret, want: "basic"},
		{name: "lowercase scheme", header: "bearer " + secret, want: "bearer"},
		{name: "some other scheme", header: "Negotiate " + secret, want: "other"},
		{name: "no space, so no scheme to read", header: secret, want: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			header := http.Header{}
			if tt.header != "" {
				header.Set("Authorization", tt.header)
			}
			assert.Equal(t, tt.want, authScheme(header))

			line := renderedRequest(t, requestFor(t, "https://reg.example.com/v2/team/m/blobs/sha256:ab", header))
			assert.Contains(t, line, "auth="+tt.want)
			assert.NotContains(t, line, secret)
			assert.NotContains(t, line, secret[:8], "not even a prefix of a credential may be rendered")
		})
	}
}

// TestRequestLineIsDefaultDeny checks that the allow-list is the whole story: a
// header nobody named is not shortened or masked, it is absent.
func TestRequestLineIsDefaultDeny(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set("X-Secret", "hunter2")
	header.Set("Cookie", "session=abcdef123456")
	header.Set("Authorization", "Bearer "+secret)
	header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")

	line := renderedRequest(t, requestFor(t, "https://reg.example.com/v2/team/m/manifests/v1", header))

	assert.Contains(t, line, `accept="application/vnd.oci.image.manifest.v1+json"`)
	assert.NotContains(t, line, "hunter2")
	assert.NotContains(t, line, "X-Secret")
	assert.NotContains(t, line, "Cookie")
	assert.NotContains(t, line, "session")
}

// TestRedactURL checks every rule the URL redaction claims, one case each.
func TestRedactURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		target      string
		want        string
		notContains []string
	}{
		{
			name:   "userinfo is dropped",
			target: "https://robot:" + secret + "@reg.example.com/v2/team/m/blobs/uploads/9f",
			want:   "https://reg.example.com/v2/team/m/blobs/uploads/9f",
		},
		{
			name:   "query values are elided, names kept and sorted",
			target: "https://reg.example.com/v2/team/m/blobs/uploads/9f?zeta=z&state=" + secret + "&alpha=a",
			want:   "https://reg.example.com/v2/team/m/blobs/uploads/9f?alpha=…&state=…&zeta=…",
		},
		{
			name: "the digest parameter keeps its value",
			target: "https://reg.example.com/v2/team/m/blobs/uploads/9f?state=" + secret +
				"&digest=sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			want: "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=sha256:" +
				"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08&state=…",
		},
		{
			name:   "a digest parameter whose value is not a digest",
			target: "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=notadigest",
			want:   "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=…",
		},
		{
			name:   "a digest parameter carrying something else entirely",
			target: "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=sha256:SHOUTING",
			want:   "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=…",
		},
		{
			name: "sixty-four hex bytes, but not lowercase",
			target: "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=sha256:" +
				"9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08",
			want: "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=…",
		},
		{
			name:   "a digest parameter carrying a credential under a trusted name",
			target: "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=" + secret,
			want:   "https://reg.example.com/v2/team/m/blobs/uploads/9f?digest=…",
		},
		{
			name:   "a query Go cannot parse is summarized whole",
			target: "https://reg.example.com/v2/team/m/blobs/uploads/9f?token=ab%2",
			want:   "https://reg.example.com/v2/team/m/blobs/uploads/9f?…",
		},
		{
			name: "a digest in the path is never shortened",
			target: "https://reg.example.com/v2/team/m/blobs/sha256:" +
				"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			want: "https://reg.example.com/v2/team/m/blobs/sha256:" +
				"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		},
		{
			name:   "nothing to redact",
			target: "http://127.0.0.1:5000/v2/",
			want:   "http://127.0.0.1:5000/v2/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := url.Parse(tt.target)
			require.NoError(t, err)

			got := redactURL(parsed)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, secret)
			assert.NotContains(t, got, secret[:8])
		})
	}
}

// TestRedactURLOfNothing checks the nil case, which a log line reaches only if a
// request was built without a URL.
func TestRedactURLOfNothing(t *testing.T) {
	t.Parallel()

	assert.Empty(t, redactURL(nil))
}

// TestRedactURLReescapesParameterNames is the injection check. A parameter name is
// the peer's to choose, and it lands verbatim in a line whose whole meaning is
// carried by spaces and newlines, so a name that decodes to a newline and a
// forged line prefix has to come back escaped.
func TestRedactURLReescapesParameterNames(t *testing.T) {
	t.Parallel()

	// The name decodes to "\nhttp< forged": a newline, then the prefix of a
	// response line, then a space that would split the next field.
	const target = "https://reg.example.com/v2/team/m/blobs/uploads/9f?%0Ahttp%3C+forged=1"

	parsed, err := url.Parse(target)
	require.NoError(t, err)

	got := redactURL(parsed)
	assert.Equal(t, "https://reg.example.com/v2/team/m/blobs/uploads/9f?%0Ahttp%3C+forged=…", got)

	_, query, found := strings.Cut(got, "?")
	require.True(t, found)
	assert.NotContains(t, query, "\n")
	assert.NotContains(t, query, " ")

	reparsed, err := url.Parse(got)
	require.NoError(t, err, "a redacted URL must still be a URL")
	assert.Equal(t, got, reparsed.String())

	line := renderedRequest(t, requestFor(t, target, nil))
	assert.Equal(t, 1, strings.Count(line, "\n"), "one request must stay one line")
	assert.NotContains(t, line, "http< ", "a forged line prefix must not survive into the log")
}

// TestIsDigest checks the gate on the one query value that passes through. The
// check is on the value and never on the parameter's name, because the name is
// the peer's to choose.
func TestIsDigest(t *testing.T) {
	t.Parallel()

	const hex = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "a sha256 digest", value: "sha256:" + hex, want: true},
		{name: "empty", value: "", want: false},
		{name: "no algorithm", value: hex, want: false},
		{name: "another algorithm", value: "sha512:" + hex, want: false},
		{name: "uppercase hex", value: "sha256:" + strings.ToUpper(hex), want: false},
		{name: "one byte short", value: "sha256:" + hex[:len(hex)-1], want: false},
		{name: "one byte long", value: "sha256:" + hex + "a", want: false},
		{name: "the right length, but not hex", value: "sha256:" + strings.Repeat("z", 64), want: false},
		{name: "a credential", value: secret, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, isDigest(tt.value))
		})
	}
}

// TestRenderLocation checks that every server-issued target is resolved,
// replaced, and remembered for the request that follows it.
func TestRenderLocation(t *testing.T) {
	t.Parallel()

	base, err := url.Parse("https://reg.example.com/v2/team/m/blobs/uploads/")
	require.NoError(t, err)

	tests := []struct {
		name     string
		location string
		want     string
	}{
		{name: "absent", location: "", want: ""},
		{
			name:     "relative to the request",
			location: "/v2/team/m/blobs/uploads/9f?state=" + secret,
			want:     "https://reg.example.com" + redactedTargetPath,
		},
		{
			name:     "relative to the request path",
			location: "9f?state=" + secret,
			want:     "https://reg.example.com" + redactedTargetPath,
		},
		{
			name:     "another host entirely",
			location: "https://" + secret + ".example/parts/" + secret + "?token=" + secret,
			want:     offOriginTarget,
		},
		{
			name:     "same-registry token endpoint",
			location: "https://reg.example.com/token/" + secret + "?digest=" + secret,
			want:     "https://reg.example.com" + redactedTargetPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probe := newTap(&bytes.Buffer{}, nil)
			_ = probe.renderTargetURL(base, classBlobWrite)
			got := probe.renderLocation(base, tt.location)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, secret)
			if tt.location == "" {
				return
			}

			target, err := url.Parse(tt.location)
			require.NoError(t, err)
			target = base.ResolveReference(target)
			query := target.Query()
			query.Set("digest", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08")
			target.RawQuery = query.Encode()
			kind := locationClass(target.Path)
			assert.Equal(t, tt.want, probe.renderTargetURL(target, kind))
			req := requestFor(t, target.String(), nil)
			failed := probe.errorLine(2, req, kind, 0, fmt.Errorf("request %s failed", target))
			assert.Contains(t, failed, tt.want)
			assert.Contains(t, failed, `err="`+redactedTargetError+`"`)
			assert.NotContains(t, failed, secret)
		})
	}
}

// TestLocationTrackingRetainsFixedSizeIdentities checks both the structural
// bound and the live heap after many maximum-practical peer Location paths.
func TestLocationTrackingRetainsFixedSizeIdentities(t *testing.T) {
	const (
		locationCount = 512
		locationSize  = 256 << 10
		maxLiveGrowth = 8 << 20
	)

	base, err := url.Parse("https://reg.example.com/v2/team/m/blobs/sha256:ab")
	require.NoError(t, err)
	probe := newTap(&bytes.Buffer{}, nil)
	_ = probe.renderTargetURL(base, classBlobRead)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	recordLargeLocationTargets(probe, base, locationCount, locationSize)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	probe.locationsMu.RLock()
	retained := len(probe.locationTargets)
	probe.locationsMu.RUnlock()
	runtime.KeepAlive(probe)

	assert.Len(t, locationTarget{}, sha256.Size, "one map key must remain fixed-size")
	assert.Equal(t, locationCount, retained)
	assert.Less(t, int64(after.HeapAlloc)-int64(before.HeapAlloc), int64(maxLiveGrowth),
		"Location tracking retained peer path bytes")
}

// recordLargeLocationTargets feeds count distinct Location paths of size bytes
// into probe and returns without retaining its generated strings.
func recordLargeLocationTargets(probe *tap, base *url.URL, count, size int) {
	padding := strings.Repeat("x", size)
	for i := range count {
		probe.renderLocation(base, fmt.Sprintf("https://reg.example.com/capability/%d/%s", i, padding))
	}
}

// TestQuoteOrAbsent checks the difference between a header that was not there and
// a header whose value happened to look like the marker for one.
func TestQuoteOrAbsent(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "-", quoteOrAbsent(""))
	assert.Equal(t, `"-"`, quoteOrAbsent("-"))
	assert.Equal(t, `"bytes 0-4/5"`, quoteOrAbsent("bytes 0-4/5"))
	assert.Equal(t, `"one\nline"`, quoteOrAbsent("one\nline"))
}

// TestChallengeFieldReportsOnlyPresence checks that the log distinguishes an
// absent challenge from any present challenge without rendering its contents.
func TestChallengeFieldReportsOnlyPresence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "absent", want: absent},
		{name: "present but empty", values: []string{""}, want: `"present"`},
		{
			name:   "realm with a redeemable ticket",
			values: []string{`Bearer realm="https://auth.example/token?ticket=` + secret + `"`},
			want:   `"present"`,
		},
		{
			name:   "multiple challenge lines",
			values: []string{`Basic realm="one"`, `Bearer realm="two"`},
			want:   `"present"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			headers := make(http.Header)
			for _, value := range tt.values {
				headers.Add("Www-Authenticate", value)
			}

			got := challengeField(headers)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, secret)
		})
	}
}

// TestResponseHeaderFieldsRevealPresenceOnly pins the default-deny boundary
// for every peer response field. Any ordinary header can reflect a request
// credential, so only Location's separately redacted placeholder carries a
// value.
func TestResponseHeaderFieldsRevealPresenceOnly(t *testing.T) {
	t.Parallel()

	base, err := url.Parse("https://reg.example.com/v2/team/m/blobs/sha256:ab")
	require.NoError(t, err)
	headers := http.Header{
		"Content-Type":          []string{"Bearer " + secret},
		"Content-Range":         []string{secret},
		"Location":              []string{"/v2/team/m/blobs/" + secret},
		"Docker-Content-Digest": []string{secret},
		"Retry-After":           []string{secret},
		"Www-Authenticate":      []string{secret},
	}
	probe := newTap(&bytes.Buffer{}, nil)
	_ = probe.renderTargetURL(base, classBlobRead)

	got := probe.responseHeaderFields(base, headers)
	assert.Equal(t,
		`ctype="present" crange="present" loc="https://reg.example.com/_redacted" `+
			`ddigest="present" retry-after="present" challenge="present"`,
		got,
	)
	assert.NotContains(t, got, secret)
}
