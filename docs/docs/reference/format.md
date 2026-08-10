---
title: Format
description: The bigoci artifact format contract.
---

# Format reference

A bigoci artifact stores one file as an OCI image manifest whose layers are
consecutive byte ranges of the file, called parts. This page is the contract:
an implementation that follows it can read and write bigoci artifacts without
the library. For the reasoning behind the format, see the
[design](../explanation/design.md).

## Split rule

- The pusher picks a part size `P` in bytes. Part `i` covers bytes
  `[i*P, min((i+1)*P, size))`. The final part may be shorter. A file of size
  `P` or smaller has exactly one part.
- Parts are raw byte ranges. No compression, tar, or framing.
- Reconstruction: concatenate the layers in manifest order.

## Limits

- Part count: 1 to 4096. The cap keeps manifests small (a 4096-layer
  manifest is roughly 600 KB against the 4 MiB manifest size limit common in
  registries). Pushing a larger file requires a larger `P`.
- At the default 512 MiB part size, the cap allows files up to 2 TiB.

## Manifest

A standard OCI image manifest, `application/vnd.oci.image.manifest.v1+json`,
per distribution spec v1.1.

| Field | Value |
|---|---|
| `artifactType` | `application/vnd.bigoci.file.v1` |
| `config` | the OCI empty descriptor: media type `application/vnd.oci.empty.v1+json`, size 2, digest `sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a`, content `{}` |
| `layers` | the parts, in file order, media type `application/vnd.bigoci.file.part.v1` |

The empty config blob must exist in the repository before the manifest is
pushed; registries reject manifests that reference blobs they do not hold.

Every digest in a v1 artifact uses sha256: the layer digests and the
whole-file digest.

## Annotations

Manifest-level annotations:

| Key | Value |
|---|---|
| `io.bigoci.file.digest` | sha256 of the complete file, as `sha256:<hex>` |
| `io.bigoci.file.size` | file size in bytes, as a decimal string |
| `io.bigoci.part.size` | part size `P` in bytes, as a decimal string |
| `org.opencontainers.image.title` | the file name at push time; informational |

Layers carry no annotations. A part's position is its index in `layers`; its
digest and size are in its descriptor.

`vnd.bigoci` and `io.bigoci` are private, unregistered namespaces.

## Example

A 732.5 MiB file pushed at the default 512 MiB part size:

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "artifactType": "application/vnd.bigoci.file.v1",
  "config": {
    "mediaType": "application/vnd.oci.empty.v1+json",
    "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
    "size": 2
  },
  "layers": [
    {
      "mediaType": "application/vnd.bigoci.file.part.v1",
      "digest": "sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eb7b7e3e6e0e4b6ed6a1a77e",
      "size": 536870912
    },
    {
      "mediaType": "application/vnd.bigoci.file.part.v1",
      "digest": "sha256:1f8f9a0a3e9c0f5a02cf47a1a2f8c0be7a05a2f0d5a5b3c67e2f0a8f9b0c1d2e",
      "size": 231211008
    }
  ],
  "annotations": {
    "io.bigoci.file.digest": "sha256:9c56cc51b374c3ba189210d5b6d4bf57790d351c96c47c02190ecf1e430c14ed",
    "io.bigoci.file.size": "768081920",
    "io.bigoci.part.size": "536870912",
    "org.opencontainers.image.title": "model.bin"
  }
}
```

## Determinism

The manifest is a pure function of the file bytes, the part size, and the
file name. It contains no timestamps and no other nondeterministic fields.

Writers must produce the canonical encoding:

- Compact JSON, no insignificant whitespace.
- Members in the order the example above shows.
- Annotation keys sorted lexicographically.
- String values as raw UTF-8, escaped only where JSON requires it. In
  particular, no HTML escaping of `&`, `<`, or `>`.

Two conforming writers therefore produce byte-identical manifests, and the
same manifest digest, for the same file, part size, and file name. Re-pushing
a file at the same part size reproduces the same manifest digest. Anything
bound to that digest stays valid across re-pushes: signatures, SBOM
attachments, referrers, and any index that references the manifest.

## Versioning

The `.v1` suffix in the media types is the format version. A breaking format
change means new media types. Readers keep accepting every version they know.
