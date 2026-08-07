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
// This phase talks to registries anonymously. Authentication, and the
// redirect handling a blob read needs on registries that offload content to
// presigned object storage, arrive in later phases.
package oci
