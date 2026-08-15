# Plan 012 — Satisfy `IMGOCI_UPSTREAM_REQUESTS.md`

## Goal

Implement all five upstream requests without changing canonical manifest encoding, weakening transport security, changing existing `Client.Push` behavior, or introducing a second retry owner. Ship the behavior and API changes together in bigoci `v0.2.0`, then hand the release to imgoci/go for its consumer and producer interoperability gates.

`IMGOCI_UPSTREAM_REQUESTS.md` is temporary. Delete it only after every request has landed or request 5 has been explicitly rejected from measured benchmark evidence.

## Verified repository state

- `internal/manifest/decode.go` uses exact media-type comparisons in `checkKind`, `checkConfig`, and `readLayers`. Encoding lives separately in `internal/manifest/manifest.go`; `internal/manifest/manifest_test.go` contains byte-for-byte golden coverage.
- `internal/oci/manifests.go` builds the manifest GET and currently sets only `Accept`. The repository has no manifest HEAD operation.
- `internal/oci/blobs.go` builds blob GETs and sets `Range` only for resumed reads. `Blobs.Exists` uses HEAD, but it is not a manifest read and has no body to decode.
- `internal/oci/redirect.go` manually rebuilds redirected requests. `copyAllowed` currently copies only `Range` and `Accept`, so any new read header must be added there explicitly.
- Token-realm requests are built in `internal/oci/token.go` and sent through `Repository.doExternal`; they do not use the manifest/blob request builders.
- `internal/transfer/push.go` hashes each part in `hashParts`, then opens a fresh `io.SectionReader` for every upload attempt in `uploader.attempt`. `writeManifest` encodes and publishes last.
- `oci.NewRepository` and `boundManifest` require a tag or digest. `Manifests.Put` writes to that bound path.
- The external transport seam is the structural `externalTransportLayer` interface in `internal/oci/endpoint.go`; `inspectableExternalTransport` consumes it and `cli/debug.go` already implements it.
- The requested changes need no transfer-port signature changes. `.mockery.yml` therefore should produce no mock changes.
- Public API tests live in root-package tests; OCI adapter tests live in `internal/oci`; real-registry coverage uses the zot testcontainer harness in `e2e_test.go`.
- User documentation is the Diátaxis site under `docs/docs`; `docs/docs/reference/api.md` mirrors exported APIs and `docs/docs/reference/errors.md` documents sentinel and non-sentinel failures.
- Release Please is configured for Go with `bump-minor-pre-major: true`. The feature commits below produce the requested `0.2.0` release.

## Delivery order

Phases 1–5 can be developed as narrow PRs. Phases 1 and 2 are the consumer blockers and must both merge before imgoci/go advertises BigOCI support. Phase 3 unblocks the producer. Phase 4 locks the structural seam used by imgoci/go. Phase 5 is part of this full request; implement it unless controlled before/after measurements show an unacceptable regression and the maintainer explicitly rejects it. Release only after these outcomes are settled.

Each implementation branch starts from fetched `master`, uses its own Worktrunk worktree, and integrates through a GitHub squash-merge PR. Keep `.journal` untracked on implementation branches.

## Phase 1 — Decode media types ASCII case-insensitively

**Targets**

- `internal/manifest/decode.go`: `Decode`, `checkKind`, `checkConfig`, `readLayers`
- `internal/manifest/decode_test.go`
- `docs/docs/reference/format.md` if its consumer rules currently imply exact comparison

**Implementation**

1. Import `strings` and use `strings.EqualFold` for exactly four comparisons:
   - manifest `mediaType` against `ocispec.MediaTypeImageManifest`;
   - manifest `artifactType` against `manifest.ArtifactType`;
   - config descriptor media type against the empty-config media type;
   - every layer media type against `manifest.MediaTypePart`.
2. Preserve the existing empty manifest-media-type exception: use `mediaType != "" && !strings.EqualFold(...)`.
3. Keep config digest, size, and inline-data comparisons exact.
4. Do not change exported media-type constants or any encoder path. Canonical output remains lowercase and byte-identical.
5. Update function Godoc to state that media-type type/subtype names are compared ASCII case-insensitively under RFC 6838 §4.2.

**Behavior tests**

- Decode one valid manifest whose four media-type fields use mixed and upper case; assert the decoded artifact equals the lowercase fixture.
- Retain or add negative rows proving a genuinely different manifest/artifact type still wraps `ErrNotBigociArtifact` and wrong config/layer types still fail.
- Run the existing golden encoder and encode/decode fixtures unmodified. They must prove byte-identical canonical output.

**Proof**

- `go test ./internal/manifest/...`
- PR title: `feat(manifest): accept media types ASCII case-insensitively`

## Phase 2 — Enforce identity coding on manifest and blob reads

**Targets**

- New focused helper in `internal/oci/encoding.go` (preferred over adding unrelated weight to `repository.go`), with package-level coverage in `internal/oci/encoding_test.go`
- `internal/oci/manifests.go`: `Manifests.Get`
- `internal/oci/blobs.go`: `Blobs.Get`
- `internal/oci/redirect.go`: `copyAllowed` and its redirect documentation
- `internal/oci/manifests_test.go`, `blobs_test.go`, `redirect_test.go`, token/auth tests
- Root functional/e2e tests, including `e2e_auth_test.go` where the existing authenticated registry fixtures fit
- `docs/docs/reference/errors.md`
- `docs/docs/reference/registry-compatibility.md` if it enumerates wire requirements

**Implementation**

1. Define an unexported named error type for non-identity content coding. Its message names only the safe `origin` (`METHOD /structural/path`) and fixed diagnosis. Never include the peer-controlled `Content-Encoding` value or an off-origin URL. It matches no public sentinel and carries no transient tag.
2. Add `checkIdentityEncoding(at origin, resp *http.Response) error`:
   - inspect every `resp.Header.Values("Content-Encoding")` entry;
   - split every entry on commas and trim optional whitespace;
   - accept an absent/empty value and tokens equal to `identity` under `strings.EqualFold`;
   - reject any other token;
   - ensure the caller closes the response body before returning the error.
3. In `Manifests.Get`, set `Accept-Encoding: identity` beside `Accept`. Check the final response immediately after `repo.send` and before status handling or body reads, so coded success and error bodies are both rejected deterministically.
4. In `Blobs.Get`, set `Accept-Encoding: identity` unconditionally, before the optional `Range`. Check the final response before `blobReadStart`; close it on failure.
5. Add `Accept-Encoding`—and only that header—to `copyAllowed`, so every manual redirect hop retains the identity marker. Update comments that currently describe two copied headers and Range as the mechanism suppressing transparent gzip.
6. Leave token requests, `Blobs.Exists`, uploads, and unrelated registry probes unchanged. Token JSON may continue to use Go's transparent gzip handling.

**Behavior tests**

- Helper table: absent header, blank value, mixed-case `identity`, repeated identity fields, comma-separated identity tokens, `gzip`, and `Identity, gzip`. Assert rejected errors are terminal, contain the original safe origin, and do not echo header values.
- Manifest adapter: assert the request carries `Accept-Encoding: identity`; reject coded 200 and coded non-200 responses before reading them; assert the body closes.
- Blob adapter: assert full and resumed GETs carry the header; reject coded 200/206 and coded failures; assert the body closes.
- Redirect adapter: assert a 307 re-issued to a second host retains `Accept-Encoding: identity` while existing Authorization stripping remains unchanged. Reject a coded terminal response from that host using the original registry operation label.
- Functional pull scenarios must be separate so both paths execute:
  1. gzip only the manifest response and assert `Pull` fails with the content-coding error;
  2. leave the manifest identity-coded, gzip a blob response, and assert `Pull` fails with the same error class.
- Authenticated functional scenario: gzip the actual token-realm JSON response and prove a complete transfer still succeeds. A unit-only token parser test is insufficient for this acceptance criterion.

**Documentation**

Add the terminal content-coding refusal to the “errors with no sentinel” reference, including remediation at the registry proxy or middlebox. Record the compatibility break: manifest/blob responses carrying non-identity coding now fail immediately instead of surfacing as confusing digest failures.

**Proof**

- Targeted `go test ./internal/oci/...` plus the root functional tests
- Full gate in Phase 6
- PR title: `feat(oci): require identity coding on manifest and blob reads`

## Phase 3 — Add tag-free digest publication

**API decision**

Add:

```go
func (c *Client) PushByDigest(
    ctx context.Context,
    repo Reference,
    src FileSource,
    opts ...PushOption,
) (ocispec.Descriptor, error)
```

This entry point accepts only a canonical repository-only value (`registry/name`). It rejects a tag or digest instead of silently ignoring one. Keep `Client.Push` and `Client.Pull` strict: their `Reference` still requires a tag or digest. Document the scoped grammar exception on `Reference` and `PushByDigest`. Do not make existing `Push` semantics conditional on an option.

**Targets**

- `internal/oci/repository.go`: repository parsing/construction and bound-manifest state
- `internal/oci/manifests.go`: digest-addressed `Put`
- `internal/oci/repository_test.go`, `manifests_test.go`
- `client.go`: `Client.PushByDigest` and shared private push implementation
- `client_test.go`, `example_test.go`, `fake_test.go`, `e2e_test.go`
- `docs/docs/reference/api.md`
- `docs/docs/how-to/push-and-pull.md` only if a task-oriented tag-free publication procedure materially helps users

**Implementation**

1. Add an internal repository constructor for digest publication, for example `oci.NewDigestPushRepository`. Reuse the existing parsing, auth, HTTP-client, external-transport, and option wiring. Require registry plus repository and forbid tag/digest input.
2. Represent digest-publication mode explicitly in the repository/manifests adapter; do not use an ambiguous empty bound path alone.
3. In `Manifests.Put`, encode is already complete when the body arrives. Compute `digest.FromBytes(body)` before request construction and PUT to `manifests/<computed digest>` in digest-publication mode. Return the same digest. Keep bound tag/digest behavior unchanged.
4. Guard `Manifests.Get` in write-only digest-publication mode with a clear internal misuse error.
5. Add `Client.PushByDigest`. Refactor the current private push path so `Push` and `PushByDigest` share option application, source opening, transfer construction, progress, classification, and error wrapping. They differ only in repository construction and target wording; do not duplicate the transfer body.
6. Reuse `transfer.Push` unchanged. Splitting, first-pass hashes, part uploads, empty-config publication, canonical encoding, and manifest-last ordering remain one path.
7. Add full Godoc and a public example explaining the no-tag guarantee and returned descriptor's role in an OCI index.

**Behavior tests**

- Input grammar: repository-only succeeds; malformed, tagged, and digest-bound inputs fail clearly.
- Internal adapter: the request path is `manifests/<digest.FromBytes(body)>`; ordinary bound `Manifests.Put` retains its existing path.
- Fake-registry functional test:
  - snapshot tags before and after and prove no tag was created or moved;
  - retrieve the manifest by the returned descriptor digest;
  - parse the manifest and prove every part plus the empty config exists;
  - pull by digest and compare the file bytes.
- Zot e2e: compare `/v2/<name>/tags/list` before and after, then pull by the returned digest. Use a unique repository but compare snapshots rather than assuming an empty response shape.
- Existing `Push` tests remain unchanged and pass.

**Documentation**

Add `PushByDigest` and the repository-only grammar to the API reference. Keep the existing `Push` contract and examples intact.

**Proof**

- Targeted root and `internal/oci` tests
- Zot e2e with Docker
- PR title: `feat(api): push manifests by digest without writing a tag`

## Phase 4 — Publish the external-transport wrapper contract

**Targets**

- `options.go`: `WithHTTPClient` Godoc
- `internal/oci/endpoint.go`: `externalTransportLayer` comment
- `external_transport_test.go`
- `docs/docs/reference/api.md`

**Implementation**

1. Document both exact structural methods:
   - `BigociExternalBase() http.RoundTripper` returns the transport the wrapper forwards to.
   - `BigociWrapExternal(next http.RoundTripper) http.RoundTripper` rebuilds the same wrapper layer over `next`.
2. Explain when bigoci calls them: while deriving the guarded, cookie-free transport for token realms, redirect targets, and off-origin upload sessions from a caller supplied through `WithHTTPClient`.
3. State the required observer-wrapper semantics and that a compliant wrapper over an inspectable concrete transport keeps default verified mode; callers do not need `WithUnverifiedExternalTransport` merely because of that wrapper.
4. State the semver commitment: these method names and semantics remain stable within the current major version.
5. Mirror the contract in `docs/docs/reference/api.md`. Do not create a new page for two methods.

**Public contract test**

Add a root-package wrapper type that implements both methods over a concrete `*http.Transport`. Inject it with `WithHTTPClient`, drive a cross-host path under default verified mode, and prove:

- bigoci rebuilt the wrapper around its guarded base;
- the wrapper observed the external request;
- the destination guard still applies;
- the equivalent opaque wrapper without the structural methods fails closed.

This test must fail if the internal type assertion, method names, or wrapping semantics change.

**Proof**

- Targeted external-transport tests
- Strict docs build
- PR title: `docs(api): stabilize the external transport wrapper contract`

## Phase 5 — Re-hash uploaded bytes against the queued part digest

**Decision**

Implement the check always-on. It is defense-in-depth; the caller's source-immutability precondition remains because no concurrent-mutation detector can offer a complete filesystem snapshot guarantee. Benchmark it before merge rather than adding an option that can disable integrity.

**Targets**

- `internal/transfer/push.go`: `uploader.attempt`, a small digest-verifying reader, and relevant Godoc
- Existing transfer mock fixtures and `internal/transfer/push_test.go` or a focused new test file
- Existing transfer benchmarks plus the `bench` harness

**Implementation**

1. Create one verifier per upload attempt around that attempt's fresh `io.SectionReader`. It hashes only bytes consumed by the wire path and compares against `job.dgst`.
2. Compare as soon as cumulative bytes reach `job.part.Size`, not only after receiving `io.EOF`: `net/http` can stop after exactly `Content-Length` bytes without one final EOF read.
3. Compose the reader as `tagSourceReads{r: verifier(sectionReader, expectedDigest, expectedSize)}`. A verifier mismatch then follows the existing `sourceError` unwrap path in `uploader.attempt`, overriding any adapter transient tag and ending the push immediately. Avoid a second retry/error abstraction unless a test proves the existing tag cannot preserve the mismatch.
4. Return fixed diagnostic text naming the part and stating that the source changed during the push. Do not expose file contents or create a public sentinel.
5. Construct a fresh hasher on every retry. Preserve progress semantics: wire bytes remain measured by the existing adapter callback, not by source reads.
6. Keep manifest-last ordering unchanged; a failed verification leaves no manifest.

**Behavior tests**

- Deterministic same-length mutation: a source returns bytes A during `hashParts` and bytes B during upload. Assert terminal failure, one attempt despite the adapter's transient wrapping, and zero manifest `Put` calls.
- Unchanged source succeeds.
- A transient network failure followed by retry succeeds with a fresh verifier; no state leaks from the first attempt.
- A shortened source retains the existing terminal source-read behavior.

**Benchmark gate**

1. Capture before/after results on the same machine and registry fixture with identical commit/tool/config inputs.
2. Measure cold push because it streams and re-hashes part bodies. Warm push is a control: it should remain effectively unchanged because existing blobs skip upload.
3. Use repeated populations and report median/distribution with the harness metadata; do not compare isolated wall-clock samples or old hardware runs.
4. Record throughput and CPU deltas in the PR. The request defines no numeric threshold, so do not invent one. If the measured regression is material relative to run variance or makes the current measured defaults invalid, stop before merge and obtain the maintainer's explicit accept/reject decision. Otherwise merge the always-on check.

**Proof**

- Targeted transfer tests under the race detector
- Controlled before/after benchmark report
- PR title: `feat(transfer): verify part bytes during upload`

## Phase 6 — Integrated verification, release, and downstream hand-off

Run after Phases 1–5 merge, or after Phase 5 has a documented evidence-based maintainer rejection.

1. Run the repository's full local gate: `moon run root:check`.
2. Run real-registry functional coverage with Docker:
   - case-varied manifest pulls;
   - separately coded manifest and blob failures;
   - gzip token realm success;
   - cross-host redirect retaining identity and rejecting a coded storage response;
   - digest publication with unchanged tag listing and pull by returned digest;
   - upload mutation refusal with no manifest.
3. Run `moon run root:mocks` and require no diff because the plan changes no port.
4. Verify `git ls-files .journal` prints nothing in every implementation worktree.
5. Verify docs updates shipped with their behavior PRs and the strict MkDocs build passed.
6. Let Release Please prepare `0.2.0`. Review generated `CHANGELOG.md` for every landed request and add the compatibility note prominently: manifest and blob reads now reject non-identity content coding, so compressing registry front ends or middleboxes must be corrected.
7. Delete `IMGOCI_UPSTREAM_REQUESTS.md` in the final closeout commit after all decisions above are represented in merged code, tests, docs, or benchmark evidence.
8. Publish `0.2.0` through the existing release workflow.
9. Downstream, imgoci/go bumps its bigoci pin and runs the §6.4/§6.6 interoperability fixtures from `~/code/imgoci/go/.wt/journal-jmgilman/.journal/001/ARCHITECTURE.md`: case-varied media types, coded-response refusal, gzip token realm, cross-host redirect under default verified mode with the structural wrapper, tag-free producer publication, and digest-identical manifest bytes.

## Invariants and risks

- **Canonical encoding:** no phase changes `manifest.Encode`, constants, member order, or lowercase output. Golden bytes must remain unchanged.
- **Safe errors:** new errors use fixed text plus the safe original `origin`; peer header values and redirected URLs never reach messages.
- **Retry ownership:** adapters classify; `internal/transfer` schedules. Content-coding and source-mutation failures are terminal.
- **Credential boundary:** `copyAllowed` gains exactly `Accept-Encoding`; Authorization remains governed only by the existing same-origin rule.
- **Token compression:** enforcement is request-scoped, never attached indiscriminately to the shared external client.
- **Reference grammar:** repository-only input is accepted only by `PushByDigest`; existing operations keep tag/digest requirements.
- **Generated mocks:** ports remain unchanged. Any implementation that changes a port must regenerate every affected mock and explain why the simpler adapter-mode design was insufficient.
- **Source stability:** wire re-hashing closes the known registry-verification gap but does not remove the documented caller precondition.
- **Release scope:** requests 1 and 2 must ship together. Request 5 may be omitted only by an explicit benchmark-backed maintainer decision, not by silent deferral.

## Requirement traceability

| Request | Implementation phase | Primary targets | Required proof |
|---|---:|---|---|
| §1 manifest `mediaType` and `artifactType` case folding | 1 | `checkKind` | mixed-case decode; wrong type still `ErrNotBigociArtifact` |
| §1 config media type case folding | 1 | `checkConfig` media-type arm | mixed-case decode; digest/size/data exact |
| §1 layer media type case folding | 1 | `readLayers` | mixed-case layer decode |
| §1 encoder unchanged | 1, 6 | no encoder changes | existing golden bytes pass unmodified |
| §2 manifest GET identity marker and response check | 2 | `Manifests.Get` | header assertion; coded manifest fails |
| §2 blob GET identity marker and response check | 2 | `Blobs.Get` | full/resumed header assertions; coded blob fails |
| §2 redirect propagation | 2 | `copyAllowed`, `nextHop` | second host receives identity marker |
| §2 coded redirect response rejected | 2 | final response check | origin-safe terminal failure |
| §2 token realm unaffected | 2 | unchanged token path | gzipped token JSON completes a transfer |
| §3 no-tag digest publication | 3 | digest repository mode, `Manifests.Put`, `PushByDigest` | unchanged tag snapshot; descriptor retrieves manifest |
| §3 complete graph | 3 | unchanged `transfer.Push` | parts and empty config exist; pull by digest passes |
| §3 existing `Push` unchanged | 3, 6 | shared private push core | existing test suite passes |
| §4 stable seam docs | 4 | `WithHTTPClient` Godoc, API reference | both signatures, semantics, call timing, semver statement published |
| §4 seam regression lock | 4 | `external_transport_test.go` | compliant wrapper works in verified mode; opaque wrapper fails closed |
| §5 upload wire re-hash | 5 | `uploader.attempt` verifier | deterministic mutation fails terminally; no manifest |
| §5 performance evidence | 5 | transfer benchmarks, `bench` harness | controlled before/after cold and warm results |
| Cross-cutting docs and compatibility note | 2–6 | docs site, release changelog | strict docs build and reviewed `0.2.0` notes |
| Temporary request deletion | 6 | `IMGOCI_UPSTREAM_REQUESTS.md` | absent after every request is resolved |

## Final acceptance checklist

- [ ] All four decoder comparisons are ASCII case-insensitive; all digest/size/data checks remain exact.
- [ ] Existing canonical manifest golden bytes are unchanged.
- [ ] Manifest and blob GETs, including manual redirects, carry `Accept-Encoding: identity`.
- [ ] Every final manifest/blob response accepts only absent/empty or `identity` content-coding tokens across repeated header fields.
- [ ] Coded manifest, blob, and redirected storage responses fail terminally with origin-safe errors and closed bodies.
- [ ] A gzipped token realm still completes authentication and transfer.
- [ ] `PushByDigest` accepts only repository-only input, writes no tag, returns a retrievable descriptor, and publishes the complete blob graph.
- [ ] Existing `Push` behavior and tests remain unchanged.
- [ ] Both external-wrapper methods, semantics, invocation timing, and semver commitment are published and protected by a public-surface test.
- [ ] Uploaded part bytes are re-hashed per attempt; deterministic mutation fails before manifest publication.
- [ ] Controlled benchmark evidence supports the always-on check or records an explicit maintainer rejection.
- [ ] Ports and generated mocks are unchanged.
- [ ] Targeted tests, race tests, real-registry scenarios, `moon run root:check`, strict docs build, and release workflows pass.
- [ ] Release `0.2.0` contains the identity-coding compatibility note.
- [ ] `IMGOCI_UPSTREAM_REQUESTS.md` is deleted after all five requests are resolved.
