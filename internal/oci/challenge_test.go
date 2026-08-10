package oci

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ghcrChallenge is the challenge GHCR answers an unauthenticated repository
// request with, copied from the wire. It is the first row of the table
// because it is the first challenge bigoci will ever meet, and because the
// comma inside its quoted scope is what rules out splitting the header on
// commas.
const ghcrChallenge = `Bearer realm="https://ghcr.io/token",service="ghcr.io",` +
	`scope="repository:team/artifact:pull,push"`

// TestParseChallenge walks the header shapes RFC 9110 allows and the ones
// hostile peers send. The comma-inside-a-quoted-scope row leads because it
// is the shape every real bearer challenge has and the one a naive
// comma-split breaks on.
func TestParseChallenge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		header      string
		wantScheme  string
		wantRealm   string
		wantService string
		wantScopes  []string
		wantErr     bool
	}{
		{
			name:        "a comma inside a quoted scope is part of the scope",
			header:      ghcrChallenge,
			wantScheme:  schemeBearer,
			wantRealm:   "https://ghcr.io/token",
			wantService: "ghcr.io",
			wantScopes:  []string{"repository:team/artifact:pull,push"},
		},
		{
			name:       "a bearer challenge may carry nothing but a realm",
			header:     `Bearer realm="https://auth.example.com/token"`,
			wantScheme: schemeBearer,
			wantRealm:  "https://auth.example.com/token",
		},
		{
			name:       "a basic challenge is answered when it is all there is",
			header:     `Basic realm="registry"`,
			wantScheme: schemeBasic,
			wantRealm:  "registry",
		},
		{
			name:        "bearer wins wherever in the list it appears",
			header:      `Basic realm="registry", Bearer realm="https://auth.example.com/token",service="reg"`,
			wantScheme:  schemeBearer,
			wantRealm:   "https://auth.example.com/token",
			wantService: "reg",
		},
		{
			name:       "bearer wins when it comes first as well",
			header:     `Bearer realm="https://auth.example.com/token", Basic realm="registry"`,
			wantScheme: schemeBearer,
			wantRealm:  "https://auth.example.com/token",
		},
		{
			name:       "a backslash escape stands for the character after it",
			header:     `Bearer realm="https://auth.example.com/token",service="a \"quoted\" name"`,
			wantScheme: schemeBearer,
			wantRealm:  "https://auth.example.com/token",
			// The escapes are undone, so the value is what the registry meant
			// rather than what it had to spell.
			wantService: `a "quoted" name`,
		},
		{
			name:        "scheme and parameter names are matched without regard to case",
			header:      `bEaReR ReAlM="https://auth.example.com/token",SERVICE="reg",Scope="repository:a:pull"`,
			wantScheme:  schemeBearer,
			wantRealm:   "https://auth.example.com/token",
			wantService: "reg",
			wantScopes:  []string{"repository:a:pull"},
		},
		{
			name:        "parameters are read in whatever order they arrive",
			header:      `Bearer scope="repository:a:pull",service="reg",realm="https://auth.example.com/token"`,
			wantScheme:  schemeBearer,
			wantRealm:   "https://auth.example.com/token",
			wantService: "reg",
			wantScopes:  []string{"repository:a:pull"},
		},
		{
			name:       "a scope parameter carries a space separated list",
			header:     `Bearer realm="https://auth.example.com/token",scope="repository:a:pull repository:b:pull,push"`,
			wantScheme: schemeBearer,
			wantRealm:  "https://auth.example.com/token",
			wantScopes: []string{"repository:a:pull", "repository:b:pull,push"},
		},
		{
			name:        "an unquoted parameter value is a token",
			header:      `Bearer realm="https://auth.example.com/token",service=reg`,
			wantScheme:  schemeBearer,
			wantRealm:   "https://auth.example.com/token",
			wantService: "reg",
		},
		{
			name: "an unquoted value carrying characters a token cannot hold is refused",
			// A colon and a slash are not token characters, so a URL has to
			// arrive quoted. A header that spells one bare is malformed, and
			// reading it as far as "https" and calling that a realm would be
			// worse than refusing it.
			header:  `Bearer realm=https://auth.example.com/token`,
			wantErr: true,
		},
		{
			name:       "extra whitespace and empty list elements are skipped",
			header:     `  Bearer  realm = "https://auth.example.com/token" , , service = "reg"  `,
			wantScheme: schemeBearer,
			wantRealm:  "https://auth.example.com/token",
			// The whitespace around the equals signs is the grammar's, not a
			// value's.
			wantService: "reg",
		},
		{name: "an absent header is not a challenge", header: "", wantErr: true},
		{
			name:    "a bearer challenge naming no realm is refused",
			header:  `Bearer service="reg",scope="repository:a:pull"`,
			wantErr: true,
		},
		{
			name:    "a bearer challenge naming no realm is refused even beside a usable basic one",
			header:  `Bearer service="reg", Basic realm="registry"`,
			wantErr: true,
		},
		{
			name:    "a scheme this package does not implement is refused",
			header:  `Negotiate realm="https://auth.example.com/token"`,
			wantErr: true,
		},
		{
			name:    "an unterminated quoted string is refused",
			header:  `Bearer realm="https://auth.example.com/token`,
			wantErr: true,
		},
		{
			name:    "a parameter before any scheme is refused",
			header:  `realm="https://auth.example.com/token"`,
			wantErr: true,
		},
		{
			name:    "a header made of characters the grammar has no place for is refused",
			header:  `((((`,
			wantErr: true,
		},
		{
			name:    "nine kilobytes of garbage is not read at all",
			header:  strings.Repeat("x", 9<<10),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			asked, err := parseChallenge(tt.header)

			if tt.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrUnauthorized, "a challenge nobody can answer is a refusal")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantScheme, asked.scheme)
			assert.Equal(t, tt.wantRealm, asked.realm)
			assert.Equal(t, tt.wantService, asked.service)
			assert.Equal(t, tt.wantScopes, asked.scopes)
		})
	}
}

// TestParseChallengeDoesNotRepeatWhatItCouldNotRead pins that an unusable
// challenge contributes no peer-controlled bytes to a public error.
func TestParseChallengeDoesNotRepeatWhatItCouldNotRead(t *testing.T) {
	t.Parallel()

	const secret = "malformed-challenge-secret"

	_, err := parseChallenge("Negotiate " + secret + strings.Repeat("z", challengeLimit-20))

	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), "Negotiate")
}

// TestValidateRealm walks the realm shapes a challenge can name against
// the rules that decide where a credential may be sent.
func TestValidateRealm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		realm      string
		repoScheme string
		repoHost   string
		wantQuery  string
		wantErr    bool
	}{
		{
			name:       "an absolute https realm is the token endpoint",
			realm:      "https://auth.example.com/token",
			repoScheme: schemeHTTPS,
		},
		{
			name:       "a realm on a host other than the registry is allowed",
			realm:      "https://auth.docker.io/token",
			repoScheme: schemeHTTPS,
		},
		{
			name:       "a public IP literal is allowed",
			realm:      "https://8.8.8.8/token",
			repoScheme: schemeHTTPS,
		},
		{
			name:       "the registry's own loopback IP is allowed on another https port",
			realm:      "https://127.0.0.1:6000/token",
			repoScheme: schemeHTTPS,
			repoHost:   "127.0.0.1:5000",
		},
		{
			name:       "equivalent spellings of the registry's own IPv6 loopback are allowed",
			realm:      "https://[::1]:6000/token",
			repoScheme: schemeHTTPS,
			repoHost:   "[0:0:0:0:0:0:0:1]:5000",
		},
		{
			name:       "an IPv4 loopback realm on another host is refused",
			realm:      "https://127.0.0.1/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "an IPv4-mapped loopback realm on another host is refused",
			realm:      "https://[::ffff:127.0.0.1]/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "a private IPv4 realm on another host is refused",
			realm:      "https://10.0.0.1/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "a link-local IPv4 realm on another host is refused",
			realm:      "https://169.254.169.254/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "an unspecified IPv4 realm on another host is refused",
			realm:      "https://0.0.0.0/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "an IPv6 loopback realm on another host is refused",
			realm:      "https://[::1]/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "a private IPv6 realm on another host is refused",
			realm:      "https://[fd00::1]/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "a link-local IPv6 realm on another host is refused",
			realm:      "https://[fe80::1]/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "a link-local multicast realm on another host is refused",
			realm:      "https://[ff02::1]/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "an unspecified IPv6 realm on another host is refused",
			realm:      "https://[::]/token",
			repoScheme: schemeHTTPS,
			repoHost:   "registry.example.com",
			wantErr:    true,
		},
		{
			name:       "the realm's own query is kept",
			realm:      "https://auth.example.com/token?tenant=acme",
			repoScheme: schemeHTTPS,
			wantQuery:  "tenant=acme",
		},
		{
			name:       "a plain http realm is refused against an https registry",
			realm:      "http://auth.example.com/token",
			repoScheme: schemeHTTPS,
			wantErr:    true,
		},
		{
			name:       "a plain http realm on the registry's own host is accepted against a plain http registry",
			realm:      "http://127.0.0.1:5000/token",
			repoScheme: schemeHTTP,
			repoHost:   "127.0.0.1:5000",
		},
		{
			name:       "a plain http realm on another host is refused even against a plain http registry",
			realm:      "http://auth.example.com/token",
			repoScheme: schemeHTTP,
			repoHost:   "127.0.0.1:5000",
			wantErr:    true,
		},
		{
			name:       "a realm carrying userinfo is refused",
			realm:      "https://someone:secret@auth.example.com/token",
			repoScheme: schemeHTTPS,
			wantErr:    true,
		},
		{
			name:       "a relative realm is refused",
			realm:      "/token",
			repoScheme: schemeHTTPS,
			wantErr:    true,
		},
		{
			name:       "a realm with no host is refused",
			realm:      "https:///token",
			repoScheme: schemeHTTPS,
			wantErr:    true,
		},
		{
			name:       "a realm carrying a fragment is refused",
			realm:      "https://auth.example.com/token#part",
			repoScheme: schemeHTTPS,
			wantErr:    true,
		},
		{
			name:       "a realm that is not a URL at all is refused",
			realm:      "https://auth.example.com/token\x7f\x00",
			repoScheme: schemeHTTPS,
			wantErr:    true,
		},
		{name: "an empty realm is refused", realm: "", repoScheme: schemeHTTPS, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			endpoint, err := validateRealm(tt.realm, tt.repoScheme, tt.repoHost)

			if tt.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrUnauthorized, "a realm bigoci will not dial is a refusal")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantQuery, endpoint.RawQuery)
		})
	}
}

// TestScopesLockedKeysOnTheMethodAndNotTheChallenge pins the one thing that
// must not move under the token cache.
//
// A registry states the scope of the request it refused, so a push — whose
// blob checks are reads and whose uploads are writes — collects two different
// challenge scopes and alternates between them. A key drawn from the challenge
// would therefore change under a token that was already filed, and the entry
// recording that the token had worked would stop being the entry the next
// request of the same method reads. The end of that story is a working
// credential reported as refused.
func TestScopesLockedKeysOnTheMethodAndNotTheChallenge(t *testing.T) {
	t.Parallel()

	state := newAuthState(&Repository{name: "team/artifact"}, nil, time.Now)

	asked, read := state.scopesLocked(http.MethodHead)
	assert.Equal(t, []scope{"repository:team/artifact:pull"}, asked)
	assert.Equal(t, scopeKey("repository:team/artifact:pull"), read)

	// What a refusal of a write looks like: the registry names the access that
	// request needed, and it is wider than a read's.
	state.challenge = challenge{scopes: []string{"repository:team/artifact:pull,push"}}

	asked, again := state.scopesLocked(http.MethodHead)
	assert.Equal(
		t,
		[]scope{"repository:team/artifact:pull", "repository:team/artifact:pull,push"},
		asked,
		"the token request asks for what the challenge said as well as what the method needs",
	)
	assert.Equal(t, read, again, "the key a read's token is filed under must not have moved")
}

// TestParseChallengeSurvivesATokenSixtyEightSibling pins the resync rule: a
// credential-carrying scheme this package does not read costs its own
// challenge, never the usable one beside it.
func TestParseChallengeSurvivesATokenSixtyEightSibling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		header  string
		wantErr bool
	}{
		{
			name:   "a token68 challenge before the bearer",
			header: `Negotiate abc==, Bearer realm="https://auth.example.com/token"`,
		},
		{
			name:   "a token68 challenge after the bearer",
			header: `Bearer realm="https://auth.example.com/token", Negotiate abc==`,
		},
		{
			name:    "a token68 challenge alone is still a scheme bigoci does not implement",
			header:  `Negotiate abc==`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseChallenge(tt.header)

			if tt.wantErr {
				require.ErrorIs(t, err, ErrUnauthorized)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, schemeBearer, got.scheme)
			assert.Equal(t, "https://auth.example.com/token", got.realm)
		})
	}
}

// TestChallengeHeaderJoinsEveryFieldLine pins the RFC 9110 equivalence: a
// registry stating its schemes as separate header lines is read the same as
// one comma-joined line, so a Bearer on the second line is still seen past a
// Basic on the first.
func TestChallengeHeaderJoinsEveryFieldLine(t *testing.T) {
	t.Parallel()

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add(headerChallenge, `Basic realm="registry"`)
	resp.Header.Add(headerChallenge, `Bearer realm="https://auth.example.com/token",service="reg"`)

	got, err := parseChallenge(challengeHeader(resp))

	require.NoError(t, err)
	assert.Equal(t, schemeBearer, got.scheme, "the second line's Bearer must win over the first line's Basic")
	assert.Equal(t, "https://auth.example.com/token", got.realm)
}

// TestValidateRealmNeverQuotesRegistrySelectedMaterial pins that no invalid
// realm shape makes its userinfo, path, query, fragment, or malformed source
// text public error material.
func TestValidateRealmNeverQuotesRegistrySelectedMaterial(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		realm   string
		secrets []string
	}{
		{
			name:    "userinfo",
			realm:   "https://someone:hunter2@auth.example.com/private-path?ticket=query-ticket#private-fragment",
			secrets: []string{"hunter2", "private-path", "query-ticket", "private-fragment"},
		},
		{
			name:    "fragment",
			realm:   "https://auth.example.com/private-path?ticket=query-ticket#private-fragment",
			secrets: []string{"private-path", "query-ticket", "private-fragment"},
		},
		{
			name:    "malformed URL",
			realm:   "https://auth.example.com/%zz/private-path?ticket=query-ticket",
			secrets: []string{"%zz", "private-path", "query-ticket"},
		},
		{
			name:    "unsafe scheme",
			realm:   "ftp://auth.example.com/private-path?ticket=query-ticket",
			secrets: []string{"private-path", "query-ticket"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := validateRealm(tt.realm, schemeHTTPS, "registry.example")

			require.ErrorIs(t, err, ErrUnauthorized)
			for _, secret := range tt.secrets {
				assert.NotContains(t, err.Error(), secret)
			}
		})
	}
}
