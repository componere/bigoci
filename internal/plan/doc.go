// Package plan computes how a file splits into the fixed-size parts a bigoci
// artifact stores.
//
// The split rule is part of the artifact format contract: for a part size P,
// part i covers bytes [i*P, min((i+1)*P, size)). The last part may be
// shorter than P, a file of size P or smaller has exactly one part, and the
// number of parts never exceeds [MaxParts]. The contract is documented at
// https://imgoci.github.io/bigoci/reference/format/.
//
// A [Plan] is an immutable value that answers questions about one split
// arithmetically. It never materializes a slice, so planning a transfer
// costs nothing regardless of how many parts the file has.
//
// The package is pure: it performs no I/O and reads nothing but the two
// numbers passed to [New].
package plan
