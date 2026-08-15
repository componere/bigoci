package oci

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	// headerAcceptEncoding is the request header that asks the far end not to
	// apply a content coding. Setting it also stops [net/http] from adding
	// gzip on its own, so the bytes this package hashes are the bytes that
	// were stored.
	headerAcceptEncoding = "Accept-Encoding"

	// headerContentEncoding is the response header that names the content
	// coding applied to the body.
	headerContentEncoding = "Content-Encoding"

	// codingIdentity is the only content-coding token a manifest or blob
	// read accepts. It means the body is the stored bytes, untransformed.
	codingIdentity = "identity"
)

// contentCodingError reports a manifest or blob response that arrived under
// a content coding other than identity.
//
// It matches no public sentinel and is not marked transient: asking again
// produces the same coded body. The message names the registry request and a
// fixed diagnosis. The peer-controlled Content-Encoding value never appears,
// because a registry or middlebox can put anything there, including a
// reflected credential.
type contentCodingError struct {
	// at is the registry request the coded response answered.
	at origin
}

// Error names the original registry request and the identity-coding rule.
func (e *contentCodingError) Error() string {
	return fmt.Sprintf("%s: the response is not identity coded", e.at)
}

// checkIdentityEncoding reports whether a final manifest or blob response
// arrived identity-coded.
//
// Every Content-Encoding field is inspected, including repeated header
// lines, and each field is split on commas the way RFC 9110 joins list
// values. Tokens are compared without regard to ASCII case. An absent
// header, a blank value, and the token "identity" are accepted; any other
// token is refused.
//
// The check belongs on the response that carried the body, which after a
// redirect is the object store's rather than the registry's. That changes
// nothing about the rule, and it is why at is threaded in — a storage URL
// has no business in an error message. The caller closes the body before
// returning the error, the same way a status failure does.
func checkIdentityEncoding(at origin, resp *http.Response) error {
	for _, field := range resp.Header.Values(headerContentEncoding) {
		for token := range strings.SplitSeq(field, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if !strings.EqualFold(token, codingIdentity) {
				return &contentCodingError{at: at}
			}
		}
	}

	return nil
}
