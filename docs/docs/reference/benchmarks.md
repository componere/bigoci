# Benchmarks

The measured throughput behind the library's defaults: a 512 MiB part size
and 4 workers. The numbers come from one bare-metal measurement session run
in August 2026 with the repository's `bench/` harness, which drives only the
public API (`Push`, `Pull`, and their options).

## Method

Two dedicated servers in one Latitude datacenter (Dallas): a 6-core client
running the harness and a 12-core registry box serving
[zot](https://zotregistry.dev) v2.1.20 and CNCF Distribution 2.8.3 from
NVMe-backed volumes. The raw link measured 9.42 Gbit/s (iperf3, four
streams), so 1178 MB/s is the ceiling every local number reads against.
Decimal MB/s throughout.

Every transfer used freshly generated random bytes, so a deduplicating
registry could never skip an upload. Each matrix cell ran three iterations
(two against GHCR); tables show medians. A status-counting transport
recorded every non-2xx/3xx response during timed transfers. All 333
recorded measurements succeeded; none drew a 429 or 503.

Two caveats frame the numbers. Pulls right after pushes read from the
registry's page cache, which matches how hosted registries serve hot blobs
but understates cold-disk cost. And a push reads and hashes its source once
regardless of the network: on the client's hardware that single-pass
sha256-plus-read floor measured ~1125 MB/s, which is the honest ceiling for
low worker counts.

## Part size × workers, zot, 16 GiB file

Cold push, median MB/s:

| part \ workers | 4 | 8 |
|---|---|---|
| 64 MiB | 913.5 | 948.6 |
| 128 MiB | 986.8 | 1056.8 |
| 256 MiB | 1067.4 | 1069.5 |
| 512 MiB | 1040.0 | 1052.8 |

Cold pull sustained 897–925 MB/s in every cell of the same matrix.

At the large-file end the default is within 2% of the best measured cell,
and small parts pay real per-part overhead: 64 MiB parts cost 11% against
the best. Doubling workers from four to eight bought 1–3%.

## Registry cross-check, CNCF Distribution, 8 GiB file

Cold push, median MB/s:

| part \ workers | 2 | 4 | 8 |
|---|---|---|---|
| 256 MiB | 678.4 | 758.8 | 917.1 |
| 512 MiB | 655.8 | 907.1 | 908.4 |

Cold pull sat at 717–727 MB/s in every cell. Distribution is slower than
zot on the same hardware and favors the larger part at four workers —
registry implementations vary, and the default lands near the best on
both.

## Real cloud registry, GHCR, 1 GiB file

Measured from the Dallas client over the public internet, authenticated,
against a private package (deleted after the run).

Cold push, median MB/s:

| part \ workers | 2 | 4 | 8 |
|---|---|---|---|
| 256 MiB | 89.4 | 93.8 | 78.0 |
| 512 MiB | 57.3 | 98.6 | 92.0 |

Cold pull, median MB/s:

| part \ workers | 2 | 4 | 8 |
|---|---|---|---|
| 256 MiB | 88.6 | 145.0 | 155.2 |
| 512 MiB | 58.5 | 86.9 | 88.7 |

GHCR accepts pushes at roughly 90–100 MB/s whatever the shape — the
85–90 MB/s per-connection figure the design borrowed from S3 measurements,
observed directly. Pulls scale somewhat with workers at the smaller part
size because more parts mean more parallel presigned downloads. No cell
drew a single 429 or 503, at any worker count.

## What the data decided

- **Part size stays 512 MiB.** Within 2% of the best cell at 16 GiB on
  zot, best on Distribution at the default worker count, tied within noise
  on GHCR — and a lost part still costs seconds to retry.
- **Workers stay 4.** About 90% of a 10 Gbit/s link on push; eight workers
  bought at most 3% locally and nothing reliable against GHCR.
- **Worker count does not self-tune.** Zero throttling responses across
  every recorded transfer, and the fixed default landed within a few
  percent of best across a 40× spread in per-connection throughput. The
  design's open question is closed in
  [the design document](../explanation/design.md#open-questions);
  `WithWorkers` is the escape hatch.

## Reproducing

The harness, its run specs, and the server runbook live under `bench/` in
the repository. The staged specs (`bench/specs/`) reproduce this matrix;
`bench/latitude/README.md` is the operator runbook. Raw results are
preserved in the engineering journal, not the repository.
