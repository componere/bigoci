---
title: Registry compatibility
description: Dated bigoci push and pull results against hosted OCI registries.
---

# Registry compatibility

These results come from one bigoci campaign against four hosted registries.
Hosted-registry behavior can change without a bigoci release, so each result is
a dated observation rather than a permanent support guarantee.

The
[go-oci-blob registry compatibility matrix](https://imgoci.github.io/go-oci-blob/reference/registry-compatibility/)
tests the embedded blob client directly against nine registries. This page
tests bigoci's complete file path: authentication, part splitting, manifest
publication, tagged and digest-reference pulls, retries, and file
reconstruction. The two matrices answer different questions and can report
different results.

## Verification identity

| Field | Value |
|---|---|
| Campaign date | 2026-08-14 to 2026-08-15 |
| bigoci commit | `56d6a26df24745aff5ba4ff538086a6829170bac` |
| go-oci-blob | `v1.1.1` |
| Go | `go1.26.5 darwin/arm64` |
| Independent control | ORAS CLI |
| Multipart configuration | 8 workers, 256 KiB parts |

The reusable campaign rows are in
[`cli/conformance_test.go`](https://github.com/imgoci/bigoci/blob/56d6a26df24745aff5ba4ff538086a6829170bac/cli/conformance_test.go).
GHCR runs those rows through the hand-triggered
[conformance workflow](https://github.com/imgoci/bigoci/blob/56d6a26df24745aff5ba4ff538086a6829170bac/.github/workflows/conformance.yml).
ECR, `gcr.io`, and Quay.io were provisioned and exercised manually with the
same bigoci CLI. The `gcr.io` row used a Google Artifact Registry repository
through its `gcr.io` compatibility hostname.

## Result labels

| Symbol | Label | Meaning |
|---|---|---|
| :material-check:{ .result-pass title="PASS" } | PASS | The bigoci path worked. Successful artifact results were also checked with ORAS. |
| :material-close:{ .result-no title="NO" } | NO | The registry and bigoci combination did not complete the path. |
| :material-minus:{ .result-na title="N/A" } | N/A | The campaign did not exercise the path on this registry. |

## Results by registry

Hover over a path name for the exact behavior that row verified.

=== "ECR"

    Amazon ECR Private.

    | Path | Result |
    |---|:---:|
    | <span title="The authenticated campaign pushed and pulled a 2 MiB file as eight 256 KiB parts and compared the result byte-for-byte.">Multipart tagged round-trip</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The campaign pulled an artifact by manifest digest and compared the result byte-for-byte.">Digest-reference round-trip</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The private repository refused an anonymous pull before the campaign uploaded its payload.">Anonymous pull refused</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The private repository rejected a pull made with invalid credentials as bigoci.ErrUnauthorized with exit code 6.">Invalid credential rejected</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="No registry authorization, cookie, proxy credential, or referrer data follows an authenticated redirect to off-origin blob storage.">Off-origin credential isolation</span> | :material-minus:{ .result-na title="N/A" } |
    | <span title="ORAS fetched the manifest, config, and every part; declared sizes and SHA-256 digests matched.">Independent manifest and part verification</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="Push and pull of a bigoci empty-file artifact, including its zero-byte part and two-byte OCI config.">Empty-file round-trip</span> | :material-close:{ .result-no title="NO" } |

=== "GHCR"

    GitHub Container Registry.

    | Path | Result |
    |---|:---:|
    | <span title="The authenticated campaign pushed and pulled a 2 MiB file as eight 256 KiB parts and compared the result byte-for-byte.">Multipart tagged round-trip</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The campaign pulled an artifact by manifest digest and compared the result byte-for-byte.">Digest-reference round-trip</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The private repository refused an anonymous pull before the campaign uploaded its payload.">Anonymous pull refused</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The private repository rejected a pull made with invalid credentials as bigoci.ErrUnauthorized with exit code 6.">Invalid credential rejected</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="No registry authorization, cookie, proxy credential, or referrer data followed an authenticated redirect to off-origin blob storage.">Off-origin credential isolation</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="ORAS fetched the manifest, config, and every part; declared sizes and SHA-256 digests matched.">Independent manifest and part verification</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="Push and pull of a bigoci empty-file artifact, including its zero-byte part and two-byte OCI config.">Empty-file round-trip</span> | :material-check:{ .result-pass title="PASS" } |

=== "gcr.io"

    Google Artifact Registry through its `gcr.io` compatibility hostname.

    | Path | Result |
    |---|:---:|
    | <span title="The authenticated campaign pushed and pulled a 2 MiB file as eight 256 KiB parts and compared the result byte-for-byte.">Multipart tagged round-trip</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The campaign pulled an artifact by manifest digest and compared the result byte-for-byte.">Digest-reference round-trip</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The private repository refused an anonymous pull before the campaign uploaded its payload.">Anonymous pull refused</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The private repository rejected a pull made with invalid credentials as bigoci.ErrUnauthorized with exit code 6.">Invalid credential rejected</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="No registry authorization, cookie, proxy credential, or referrer data follows an authenticated redirect to off-origin blob storage.">Off-origin credential isolation</span> | :material-minus:{ .result-na title="N/A" } |
    | <span title="ORAS fetched the manifest, config, and every part; declared sizes and SHA-256 digests matched.">Independent manifest and part verification</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="Push and pull of a bigoci empty-file artifact, including its zero-byte part and two-byte OCI config.">Empty-file round-trip</span> | :material-close:{ .result-no title="NO" } |

=== "Quay.io"

    Quay.io, using a public campaign repository.

    | Path | Result |
    |---|:---:|
    | <span title="The authenticated campaign pushed and pulled a 2 MiB file as eight 256 KiB parts and compared the result byte-for-byte.">Multipart tagged round-trip</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The campaign pulled an artifact by manifest digest and compared the result byte-for-byte.">Digest-reference round-trip</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="The public repository accepted anonymous pulls, so refusal was not a valid control.">Anonymous pull refused</span> | :material-minus:{ .result-na title="N/A" } |
    | <span title="The public repository rejected a push made with invalid credentials as bigoci.ErrUnauthorized with exit code 6.">Invalid credential rejected</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="No registry authorization, cookie, proxy credential, or referrer data follows an authenticated redirect to off-origin blob storage.">Off-origin credential isolation</span> | :material-minus:{ .result-na title="N/A" } |
    | <span title="ORAS fetched the manifest, config, and every part; declared sizes and SHA-256 digests matched.">Independent manifest and part verification</span> | :material-check:{ .result-pass title="PASS" } |
    | <span title="Push and pull of a bigoci empty-file artifact, including its zero-byte part and two-byte OCI config.">Empty-file round-trip</span> | :material-close:{ .result-no title="NO" } |

## Empty-file behavior

The empty-file row includes both push and pull. A successful manifest write is
not a PASS unless bigoci can pull the empty file again.

- **Amazon ECR Private:** ECR rejected the zero-byte part during the final blob
  upload commit with `400 Bad Request`. A fresh repository reproduced the
  failure.
- **GHCR:** bigoci pushed and pulled the empty file. ORAS independently fetched
  its manifest and zero-byte layer.
- **`gcr.io`:** `gcr.io` rejected the zero-byte part during the final blob
  upload commit with `400 Bad Request`. A fresh repository reproduced the
  failure.
- **Quay.io:** Quay accepted the zero-byte upload and manifest, then redirected
  the layer read to an off-origin object that returned `404 Not Found`. Both
  authenticated and anonymous pulls failed after four attempts. A fresh
  repository reproduced the failure.

The go-oci-blob campaign dated 2026-08-12 reported `NO` for a standalone empty
blob on GHCR. This campaign dated 2026-08-14 reported `PASS` for bigoci's
empty-file path. These are dated observations of different end-to-end
operations; neither result replaces the other.

## Authentication and redirect scope

The ECR, GHCR, and `gcr.io` repositories exercised refusal of anonymous pulls.
The Quay campaign used a public repository, so anonymous pull was the expected
control and the campaign tested an invalid credential on push instead.
Private Quay authentication is therefore `N/A`.

Only the GHCR campaign exercised an authenticated registry-to-storage redirect
and verified that the credential did not follow it. Quay's anonymous pulls
also carried no credential to redirected storage, but that does not test
credential stripping. The other redirect cells remain `N/A` rather than
inferring a result from unexercised paths.

## Content coding

Manifest and blob GETs now send `Accept-Encoding: identity` and refuse a
response whose `Content-Encoding` is not identity. A compressing registry
front end or middlebox fails immediately as a structural error with no
sentinel, instead of as a digest mismatch or as a transparent-gzip
disagreement between a direct read and a redirected one.

This campaign did not exercise coded responses. The rows above do not speak
to a registry that compresses the distribution API; those deployments must
turn that coding off before a bigoci pull can succeed. Token-endpoint JSON
may still be gzipped.

## Coverage boundary

This campaign does not establish bigoci compatibility for registries absent
from the table. Consult the go-oci-blob matrix for lower-level evidence about
other registries, then run bigoci's complete push and pull paths before making
a bigoci compatibility claim.
