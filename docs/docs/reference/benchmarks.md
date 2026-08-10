# Benchmarks

The measured throughput behind the library's defaults: a 512 MiB part size
and 8 workers. The numbers come from bare-metal measurement sessions run in
August 2026 with the repository's `bench/` harness, which drives only the
public API (`Push`, `Pull`, and their options).

## Method

The same-site matrix used two dedicated servers in one Latitude datacenter
(Dallas): a 6-core client running the harness and a 12-core registry box serving
[zot](https://zotregistry.dev) v2.1.20 and CNCF Distribution 2.8.3 from
NVMe-backed volumes. The raw link measured 9.42 Gbit/s (iperf3, four
streams), so 1178 MB/s is the ceiling every local number reads against.
The corrected GHCR slice used a fresh client with the same 6-core plan in
Dallas. Decimal MB/s throughout.

Every cold-push fixture used freshly generated random bytes, so a deduplicating
registry could never skip an upload. Each published matrix cell ran three
iterations; tables show medians. A status-counting transport recorded every
non-2xx/3xx response during timed transfers. The published dataset contains
309 same-site rows and 24 corrected GHCR rows. All 333 measurements succeeded;
none drew a 429 or 503.

The first GHCR slice used a 1 GiB file. It is superseded here because 256 MiB
parts gave it only four parts and 512 MiB parts only two. A configured
eight-worker cell therefore started at most four or two workers. The corrected
4 GiB slice below has 16 or 8 parts, so every configured worker can be active.

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

At the large-file end the default is within 1.6% of the best measured cell,
and small parts pay real per-part overhead: 64 MiB parts cost 11% against
the best. At the selected 512 MiB size, doubling workers from four to eight
raised push by 1.2% and left pull effectively unchanged.

## Registry cross-check, CNCF Distribution, 8 GiB file

Cold push, median MB/s:

| part \ workers | 2 | 4 | 8 |
|---|---|---|---|
| 256 MiB | 678.4 | 758.8 | 917.1 |
| 512 MiB | 655.8 | 907.1 | 908.4 |

Cold pull sat at 717–727 MB/s in every cell. Distribution is slower than
zot on the same hardware. At eight workers, 512 MiB push was one percent below
the best cell, and the two part sizes' pull medians differed by 0.8%.
Registry implementations vary, and the default lands near the best on both
local registries.

## Real cloud registry, GHCR, 4 GiB file

Measured from the matched Dallas client over the public internet,
authenticated, against private packages deleted immediately after the run.
Every cell ran three iterations. Values are median MB/s, with the observed
minimum–maximum in parentheses.

Cold push, median MB/s:

| part \ workers | 4 | 8 |
|---|---|---|
| 256 MiB | 111.2 (109.8–115.5) | 112.3 (112.2–121.2) |
| 512 MiB | 109.0 (108.6–111.2) | 109.5 (101.2–109.9) |

Cold pull, median MB/s:

| part \ workers | 4 | 8 |
|---|---|---|
| 256 MiB | 163.2 (150.5–164.0) | 273.4 (233.0–280.2) |
| 512 MiB | 161.6 (141.8–166.3) | 262.1 (231.5–273.0) |

These are aggregate whole-file rates, not per-connection measurements. Moving
from four to eight workers changed push medians by at most one percent and
raised pull medians by 62–68%. At a fixed worker count, 512 MiB stayed within
1–4% of 256 MiB. This targeted run observed zero 429 or 503 responses with up
to eight maximum-active workers.

## What the data decided

- **Part size stays 512 MiB.** At eight workers it was within 1.6% of zot's
  best cell and one percent of Distribution's. Its GHCR push and pull medians
  were within 2.5% and 4.1% of 256 MiB, while a lost part still costs seconds
  rather than minutes to retry.
- **Workers increase to 8.** At 512 MiB, eight workers changed same-site
  medians by at most 1.3% and GHCR push by 0.4%, while raising GHCR pull from
  161.6 to 262.1 MB/s.
- **Worker count does not self-tune in v1.** The corrected fixed default
  captures the observed GHCR pull scaling, and no published transfer drew a
  throttle response. The design's open question is closed in
  [the design document](../explanation/design.md#open-questions);
  `WithWorkers` remains the override. Revisit adaptation when an observed
  registry or path supplies a concrete trigger.

## Reproducing

The harness, its run specs, and the server runbook live under `bench/` in
the repository. The staged specs (`bench/specs/`) reproduce this matrix;
`bench/latitude/README.md` is the operator runbook. Raw results are
preserved in the engineering journal, not the repository.
