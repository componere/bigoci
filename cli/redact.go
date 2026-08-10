package main

import (
	"crypto/sha256"
	"fmt"
	"io"
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
	// responseHeaderPresent says a peer header was present without rendering any
	// of its bytes. A registry sees request credentials and can reflect them into
	// an otherwise ordinary response header.
	responseHeaderPresent = "present"
	// redactedTargetPath replaces the path and query of a withheld request to
	// the registry itself.
	redactedTargetPath = "/_redacted"
	// offOriginTarget replaces every URL outside the first registry origin. The
	// reserved invalid domain remains a parseable URL without naming the peer.
	offOriginTarget = "https://off-origin.invalid/_redacted"
	// redactedTargetError replaces every transport error detail. Besides
	// repeating a target URL, a protocol error can include raw peer response
	// bytes that reflect an Authorization credential.
	redactedTargetError = "transport failure detail redacted"
	// tokenScopeParameter is added by bigoci to every bearer-token exchange.
	// It identifies an exchange even when a peer chooses a realm path that
	// masquerades as a distribution endpoint.
	tokenScopeParameter = "scope"
	// sha256HexLen is how many hex bytes follow the algorithm in a sha256
	// digest.
	sha256HexLen = 64
)

// locationTarget is the fixed-size identity of a resolved server-issued
// Location. Retaining a digest rather than peer-controlled URL strings bounds
// the tap's live memory by the number of Locations, not their byte length. A
// hash collision can only over-redact an unrelated target; it cannot make a
// recorded target visible.
type locationTarget [sha256.Size]byte

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
// A Location is resolved against base, withheld, and remembered so a later
// request to its target stays withheld. Every other peer header is represented
// only by presence because a registry can reflect a request credential into
// any otherwise ordinary header value.
func (t *tap) responseHeaderFields(base *url.URL, h http.Header) string {
	return fmt.Sprintf(
		"ctype=%s crange=%s loc=%s ddigest=%s retry-after=%s challenge=%s",
		responsePresenceField(h, "Content-Type"),
		responsePresenceField(h, "Content-Range"),
		quoteOrAbsent(t.renderLocation(base, h.Get("Location"))),
		responsePresenceField(h, "Docker-Content-Digest"),
		responsePresenceField(h, "Retry-After"),
		challengeField(h),
	)
}

// renderTargetURL applies the tap's origin and Location-derived target
// boundaries to one request target.
// Normal distribution paths remain useful on the registry itself; token and
// other same-origin paths, and every off-origin target, become stable URLs that
// retain the frozen log field grammar without retaining peer credentials.
func (t *tap) renderTargetURL(target *url.URL, kind class) string {
	if target == nil {
		return ""
	}
	if !t.sameRegistryOrigin(target) {
		return offOriginTarget
	}
	if t.isLocationTarget(target) || redactSameRegistryTarget(target, kind) {
		safe := url.URL{
			Scheme: t.registryScheme,
			Host:   t.registryHost,
			Path:   redactedTargetPath,
		}

		return safe.String()
	}

	return redactURL(target)
}

// redactSameRegistryTarget reports whether a request at the registry origin
// loses its path and query. Besides class=other endpoints, this catches bearer
// exchanges whose peer-chosen realm path mimics a distribution endpoint: the
// library itself adds a scope query parameter to every such exchange.
func redactSameRegistryTarget(target *url.URL, kind class) bool {
	return kind == classOther ||
		(target != nil && target.RawQuery != "" && target.Query().Has(tokenScopeParameter))
}

// sameRegistryOrigin reports whether target shares the first request's scheme
// and host. The first request is the registry request that begins a transfer;
// token exchanges and redirect targets can only follow its response.
func (t *tap) sameRegistryOrigin(target *url.URL) bool {
	if target == nil {
		return false
	}

	t.registryOnce.Do(func() {
		t.registryScheme = target.Scheme
		t.registryHost = target.Host
	})

	return strings.EqualFold(target.Scheme, t.registryScheme) &&
		strings.EqualFold(target.Host, t.registryHost)
}

// rememberLocationTarget records a resolved server-issued Location target.
// Every later request sharing its origin and escaped path is redacted even when
// the peer chose a path that resembles a public distribution endpoint.
func (t *tap) rememberLocationTarget(target *url.URL) {
	if target == nil {
		return
	}
	identity := locationTargetOf(target)

	t.locationsMu.Lock()
	t.locationTargets[identity] = struct{}{}
	t.locationsMu.Unlock()
}

// isLocationTarget reports whether target shares the origin and escaped path
// of a server-issued Location. Query is intentionally ignored because upload
// completion adds a digest and signed redirects may rotate query parameters.
func (t *tap) isLocationTarget(target *url.URL) bool {
	if target == nil {
		return false
	}
	identity := locationTargetOf(target)

	t.locationsMu.RLock()
	_, exists := t.locationTargets[identity]
	t.locationsMu.RUnlock()

	return exists
}

// locationTargetOf reduces target to the stable identity used across a
// Location response and the request that follows it. NUL separators make the
// component boundary unambiguous; these parsed URL components cannot contain a
// raw NUL, and EscapedPath renders one as percent-encoding.
func locationTargetOf(target *url.URL) locationTarget {
	digest := sha256.New()
	_, _ = io.WriteString(digest, strings.ToLower(target.Scheme))
	_, _ = io.WriteString(digest, "\x00")
	_, _ = io.WriteString(digest, strings.ToLower(target.Host))
	_, _ = io.WriteString(digest, "\x00")
	_, _ = io.WriteString(digest, target.EscapedPath())

	var identity locationTarget
	digest.Sum(identity[:0])

	return identity
}

// renderLocation resolves one response Location and applies the same origin
// boundary as a request URL. Every resolved target is remembered before it is
// rendered, so neither the Location nor a later request can reveal it.
func (t *tap) renderLocation(base *url.URL, location string) string {
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
	t.rememberLocationTarget(target)

	return t.renderTargetURL(target, locationClass(target.Path))
}

// locationClass assigns the original distribution class from path alone so a
// Location-derived request keeps the frozen class field even while hidden.
func locationClass(path string) class {
	switch {
	case strings.Contains(path, "/blobs/uploads/"):
		return classBlobWrite
	case strings.Contains(path, "/blobs/"):
		return classBlobRead
	case strings.Contains(path, "/manifests/"):
		return classManifestRead
	default:
		return classOther
	}
}

// redactURL renders a same-registry distribution URL with nothing secret left
// in it. The tap applies its origin and request-class boundary before calling
// this function.
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

// redactQuery renders a distribution query with its parameter names kept and
// sorted and its values elided.
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

// responsePresenceField reports whether a peer response header was present
// without rendering or interpreting any of its values.
func responsePresenceField(h http.Header, name string) string {
	if len(h.Values(name)) == 0 {
		return absent
	}

	return strconv.Quote(responseHeaderPresent)
}

// challengeField reports whether any authentication challenge header value was
// present, without rendering the challenge or interpreting its syntax.
func challengeField(h http.Header) string {
	return responsePresenceField(h, "Www-Authenticate")
}
