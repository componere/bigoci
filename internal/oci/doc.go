// Package oci speaks the OCI distribution spec to one repository over
// [net/http].
//
// bigoci owns this transport instead of importing an OCI SDK, because the
// library's value — parallel part transfers, per-part retry, and streaming
// that never buffers a part — lives in the HTTP layer. The protocol surface
// it needs is six endpoints: HEAD and GET on a blob, POST and PUT to upload
// one, and GET and PUT on a manifest. The reasoning is documented at
// https://componere.github.io/bigoci/explanation/design/.
//
// [NewRepository] parses a reference into the repository it names.
// [Repository.Blobs] and [Repository.Manifests] return the two adapters,
// which implement the transfer package's Blobs and Manifests ports. The
// manifest adapter is bound to the reference's tag or digest at construction,
// so the core asks for "the manifest" and never learns the reference grammar.
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
// unexpected status comes back plain. A blob read's body is wrapped so a
// connection that breaks part way through a part is marked the same way, and
// so is a manifest body that dies mid-read. The core therefore decides
// whether to try again without knowing that HTTP, status codes, or
// connections exist, and the attempt budget it spends is the only one there
// is.
//
// This phase talks to registries anonymously. Authentication, and the
// redirect handling a blob read needs on registries that offload content to
// presigned object storage, arrive in later phases.
package oci
