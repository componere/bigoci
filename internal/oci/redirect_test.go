package oci

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/retry"
)

// redirectOffset is the byte a ranged row asks the blob from. It is inside the
// payload and not at its start, so a reported offset of zero and a reported
// offset of this one cannot be confused.
const redirectOffset = 10

// followedParam marks a location the registry sent to itself, so the fixture
// can redirect the first request and serve the second.
const followedParam = "followed"

// TestARedirectedBlobReadReachesStorageCarryingNothing is the row the whole
// file is about: the registry hands a blob read to an object store on another
// port of the same host, and the request that arrives there carries neither
// the credential the registry demanded nor the cookie the caller's jar held.
//
// The same host and a different port is exactly the shape the standard
// library's own rule reads as one party — it compares domains and ignores
// ports — so an automatic follow would have forwarded the bearer token here.
func TestARedirectedBlobReadReachesStorageCarryingNothing(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

	repo := fake.repository(t)

	body, start, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
	require.NoError(t, err)

	defer body.Close()

	content, err := io.ReadAll(body)
	require.NoError(t, err)

	assert.Equal(t, authPayload, string(content), "the blob comes off the store, whole")
	assert.Zero(t, start, "a read from the first byte starts at the first byte, wherever it was served from")

	asked := store.all()
	require.Len(t, asked, 1)
	assert.Empty(t, asked[0].header.Get(headerAuthorization), "the registry's credential must not reach storage")
	assert.Empty(t, asked[0].header.Get("Cookie"), "and neither may a cookie")
	assert.Equal(t, signatureValue, asked[0].query.Get(signatureParam), "the location's own query is what it carries")

	read := fake.repositoryRequests()
	assert.Equal(
		t, bearerHeader("token-1"), read[len(read)-1].authorization,
		"the registry request carried a credential, so there was something to leak",
	)
}

// TestAStoreThatRequiresACredentialFailsTheRead is the positive control for
// the row above.
//
// Without it, a fixture whose store never ran at all would pass the no-leak
// assertions perfectly: nothing arrived, so nothing arrived carrying a
// credential. Flipping the store to serve only requests that authenticate has
// to break the read, which is what proves the handler is reached and its
// records are of real traffic.
func TestAStoreThatRequiresACredentialFailsTheRead(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	store.requires = true
	fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

	repo := fake.repository(t)

	_, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)

	require.Error(t, err, "a store that demands the credential bigoci withholds must not serve the blob")
	assert.Len(t, store.all(), 1, "and it must have been asked, or the row above proves nothing")
}

// TestEveryRedirectStatusIsFollowedForAReadAndForNothingElse walks the follow
// table. A read is re-issued at whatever location the registry names; a write
// is not, whatever status names it.
func TestEveryRedirectStatusIsFollowedForAReadAndForNothingElse(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			fake := newAuthRegistry(t)
			store := newBlobStore(t)
			fake.answerAs = redirectBlobReads(store, status)

			repo := fake.repository(t)

			body, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
			require.NoError(t, err)

			defer body.Close()

			content, err := io.ReadAll(body)
			require.NoError(t, err)
			assert.Equal(t, authPayload, string(content))

			asked := store.all()
			require.Len(t, asked, 1)
			assert.Equal(t, http.MethodGet, asked[0].method, "a redirected GET stays a GET, 303 included")
		})
	}
}

// TestAHeadIsFollowedExceptWhereItWouldBecomeAGet is the other half of the
// follow table, read through the one request bigoci makes with HEAD.
//
// A blob check is re-issued like any other read. A 303 is the exception, and
// the reason is what the status means rather than what it costs: it says to
// fetch the result of what was just done with a GET, and a HEAD turned into a
// request for a body is an answer to a question nobody asked. bigoci wants to
// know whether the blob is there, and a peer that will only say so by sending
// it has not answered.
func TestAHeadIsFollowedExceptWhereItWouldBecomeAGet(t *testing.T) {
	t.Parallel()

	t.Run("a temporary redirect is followed", func(t *testing.T) {
		t.Parallel()

		fake := newAuthRegistry(t)
		store := newBlobStore(t)
		fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

		repo := fake.repository(t)

		found, err := repo.Blobs().Exists(t.Context(), authDigest())
		require.NoError(t, err)
		assert.True(t, found, "the store holds the blob, so the check that reached it says so")

		asked := store.all()
		require.Len(t, asked, 1)
		assert.Equal(t, http.MethodHead, asked[0].method, "a redirected HEAD stays a HEAD")
		assert.Empty(t, asked[0].header.Get(headerAuthorization), "and still carries nothing")
	})

	t.Run("a see-other is not", func(t *testing.T) {
		t.Parallel()

		fake := newAuthRegistry(t)
		store := newBlobStore(t)
		fake.answerAs = redirectBlobReads(store, http.StatusSeeOther)

		repo := fake.repository(t)

		_, err := repo.Blobs().Exists(t.Context(), authDigest())
		require.Error(t, err)

		_, transient := retry.IsTransient(err)
		assert.False(t, transient, "a registry that answers a check this way will answer the next one the same")

		var status *StatusError

		require.ErrorAs(t, err, &status)
		assert.Equal(t, http.StatusSeeOther, status.Status)
		assert.Equal(t, http.MethodHead, status.Method)
		assert.Contains(t, status.Path, "/blobs/", "the failure is the registry's, reported against the registry")

		assert.Empty(t, store.all(), "and nothing was sent to the location it named")
	})
}

// TestARangeSurvivesTheHop pins the two answers a ranged read may get once the
// content is on somebody else's server.
//
// A store that honors the range answers 206 from the byte that was asked for,
// and the read starts there. A store that ignores it answers 200 with the
// whole blob, which is legal and useful, and the read starts at zero. Both are
// the rule the registry path already follows; the point of the row is that a
// redirect does not change it, and that the Range header survives the re-issue
// to make either answer possible.
func TestARangeSurvivesTheHop(t *testing.T) {
	t.Parallel()

	t.Run("a store that honors the range", func(t *testing.T) {
		t.Parallel()

		fake := newAuthRegistry(t)
		store := newBlobStore(t)
		fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

		repo := fake.repository(t)

		body, start, err := repo.Blobs().Get(t.Context(), authDigest(), redirectOffset)
		require.NoError(t, err)

		defer body.Close()

		content, err := io.ReadAll(body)
		require.NoError(t, err)

		assert.Equal(t, int64(redirectOffset), start)
		assert.Equal(t, authPayload[redirectOffset:], string(content))

		asked := store.all()
		require.Len(t, asked, 1)
		assert.Equal(t, rangeFrom(redirectOffset), asked[0].header.Get("Range"), "the range crossed the hop")
	})

	t.Run("a store that answers the whole blob instead", func(t *testing.T) {
		t.Parallel()

		fake := newAuthRegistry(t)
		store := newBlobStore(t)
		store.serveAs = func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, authPayload)
		}
		fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

		repo := fake.repository(t)

		body, start, err := repo.Blobs().Get(t.Context(), authDigest(), redirectOffset)
		require.NoError(t, err)

		defer body.Close()

		content, err := io.ReadAll(body)
		require.NoError(t, err)

		assert.Zero(t, start, "a 200 answering a range request starts at the first byte and says so")
		assert.Equal(t, authPayload, string(content))
	})
}

// TestAStoreRefusalIsNeverTheCallersCredentials is the off-registry table.
//
// The four statuses a signed URL answers when its signature has stopped
// working — expired, revoked, or naming an object that moved — are worth
// another attempt, because the next attempt asks the registry again and gets a
// fresh signature. None of them may reach a caller as [ErrUnauthorized] or
// [ErrNotFound]: one would send somebody off to fix credentials that are
// working, the other would report an artifact that is sitting right there as
// missing.
func TestAStoreRefusalIsNeverTheCallersCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		retryAfter    string
		wantTransient bool
		wantAfter     time.Duration
	}{
		{name: "an expired signature reads as unauthorized", status: http.StatusUnauthorized, wantTransient: true},
		{name: "or as forbidden", status: http.StatusForbidden, wantTransient: true},
		{name: "or as a missing object", status: http.StatusNotFound, wantTransient: true},
		{name: "or as one that is gone", status: http.StatusGone, wantTransient: true},
		{
			name:          "a throttled store is worth waiting for",
			status:        http.StatusTooManyRequests,
			retryAfter:    "2",
			wantTransient: true,
			wantAfter:     2 * time.Second,
		},
		{name: "so is one having a bad minute", status: http.StatusServiceUnavailable, wantTransient: true},
		{name: "a malformed request is not", status: http.StatusBadRequest},
		{name: "and neither is a refused size", status: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newAuthRegistry(t)
			store := newBlobStore(t)
			store.serveAs = func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}

				http.Error(w, storageDetail, tt.status)
			}
			fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

			repo := fake.repository(t)

			_, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
			require.Error(t, err)

			after, transient := retry.IsTransient(err)
			assert.Equal(t, tt.wantTransient, transient)
			assert.Equal(t, tt.wantAfter, after)

			require.NotErrorIs(t, err, ErrUnauthorized, "an expired signature is not the caller's credentials")
			require.NotErrorIs(t, err, ErrNotFound, "a store that lost a signed request is not a missing blob")
			require.NotErrorIs(t, err, ErrTooLarge)

			var status *StatusError

			require.NotErrorAs(t, err, &status, "the registry did not say this, so nothing may report it as if it had")

			assertNamesNoSignature(t, err, store.host(t))
		})
	}
}

// TestAStoreThatCannotBeReachedIsWorthAnotherAttempt covers the other way a
// hop fails: no answer at all.
//
// The error the standard library builds for one renders the whole URL, query
// included, which for a presigned location is the signature. What comes back
// names the host and the failure and nothing else.
func TestAStoreThatCannotBeReachedIsWorthAnotherAttempt(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	dead := deadStorage(t)
	fake.answerAs = func(w http.ResponseWriter, r *http.Request) bool {
		if !isBlobRead(r) {
			return false
		}

		w.Header().Set(headerLocation, "http://"+dead+storagePrefix+"?"+signatureParam+"="+signatureValue)
		w.WriteHeader(http.StatusTemporaryRedirect)

		return true
	}

	repo := fake.repository(t)

	_, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.True(t, transient, "a connection that would not open is worth another attempt")

	assertNamesNoSignature(t, err, dead)
}

// TestTheRedirectChainIsBounded pins the hop limit. A store that keeps
// pointing at itself is a loop, and bigoci stops rather than following it.
func TestTheRedirectChainIsBounded(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	store.serveAs = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerLocation, store.server.URL+storagePrefix+"/again")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
	fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

	repo := fake.repository(t)

	_, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.False(t, transient, "a loop answers the same way every time")
	assert.Contains(t, err.Error(), "redirected more than 3 times")
	assert.Len(t, store.all(), maxRedirectHops, "the chain stops at the limit rather than one hop past it")
}

// TestALocationBigociWillNotFollow walks the locations that end a read instead
// of continuing it.
//
// Each is a peer choosing something on bigoci's behalf: where to go when it
// said nothing, what a URL is, what protocol to speak, or — the worst of them
// — which credential to present, because [net/http] turns userinfo in a URL
// into a Basic header. None of the messages may quote the location back.
func TestALocationBigociWillNotFollow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		location func(store *blobStore) string
		want     string
	}{
		{
			name:     "no location at all",
			location: func(*blobStore) string { return "" },
			want:     "redirected without a Location header",
		},
		{
			name:     "a location that is not http",
			location: func(*blobStore) string { return "ftp://storage.example.com/blob" },
			want:     `redirected to a "ftp" location`,
		},
		{
			name:     "a location naming no host",
			location: func(*blobStore) string { return "http:///storage" },
			want:     "redirected to a location naming no host",
		},
		{
			name: "a location choosing bigoci's credential",
			location: func(store *blobStore) string {
				return strings.Replace(store.server.URL, "http://", "http://someone:"+storageSecret+"@", 1) +
					storagePrefix
			},
			want: "carries a user name and password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newAuthRegistry(t)
			store := newBlobStore(t)
			location := tt.location(store)
			fake.answerAs = func(w http.ResponseWriter, r *http.Request) bool {
				if !isBlobRead(r) {
					return false
				}

				if location != "" {
					w.Header().Set(headerLocation, location)
				}
				w.WriteHeader(http.StatusTemporaryRedirect)

				return true
			}

			repo := fake.repository(t)

			_, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
			require.Error(t, err)

			_, transient := retry.IsTransient(err)
			assert.False(t, transient, "a location bigoci will not follow is not one it will follow later")
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), storageSecret, "an error is no place for a secret a peer offered")
			assert.Empty(t, store.all(), "nothing was sent to a location bigoci refused")
		})
	}
}

// TestAnHTTPSRepositoryIsNeverRedirectedToPlainHTTP pins the downgrade rule,
// which needs a repository whose own scheme is https to mean anything.
//
// A registry reached over TLS pointing at a plain-http location is either a
// misconfiguration or somebody standing in the middle, and either way the
// bytes of a blob a caller is about to trust would arrive in the clear.
func TestAnHTTPSRepositoryIsNeverRedirectedToPlainHTTP(t *testing.T) {
	t.Parallel()

	store := newBlobStore(t)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerLocation, store.server.URL+storagePrefix)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	address, err := url.Parse(server.URL)
	require.NoError(t, err)

	repo, err := NewRepository(address.Host+"/"+authRepo+":"+authTag, WithHTTPClient(server.Client()))
	require.NoError(t, err)

	_, _, err = repo.Blobs().Get(t.Context(), authDigest(), 0)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "redirected an https request to plain http")
	assert.Empty(t, store.all(), "the cleartext hop was never made")
}

// TestABlobUploadIsNeverRedirected pins the half of the follow table that
// refuses.
//
// The body of a blob upload is a section of a file on disk that the standard
// library will not rewind, so a re-issue would have nothing to send. bigoci
// does not have to notice that at the moment of the redirect, because it never
// re-issues a write at all: the failure is the unexpected status it is, the
// registry keeps the one copy of the body it already read, and the
// orchestrator decides what a failed part is worth.
func TestABlobUploadIsNeverRedirected(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	fake.answerAs = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}

		w.Header().Set(headerLocation, store.location(r))
		w.WriteHeader(http.StatusTemporaryRedirect)

		return true
	}

	repo := fake.repository(t)

	err := repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)), strings.NewReader(authPayload))
	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.False(t, transient, "a registry that redirects an upload will redirect the next one too")

	var status *StatusError
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusTemporaryRedirect, status.Status)
	assert.Equal(t, http.MethodPut, status.Method)
	assert.Contains(t, status.Path, "/blobs/uploads/", "the failure is reported against the registry, not the store")

	assert.Empty(t, store.all(), "the store was never asked for anything")

	sent := 0

	for _, one := range fake.repositoryRequests() {
		if one.method == http.MethodPut {
			sent++

			assert.Equal(t, len(authPayload), one.bodyLen)
		}
	}

	assert.Equal(t, 1, sent, "the body that can only be sent once was sent once")
}

// TestASameOriginRedirectKeepsTheCredential is the one case the header is kept
// for.
//
// A registry that redirects inside itself — to another path, another endpoint,
// the same host and port it was already talking to — is still the registry,
// and it still wants the token it demanded. Dropping the header everywhere
// would break that, which is why the rule is same-origin rather than
// clean-always.
func TestASameOriginRedirectKeepsTheCredential(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.answerAs = func(w http.ResponseWriter, r *http.Request) bool {
		if !isBlobRead(r) || r.URL.Query().Get(followedParam) != "" {
			return false
		}

		w.Header().Set(headerLocation, r.URL.Path+"?"+followedParam+"=1")
		w.WriteHeader(http.StatusTemporaryRedirect)

		return true
	}

	repo := fake.repository(t)

	body, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
	require.NoError(t, err, "the re-issue must still authenticate, or the registry refuses it")

	defer body.Close()

	content, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, authPayload, string(content))

	var reads []authRecord

	for _, one := range fake.repositoryRequests() {
		if one.method == http.MethodGet && strings.Contains(one.path, "/blobs/") {
			reads = append(reads, one)
		}
	}

	// Three: the bare read the fixture challenges, the re-issue that answers
	// the challenge and is redirected, and the hop that follows the location.
	require.Len(t, reads, 3)
	assert.Equal(t, "1", reads[2].query.Get(followedParam), "the last read is the one the location named")
	assert.Equal(t, reads[1].authorization, reads[2].authorization, "the same origin gets the same credential")
	assert.Equal(t, bearerHeader("token-1"), reads[2].authorization)
}

// TestTheLocationsRedirectTargetRefuses is the validation table read straight
// off the function that enforces it.
//
// One of its rows cannot be reached through a real read: the standard library
// resolves a Location before it asks the redirect policy what to do, so a
// location that is not a URL fails there and this package never sees the
// response. The rule is still bigoci's to state, so it is stated and checked
// here rather than left to a comment.
func TestTheLocationsRedirectTargetRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		scheme   string
		location string
		want     string
	}{
		{
			name:     "a location that is not a URL",
			scheme:   schemeHTTPS,
			location: ":the-location",
			want:     "redirected to a Location that is not a URL",
		},
		{
			name:     "a location that is not http",
			scheme:   schemeHTTPS,
			location: "ftp://storage.example.com/blob",
			want:     `redirected to a "ftp" location`,
		},
		{
			name:     "a plain-http location for an https repository",
			scheme:   schemeHTTPS,
			location: "http://storage.example.com/blob",
			want:     "redirected an https request to plain http",
		},
		{
			name:     "the same location where the repository is plain http too",
			scheme:   schemeHTTP,
			location: "http://storage.example.com/blob",
		},
		{
			name:     "a location carrying a credential of the registry's choosing",
			scheme:   schemeHTTPS,
			location: "https://someone:" + storageSecret + "@storage.example.com/blob",
			want:     "carries a user name and password",
		},
		{
			name:     "a relative location, which is the registry redirecting inside itself",
			scheme:   schemeHTTPS,
			location: "/v2/other/blobs/sha256:abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, err := NewRepository("registry.example.com/"+authRepo+":"+authTag, plainWhen(tt.scheme))
			require.NoError(t, err)

			endpoint := repo.endpoint("blobs/" + authDigest().String())

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint.String(), nil)
			require.NoError(t, err)

			resp := &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{headerLocation: []string{tt.location}},
			}

			target, err := repo.redirectTarget(originOf(req), req, resp)

			if tt.want == "" {
				require.NoError(t, err)
				assert.NotNil(t, target)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), storageSecret)
		})
	}
}

// plainWhen returns the option that puts a repository on scheme. It exists so
// the table above can name a scheme in a field instead of branching on one.
func plainWhen(scheme string) Option {
	if scheme == schemeHTTP {
		return WithPlainHTTP()
	}

	return func(*settings) {}
}

// TestACallersCookieNeverReachesStorage pins the second client's one
// difference.
//
// Cookies are scoped to a host and not to a port, so a jar holding one for the
// registry would hand it to an object store on the next port up. The registry
// gets it, because it is the registry's cookie; nothing else does.
func TestACallersCookieNeverReachesStorage(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	address, err := url.Parse(fake.server.URL)
	require.NoError(t, err)
	jar.SetCookies(address, []*http.Cookie{{Name: "session", Value: storageSecret, Path: "/"}})

	repo := fake.repository(t, WithHTTPClient(&http.Client{
		Transport: fake.server.Client().Transport,
		Jar:       jar,
	}))

	body, _, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
	require.NoError(t, err)

	defer body.Close()

	_, err = io.Copy(io.Discard, body)
	require.NoError(t, err)

	read := fake.repositoryRequests()
	require.NotEmpty(t, read)
	assert.Contains(
		t, read[len(read)-1].cookie, storageSecret,
		"the jar reached the registry, or this row proves nothing about where it stopped",
	)

	asked := store.all()
	require.Len(t, asked, 1)
	assert.Empty(t, asked[0].header.Get("Cookie"), "and it stopped there")
}

// TestTheCallersClientIsNeverMutated pins the derivation. Two clients come out
// of the caller's and the caller's own is untouched, so a program that shares
// one client with the rest of its work does not find its redirect policy
// quietly changed.
func TestTheCallersClientIsNeverMutated(t *testing.T) {
	t.Parallel()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	caller := &http.Client{Transport: http.DefaultTransport, Jar: jar, Timeout: time.Minute}

	repo, err := NewRepository("registry.example.com/"+authRepo+":"+authTag, WithHTTPClient(caller))
	require.NoError(t, err)

	assert.Nil(t, caller.CheckRedirect, "the caller's client keeps following redirects the way it always did")
	assert.Equal(t, jar, caller.Jar)

	assert.NotNil(t, repo.client.CheckRedirect, "the registry client decides for itself what a 3xx means")
	assert.Equal(t, jar, repo.client.Jar, "and still carries the caller's cookies to the registry")
	assert.NotNil(t, repo.external.CheckRedirect)
	assert.Nil(t, repo.external.Jar, "the external client carries none anywhere")

	for _, derived := range []*http.Client{repo.client, repo.external} {
		assert.Equal(t, caller.Transport, derived.Transport, "every request still crosses the caller's transport")
		assert.Equal(t, caller.Timeout, derived.Timeout)
	}
}

// TestSameOriginComparesLikeTheWeb pins the origin rule's edges: DNS names
// compare without case, a port a scheme implies counts as present, and
// anything else differing — scheme, port, host — is another party.
func TestSameOriginComparesLikeTheWeb(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "the same URL is the same origin", a: "https://reg.example/v2/", b: "https://reg.example/x", want: true},
		{name: "host case does not make a stranger", a: "https://reg.example", b: "https://REG.Example", want: true},
		{
			name: "an implied port equals its explicit spelling",
			a:    "https://reg.example",
			b:    "https://reg.example:443",
			want: true,
		},
		{name: "a different port is a different party", a: "http://127.0.0.1:5000", b: "http://127.0.0.1:5001"},
		{name: "a subdomain is a different party", a: "https://reg.example", b: "https://cdn.reg.example"},
		{name: "a scheme change is a different party", a: "https://reg.example", b: "http://reg.example"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, err := url.Parse(tt.a)
			require.NoError(t, err)
			b, err := url.Parse(tt.b)
			require.NoError(t, err)

			assert.Equal(t, tt.want, sameOrigin(a, b))
		})
	}
}

// TestARefusedRedirectQuotesNoBody pins the leak the refusal arm would
// otherwise open: servers render their own Location into a 3xx body as a
// courtesy, and for a blob read that Location is a presigned URL — so a
// redirect this package refuses to follow must not read the body into the
// error the way every other unexpected status does.
func TestARefusedRedirectQuotesNoBody(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.answerAs = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}

		w.Header().Set(headerLocation, "https://store.example"+storagePrefix+"?"+signatureParam+"="+signatureValue)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte(`<html><body><a href="https://store.example` + storagePrefix +
			`?` + signatureParam + `=` + signatureValue + `">Temporary Redirect</a></body></html>`))

		return true
	}

	repo := fake.repository(t)

	err := repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)), strings.NewReader(authPayload))
	require.Error(t, err)

	var status *StatusError
	require.ErrorAs(t, err, &status, "an unfollowable redirect from the registry is the registry's own answer")
	assert.Equal(t, http.StatusTemporaryRedirect, status.Status)
	assert.Empty(t, status.Detail, "a 3xx body is a rendering of the Location and never becomes detail")
	assert.NotContains(t, err.Error(), signatureParam+"=", "the signature in the body stays out of the message")
	assert.NotContains(t, err.Error(), "store.example", "and so does the location")
}

// TestAnUnfollowableRedirectOffOriginIsNotTheRegistrys pins the reporting
// rule at the far end of a chain: a store that answers a followed hop with a
// redirect this package will not take said something the registry never said,
// so the failure is the store's — not a [StatusError] whose message reads
// "registry returned".
func TestAnUnfollowableRedirectOffOriginIsNotTheRegistrys(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	store := newBlobStore(t)
	store.serveAs = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerLocation, "/see-other")
		w.WriteHeader(http.StatusSeeOther)
	}
	fake.answerAs = redirectBlobReads(store, http.StatusTemporaryRedirect)

	repo := fake.repository(t)

	_, err := repo.Blobs().Exists(t.Context(), authDigest())
	require.Error(t, err)

	var status *StatusError
	require.NotErrorAs(t, err, &status, "the registry did not answer 303; the store did")
	assert.Contains(t, err.Error(), store.host(t))
	assert.NotContains(t, err.Error(), "registry returned")
}

// TestARefusalOnAnOnOriginHopIsAnswered pins the wiring that keeps the auth
// state machine in the loop when a registry redirects inside itself: a hop
// that stays on the registry's origin and answers 401 is the registry
// speaking, and it gets the refresh-once answer any refusal gets — not a
// terminal error that never consulted the challenge.
func TestARefusalOnAnOnOriginHopIsAnswered(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)

	var relayAsked atomic.Int64
	fake.answerAs = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/relay":
			if relayAsked.Add(1) == 1 {
				// The first hop arrives carrying the token the original
				// request earned; refusing it here is the registry saying
				// that token stopped working between the redirect and the
				// hop.
				w.Header().Set(headerChallenge, fake.challenge())
				w.WriteHeader(http.StatusUnauthorized)

				return true
			}

			_, _ = io.WriteString(w, authPayload)

			return true
		case isBlobRead(r):
			w.Header().Set(headerLocation, "/relay")
			w.WriteHeader(http.StatusTemporaryRedirect)

			return true
		default:
			return false
		}
	}

	creds := &staticCredentials{cred: Credential{Username: "someone", Password: "the-secret"}}
	repo := fake.repository(t, WithCredentials(creds))

	body, start, err := repo.Blobs().Get(t.Context(), authDigest(), 0)
	require.NoError(t, err, "the refusal on the hop must be answered, not reported")
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, authPayload, string(got))
	assert.Zero(t, start)
	assert.GreaterOrEqual(t, len(fake.tokenRequests()), 2,
		"the hop's refusal must have driven a second token exchange")
}
