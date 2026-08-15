// Package oci speaks the OCI distribution spec to one repository over
// [net/http].
//
// bigoci owns this transport instead of importing an OCI SDK, because the
// library's value — parallel part transfers, per-part retry, and streaming
// that never buffers a part — lives in the HTTP layer. The protocol surface
// it needs is six endpoints: HEAD and GET on a blob, POST and PUT to upload
// one, and GET and PUT on a manifest. The reasoning is documented at
// https://imgoci.github.io/bigoci/explanation/design/.
//
// [NewRepository] parses a reference into the repository it names and binds
// the manifest adapter to the tag or digest that reference carried.
// [NewDigestPushRepository] parses a repository-only reference and puts the
// adapter in digest-publication mode: Put writes at the digest of the body,
// and Get is unsupported. [Repository.Blobs] and [Repository.Manifests]
// return the two adapters, which implement the transfer package's Blobs and
// Manifests ports. Either way the address is fixed at construction, so the
// core asks for "the manifest" and never learns the reference grammar.
//
// Nothing here buffers blob content: a read hands back the response body
// unread, and a write streams its reader onto the wire under an explicit
// Content-Length. Manifests are the exception, because they are small and
// both directions need the whole document in hand to digest it.
//
// Every failure this package returns is classified for the retry policy
// before it leaves, and this package never retries anything itself. A 429 or
// a 5xx, and any request that never got a response, come back marked
// transient, carrying the wait a Retry-After header asked for; every other
// unexpected status comes back plain, and so does a request whose own
// context had already ended — that is the transfer stopping, not the
// registry failing. A blob read's body is wrapped so a
// connection that breaks part way through a part is marked the same way, and
// so is a manifest body that dies mid-read. The core therefore decides
// whether to try again without knowing that HTTP, status codes, or
// connections exist, and the attempt budget it spends is the only one there
// is.
//
// Authentication is a pre-condition of a request rather than a recovery from
// one. A repository holds what the registry challenged with and the token
// acquired for each scope, the request builder stamps the Authorization
// header while it builds a request, and every request — the ordinary ones and
// the token exchanges alike — goes out through the caller's client, so
// nothing watching that client is blind to any of them. A registry that never
// challenges costs nothing at all: no probe, no header, and not one extra
// request.
//
// Two things make this package send a request a second time, and both are
// the registry demanding a different request rather than failing to answer
// one: a challenge, answered when it changed what the request would carry and
// the standard library says the body can be produced again, and a redirect,
// re-issued as the paragraph below describes. A blob upload's body cannot be
// produced again, by construction, so a refusal in the middle of one comes
// back marked worth repeating and the orchestrator opens the file again. No
// identical request is ever repeated.
//
// Registries that keep their blob content in object storage answer a read
// with a location rather than with bytes, and this package follows that
// location itself instead of letting [net/http] do it. Automatic following is
// off for every request; a read is re-issued up to three times, each time as a
// request built from nothing and carrying two headers and no more. The
// credential goes along only when the location is the registry itself —
// same scheme, same host, same port — so a request to signed storage arrives
// with no Authorization header and no cookie, and a location is never stored:
// the next attempt asks the registry again and follows whatever fresh location
// it sends. Beyond the registry, a refusal reads differently: a signature that
// has expired looks like a 403 or a 404 and is worth another attempt, and
// never means the caller's credentials or a missing artifact. No error this
// package returns names a signed location; every one of them names the
// registry request it started as.
package oci
