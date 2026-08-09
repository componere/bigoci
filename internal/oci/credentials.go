package oci

import (
	"context"
	"net/http"
	"slices"
)

// The action lists a token request ever asks for. The distribution spec
// spells a repository scope as "repository:<name>:<actions>", and bigoci
// needs exactly two of them.
const (
	// actionsPull covers the reads: a blob check, a blob read, a manifest
	// read.
	actionsPull = "pull"
	// actionsPushPull covers everything else, which is every write and the
	// upload session that precedes one.
	actionsPushPull = "pull,push"
)

// Registry names one registry by host, with a port when the reference that
// named it carried one: "ghcr.io", "127.0.0.1:5000".
//
// It is the key a credential is looked up under, and it is always the host
// bigoci dialed. A bearer challenge names the same registry again in the
// issuer's own vocabulary, in its service parameter, and that name is never
// used as a lookup key: a registry that could choose which credential bigoci
// presents would be choosing which secret leaves the machine.
type Registry string

// Credential is what bigoci presents to one registry. It mirrors the shape a
// Docker configuration file stores, because that file is where a credential
// usually comes from.
//
// The zero value is the anonymous credential, which is a credential and not
// the absence of one: bigoci still performs the token exchange with it,
// because registries that require a bearer token for public reads answer an
// unauthenticated token request with a public-access token.
type Credential struct {
	// Username is the account name presented to the token endpoint.
	Username string
	// Password is the secret, or the personal access token, that goes with
	// Username.
	Password string
	// IdentityToken is the OAuth2 refresh token some logins store in place of
	// a password. bigoci cannot exchange one, and reads the field so it can
	// say so rather than quietly presenting nothing at all.
	IdentityToken string
	// RegistryToken is a bearer token to present verbatim, with no exchange.
	RegistryToken string
}

// Empty reports whether the credential carries nothing to present, which is
// the anonymous credential.
func (c Credential) Empty() bool {
	return c.Username == "" && c.Password == "" && c.IdentityToken == "" && c.RegistryToken == ""
}

// Credentials resolves the credential bigoci should present to one registry.
//
// A registry the resolver knows nothing about is the zero [Credential] and a
// nil error: anonymous is an answer, not a failure. An error means the lookup
// itself could not be performed — an unreadable configuration file, a
// credential helper that would not run — and it ends the transfer, because a
// transfer that quietly fell back to anonymous would fail later and somewhere
// less obvious.
//
// Implementations must be safe for concurrent use and must not retry: a
// lookup runs inside an attempt the orchestrator is already counting.
type Credentials interface {
	// Credential returns what bigoci should present to registry.
	Credential(ctx context.Context, registry Registry) (Credential, error)
}

// WithCredentials answers a registry's challenges with what c resolves for
// the registry this repository is on. A nil Credentials is ignored, so a
// caller may pass one through unconditionally.
//
// Leaving it unset does not turn authentication off. A registry that
// challenges still gets the full bearer exchange, made with the anonymous
// credential, because that is what registries which require a token for
// public reads expect. It only means bigoci has no user name or secret to
// offer when the exchange asks for one.
func WithCredentials(c Credentials) Option {
	return func(s *settings) {
		if c != nil {
			s.creds = c
		}
	}
}

// scope is one access grant a token covers, in the distribution spec's
// "repository:<name>:<actions>" grammar.
//
// It stays inside this package. Nothing above the adapter names a scope, and
// nothing below it needs to: the core asks for a blob and this package works
// out what access reading one takes.
type scope string

// scopeKey is the cache key one repository's token is held under: the access
// the request that asked for it needed, which is one of the two [scopeFor]
// returns.
type scopeKey string

// scopeFor returns the scope a request of this method needs against the
// repository named name.
//
// Deriving it from the method alone is what holds the token cache to two
// entries per repository, and it is why bigoci never asks for push access on
// a pull: an anonymous request for "pull,push" is refused outright at some
// registries, which would break every anonymous pull that a wider scope was
// meant to make cheaper.
func scopeFor(method, name string) scope {
	if method == http.MethodGet || method == http.MethodHead {
		return scope("repository:" + name + ":" + actionsPull)
	}

	return scope("repository:" + name + ":" + actionsPushPull)
}

// mergeScopes returns the grant set a token request asks for: the access this
// request needs, widened by whatever the registry's challenge asked for.
//
// The union is deduplicated and sorted, so a challenge changes what is asked
// for but never the order it is asked in, and a registry sees the same request
// however it happened to spell the scope it wanted.
//
// What is deliberately not derived from this set is the key the resulting
// token is cached under. A challenge is a moving thing — registries state the
// scope of the request they refused, so the set changes with every refusal of
// a different method — and a key that moved with it would file a token under
// one name and go looking for it under another, losing track of which token
// had been proven and which had not. The access a method needs never moves,
// which is what holds the cache to two entries per repository.
func mergeScopes(want scope, offered []string) []scope {
	merged := make([]scope, 0, len(offered)+1)
	merged = append(merged, want)

	for _, one := range offered {
		merged = append(merged, scope(one))
	}

	slices.Sort(merged)

	return slices.Compact(merged)
}
