package oci

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/retry"
)

// workers is how many requests the concurrency rows run at once. Four is the
// number a transfer runs parts with, and it is the number that makes a
// stampede visible: three of them have to find what the fourth acquired.
const workers = 4

// fixedClock is a clock a test moves by hand, so the expiry rows read a token
// across its lifetime without waiting for one.
type fixedClock struct {
	// mu guards at. The clock is read from whichever goroutine holds a token.
	mu sync.Mutex
	// at is what the clock currently reads.
	at time.Time
}

// newClock returns a clock reading an arbitrary fixed instant.
func newClock() *fixedClock {
	return &fixedClock{at: time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)}
}

// now reads the clock.
func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.at
}

// advance moves the clock forward by d.
func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.at = c.at.Add(d)
}

// staticCredentials is a credential source that answers with one fixed
// credential. The generated mock for the port (internal/auth/mocks) cannot be
// imported here: these tests are whitebox package oci to reach the unexported
// state machine and clock, and the mock package imports this one. That is the
// whole reason a hand-built double exists in a repository whose rule is that
// mocks are generated.
type staticCredentials struct {
	// cred is what every lookup returns.
	cred Credential
	// err is what every lookup fails with, when it fails.
	err error

	// mu guards asked.
	mu sync.Mutex
	// asked are the registries the lookups named.
	asked []Registry
}

// Credential returns the fixed credential and records what it was asked for.
func (s *staticCredentials) Credential(_ context.Context, registry Registry) (Credential, error) {
	s.mu.Lock()
	s.asked = append(s.asked, registry)
	s.mu.Unlock()

	if s.err != nil {
		return Credential{}, s.err
	}

	return s.cred, nil
}

// TestAnonymousRegistryCostsNothing is the regression gate the whole design
// rests on: against a registry that never challenges, learning to
// authenticate changed nothing. One request goes out, it carries no
// credential, and the bodyless version check every alternative design needed
// is never sent.
func TestAnonymousRegistryCostsNothing(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.accepts = func(*http.Request) bool { return true }

	repo := fake.repository(t)

	_, _, err := repo.Manifests().Get(t.Context())
	require.NoError(t, err)

	exists, err := repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)
	require.True(t, exists)

	made := fake.all()
	require.Len(t, made, 2, "a registry that does not challenge is asked exactly what it was asked before")

	for _, one := range made {
		assert.Empty(t, one.authorization, "nothing stamps a header a registry never asked for")
		assert.NotEqual(t, apiPrefix, one.path, "no probe, no ping")
	}
}

// TestChallengedRequestAuthenticatesAndIsSentAgain walks the whole dance:
// the bare request, the challenge, the exchange at the realm the challenge
// named, and the re-issue that carries the token.
func TestChallengedRequestAuthenticatesAndIsSentAgain(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	repo := fake.repository(t)

	body, _, err := repo.Manifests().Get(t.Context())

	require.NoError(t, err)
	assert.JSONEq(t, authManifest, string(body))

	made := fake.all()
	require.Len(t, made, 3, "the challenged request, the exchange, and the re-issue")

	assert.Empty(t, made[0].authorization, "the first request goes out bare; the challenge is what teaches it")

	assert.Equal(t, tokenPath, made[1].path)
	assert.Equal(t, http.MethodGet, made[1].method, "the exchange is a GET; the secret never rides in a body")
	assert.Equal(t, authService, made[1].query.Get(tokenServiceParam))
	assert.Equal(
		t,
		[]string{"repository:" + authRepo + ":" + actionsPull},
		made[1].query[tokenScopeParam],
		"a read asks for pull and nothing more",
	)

	assert.Equal(t, made[0].path, made[2].path, "the re-issue is the same request")
	assert.Equal(t, "Bearer token-1", made[2].authorization)
}

// TestChallengeScopeIsMergedIntoTheRequest pins that a challenge asking for
// access of its own is answered with the union rather than with either half.
func TestChallengeScopeIsMergedIntoTheRequest(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.challengeScope = "repository:" + authRepo + ":pull,push"

	repo := fake.repository(t)

	require.NoError(t, repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)),
		strings.NewReader(authPayload)))

	asked := fake.tokenRequests()
	require.Len(t, asked, 1)
	assert.Equal(
		t,
		[]string{"repository:" + authRepo + ":pull,push"},
		asked[0].query[tokenScopeParam],
		"the write's own scope and the challenge's are the same grant, so the union is one element",
	)
}

// TestRefusedCredentialsBurnNoBudget is the numeric no-burn row. Four workers
// meet the same refusal at once, the token endpoint is asked exactly once,
// and not one of the failures is worth repeating — so the orchestrator above
// takes no wait and spends no attempt on a password that will not get better.
func TestRefusedCredentialsBurnNoBudget(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.wantUser = "someone"
	fake.wantPass = "the-right-secret"

	creds := &staticCredentials{cred: Credential{Username: "someone", Password: "the-wrong-secret"}}
	repo := fake.repository(t, WithCredentials(creds))

	failures := make([]error, workers)

	var group sync.WaitGroup

	for i := range workers {
		group.Go(func() {
			_, failures[i] = repo.Blobs().Exists(t.Context(), authDigest())
		})
	}

	group.Wait()

	assert.Len(t, fake.tokenRequests(), 1, "four workers, one exchange: the entry absorbs the refusal")

	for i, err := range failures {
		require.ErrorIs(t, err, ErrUnauthorized, "worker %d", i)

		after, transient := retry.IsTransient(err)
		assert.False(t, transient, "worker %d: a refused password is not worth repeating", i)
		assert.Zero(t, after, "worker %d: nothing asked for a wait", i)
	}
}

// TestAnonymousRefusedByAPrivateRepositoryIsTerminal pins the second half of
// D8's unproven rule: a token bigoci was handed and that was then refused has
// had its one chance, and a second refusal ends the scope rather than buying
// another exchange.
func TestAnonymousRefusedByAPrivateRepositoryIsTerminal(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.accepts = func(*http.Request) bool { return false }

	repo := fake.repository(t)

	_, _, err := repo.Manifests().Get(t.Context())

	require.ErrorIs(t, err, ErrUnauthorized)

	_, transient := retry.IsTransient(err)
	assert.False(t, transient)

	assert.Len(t, fake.tokenRequests(), 1, "one exchange, and no second one against a token already refused")
	assert.Len(t, fake.repositoryRequests(), 2, "the bare request and the one re-issue, and nothing after")
}

// TestExpiredTokenOnAReplayableRequestCostsNothing is D8's free row: a
// request the standard library can produce again is simply produced again,
// inside the same call, so an expired token costs a round trip and no
// attempt at all.
func TestExpiredTokenOnAReplayableRequestCostsNothing(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	repo := fake.repository(t)

	_, _, err := repo.Manifests().Get(t.Context())
	require.NoError(t, err, "the first read proves the token")

	fake.retire("token-1")

	_, _, err = repo.Manifests().Get(t.Context())
	require.NoError(t, err, "the refusal is answered and the read completes")

	assert.Len(t, fake.tokenRequests(), 2, "the proven token was retired and one fresh one was minted")

	made := fake.repositoryRequests()
	require.Len(t, made, 4)
	assert.Equal(t, "Bearer token-1", made[2].authorization, "the second read went out with what it had")
	assert.Equal(t, "Bearer token-2", made[3].authorization, "and was sent again with what the refusal bought")
}

// TestExpiredTokenOnABlobUploadNeverSendsASecondBody is the ruling the whole
// re-issue rule exists for. A blob upload streams a section of a file, and
// [net/http] cannot produce those bytes twice, so the refusal is answered —
// the next attempt will carry a working token — but this request stops here
// and the orchestrator, which can open the file again, is the one that
// repeats it.
func TestExpiredTokenOnABlobUploadNeverSendsASecondBody(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.accepts = func(r *http.Request) bool {
		refused := r.Method == http.MethodPut && r.Header.Get(headerAuthorization) == "Bearer token-1"

		return !refused && fake.isLive(r.Header.Get(headerAuthorization))
	}

	repo := fake.repository(t)

	err := repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)), strings.NewReader(authPayload))

	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnauthorized, "the refusal stays readable under the mark")

	after, transient := retry.IsTransient(err)
	assert.True(t, transient, "the orchestrator re-streams from disk, so this is worth repeating")
	assert.Zero(t, after, "nothing asked for a wait")

	var bodies int

	for _, one := range fake.repositoryRequests() {
		if one.method == http.MethodPut {
			bodies++

			assert.Equal(t, len(authPayload), one.bodyLen)
		}
	}

	assert.Equal(t, 1, bodies, "the body went out once and was never produced a second time")

	// The next attempt is the point of having refreshed at all.
	require.NoError(t, repo.Blobs().Put(t.Context(), authDigest(), int64(len(authPayload)),
		strings.NewReader(authPayload)))

	made := fake.repositoryRequests()
	assert.Equal(t, "Bearer token-2", made[len(made)-1].authorization, "the next call carries a different header")
}

// TestAChallengeThatNamesAnotherScopeDoesNotStrandAToken pins the token cache
// against the one thing that moves under it.
//
// A registry states the scope of the request it refused, so a push collects a
// read's grant from its blob checks and a write's from its uploads, and the
// last challenge to arrive is whichever request was refused last. A token
// filed under a key drawn from that would be looked for under a different name
// the moment the other kind of request was refused — and what turns up under
// the old name is a token that has already been retired, handed to the caller
// as though a worker had just refreshed it. The transfer then ends as
// "credentials refused" while holding a credential that works.
func TestAChallengeThatNamesAnotherScopeDoesNotStrandAToken(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.challengeScope = "repository:" + authRepo + ":pull"

	repo := fake.repository(t)

	_, _, err := repo.Manifests().Get(t.Context())
	require.NoError(t, err, "the first read proves the token it minted")

	// The registry starts refusing in a write's vocabulary, which is what one
	// answering a refused upload sends.
	fake.statesScope("repository:" + authRepo + ":pull,push")
	fake.retire("token-1")

	_, _, err = repo.Manifests().Get(t.Context())
	require.NoError(t, err, "an expired token is refreshed however the challenge spelled the scope")

	// And back to a read's, which is what the read that just failed is told.
	fake.statesScope("repository:" + authRepo + ":pull")
	fake.retire("token-2")

	_, _, err = repo.Manifests().Get(t.Context())
	require.NoError(t, err, "the refresh must come from what this read holds, not from an older entry")

	assert.Len(t, fake.tokenRequests(), 3, "one mint per expiry, and not one more")
}

// TestForbiddenCarryingAChallengeTakesTheRefreshPath pins the 403 split's
// first half. A registry that answers an unreadable bearer with 403 rather
// than 401 is stating the same requirement, and it gets the same one chance
// to be answered.
func TestForbiddenCarryingAChallengeTakesTheRefreshPath(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.refusalStatus = http.StatusForbidden

	repo := fake.repository(t)

	body, _, err := repo.Manifests().Get(t.Context())

	require.NoError(t, err)
	assert.JSONEq(t, authManifest, string(body))
	assert.Len(t, fake.tokenRequests(), 1)
}

// TestBareForbiddenIsTerminal pins the other half. A 403 stating no
// requirement is a permission answer, or a firewall's; presenting the same
// identity again cannot change it, so nothing is exchanged and nothing is
// sent twice.
func TestBareForbiddenIsTerminal(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.refusalStatus = http.StatusForbidden
	fake.omitChallenge = true

	repo := fake.repository(t)

	_, err := repo.Blobs().Exists(t.Context(), authDigest())

	require.ErrorIs(t, err, ErrUnauthorized)

	_, transient := retry.IsTransient(err)
	assert.False(t, transient)

	var status *StatusError

	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusForbidden, status.Status)

	assert.Empty(t, fake.tokenRequests(), "nothing was exchanged")
	assert.Len(t, fake.repositoryRequests(), 1, "and nothing was sent twice")
}

// TestUnusableChallengeIsTerminalAndKeepsTheRegistrysAnswer pins what
// happens when the registry refuses a request and then says nothing usable
// about how to fix it. There is nothing to answer, so nothing is exchanged
// and nothing is sent twice — and the status the registry did send stays
// reachable underneath, because that is the part a caller can act on.
func TestUnusableChallengeIsTerminalAndKeepsTheRegistrysAnswer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		bare   bool
		// dialed says the challenge parsed and was only refused once bigoci
		// looked at where it pointed, which happens after the registry's own
		// answer has been left behind.
		dialed bool
	}{
		{name: "a refusal stating no requirement at all", bare: true},
		{name: "a scheme this package does not implement", header: `Negotiate realm="https://elsewhere/token"`},
		{name: "a bearer challenge naming no realm", header: `Bearer service="fixture"`},
		{name: "a challenge nothing can be read out of", header: `Bearer realm="unterminated`},
		{
			name:   "a challenge naming a realm bigoci will not dial",
			header: `Bearer realm="https://someone:secret@auth.example.com/token"`,
			dialed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newAuthRegistry(t)
			fake.accepts = func(*http.Request) bool { return false }
			fake.omitChallenge = tt.bare
			fake.challengeAs = tt.header

			repo := fake.repository(t)

			_, err := repo.Blobs().Exists(t.Context(), authDigest())

			require.ErrorIs(t, err, ErrUnauthorized)

			_, transient := retry.IsTransient(err)
			assert.False(t, transient)

			assert.Empty(t, fake.tokenRequests(), "there was nothing to exchange with")

			if tt.dialed {
				return
			}

			var status *StatusError

			require.ErrorAs(t, err, &status, "the registry's own answer stays reachable")
			assert.Equal(t, http.StatusUnauthorized, status.Status)
		})
	}
}

// TestTokenIsReusedUntilItsMarginAndThenReminted reads a token across its
// lifetime on a clock the test moves by hand. The lifetime under test is the
// one the registries that matter actually produce: none stated at all, which
// takes the spec's sixty-second default and therefore a thirty-second margin.
func TestTokenIsReusedUntilItsMarginAndThenReminted(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	clock := newClock()
	repo := fake.repository(t, withClock(clock.now))

	_, err := repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)
	require.Len(t, fake.tokenRequests(), 1)

	clock.advance(29 * time.Second)

	_, err = repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)
	assert.Len(t, fake.tokenRequests(), 1, "inside the margin the token on hand is still the answer")

	clock.advance(2 * time.Second)

	_, err = repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)
	assert.Len(t, fake.tokenRequests(), 2, "past it a fresh one is minted before the request goes out")

	made := fake.repositoryRequests()
	assert.Equal(t, "Bearer token-2", made[len(made)-1].authorization)
}

// TestStatedLifetimeSetsTheMargin pins the other half of the expiry rule: a
// short lifetime gives back half of itself rather than the whole thirty
// seconds, which is the only reason a ten-second token is usable at all.
func TestStatedLifetimeSetsTheMargin(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	fake.expiresIn = 10

	clock := newClock()
	repo := fake.repository(t, withClock(clock.now))

	_, err := repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)

	clock.advance(4 * time.Second)

	_, err = repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)
	assert.Len(t, fake.tokenRequests(), 1, "four seconds into a ten second token, the margin has not been reached")

	clock.advance(2 * time.Second)

	_, err = repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)
	assert.Len(t, fake.tokenRequests(), 2, "six seconds in, half the lifetime is gone and so is the token")
}

// TestExpiryMintsOnceUnderConcurrentRequests is the stampede row for the
// expiry path: four workers all find the token expired at the same instant
// and exactly one exchange goes out.
func TestExpiryMintsOnceUnderConcurrentRequests(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	clock := newClock()
	repo := fake.repository(t, withClock(clock.now))

	_, err := repo.Blobs().Exists(t.Context(), authDigest())
	require.NoError(t, err)
	require.Len(t, fake.tokenRequests(), 1)

	clock.advance(time.Minute)

	var group sync.WaitGroup

	failures := make([]error, workers)

	for i := range workers {
		group.Go(func() {
			_, failures[i] = repo.Blobs().Exists(t.Context(), authDigest())
		})
	}

	group.Wait()

	for i, err := range failures {
		require.NoError(t, err, "worker %d", i)
	}

	assert.Len(t, fake.tokenRequests(), 2, "one exchange served all four workers")
}

// TestWaitersStopWhenTheirContextEnds pins that a request waiting on somebody
// else's exchange is still the caller's request: it ends when the caller says
// so, rather than when the registry gets around to answering.
//
// The waiters ask the auth state directly, because that is the only way to
// know they are waiting rather than merely slow: once an exchange is in
// flight there is no other path through authorize, so a request that has not
// come back is a request parked on the exchange.
func TestWaitersStopWhenTheirContextEnds(t *testing.T) {
	t.Parallel()

	fake := newAuthRegistry(t)
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)

	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	fake.serveTokenAs = func(w http.ResponseWriter, _ *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}

		<-release
		fake.grant("token-1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"token-1"}`))
	}

	repo := fake.repository(t)

	minted := make(chan error, 1)

	go func() {
		_, err := repo.Blobs().Exists(t.Context(), authDigest())
		minted <- err
	}()

	// The exchange has reached the token endpoint, so the entry every other
	// request for this scope finds is the one in flight.
	<-arrived

	waiting, cancel := context.WithCancel(t.Context())
	failures := make(chan error, workers-1)

	for range workers - 1 {
		go func() {
			_, err := repo.auth.authorize(waiting, http.MethodHead)
			failures <- err
		}()
	}

	cancel()

	for range workers - 1 {
		select {
		case err := <-failures:
			require.ErrorIs(t, err, context.Canceled, "a waiter ends when its caller does")
		case <-time.After(5 * time.Second):
			t.Fatal("a waiter did not return when its context ended")
		}
	}

	assert.Len(t, fake.tokenRequests(), 1, "nobody who gave up started an exchange of their own")

	releaseOnce()
	require.NoError(t, <-minted, "the exchange the waiters left behind still finishes")
}
