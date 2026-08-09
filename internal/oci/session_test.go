package oci

import (
	"net/http"
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
	fake.answerAs = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}

		w.Header().Set(headerLocation, store.server.URL+storagePrefix+"/up?state=signed-state")
		w.WriteHeader(http.StatusAccepted)

		return true
	}

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
