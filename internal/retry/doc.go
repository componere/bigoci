// Package retry decides which failures are worth another attempt, and how
// long to wait before making one.
//
// The package holds two halves of a single subject. The first is a
// vocabulary: [Transient] marks a failure that repeating the request could
// fix, and [IsTransient] reads that mark back off an error however deeply it
// has since been wrapped. The second is the loop that acts on the mark, [Do],
// together with the [Policy] that says how many attempts it makes and how the
// waits between them grow.
//
// A mark is produced by whichever layer diagnosed the failure. In practice
// that is an adapter: only the code that spoke to the far end can tell a
// dropped connection from a refused request, while everything above it holds
// nothing but an error value. The one exception is a failure the orchestrator
// diagnoses from its own byte accounting — a part whose body ended before the
// manifest said it would — which it is entitled to mark itself. Marks are
// consumed in exactly one place, [Do], which is the only thing in bigoci that
// repeats an operation.
//
// An error nobody marked is terminal, and that default is the load-bearing
// half of the design. A failure no layer recognized is one bigoci does not
// understand, and sending it three more times turns an immediate answer into
// a slow one without making it any more correct. Retrying is opted into per
// failure, by the layer that knows enough to opt in.
//
// Nothing here performs I/O. Waiting and randomness reach the loop as the
// [Policy.Sleep] and [Policy.Rand] fields, so a test reads an entire backoff
// schedule out of a slice with no clock anywhere in the frame.
package retry
