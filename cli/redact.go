package main

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const (
	// absent is what a log line writes where a header was not present at all.
	// It is bare and never quoted, so it cannot be read as a header whose value
	// happened to be a dash.
	absent = "-"
	// elided replaces the value of every query parameter but the digest.
	elided = "…"
	// authNone is the auth field of a request that carried no credential.
	authNone = "none"
	// authBearer is the auth field of a request that carried a bearer token.
	authBearer = "bearer"
	// authBasic is the auth field of a request that carried basic credentials.
	authBasic = "basic"
	// authOther is the auth field of a request whose credential used some other
	// scheme, or none this CLI could read a scheme out of.
	authOther = "other"
	// challengeLimit is how many bytes of an authentication challenge a log line
	// keeps. A challenge names a realm and a scope and carries no secret, but it
	// can run long.
	challengeLimit = 200
	// sha256HexLen is how many hex bytes follow the algorithm in a sha256
	// digest.
	sha256HexLen = 64
)

// authScheme names the kind of credential a request carried, and nothing else.
//
// The answer is exactly one of none, bearer, basic, or other. The credential
// itself is unrepresentable in this CLI's output: no prefix of it, no length, no
// fingerprint, nothing that narrows a guess. That is a property of there being
// no code that could render one, not of remembering not to.
func authScheme(h http.Header) string {
	value := h.Get("Authorization")
	if value == "" {
		return authNone
	}

	scheme, _, _ := strings.Cut(value, " ")
	switch strings.ToLower(scheme) {
	case authBearer:
		return authBearer
	case authBasic:
		return authBasic
	default:
		return authOther
	}
}

// requestHeaderFields renders the request headers a log line may show, in the
// order the line writes them.
//
// The three named here are the whole allow-list for a request, alongside the
// scheme of the Authorization header. There is no escape hatch and no way to add
// one at runtime: a cookie, a private header, anything a later phase starts
// sending, has no path to the log at all.
func requestHeaderFields(h http.Header) string {
	return fmt.Sprintf(
		"type=%s range=%s accept=%s",
		headerField(h, "Content-Type"), headerField(h, "Range"), headerField(h, "Accept"),
	)
}

// responseHeaderFields renders the response headers a log line may show, in the
// order the line writes them, and is the whole allow-list for a response.
//
// A Location is resolved against base, the URL of the request that got it, and
// then redacted like any other URL. A challenge is kept as it came, truncated,
// because it is what a later phase will ask a token for and it holds nothing
// secret.
func responseHeaderFields(base *url.URL, h http.Header) string {
	return fmt.Sprintf(
		"ctype=%s crange=%s loc=%s ddigest=%s retry-after=%s challenge=%s",
		headerField(h, "Content-Type"),
		headerField(h, "Content-Range"),
		quoteOrAbsent(redactLocation(base, h.Get("Location"))),
		headerField(h, "Docker-Content-Digest"),
		headerField(h, "Retry-After"),
		quoteOrAbsent(truncateChallenge(h.Get("Www-Authenticate"))),
	)
}

// redactURL renders u for a log line with nothing secret left in it.
//
// Userinfo is dropped outright. Every query parameter keeps its name and loses
// its value, except a digest that verifiably is one, which is public and is
// what correlates a line with a blob. Names are sorted so two runs of the same
// transfer produce the same text. The path is rendered as it stands, digests
// and all: being able to grep a digest out of the log is worth more than a
// shorter line.
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}

	safe := *u
	safe.User = nil
	if safe.RawQuery != "" {
		safe.RawQuery = redactQuery(safe.RawQuery)
	}

	return safe.String()
}

// redactQuery renders a query with its parameter names kept and sorted and its
// values elided.
//
// Everything re-emitted here lands verbatim in the log line, and every byte of
// it was chosen by the peer being logged, so nothing goes out unescaped: names
// are query-escaped again, and the one value that may pass — a digest — only
// does when [isDigest] says its bytes really are one. A query Go cannot parse
// is summarized as the elision mark alone rather than rendered partially, so a
// line never shows a shorter query than the request carried.
func redactQuery(rawQuery string) string {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return elided
	}

	var b strings.Builder
	for i, name := range slices.Sorted(maps.Keys(values)) {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(name))
		b.WriteByte('=')

		if value := values.Get(name); name == "digest" && isDigest(value) {
			b.WriteString(value)

			continue
		}
		b.WriteString(elided)
	}

	return b.String()
}

// isDigest reports whether value is a sha256 digest, the only kind the format's
// first version writes.
//
// The check is on the value, not the parameter's name, because the name is the
// peer's to choose: a host that calls its signed token "digest" must still see
// it elided, and nothing that passes this check can carry one — sixty-four hex
// bytes name a blob and split no log field.
func isDigest(value string) bool {
	const prefix = "sha256:"
	rest, ok := strings.CutPrefix(value, prefix)
	if !ok || len(rest) != sha256HexLen {
		return false
	}

	for _, c := range []byte(rest) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// redactLocation resolves the Location a response gave against base, the URL of
// the request that got it, and then redacts it like any other URL.
//
// An empty or unparseable Location renders as absent, because there is nothing
// there worth showing and a redirect target is not a place to print bytes the
// CLI did not understand.
func redactLocation(base *url.URL, location string) string {
	if location == "" {
		return ""
	}

	target, err := url.Parse(location)
	if err != nil {
		return ""
	}
	if base != nil {
		target = base.ResolveReference(target)
	}

	return redactURL(target)
}

// headerField renders one allow-listed header for a log line.
func headerField(h http.Header, name string) string {
	return quoteOrAbsent(h.Get(name))
}

// quoteOrAbsent quotes a value for a log line, or writes the bare dash that means
// the field was not there.
//
// Quoting is what keeps one line one line and one field one field, whatever a
// registry decided to put in a header.
func quoteOrAbsent(value string) string {
	if value == "" {
		return absent
	}

	return strconv.Quote(value)
}

// truncateChallenge cuts an authentication challenge to the length a log line
// keeps and marks that it did.
func truncateChallenge(challenge string) string {
	if len(challenge) <= challengeLimit {
		return challenge
	}

	return challenge[:challengeLimit] + elided
}
