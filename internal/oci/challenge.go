package oci

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The authentication schemes this package can answer, lowercased because
// [parseChallenge] folds every scheme name it reads.
const (
	// schemeBearer is the scheme the distribution spec's token flow uses: the
	// registry names an endpoint, bigoci asks it for a token, and the token
	// rides on the next request.
	schemeBearer = "bearer"
	// schemeBasic is the scheme a registry names when it takes a user name and
	// password straight off the request.
	schemeBasic = "basic"
)

// tcharSpecials are the punctuation characters RFC 9110 allows in a token,
// beside the letters and digits.
const tcharSpecials = "!#$%&'*+-.^_`|~"

// challengeLimit caps how much of a WWW-Authenticate header this package
// reads. RFC 9110 sets no limit, and a real challenge is well under a
// kilobyte; a header past this is not a challenge but whatever else turned
// out to be on the other end of the connection.
const challengeLimit = 8 << 10

// challenge is what a registry answered a refused request with, reduced to
// the three parameters a token exchange has any use for.
type challenge struct {
	// scheme is the authentication scheme, lowercased.
	scheme string
	// realm is the token endpoint a bearer challenge names. It is empty for a
	// Basic challenge, which needs no endpoint.
	realm string
	// service is what the token endpoint expects in its service parameter,
	// empty when the challenge named none.
	service string
	// scopes are the access grants the challenge asked for, which a token
	// request asks for on top of what the method itself needs.
	scopes []string
}

// set records one auth-param, ignoring every parameter the token exchange has
// no use for. Names are matched case-insensitively, as RFC 9110 requires.
//
// A scope parameter carries a space-separated list, so it is split on
// whitespace and never on commas: the first challenge bigoci meets in the
// wild spells a scope "repository:team/artifact:pull,push", and a comma is
// part of that value rather than a separator.
func (c *challenge) set(name, value string) {
	switch strings.ToLower(name) {
	case "realm":
		c.realm = value
	case "service":
		c.service = value
	case "scope":
		c.scopes = strings.Fields(value)
	}
}

// challengeHeader returns everything a response said in WWW-Authenticate, as
// the single comma-joined value RFC 9110 makes several field lines equivalent
// to. Reading only the first line would make the choice of scheme depend on
// which line a registry happened to write first: one that states Basic and
// Bearer as separate lines must still have its Bearer seen.
func challengeHeader(resp *http.Response) string {
	return strings.Join(resp.Header.Values(headerChallenge), ", ")
}

// parseChallenge reads a WWW-Authenticate header and returns the challenge
// bigoci will answer: the Bearer one where the registry offered it, the Basic
// one otherwise.
//
// The header is a list, and its grammar is why this is a scanner rather than
// a split: a comma separates challenges and parameters alike, but a comma
// inside a quoted value separates nothing. Splitting on commas breaks on the
// very first scope a registry sends.
//
// Everything unusable — an absent header, one this package cannot read, a
// scheme it does not implement, a bearer challenge naming no realm — comes
// back as an [authError], which matches [ErrUnauthorized]. The caller cannot
// authenticate and no further request would change that.
func parseChallenge(header string) (challenge, error) {
	if header == "" {
		return challenge{}, &authError{
			reason: "the registry refused the request without saying how to authenticate",
		}
	}

	if len(header) > challengeLimit {
		return challenge{}, &authError{
			reason: "the registry answered with a WWW-Authenticate header of " +
				strconv.Itoa(len(header)) + " bytes, past the " + strconv.Itoa(challengeLimit) + " byte limit",
		}
	}

	offered, err := scanChallenges(header)
	if err != nil {
		return challenge{}, err
	}

	return pickChallenge(offered)
}

// scanChallenges reads every challenge the header lists, in the order they
// appear.
//
// One token can be either a scheme name or a parameter name, and only what
// follows tells them apart: a parameter is followed by an equals sign and a
// scheme is not. That single lookahead is the whole of the ambiguity RFC 9110
// leaves in the grammar.
func scanChallenges(header string) ([]challenge, error) {
	scan := &scanner{header: header}

	var offered []challenge

	for {
		scan.delims()
		if scan.done() {
			return offered, nil
		}

		name, ok := scan.token()
		if !ok {
			return nil, unreadableChallenge()
		}

		if !scan.equals() {
			offered = append(offered, challenge{scheme: strings.ToLower(name)})

			continue
		}

		value, ok := scan.value()
		if !ok {
			if len(offered) == 0 {
				return nil, unreadableChallenge()
			}

			// A value this scanner cannot read — RFC 9110's other production,
			// a token68 credential such as "Negotiate abc==" — costs the
			// challenge it belongs to, not the whole header: a usable Bearer
			// beside it must survive. The challenge is voided in place rather
			// than removed, so anything else attaching to it before the next
			// scheme lands somewhere no one will read.
			offered[len(offered)-1] = challenge{}
			scan.resync()

			continue
		}
		if len(offered) == 0 {
			return nil, unreadableChallenge()
		}

		offered[len(offered)-1].set(name, value)
	}
}

// pickChallenge chooses which of the challenges a registry offered bigoci
// answers.
//
// Bearer wins wherever it appears, because it is the scheme the distribution
// spec's token flow uses and the only one that can carry a scope. Basic is
// the fallback. A bearer challenge that names no realm is refused rather than
// demoted to the Basic beside it: a registry that offered a token endpoint
// and then failed to name it is broken in a way worth reporting.
func pickChallenge(offered []challenge) (challenge, error) {
	fallback := -1

	for i, one := range offered {
		switch one.scheme {
		case schemeBearer:
			if one.realm == "" {
				return challenge{}, &authError{
					reason: "the registry's bearer challenge names no realm",
				}
			}

			return one, nil
		case schemeBasic:
			if fallback < 0 {
				fallback = i
			}
		}
	}

	if fallback >= 0 {
		return offered[fallback], nil
	}

	return challenge{}, &authError{
		reason: "the registry asked for an authentication scheme bigoci does not implement",
	}
}

// validateRealm checks the realm a bearer challenge named and returns the URL
// the token request goes to.
//
// The realm may point anywhere, including at a host other than the registry —
// the protocol requires that, and every large registry uses it. What makes
// the freedom safe is on the other side of the exchange: the credential
// bigoci looks up is always the one for the host it dialed, never one for the
// host the challenge named.
//
// What the realm may not be is anything that changes what leaves the machine
// or where it lands. It must be absolute and https, because a token is a
// bearer secret and http would hand it to anyone watching; repoScheme relaxes
// that to http only for the local registries [WithPlainHTTP] exists for, and
// then only to the repository's own host — a plain-http realm anywhere else
// is a cleartext path carrying the user's credential to a third party. It
// may not carry userinfo, which [net/http] would turn into a Basic header of
// the registry's choosing, and it may not carry a fragment, which no request
// would ever send. A different host may not be a local or private IP literal:
// a public registry does not get to turn token acquisition into a request to
// an internal service. Its own query is kept and the exchange's parameters
// are merged on top.
//
// No failure renders any part of the realm. Registry-selected URLs routinely
// carry bearer tickets in their path and query, and malformed challenge text
// has no safe substring to quote into a public error.
func validateRealm(realm, repoScheme, repoHost string) (*url.URL, error) {
	endpoint, err := url.Parse(realm)
	if err != nil {
		return nil, &authError{reason: "the registry's bearer challenge names a realm that is not a URL"}
	}

	secure := endpoint.Scheme == schemeHTTPS || (endpoint.Scheme == schemeHTTP && repoScheme == schemeHTTP)
	if !secure {
		return nil, &authError{reason: "the registry's bearer challenge names a realm that is not an https URL"}
	}

	if endpoint.Scheme == schemeHTTP && endpoint.Host != repoHost {
		return nil, &authError{
			reason: "the registry's bearer challenge names a plain-http realm on another host, " +
				"which would carry the credential in the clear to a third party",
		}
	}

	if endpoint.Host == "" {
		return nil, &authError{reason: "the registry's bearer challenge names a realm with no host"}
	}

	if endpoint.User != nil {
		return nil, &authError{reason: "the registry's bearer challenge names a realm carrying a user name"}
	}

	if endpoint.Fragment != "" {
		return nil, &authError{reason: "the registry's bearer challenge names a realm carrying a fragment"}
	}

	if restrictedIPTarget(endpoint.Hostname(), repoHost) {
		return nil, &authError{
			reason: "the registry's bearer challenge names a local or private IP address " +
				"that is not the registry",
		}
	}

	return endpoint, nil
}

// unreadableChallenge reports a header this package could not read without
// repeating registry-controlled challenge bytes into the public error.
func unreadableChallenge() error {
	return &authError{
		reason: "the registry's WWW-Authenticate header is not a challenge this package can read",
	}
}

// scanner walks a WWW-Authenticate header one RFC 9110 token, quoted string,
// or delimiter at a time.
type scanner struct {
	// header is the value being read.
	header string
	// pos is how far into header the scan has got.
	pos int
}

// done reports whether the whole header has been read.
func (s *scanner) done() bool {
	return s.pos >= len(s.header)
}

// space advances past optional whitespace.
func (s *scanner) space() {
	for s.pos < len(s.header) && (s.header[s.pos] == ' ' || s.header[s.pos] == '\t') {
		s.pos++
	}
}

// resync advances to the next top-level comma, skipping quoted strings on the
// way, so one unreadable value costs the challenge it belongs to rather than
// the whole header. A quote it enters and nobody closed runs to the end,
// which ends the scan.
func (s *scanner) resync() {
	for s.pos < len(s.header) {
		switch s.header[s.pos] {
		case ',':
			return
		case '"':
			_, _ = s.quoted()
		default:
			s.pos++
		}
	}
}

// delims advances past the whitespace and commas that separate the elements
// of the header's list. Empty elements are legal in an RFC 9110 list, so a
// run of commas is a run of nothing rather than an error.
func (s *scanner) delims() {
	for s.pos < len(s.header) {
		if c := s.header[s.pos]; c != ' ' && c != '\t' && c != ',' {
			return
		}

		s.pos++
	}
}

// equals reports whether an equals sign comes next, consuming it and the
// whitespace around it when it does and leaving the scan exactly where it was
// when it does not. It is the lookahead that tells a parameter name apart
// from the next challenge's scheme.
func (s *scanner) equals() bool {
	saved := s.pos

	s.space()

	if s.pos < len(s.header) && s.header[s.pos] == '=' {
		s.pos++
		s.space()

		return true
	}

	s.pos = saved

	return false
}

// token reads one RFC 9110 token, which is how the grammar spells a scheme
// name, a parameter name, and an unquoted parameter value.
func (s *scanner) token() (string, bool) {
	start := s.pos
	for s.pos < len(s.header) && isTokenChar(s.header[s.pos]) {
		s.pos++
	}

	if s.pos == start {
		return "", false
	}

	return s.header[start:s.pos], true
}

// value reads a parameter's value, which the grammar spells either as a bare
// token or as a quoted string.
func (s *scanner) value() (string, bool) {
	if s.pos < len(s.header) && s.header[s.pos] == '"' {
		return s.quoted()
	}

	return s.token()
}

// quoted reads a quoted string and returns what it stands for, with the
// backslash escapes inside it undone. A string nobody closed is not a value.
func (s *scanner) quoted() (string, bool) {
	s.pos++

	var out strings.Builder

	for s.pos < len(s.header) {
		c := s.header[s.pos]

		switch {
		case c == '"':
			s.pos++

			return out.String(), true
		case c == '\\' && s.pos+1 < len(s.header):
			out.WriteByte(s.header[s.pos+1])
			s.pos += 2
		default:
			out.WriteByte(c)
			s.pos++
		}
	}

	return "", false
}

// isTokenChar reports whether c may appear in an RFC 9110 token.
func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return strings.IndexByte(tcharSpecials, c) >= 0
	}
}
