// Package bigoci uploads and downloads large files to and from OCI
// registries. Large means 5 GB and up, into the tens of GB. That is the
// whole library.
//
// bigoci stores a file as fixed-size parts pushed as OCI blobs, listed in
// order as the layers of a standard OCI image manifest. This makes push and
// pull parallel, retryable, and resumable on every registry. The design and
// the artifact format are documented at
// https://componere.github.io/bigoci/.
//
// # Usage
//
// A [Client] holds the transport settings and nothing else, so one client
// serves any number of transfers. Each direction is a single call:
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
// many parts either direction moves at once.
//
// # Errors
//
// [ErrNotFound], [ErrNotBigociArtifact], and [ErrDigestMismatch] are the
// failures a caller branches on. Both directions run every error they return
// through the same check, so [errors.Is] answers for the whole chain no
// matter how deep the failure started.
//
// The design names two more: a registry that refused the request, and a part
// a registry rejected as too large, which is how a layer size cap surfaces.
// Neither can happen in this phase, so both arrive with the authentication
// and retry phases that raise them.
//
// # This phase
//
// Push and pull move a file end to end. What they do not do yet: a failed
// request surfaces instead of being retried, a pull fetches every part rather
// than resuming into a partial file it finds, and every request is anonymous.
// Retries, resume, and authentication arrive in later phases, in that order.
//
// bigoci is dual-licensed under Apache-2.0 and MIT, at your option.
package bigoci
