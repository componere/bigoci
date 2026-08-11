// Package bigoci uploads and downloads large files to and from OCI
// registries. Large means 5 GB and up, into the tens of GB. That is the
// whole library.
//
// bigoci stores a file as fixed-size parts pushed as OCI blobs, listed in
// order as the layers of a standard OCI image manifest. This makes push and
// pull parallel, retryable, and resumable on every registry. The design and
// the artifact format are documented at
// https://imgoci.github.io/bigoci/.
//
// # Usage
//
// A [Client] holds transfer-wide settings and one shared external connection
// pool, so one client serves any number of transfers. Each direction is a
// single call:
//
//	client, err := bigoci.New()
//	if err != nil {
//		return err
//	}
//
//	desc, err := client.Push(ctx, "registry.example.com/team/model:v1", bigoci.FromFile("model.bin"))
//	if err != nil {
//		return err
//	}
//
//	// desc.Digest names that artifact exactly, wherever the tag points later.
//	ref := bigoci.Reference("registry.example.com/team/model@" + desc.Digest.String())
//	if err := client.Pull(ctx, ref, bigoci.ToFile("model.bin")); err != nil {
//		return err
//	}
//
// A push splits at [DefaultPartSize] and names the artifact after the file it
// read; [WithPartSize] and [WithTitle] change both. [WithWorkers] sets how
// many parts either direction moves at once, and [WithProgress] watches
// either direction move them.
//
// # Errors
//
// [ErrNotFound], [ErrUnauthorized], [ErrNotBigociArtifact],
// [ErrDigestMismatch], and [ErrPartTooLarge] are the failures a caller
// branches on. Both directions run every error they return through the same
// check, so [errors.Is] answers for the whole chain no matter how deep the
// failure started.
//
// # Reliability
//
// Push and pull move a file end to end and retry transient failures: a
// dropped connection, a 429, a 5xx, or a part whose body ends early costs a
// bounded number of attempts with growing jittered waits, never the
// transfer. A pull resumes into the partial file an earlier run left behind,
// fetching only the parts that do not verify. [WithDockerCredentials] and
// [WithCredentials] authenticate a transfer to registries that ask for a
// credential; a client built with neither still answers a token challenge,
// anonymously.
//
// bigoci is dual-licensed under Apache-2.0 and MIT, at your option.
package bigoci
