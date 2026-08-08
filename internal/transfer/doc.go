// Package transfer moves one file between a local file and an OCI
// repository.
//
// The package holds the transfer orchestrator — [Push] and [Pull], with the
// worker scheduling and verification bookkeeping that drive them — together
// with the ports it consumes: [Blobs] and [Manifests] for the registry end of
// a transfer, [Source] and [Sink] for the file end.
//
// Every registry operation runs under the policy in the
// [github.com/componere/bigoci/internal/retry] package: a part, the empty
// config blob, and each manifest call are attempted again while some layer
// under the failure marked it worth repeating, and a failure nobody marked is
// terminal. The orchestrator is the only thing in bigoci that repeats an
// operation, so the decision is made in the core while the knowledge behind
// it stays in the adapter that owns the connection.
//
// Resume and progress accounting are not here yet: a pull fetches every part
// even when an earlier attempt left some of them on disk.
//
// # The port model
//
// bigoci is hexagonal, and this package is the seam. The core — the
// orchestrator here, together with the plan and manifest packages — is pure
// logic that performs no I/O. Everything that touches a network or a disk
// sits behind a port declared here and is implemented by an adapter
// elsewhere: the oci package speaks the distribution spec over [net/http]
// and implements [Blobs] and [Manifests], and the file package implements
// [Source] and [Sink] against the OS filesystem.
//
// The ports belong to the consumer rather than to the implementers, which is
// why they live in this package and not in the adapters. Two things follow.
// The orchestrator tests against mockery-generated mocks with no registry
// and no scratch files, so failure injection — a dropped connection
// mid-part, a digest that does not match, an out-of-order completion — is
// ordinary unit testing. And an adapter can be replaced, by a caller or by
// bigoci itself, without the core noticing.
//
// The design is documented at
// https://componere.github.io/bigoci/explanation/design/.
package transfer
