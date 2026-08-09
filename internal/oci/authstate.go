package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/componere/bigoci/internal/retry"
)

// tokenMargin is the most of a token's stated lifetime bigoci gives back. A
// token is stopped being presented this long before the registry says it ends,
// so a request that leaves in time cannot arrive after.
const tokenMargin = 30 * time.Second

// shortTokenShare is the fraction of a short token's lifetime given back
// instead of the full margin. Half is what keeps a lifetime under a minute
// usable at all: giving back thirty seconds of a ten-second token would leave
// nothing to present.
const shortTokenShare = 2

// authError reports that a request could not be authenticated: a challenge
// bigoci cannot answer, a credential it cannot exchange, or a credential the
// registry refused.
//
// It matches [ErrUnauthorized] through [errors.Is] without wrapping the
// sentinel, the same way a [StatusError] does, so a refusal says what is
// wrong once instead of stacking the word "unauthorized" onto a message that
// already carries it.
//
// Wrapping the refusal itself is deliberate where there is one. A refusal the
// adapter answered still ends the transfer when the attempts run out, and the
// error a caller reads then has to be the one that says the credentials were
// the problem — so the [StatusError] the registry sent stays reachable
// underneath, and so does the sentinel it matches.
type authError struct {
	// reason says what went wrong, in the terms of whoever has to fix it.
	reason string
	// cause is the failure underneath, when there is one: the refusal the
	// registry answered with, or the error that made a lookup impossible.
	cause error
}

// Error renders the cause first and the reason after it, so a refusal reads
// as the registry's answer followed by what bigoci made of it.
func (e *authError) Error() string {
	if e.cause == nil {
		return e.reason
	}

	return e.cause.Error() + ": " + e.reason
}

// Is makes every authentication failure match [ErrUnauthorized], whatever
// part of the exchange it happened in.
func (e *authError) Is(target error) bool {
	return target == ErrUnauthorized
}

// Unwrap exposes the failure underneath, so the [StatusError] a refusal
// carries stays reachable through [errors.As].
func (e *authError) Unwrap() error {
	return e.cause
}

// entryState is how far one scope's token has got.
type entryState int

const (
	// stateAcquiring means an exchange is in flight and the entry's ready
	// channel is still open.
	stateAcquiring entryState = iota
	// stateUnproven means a token was acquired and no request carrying it has
	// come back from the registry yet. A refusal against one of these is the
	// end of the road: refreshing a credential that has never worked would
	// only produce the same refusal again.
	stateUnproven
	// stateProven means a request carrying the token was answered with
	// something other than a refusal. A 404 proves a token as well as a 200
	// does. A refusal against one of these is a token that expired, and it is
	// worth exactly one refresh.
	stateProven
	// stateDenied means the registry refused this scope and nothing bigoci can
	// do changes that. It absorbs: every later request for the same scope
	// fails against it without sending anything.
	stateDenied
)

// entry is one scope's token: the header a request presents, when it was
// acquired, how long the registry said it is good for, and how much is known
// about whether it works.
type entry struct {
	// header is the Authorization header value a request carries, empty until
	// the acquisition that created this entry finishes.
	header string
	// acquired is the reading of the auth state's clock taken when the token
	// arrived.
	acquired time.Time
	// lifetime is how long the token is good for, or zero when it does not
	// expire — a Basic header, or a token presented verbatim.
	lifetime time.Duration
	// state is how far this entry has got.
	state entryState
	// err is what the acquisition failed with, nil while it has not.
	err error
	// ready is closed once the acquisition has finished. Requests that find an
	// exchange already in flight wait on it rather than starting a second one,
	// which is what holds a stampede of workers to a single token request.
	ready chan struct{}
}

// authState is one repository's authentication: what the registry challenged
// with, and the token held for each scope bigoci has needed.
//
// The state starts empty and stays empty against a registry that never
// challenges. That is the whole of the anonymous story: no probe goes out, no
// header is set, and a transfer costs exactly what it cost before this
// package learned to authenticate.
//
// Every field below is guarded by mu, and mu is never held across a network
// call. An exchange runs outside it and publishes its result by closing the
// entry's ready channel.
type authState struct {
	// repo is the repository these tokens are for. The auth state borrows its
	// client, its scheme, and the host and name that form a scope.
	repo *Repository
	// creds resolves what to present to the registry, nil when the caller
	// configured nothing, which is the anonymous credential.
	creds Credentials
	// now reads the clock the expiry rule is measured on. Production is
	// [time.Now], whose readings carry a monotonic reading, so a clock the
	// operating system steps cannot make a live token look dead.
	now func() time.Time

	// mu guards everything below.
	mu sync.Mutex
	// challenge is what the registry asked for, valid once challenged is set.
	challenge challenge
	// challenged records that the registry has stated a requirement.
	challenged bool
	// replied records that the registry has answered a request at all. Once it
	// has, the bodyless probe has nothing left to learn: a registry that means
	// to challenge does it on the first request it sees.
	replied bool
	// entries hold one token per scope a method needs — never per merged
	// scope, which would strand tokens; see [authState.scopesLocked].
	entries map[scopeKey]*entry
}

// newAuthState returns the auth state of one repository, with nothing
// resolved and nothing held.
func newAuthState(repo *Repository, creds Credentials, now func() time.Time) *authState {
	return &authState{
		repo:    repo,
		creds:   creds,
		now:     now,
		entries: make(map[scopeKey]*entry),
	}
}

// authorize returns the Authorization header a request built for method must
// carry, acquiring a token when there is none on hand and waiting on the
// exchange when another request already started one.
//
// The answer before the registry has challenged is no header at all. That is
// what makes a registry which never challenges cost nothing: this call takes
// a lock, reads a boolean, and returns.
func (a *authState) authorize(ctx context.Context, method string) (string, error) {
	a.mu.Lock()

	if !a.challenged {
		a.mu.Unlock()

		return "", nil
	}

	scopes, key := a.scopesLocked(method)
	held := a.entries[key]

	switch {
	case held != nil && held.state == stateAcquiring:
		a.mu.Unlock()

		return a.wait(ctx, held)
	case held != nil && held.state == stateDenied:
		err := held.err
		a.mu.Unlock()

		return "", err
	case held != nil && a.usableLocked(held):
		header := held.header
		a.mu.Unlock()

		return header, nil
	default:
		fresh := a.beginLocked(key)
		asked := a.challenge
		a.mu.Unlock()

		return a.mint(ctx, fresh, key, asked, scopes)
	}
}

// answered records the registry's reply to a request it did not refuse.
//
// Any reply at all settles what the probe exists to find out, so the probe is
// closed off here. A reply to a request that carried a credential also proves
// that credential: the registry read it, accepted it, and answered the
// question asked — and a 404 proves a token exactly as well as a 200 does,
// because a registry that did not like the token would have said so instead.
//
// What is proven is found by the header the request carried, not by the scope
// its method implies. The two are the same until a challenge arrives naming a
// scope, which changes what every later request of that method asks for and so
// changes which entry the method's scope points at. The token that came back
// is a fact about that token; a lookup that could miss it would leave a
// working credential marked as never having worked, and the next refusal
// against it would end the transfer.
func (a *authState) answered(header string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.replied = true

	if header == "" {
		return
	}

	for _, held := range a.entries {
		if held.state == stateUnproven && held.header == header {
			held.state = stateProven
		}
	}
}

// refused records that a request carrying sent was refused, and reports what
// the next request for the same access should carry instead.
//
// The rule it enforces is the one that makes a refusal safe to answer: a
// refusal is worth acting on only when acting on it changes what the next
// request will present. A credential that was itself a refresh and has never
// carried a successful request has already had its chance, so a refusal
// against one ends the scope for good and every worker behind it fails
// without sending anything.
//
// A stampede costs one exchange. Four workers whose requests are refused at
// once find, one after another, that the entry no longer holds what their
// request carried — another worker has already replaced it — and take the
// replacement rather than minting a second one.
func (a *authState) refused(ctx context.Context, method, sent string, asked challenge, status error) (string, error) {
	a.mu.Lock()

	a.challenge = asked
	a.challenged = true
	a.replied = true

	scopes, key := a.scopesLocked(method)
	held := a.entries[key]

	switch {
	case held != nil && held.state == stateAcquiring:
		a.mu.Unlock()

		return a.wait(ctx, held)
	case held != nil && held.state == stateDenied:
		err := held.err
		a.mu.Unlock()

		return "", err
	case held != nil && held.header != sent:
		header := held.header
		a.mu.Unlock()

		return header, nil
	case held != nil && held.state == stateUnproven:
		err := deniedError(status, key, a.repo.host)
		held.state = stateDenied
		held.err = err
		a.mu.Unlock()

		return "", err
	default:
		fresh := a.beginLocked(key)
		a.mu.Unlock()

		return a.mint(ctx, fresh, key, asked, scopes)
	}
}

// deny records that the credential a re-issued request carried was refused
// too, and reports the failure that ends the request.
//
// This is the second half of the two-send rule: the first refusal bought one
// refresh, and a refusal of what that refresh produced is the scope's verdict.
// The one exception is a credential another worker has already replaced, which
// is not a verdict about the scope at all — that request simply went out
// carrying something that had been retired, and repeating it will carry the
// replacement.
func (a *authState) deny(method, sent string, status error) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, key := a.scopesLocked(method)

	held := a.entries[key]
	if held == nil || held.header != sent {
		return retry.Transient(&authError{
			cause:  status,
			reason: "the credential this request carried had already been replaced; the next one carries it",
		}, 0)
	}

	if held.state == stateDenied {
		return held.err
	}

	err := deniedError(status, key, a.repo.host)
	held.state = stateDenied
	held.err = err

	return err
}

// resolve learns what the registry challenges with before a request that can
// only be sent once goes out.
//
// A body [net/http] cannot produce a second time is a body that would be
// spent by the time a challenge came back, leaving nothing to re-send. So a
// request carrying one asks first, with a bodyless version check that costs
// one round trip and happens at most once per repository.
//
// It is unreachable as this package stands: every body-bearing blob upload is
// preceded by the bodyless POST that opens its session, and any reply to that
// POST settles the question. What the guard buys is that a future refactor
// which reversed that order fails loudly here instead of hanging on a spent
// reader.
func (a *authState) resolve(ctx context.Context) error {
	a.mu.Lock()
	settled := a.challenged || a.replied
	a.mu.Unlock()

	if settled {
		return nil
	}

	endpoint := &url.URL{Scheme: a.repo.scheme, Host: a.repo.host, Path: apiPrefix}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("build the version check for %s: %w", a.repo.host, err)
	}

	resp, err := a.repo.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	drain(resp.Body)

	return a.learn(resp)
}

// learn records what the version check's response says about authentication.
//
// Only a refusal states a requirement, and only realm and service are taken
// from it. A registry-wide challenge names a scope too, but it is a placeholder
// standing for whatever repository the next request happens to address — GHCR
// sends a literal example — so reading it would ask for access to somebody
// else's repository.
func (a *authState) learn(resp *http.Response) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.replied = true

	raw := challengeHeader(resp)
	if a.challenged || !isRefusal(resp.StatusCode) || raw == "" {
		return nil
	}

	asked, err := parseChallenge(raw)
	if err != nil {
		return fmt.Errorf("GET %s: %w", apiPrefix, err)
	}

	asked.scopes = nil
	a.challenge = asked
	a.challenged = true

	return nil
}

// mint performs one token exchange for an entry and publishes its outcome to
// every request waiting on it.
func (a *authState) mint(
	ctx context.Context,
	held *entry,
	key scopeKey,
	asked challenge,
	scopes []scope,
) (string, error) {
	header, lifetime, err := a.acquire(ctx, asked, scopes)

	a.mu.Lock()
	a.finishLocked(held, key, header, lifetime, err)
	a.mu.Unlock()

	close(held.ready)

	if err != nil {
		return "", err
	}

	return header, nil
}

// wait blocks until the exchange another request started has finished, and
// reports what it produced. A caller whose own context ends first stops
// waiting and says so.
func (a *authState) wait(ctx context.Context, held *entry) (string, error) {
	select {
	case <-held.ready:
	case <-ctx.Done():
		return "", fmt.Errorf("wait for a registry token: %w", ctx.Err())
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if held.err != nil {
		return "", held.err
	}

	return held.header, nil
}

// beginLocked puts a fresh in-flight entry at key, replacing whatever was
// there, and returns it. The caller holds mu.
func (a *authState) beginLocked(key scopeKey) *entry {
	fresh := &entry{state: stateAcquiring, ready: make(chan struct{})}
	a.entries[key] = fresh

	return fresh
}

// finishLocked records an acquisition's outcome on its entry. The caller holds
// mu.
//
// A refused credential is kept, denied, so every worker behind it fails at
// once and no second exchange goes out. Anything else — a token endpoint
// having a bad minute, an answer that was not a token document — is this
// attempt failing rather than this credential being wrong, so the entry is
// dropped and a later attempt starts over.
func (a *authState) finishLocked(held *entry, key scopeKey, header string, lifetime time.Duration, err error) {
	if err == nil {
		held.header = header
		held.acquired = a.now()
		held.lifetime = lifetime
		held.state = stateUnproven

		return
	}

	held.err = err

	if errors.Is(err, ErrUnauthorized) {
		held.state = stateDenied

		return
	}

	if a.entries[key] == held {
		delete(a.entries, key)
	}
}

// scopesLocked returns the scope set a request of method asks a token endpoint
// for, and the key that token is cached under. The caller holds mu.
//
// The two are drawn from different places on purpose. What is asked for
// includes whatever the registry's last challenge said it wanted; what the
// answer is filed under is the access the method itself needs, and nothing
// else. [mergeScopes] says why.
func (a *authState) scopesLocked(method string) ([]scope, scopeKey) {
	want := scopeFor(method, a.repo.name)

	return mergeScopes(want, a.challenge.scopes), scopeKey(want)
}

// usableLocked reports whether an entry's token is still worth presenting.
// The caller holds mu.
//
// The token is given up a margin before the registry says it ends, so a
// request that passes the check still authenticates when it arrives. The
// margin is half of a short lifetime and thirty seconds of a long one, and it
// rests on one property of registries: a request is authorized when its
// headers are read, not when its body finishes. A part large enough to take
// longer than a whole token lifetime to upload would otherwise be
// unauthorized by the time it landed.
//
// A credential with no stated lifetime does not expire and is always usable.
func (a *authState) usableLocked(held *entry) bool {
	if held.lifetime == 0 {
		return true
	}

	return a.now().Sub(held.acquired) < held.lifetime-marginFor(held.lifetime)
}

// marginFor returns how long before a token's stated end bigoci stops
// presenting it.
func marginFor(lifetime time.Duration) time.Duration {
	if half := lifetime / shortTokenShare; half < tokenMargin {
		return half
	}

	return tokenMargin
}

// deniedError is the terminal verdict on one scope: what bigoci presented was
// refused, and no further request can change it. The message names the access
// that was refused, because a credential that reads a repository but cannot
// write it fails here and nowhere else.
func deniedError(status error, key scopeKey, host string) error {
	return &authError{
		cause: status,
		reason: fmt.Sprintf(
			`the credentials bigoci presented were refused for %s; run "docker login %s"`, key, host,
		),
	}
}
