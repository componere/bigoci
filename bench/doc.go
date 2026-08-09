// Command bench measures bigoci's transfer throughput against real
// registries and turns the numbers into the library's defaults.
//
// This is measurement tooling: never published, never released, never
// versioned. It exists to answer two questions the design document leaves
// open — what part size and worker count the library should default to, and
// whether worker count needs to self-tune — and to keep those answers honest
// as the implementation evolves. The local replace directive in its go.mod
// makes installing it from a module proxy impossible on purpose.
//
// # Commands
//
//	bench run -spec <spec.json> -out <results.jsonl> [-resume] [-endpoint name=host:port]
//	bench summarize -in <results.jsonl>
//
// A run walks the cross-product a spec file describes — targets x part sizes
// x worker counts x file sizes x iterations — and appends one JSONL row per
// timed scenario. Scenarios are phases within an iteration, not a matrix
// axis: every iteration generates a unique fixture, pushes it cold, may
// re-push it warm, and may pull it back, and the spec's scenario list picks
// which of those phases are timed and recorded. Summarize reads the rows
// back and renders median-throughput grids and a long-form statistics table
// as Markdown.
//
// The harness drives only the library's public API. It never provisions
// servers, never starts registries, and never schedules anything: the
// operator runbook under latitude/ owns the machines, and a human owns the
// trigger.
package main
