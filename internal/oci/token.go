package oci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/imgoci/bigoci/internal/retry"
)

// The query parameters a token request carries beside whatever the realm
// already had.
const (
	// tokenServiceParam names the registry the token is for, in the issuer's
	// own vocabulary rather than bigoci's.
	tokenServiceParam = "service"
	// tokenScopeParam names one access grant. It repeats, once per grant.
	tokenScopeParam = "scope"
)

// tokenBodyLimit caps the token endpoint's answer. A token document is a few
// hundred bytes; anything past this is not one.
const tokenBodyLimit = 64 << 10

// defaultTokenLifetime is how long a token is treated as good for when the
// token endpoint did not say. It is the distribution spec's own default, and
// it is the value that governs at registries which send no expires_in at all.
const defaultTokenLifetime = 60 * time.Second

// tokenResponse is the token endpoint's answer, reduced to what bigoci reads
// off it.
type tokenResponse struct {
	// Token is the bearer token under the distribution spec's field name.
	Token string `json:"token"`
	// AccessToken is the same value under the OAuth2 field name. Registries
	// send one, the other, or both, so both are read and the spec's name wins.
	AccessToken string `json:"access_token"`
	// ExpiresIn is how many seconds the token is good for. An absent field
	// reads as zero, which is the same answer as a nonsense one and takes the
	// same default.
	ExpiresIn int `json:"expires_in"`
}

// acquire produces the Authorization header value that answers a challenge,
// together with how long it is good for — zero when it does not expire.
//
// The order of the branches is what keeps a credential from being quietly
// downgraded. A credential bigoci cannot use is refused out loud before
// anything is sent, because the alternative is an anonymous exchange that
// succeeds, a pull that works, and a push that fails much later for a reason
// nobody can connect to the credential that was never presented.
func (a *authState) acquire(ctx context.Context, asked challenge, scopes []scope) (string, time.Duration, error) {
	cred, err := a.credential(ctx)
	if err != nil {
		return "", 0, err
	}

	if identityOnly(cred) {
		return "", 0, &authError{
			reason: fmt.Sprintf(
				"the stored credential for %s is an identity token, which bigoci cannot exchange; "+
					`run "docker login %s" with a password or an access token instead`,
				a.repo.host, a.repo.host,
			),
		}
	}

	if cred.RegistryToken != "" {
		return bearerHeader(cred.RegistryToken), 0, nil
	}

	if asked.scheme == schemeBasic {
		header, err := basicHeader(cred, a.repo.host)

		return header, 0, err
	}

	return a.exchange(ctx, asked, scopes, cred)
}

// credential resolves what to present to the registry this repository is on.
// A caller who configured no resolver gets the anonymous credential, which is
// an answer rather than an absence: the bearer exchange runs on it, and
// registries that hand out public-access tokens hand one over.
func (a *authState) credential(ctx context.Context) (Credential, error) {
	if a.creds == nil {
		return Credential{}, nil
	}

	cred, err := a.creds.Credential(ctx, Registry(a.repo.host))
	if err != nil {
		return Credential{}, fmt.Errorf("look up the credential for %s: %w", a.repo.host, err)
	}

	return cred, nil
}

// exchange asks the realm a bearer challenge named for a token covering
// scopes.
//
// It is a GET carrying the credential in a Basic header, which is what the
// distribution spec's token flow defines. The OAuth2 grant that posts the
// secret in a form body is never used: it puts a secret in a request body,
// where nothing bigoci does can promise to keep it out of a log, and every
// registry bigoci talks to accepts the GET.
//
// The exchange rides the repository's cookie-free external client, so the
// caller's transport still sees it but a Cookie Jar contributes no ambient
// authority. A redirect from the realm is terminal rather than followed. The
// exchange carries a Basic header, a redirected token endpoint is a shape no
// measured registry has, and failing loudly beats deciding where a credential
// goes next on a token server's say-so. Its failures classify through the same
// table a blob request's do — a token endpoint answering 503 is worth another
// attempt for the same reason a registry answering 503 is.
func (a *authState) exchange(
	ctx context.Context,
	asked challenge,
	scopes []scope,
	cred Credential,
) (string, time.Duration, error) {
	endpoint, err := validateRealm(asked.realm, a.repo.scheme, a.repo.host)
	if err != nil {
		return "", 0, err
	}
	endpoint.RawQuery = tokenQuery(endpoint, asked.service, scopes)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", 0, &authError{reason: "the registry's bearer challenge cannot form a token request"}
	}

	// The gate is the whole credential, not the user name: a secret with no
	// user beside it — a token pasted into the password field, an auth entry
	// decoding to ":token" — is still presented, as Basic with an empty user,
	// because an exchange that quietly ran anonymously with a credential in
	// hand is the silent downgrade this package exists to refuse.
	if !cred.Empty() {
		req.SetBasicAuth(cred.Username, cred.Password)
	}

	resp, err := a.repo.doExternal(req)
	if err != nil {
		var targetErr *externalTargetError
		if errors.As(err, &targetErr) {
			return "", 0, &authError{reason: targetErr.Error()}
		}

		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, tokenStatusError(resp)
	}

	return readToken(resp)
}

// tokenStatusError reports a token endpoint's status without admitting its
// path or response body into the public error. It retains the same status
// sentinels, transient table, and Retry-After behavior as a registry status.
func tokenStatusError(resp *http.Response) error {
	drain(resp.Body)

	err := &StatusError{
		Method:     http.MethodGet,
		Path:       "token endpoint",
		Status:     resp.StatusCode,
		RetryAfter: retryAfter(resp),
	}
	if transientStatus(err.Status) {
		return retry.Transient(err, err.RetryAfter)
	}

	return err
}

// tokenQuery renders a token request's query: whatever the realm already
// carried, the service the challenge named, and one scope parameter per
// merged grant.
//
// Repeating the parameter is how the distribution spec spells a set of
// grants, and registries accept the repeats. The realm's own query is kept
// because a realm is a URL the registry chose, and dropping half of it would
// be bigoci deciding it knows the endpoint better than the registry does.
func tokenQuery(endpoint *url.URL, service string, scopes []scope) string {
	query := endpoint.Query()

	if service != "" {
		query.Set(tokenServiceParam, service)
	}

	for _, one := range scopes {
		query.Add(tokenScopeParam, string(one))
	}

	return query.Encode()
}

// readToken reads a token out of the endpoint's answer.
//
// An answer that is not a token document, or one carrying no token at all, is
// terminal and deliberately not a refusal: the registry said the exchange
// succeeded. Reporting it as unauthorized would send someone off to fix
// credentials that were never the problem.
func readToken(resp *http.Response) (string, time.Duration, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenBodyLimit+1))
	if err != nil {
		return "", 0, retry.Transient(safeCause("read the token endpoint's answer", err), 0)
	}

	if len(body) > tokenBodyLimit {
		return "", 0, fmt.Errorf("the token endpoint's answer is larger than the %d byte limit", tokenBodyLimit)
	}

	var answer tokenResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", 0, errors.New("the token endpoint's answer is not a token document")
	}

	token := answer.Token
	if token == "" {
		token = answer.AccessToken
	}

	if token == "" {
		return "", 0, errors.New("the token endpoint answered with a document carrying no token")
	}

	return bearerHeader(token), lifetimeOf(answer.ExpiresIn), nil
}

// lifetimeOf turns the token endpoint's expires_in into the lifetime the
// expiry rule measures against.
//
// Absent, zero, and negative all take the spec's default. A registry that
// sends no lifetime is common enough to be the normal case rather than an
// edge one, and treating it as "forever" would hand every long transfer a
// token that stopped working somewhere in the middle.
func lifetimeOf(expiresIn int) time.Duration {
	if expiresIn <= 0 {
		return defaultTokenLifetime
	}

	return time.Duration(expiresIn) * time.Second
}

// basicHeader answers a Basic challenge with the credential's user name and
// secret. A registry that asks for one and a caller who configured none have
// nothing to agree on, and no exchange exists that would change it.
func basicHeader(cred Credential, host string) (string, error) {
	if cred.Empty() {
		return "", &authError{
			reason: fmt.Sprintf(
				`%s asked for a user name and password and none is configured; run "docker login %s"`, host, host,
			),
		}
	}

	return "Basic " + base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Password)), nil
}

// bearerHeader renders a bearer token as an Authorization header value.
func bearerHeader(token string) string {
	return "Bearer " + token
}

// identityOnly reports whether the only secret a credential carries is an
// OAuth2 identity token, which is the shape a native credential store returns
// for logins that stored a refresh token instead of a password.
func identityOnly(cred Credential) bool {
	return cred.IdentityToken != "" && cred.Password == "" && cred.RegistryToken == ""
}
