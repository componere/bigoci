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
// A pull resumes. It hashes the part ranges of a partial file an earlier run
// left behind and fetches only the ones that do not match, and a part whose
// stream broke part way through is continued from the byte it reached. None of
// that is recorded anywhere: the bytes on disk are the whole of the state, and
// a range nothing ever wrote reads back as zeros and fails its check.
//
// A transfer reports on itself when somebody asks. [PushSpec.Progress] and
// [PullSpec.Progress] take a [Report], and the orchestrator hands it a
// [Snapshot] of the whole transfer at each milestone and every few megabytes
// in between. Two byte counters carry it: what is provably in place, and what
// crossing the registry boundary cost to put it there. A transfer nobody is
// watching keeps no account — the counting wrappers are not installed and the
// recording calls return on a nil check.
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
