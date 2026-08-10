package oci

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/retry"
)

// TestAnUploadSessionOnAnotherOriginCarriesNoCredential pins the same-origin
// rule on the one registry-chosen URL that is not a redirect: the Location
// that opens an upload. A registry that hands the upload to signed storage
// has named a party its credential means nothing to, and the part's bytes go
// there with no Authorization header — the signed query is what authenticates
// the write.
func TestAnUploadSessionOnAnotherOriginCarriesNoCredential(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	store.serveAs = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}
	fake.answerAs = openUploadAt(store)

	creds := &staticCredentials{cred: Credential{Username: "someone", Password: "the-secret"}}
	repo := fake.repository(t, WithCredentials(creds))

	err := repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)), strings.NewReader(authPayload))
	require.NoError(t, err)

	arrived := store.all()
	require.Len(t, arrived, 1, "the part must have gone to the session the registry named")
	assert.Equal(t, http.MethodPut, arrived[0].method)
	assert.Empty(t, arrived[0].header.Get(headerAuthorization),
		"a session on another origin gets the signed query's trust, never the registry's credential")

	// The first POST is the bare one the challenge dance answers; the one
	// that opened the session carried the minted token. That contrast is the
	// point: the credential exists and rides to the registry, and only the
	// store goes without.
	var authenticated bool
	for _, req := range fake.repositoryRequests() {
		if req.method == http.MethodPost && req.authorization != "" {
			authenticated = true
		}
	}
	assert.True(t, authenticated, "the session-opening POST must have authenticated to the registry")
}

// TestAnOffOriginUploadCannotReplayRegistryAuthentication pins the trust
// boundary after the initial header has been stripped. A storage refusal is
// not a registry challenge and cannot cause the live repository bearer to be
// replayed to the upload session.
func TestAnOffOriginUploadCannotReplayRegistryAuthentication(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	store.serveAs = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerAuthorization) == "" {
			w.Header().Set(headerChallenge, fake.challenge())
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.WriteHeader(http.StatusCreated)
	}
	fake.answerAs = openUploadAt(store)

	repo := fake.repository(t)
	err := repo.Blobs().Put(t.Context(), authDigest(), 0, strings.NewReader(""))
	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.True(t, transient, "an expired or refused signed upload session earns a fresh attempt")
	require.NotErrorIs(t, err, ErrUnauthorized, "storage did not judge the registry credential")

	requests := store.all()
	require.Len(t, requests, 1, "the storage request must never be replayed")
	assert.Empty(t, requests[0].header.Get(headerAuthorization))
}

// TestAnOffOriginUploadCannotReplaceTheRegistryChallenge proves a refusal of
// a non-replayable upload body has no effect on the next registry request. In
// particular, a storage-selected Basic challenge cannot make the repository
// present its configured password to the registry under a different scheme.
func TestAnOffOriginUploadCannotReplaceTheRegistryChallenge(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	store.serveAs = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerChallenge, `Basic realm="storage"`)
		w.WriteHeader(http.StatusUnauthorized)
	}
	fake.answerAs = openUploadAt(store)

	creds := &staticCredentials{cred: Credential{Username: "someone", Password: "the-secret"}}
	repo := fake.repository(t, WithCredentials(creds))

	err := repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)), strings.NewReader(authPayload))
	require.Error(t, err)

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)
	assert.True(t, exists)

	for _, request := range fake.repositoryRequests() {
		assert.NotContains(t, request.authorization, "Basic ",
			"a storage challenge cannot replace the registry's Bearer challenge")
	}
}

// TestAnOffOriginUploadCarriesNoAmbientCookie pins the other authority an
// [http.Client] can add after request construction. Registry requests retain
// the caller's jar, while the separate client for an upload session has none.
func TestAnOffOriginUploadCarriesNoAmbientCookie(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	store.serveAs = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}
	fake.answerAs = openUploadAt(store)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	registryURL, err := url.Parse(fake.server.URL)
	require.NoError(t, err)
	jar.SetCookies(registryURL, []*http.Cookie{{Name: "session", Value: storageSecret, Path: "/"}})

	client := *fake.server.Client()
	client.Jar = jar
	repo := fake.repository(t, WithHTTPClient(&client))

	err = repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)), strings.NewReader(authPayload))
	require.NoError(t, err)

	repositoryRequests := fake.repositoryRequests()
	require.NotEmpty(t, repositoryRequests)
	assert.Contains(t, repositoryRequests[0].cookie, storageSecret,
		"the positive control proves the caller's jar is active for the registry")

	storageRequests := store.all()
	require.Len(t, storageRequests, 1)
	assert.Empty(t, storageRequests[0].header.Get("Cookie"))
}

// TestAnOffOriginUploadFailureCarriesNoSignedCapability proves a storage
// error document cannot copy the live upload URL into the public error. The
// status is also not a registry StatusError or an authorization verdict.
func TestAnOffOriginUploadFailureCarriesNoSignedCapability(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	store.serveAs = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, storageDetail, http.StatusForbidden)
	}
	fake.answerAs = openUploadAt(store)

	repo := fake.repository(t)
	err := repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)), strings.NewReader(authPayload))
	require.Error(t, err)

	var status *StatusError
	assert.NotErrorAs(t, err, &status, "an object store did not return a registry status")
	require.NotErrorIs(t, err, ErrUnauthorized)
	assert.NotContains(t, err.Error(), signatureValue)
	assert.NotContains(t, err.Error(), storageDetail)
	assert.NotContains(t, err.Error(), storagePrefix)
}

// TestAnUploadSessionCarryingUserinfoIsRefused pins the guard on the other
// hostile shape a session Location can take: a URL choosing a credential
// through userinfo, which [net/http] would turn into a Basic header. The
// refusal is terminal and repeats neither the URL nor the password inside it.
func TestAnUploadSessionCarryingUserinfoIsRefused(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.answerAs = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}

		w.Header().Set(headerLocation, "https://someone:"+storageSecret+"@harvest.example/up")
		w.WriteHeader(http.StatusAccepted)

		return true
	}

	repo := fake.repository(t)

	err := repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)), strings.NewReader(authPayload))
	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.False(t, transient, "a registry naming a hostile session will name it again")
	assert.NotContains(t, err.Error(), storageSecret, "the password inside the location stays out of the message")
	assert.NotContains(t, err.Error(), "harvest.example/up", "and so does the location's path")
}

// openUploadAt returns registry behavior that opens every blob upload at
// store, carrying a query value that stands in for a signed capability.
func openUploadAt(store *blobStore) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}

		w.Header().Set(headerLocation, store.server.URL+storagePrefix+"/up?state=signed-state")
		w.WriteHeader(http.StatusAccepted)

		return true
	}
}
