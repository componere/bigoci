// Package file adapts the operating system filesystem to the two file ends of
// a bigoci transfer: a [Source] is the file a push reads, and a [Sink] is the
// file a pull writes.
//
// Both types wrap one open file handle and add only the bookkeeping the ports
// need. Reads and writes go straight through to the handle, which the
// operating system serves with one positional syscall per call and which is
// therefore safe to share, so transfer workers move their parts at the same
// time without a lock and without buffering a part in memory.
//
// A Sink never writes to the destination path. It writes to a sibling file
// named with [PartialSuffix] and renames that file onto the destination in
// [Sink.Commit], so the destination is either absent or the complete content
// and is never observed half written. Closing a sink without committing
// leaves the partial file on disk on purpose: it is what a later pull resumes
// from.
//
// The push and pull paths this serves are described at
// https://imgoci.github.io/bigoci/explanation/design/.
package file
