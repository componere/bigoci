package oci

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/retry"
)

// TestATokenExchangeCarriesNoAmbientCookie proves the caller's Cookie Jar
// remains active for the registry while contributing no authority to the
// registry-selected realm. Explicit token credentials still travel in the
// Basic header the distribution protocol defines.
func TestATokenExchangeCarriesNoAmbientCookie(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.wantUser = "someone"
	fake.wantPass = "the-secret"

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	registryURL, err := url.Parse(fake.server.URL)
	require.NoError(t, err)
	jar.SetCookies(registryURL, []*http.Cookie{{Name: "session", Value: storageSecret, Path: "/"}})

	client := *fake.server.Client()
	client.Jar = jar
	creds := &staticCredentials{cred: Credential{Username: fake.wantUser, Password: fake.wantPass}}
	repo := fake.repository(t, WithHTTPClient(&client), WithCredentials(creds))

	_, _, err = repo.Manifests().Get(t.Context())
	require.NoError(t, err)

	repositoryRequests := fake.repositoryRequests()
	require.NotEmpty(t, repositoryRequests)
	assert.Contains(t, repositoryRequests[0].cookie, storageSecret,
		"the positive control proves the caller's jar is active for the registry")

	tokenRequests := fake.tokenRequests()
	require.Len(t, tokenRequests, 1)
	assert.Empty(t, tokenRequests[0].cookie)
	assert.NotEmpty(t, tokenRequests[0].authorization, "the explicit Basic credential remains present")
}

// TestTokenEndpointFailuresClassifyThroughTheSameTable pins that a token
// exchange is a registry request like any other. A token endpoint having a
// bad minute is worth another attempt, with whatever wait it asked for; one
// refusing the credential is not.
func TestTokenEndpointFailuresClassifyThroughTheSameTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		retryAfter    string
		wantTransient bool
		wantAfter     time.Duration
		wantRefusal   bool
	}{
		{
			name:          "an unavailable token endpoint is worth repeating",
			status:        http.StatusServiceUnavailable,
			retryAfter:    "2",
			wantTransient: true,
			wantAfter:     2 * time.Second,
		},
		{
			name:          "a rate limited token endpoint is worth repeating",
			status:        http.StatusTooManyRequests,
			wantTransient: true,
		},
		{
			name:        "a token endpoint refusing the credential is a refusal",
			status:      http.StatusUnauthorized,
			wantRefusal: true,
		},
		{
			name:        "a token endpoint denying the credential is a refusal too",
			status:      http.StatusForbidden,
			wantRefusal: true,
		},
		{
			name:   "any other answer from the token endpoint ends the transfer",
			status: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newAuthRegistry(t)
			fake.serveTokenAs = func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}

				w.WriteHeader(tt.status)
			}

			repo := fake.repository(t)

			_, err := repo.Blobs().Exists(t.Context(), authDigest())
			require.Error(t, err)

			after, transient := retry.IsTransient(err)
			assert.Equal(t, tt.wantTransient, transient)
			assert.Equal(t, tt.wantAfter, after)

			if tt.wantRefusal {
				assert.ErrorIs(t, err, ErrUnauthorized)

				return
			}

			assert.NotErrorIs(t, err, ErrUnauthorized, "only a refusal sends someone off to fix credentials")
		})
	}
}

// TestTokenEndpointAnswersThatCarryNoToken pins the deliberate gap in the
// refusal sentinel. The exchange succeeded as far as the registry is
// concerned, so whatever went wrong is the registry's problem and not the
// caller's credentials — reporting it as unauthorized would send someone off
// to fix a password that was never the trouble.
func TestTokenEndpointAnswersThatCarryNoToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "an empty document carries no token", body: `{}`},
		{name: "an empty token carries no token either", body: `{"token":""}`},
		{name: "an answer that is not a document at all", body: `<html>login here</html>`},
		{name: "an answer longer than the limit is not read", body: `{"token":"` + strings.Repeat("t", 64<<10) + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newAuthRegistry(t)
			fake.serveTokenAs = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}

			repo := fake.repository(t)

			_, err := repo.Blobs().Exists(t.Context(), authDigest())

			require.Error(t, err)
			require.NotErrorIs(t, err, ErrUnauthorized, "the registry said the exchange worked")

			_, transient := retry.IsTransient(err)
			assert.False(t, transient, "asking again produces the same broken answer")
		})
	}
}

// TestTokenIsReadFromEitherFieldName pins that both spellings registries use
// are read, and that the distribution spec's own field wins when a registry
// sends both.
func TestTokenIsReadFromEitherFieldName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "the spec's field", body: `{"token":"chosen"}`, want: "chosen"},
		{name: "the OAuth2 field", body: `{"access_token":"chosen"}`, want: "chosen"},
		{name: "both, with the spec's winning", body: `{"token":"chosen","access_token":"other"}`, want: "chosen"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newAuthRegistry(t)
			fake.serveTokenAs = func(w http.ResponseWriter, _ *http.Request) {
				fake.grant(tt.want)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}

			repo := fake.repository(t)

			exists, err := repo.Blobs().Exists(t.Context(), authDigest())

			require.NoError(t, err)
			assert.True(t, exists)

			made := fake.repositoryRequests()
			assert.Equal(t, bearerHeader(tt.want), made[len(made)-1].authorization)
		})
	}
}

// TestExchangeCarriesTheCredentialInABasicHeader pins A5's shape: a GET
// carrying the credential in a Basic header, and never a form body with the
// secret in it.
func TestExchangeCarriesTheCredentialInABasicHeader(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.wantUser = "someone"
	fake.wantPass = "the-secret"

	creds := &staticCredentials{cred: Credential{Username: "someone", Password: "the-secret"}}
	repo := fake.repository(t, WithCredentials(creds))

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.NoError(t, err)
	assert.True(t, exists)

	asked := fake.tokenRequests()
	require.Len(t, asked, 1)
	assert.Equal(t, http.MethodGet, asked[0].method)
	assert.Zero(t, asked[0].bodyLen, "the secret never rides in a body")
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("someone:the-secret")), asked[0].authorization)
}

// TestASecretWithNoUserNameStillAuthenticates pins the gate on the whole
// credential rather than the user name: a token pasted into the password
// field rides the exchange as Basic with an empty user, and never falls out
// into an anonymous request with a credential in hand.
func TestASecretWithNoUserNameStillAuthenticates(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)

	creds := &staticCredentials{cred: Credential{Password: "bare-token"}}
	repo := fake.repository(t, WithCredentials(creds))

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.NoError(t, err)
	assert.True(t, exists)

	asked := fake.tokenRequests()
	require.Len(t, asked, 1)
	assert.Equal(
		t,
		"Basic "+base64.StdEncoding.EncodeToString([]byte(":bare-token")),
		asked[0].authorization,
		"the exchange must present the secret rather than run anonymously",
	)
}

// TestCredentialIsLookedUpUnderTheHostBigociDialed pins the compensating
// control that makes a cross-host realm safe: the key is always the registry
// bigoci connected to, never the name the challenge gave itself.
func TestCredentialIsLookedUpUnderTheHostBigociDialed(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	creds := &staticCredentials{}
	repo := fake.repository(t, WithCredentials(creds))

	_, err := repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)

	creds.mu.Lock()
	defer creds.mu.Unlock()

	require.Len(t, creds.asked, 1)
	assert.Equal(t, Registry(fake.host(t)), creds.asked[0])
	assert.NotEqual(t, Registry(authService), creds.asked[0], "the challenge does not get to pick the key")
}

// TestCredentialLookupFailureEndsTheTransfer pins that a lookup which could
// not be performed is a failure rather than a quiet fall back to anonymous: a
// transfer that downgraded here would fail later, somewhere with far less to
// say about why.
func TestCredentialLookupFailureEndsTheTransfer(t *testing.T) {
	t.Parallel()

	broken := errors.New("the credential helper would not run")

	fake := newAuthRegistry(t)
	creds := &staticCredentials{err: broken}
	repo := fake.repository(t, WithCredentials(creds))

	_, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.ErrorIs(t, err, broken)
	assert.Empty(t, fake.tokenRequests(), "nothing was presented, because nothing was resolved")
}

// TestIdentityTokenIsRefusedOutLoud pins D8's loudest ruling. A credential
// bigoci cannot exchange must not become an anonymous exchange that quietly
// works for reads and fails for writes; it fails here, before anything is
// sent, saying what to do about it.
func TestIdentityTokenIsRefusedOutLoud(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	creds := &staticCredentials{cred: Credential{IdentityToken: "a-refresh-token"}}
	repo := fake.repository(t, WithCredentials(creds))

	_, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.ErrorIs(t, err, ErrUnauthorized)
	assert.Contains(t, err.Error(), "identity token")

	_, transient := retry.IsTransient(err)
	assert.False(t, transient)

	assert.Empty(t, fake.tokenRequests(), "the exchange that would have downgraded to anonymous never went out")
}

// TestIdentityTokenBesideAPasswordUsesThePassword pins the other side of the
// rule: the refusal is for a credential carrying nothing else, not for the
// field's mere presence.
func TestIdentityTokenBesideAPasswordUsesThePassword(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.wantUser = "someone"
	fake.wantPass = "the-secret"

	creds := &staticCredentials{cred: Credential{
		Username:      "someone",
		Password:      "the-secret",
		IdentityToken: "a-refresh-token",
	}}
	repo := fake.repository(t, WithCredentials(creds))

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.NoError(t, err)
	assert.True(t, exists)
}

// TestRegistryTokenIsPresentedVerbatim pins that a credential which already
// is a bearer token is presented as one, with no exchange at all.
func TestRegistryTokenIsPresentedVerbatim(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.grant("a-registry-token")

	creds := &staticCredentials{cred: Credential{RegistryToken: "a-registry-token"}}
	repo := fake.repository(t, WithCredentials(creds))

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.NoError(t, err)
	assert.True(t, exists)
	assert.Empty(t, fake.tokenRequests(), "a bearer token needs no exchange to become one")

	made := fake.repositoryRequests()
	assert.Equal(t, bearerHeader("a-registry-token"), made[len(made)-1].authorization)
}

// TestBasicChallenge pins the scheme that runs no exchange: the credential
// goes straight onto the request, and a registry asking for one when none is
// configured is a refusal nothing can answer.
func TestBasicChallenge(t *testing.T) {
	t.Parallel()

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("someone:the-secret"))

	tests := []struct {
		name    string
		creds   Credentials
		wantErr bool
	}{
		{
			name:  "a configured credential answers it",
			creds: &staticCredentials{cred: Credential{Username: "someone", Password: "the-secret"}},
		},
		{name: "no credential at all cannot", wantErr: true},
		{name: "and neither can an empty one", creds: &staticCredentials{}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newAuthRegistry(t)
			fake.challengeScheme = schemeBasic
			fake.accepts = func(r *http.Request) bool {
				return r.Header.Get(headerAuthorization) == want
			}

			repo := fake.repository(t, WithCredentials(tt.creds))

			_, err := repo.Blobs().Exists(t.Context(), authDigest())

			assert.Empty(t, fake.tokenRequests(), "Basic runs no exchange")

			if tt.wantErr {
				require.ErrorIs(t, err, ErrUnauthorized)

				_, transient := retry.IsTransient(err)
				assert.False(t, transient)

				return
			}

			require.NoError(t, err)

			made := fake.repositoryRequests()
			assert.Equal(t, want, made[len(made)-1].authorization)
		})
	}
}

// TestAnUploadNeverProbesBecauseItsSessionAlreadyAsked pins the reachable
// half of D6: a real upload opens with a bodyless POST, so by the time the
// body-bearing request is built the registry has already said everything the
// probe would have asked it.
func TestAnUploadNeverProbesBecauseItsSessionAlreadyAsked(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	repo := fake.repository(t)

	require.NoError(t, repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)),
		strings.NewReader(authPayload)))

	for _, one := range fake.all() {
		assert.NotEqual(t, apiPrefix, one.path, "the session's own POST is what learned the challenge")
	}
}

// TestASpentBodyOnAVirginRepositoryProbesFirst pins the unreachable half. The
// guard exists so that a refactor which sent a body-bearing request first
// fails loudly here instead of hanging on a reader nothing can rewind, and
// the only way to see it is to build that request directly.
func TestASpentBodyOnAVirginRepositoryProbesFirst(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	repo := fake.repository(t)

	session, err := url.Parse(fake.server.URL + apiPrefix + authRepo + "/blobs/uploads/session-1")
	require.NoError(t, err)

	err = repo.Blobs().completeUpload(
		t.Context(), session, authDigest(), int64(len(authPayload)), strings.NewReader(authPayload),
	)
	require.NoError(t, err)

	var probes int

	for _, one := range fake.all() {
		if one.path == apiPrefix {
			probes++
		}
	}

	assert.Equal(t, 1, probes, "the challenge is learned before the body that can only be sent once goes out")

	made := fake.repositoryRequests()
	require.Equal(t, http.MethodPut, made[len(made)-1].method)
	assert.Equal(t, "Bearer token-1", made[len(made)-1].authorization, "and the upload carries it on its first try")
	assert.Equal(t, len(authPayload), made[len(made)-1].bodyLen, "the body went out once")
}

// TestEveryRequestCrossesTheCallersTransport is the instrument's own gate.
// The challenged request, the token exchange, the re-issue, and every hop of a
// redirect all go out through the client the caller supplied, which is what
// makes a tap on that client a complete record and the no-leak checks it feeds
// non-vacuous.
//
// The redirect row is the one that could quietly stop being true. Both clients
// this package derives are copies of the caller's, so a hop keeps the caller's
// transport; a client built here instead — with its own transport, or with
// none — would send a request nothing outside could see, and the counted
// no-leak recipes would be counting an empty log.
func TestEveryRequestCrossesTheCallersTransport(t *testing.T) {
	t.Parallel()

	t.Run("a challenge, an exchange, and the re-issue", func(t *testing.T) {
		t.Parallel()

		fake := newAuthRegistry(t)
		counter := &countingTransport{next: fake.server.Client().Transport}

		repo := fake.repository(t, WithHTTPClient(&http.Client{Transport: counter}))

		_, _, err := repo.Manifests().Get(t.Context())
		require.NoError(t, err)

		manifest := apiPrefix + authRepo + "/manifests/" + authTag
		assert.Equal(t, []string{manifest, tokenPath, manifest}, counter.crossed())
	})

	t.Run("and the hop a redirect adds", func(t *testing.T) {
		t.Parallel()

		fake := newAuthRegistry(t)
		store := newBlobStore(t)
		fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

		counter := &countingTransport{next: fake.server.Client().Transport}
		repo := fake.repository(t, WithHTTPClient(&http.Client{Transport: counter}))

		body, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
		require.NoError(t, err)

		defer body.Close()

		_, err = io.Copy(io.Discard, body)
		require.NoError(t, err)

		blob := apiPrefix + authRepo + "/blobs/" + authDigest().String()
		assert.Equal(t, []string{blob, tokenPath, blob, storagePrefix + blob}, counter.crossed())
	})
}

// TestThePackageBuildsNoClientOfItsOwn is a reviewable invariant rather than
// a behavior: a client this package built would be a request the caller's tap
// could not see, and the one client it does build is the documented default
// that [WithHTTPClient] replaces.
func TestThePackageBuildsNoClientOfItsOwn(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	built := make(map[string]int)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}

		source, err := os.ReadFile(name)
		require.NoError(t, err)

		if count := strings.Count(string(source), "http.Client{"); count > 0 {
			built[name] = count
		}
	}

	assert.Equal(
		t,
		map[string]int{"repository.go": 1},
		built,
		"the only client this package constructs is the default a caller replaces",
	)
}
