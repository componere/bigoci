# Phase 6 — Benchmark harness and measured defaults

## Context

Phases 1–5 are complete: bigoci pushes/pulls large files against real registries with retries, resume, auth, and presigned redirects. Phase 6 is Measurement (`.journal/002/PLAN.md`): the design doc's Defaults table (512 MiB parts, 4 workers) is explicitly provisional, and the one open design question — adaptive worker count on 429/503 — awaits harness data. `options.go:24` (`DefaultPartSize`) and `options.go:32` (`DefaultWorkers`) carry godocs promising measured values before v1.

User constraints:
1. **Right-sized and extensible** — no over-engineered or brittle benchmark code; changing the matrix must never be a chore.
2. **Latitude bare metal via `lsh`** (authenticated; project `proj_gXQvNexkl0zpb`, SSH key `ssh_owPOap9PK0B4x` "macbook"). Billed per hour: provision once, run everything, destroy.
3. **Benchmark code cleanly separated** from the rest of the repo.

User decisions (asked and answered): two servers in one site (client box + registry box running zot AND CNCF Distribution); GHCR subset rows from the client box (session-006-style throwaway private package, PAT at run time, deleted after); **no nightly/scheduled CI job** — manual only, PLAN.md's nightly criterion amended with a dated note.

## Design

### Harness: a third module, `bench/`

Mirrors `cli/` exactly: own `go.mod` (`module github.com/componere/bigoci/bench`, `replace github.com/componere/bigoci => ../`), never released, never imported, flat `package main`, **stdlib `flag`** (the reference CLI's idiom — no cobra). No build tags: CI lints/builds/unit-tests the module; measurement only happens when a human runs the binary with a spec. The harness drives only the public API (`New`, `Push`, `Pull`, `FromFile`, `ToFile`, `WithPlainHTTP`, `WithCredentials`, `WithHTTPClient`, `WithPartSize`, `WithWorkers`) — doubling as a realism check on it.

```
bench/
  go.mod, go.sum
  main.go        # signal-wired entry (copy cli/main.go pattern)
  run.go         # flags, subcommand dispatch, run loop over cells
  spec.go        # Spec types, JSON load/validate, size-string parsing (port cli/size.go idea)
  matrix.go      # Spec -> []Cell cross-product; deterministic cell IDs
  scenario.go    # scenario interface + registry map; coldPush/warmPush/coldPull
  target.go      # target -> *bigoci.Client; status-counting RoundTripper via WithHTTPClient
  fixture.go     # seeded ChaCha8 file generation (port e2e newRandomFile idea;
                 #   seed = f(run_id, cell_id, iteration) -> unique bytes per iteration)
  result.go      # Row type, JSONL writer, resume-set loader
  summarize.go   # JSONL -> markdown median-MB/s grids + long-form stats table
  *_test.go      # unit tests: spec parse, matrix expansion, fixture determinism,
                 #   summarize output — no network
  specs/         # smoke.json, stage1.json, stage2.json, stage3-dist.json, stage4-ghcr.json
  latitude/      # provision.sh, setup-registry.sh, run.sh, destroy.sh,
                 #   zot-config.json, README.md (runbook)
  moon.yml, README.md
```

Two subcommands:
- `bench run -spec specs/stage1.json -out results/stage1.jsonl [-resume] [-endpoint zot=IP:5000]`
  - `-endpoint name=host:port` overrides spec endpoints, so checked-in specs carry placeholders and nobody edits JSON on a server.
  - `-resume` skips (cell, scenario, iteration) rows already in the output file — a crash 40 minutes into a paid run must not restart from zero (~20 lines; rows carry deterministic IDs).
- `bench summarize -in results/stage1.jsonl` — same binary (shares the Row type), emits per registry×file-size×scenario a markdown grid of median MB/s (rows=part size, cols=workers) plus mean/median/stddev/min/max and a `parts` column so `workers > parts` cells are visibly parallelism-capped. Stats inline; **no benchstat, no plotting, no stats deps**.

### Run-spec schema (checked-in JSON; the extensibility answer)

```json
{
  "run_id": "2026-08-stage1",
  "targets": [
    {"name": "zot",  "endpoint": "REGISTRY:5000", "plain_http": true, "repo_prefix": "bench"},
    {"name": "ghcr", "endpoint": "ghcr.io", "repo_prefix": "<owner>/bigoci-bench", "auth_env": "GHCR"}
  ],
  "scenarios":  ["cold-push", "warm-push", "cold-pull"],
  "part_sizes": ["64MiB", "128MiB", "256MiB", "512MiB", "1GiB"],
  "workers":    [1, 2, 4, 8, 16],
  "file_sizes": ["2GiB"],
  "iterations": 3,
  "verify":     "first"
}
```

- Matrix = targets × part_sizes × workers × file_sizes × iterations; scenarios are phases *within* an iteration, not an axis. Cell ID `zot-p512MiB-w4-f2GiB`; repo per cell `<repo_prefix>/<run_id>/<cell_id>`.
- `auth_env: "GHCR"` → `WithCredentials($GHCR_USERNAME, $GHCR_TOKEN)`. Secrets never in specs or argv.
- `verify`: `none|first|all` — full sha256 of the pulled file on iteration 0 only (hashing every 16 GiB pull would double cost); size checked on every pull; push descriptor checked non-zero.
- New stage = new spec file. New axis value = JSON edit. New scenario = one entry in scenario.go's map. No templating, no includes, no per-scenario overrides.

### Scenarios and warm/cold semantics

Per iteration: **setup (untimed)** — generate unique fixture (unique seed per iteration so zot's global dedupe can never turn a cold push into a silent no-op — load-bearing rule from session lessons); **cold-push (timed)** — fresh bytes, fresh tag; **warm-push (timed)** — same file re-pushed to a second tag in the *same repo* (HEAD-skip path; same-repo works on both zot and Distribution); **cold-pull (timed)** — fresh destination path (no partial → no resume hashing); **verify+cleanup (untimed)**.

Deliberate exclusions: no registry cache-dropping (hot-cache pull is the honest common case; dropping would need mid-run SSH into the registry box — exactly the coupling to avoid; one methodology sentence states it). No warm-pull/resume scenarios (they measure local hashing, not defaults). No 429-injection — real GHCR behavior is the evidence.

**Open-question instrumentation:** `target.go` wraps the transport with a RoundTripper counting non-2xx/3xx responses per timed phase; each row records `http_status: {"429": n, ...}`. The adaptive-concurrency verdict becomes counts + scaling curves, not vibes.

Output: JSONL, one self-contained row per (cell, scenario, iteration) — schema version, run_id, ts, cell_id, registry, scenario, part_size, workers, file_size, parts, iteration, wall_ms, mb_per_s, http_status, error, commit. A failed phase emits an `error` row and the run continues; `-resume` retries missing/failed rows.

### Registry boxes

zot v2.1.20 (matches the e2e pin) on :5000 and Distribution registry:2.8.3 on :5001, docker containers with NVMe-backed volumes, using the e2e suite's minimal zot config (error-level logs). zot dedupe stays at default-on (unique bytes make it unobservable; default config is what users run). No deletion/GC between iterations — unique repo per cell, ~700 GiB total growth fits 1.9 TB NVMe; destroy the box at the end.

### Staged matrix (~3–4 h × 2 boxes ≈ $5–7 at m4-metal-small $0.81/hr)

| Stage | Where | Matrix | Est. wall |
|---|---|---|---|
| 0 smoke | laptop, docker zot, **before provisioning** | 64 MiB file, p{16,32MiB} × w{2,4}, N=1 | ~2 min |
| 1 coarse | Latitude, zot | 2 GiB × p{64,128,256,512,1024MiB} × w{1,2,4,8,16}, N=3 | 25–35 min |
| 2 finalists | Latitude, zot | 16 GiB × p{2–3 winners} × w{2,4,8}, N=3 | 35–50 min |
| 3 Distribution | Latitude | 8 GiB × p{256,512MiB} × w{2,4,8}, N=3 | 10–15 min |
| 4 GHCR | Latitude client box → ghcr.io | 1 GiB × p{256,512MiB} × w{2,4,8}, N=2 | 10–40 min |

Stage-2 part sizes chosen by the operator from stage-1 medians (edit one JSON array — why specs are files). Interpretation caveats to carry into the methodology notes: the client hashes every part, so workers=1 cells are sha256-bound (~2 GB/s/core) — a real client-side ceiling users experience, not the network.

### Latitude orchestration (thin shell, operator-run, never CI)

`bench/latitude/`: `provision.sh <site>` (lsh create × 2 m4-metal-small, ubuntu_24_04_x64_lts, same site from stock — DAL/NYC/MIA2/LAX2/SJC2 candidates; poll ready; write `hosts.env`), `setup-registry.sh` (ssh: install docker, start both registries, curl `/v2/` readiness, **iperf3 the raw link once** so throughput numbers have a denominator), `run.sh <spec> <out>` (scp cross-compiled linux binary + spec, execute via ssh under local tmux, scp JSONL back), `destroy.sh` (lsh destroy both, reminder to delete the GHCR package). No Go toolchain on the boxes. Runbook README covers the full sequence including the smoke-test-before-spending-money rule and summarize-before-destroy rule.

## Execution plan

### PR 1 — `feat(bench): throughput harness`
All of `bench/` plus integration touches outside it:
- `.moon/workspace.yml` — add `bench: 'bench'` to sources.
- `moon.yml` (root) — add `bench:check` to `root:check` deps.
- `bench/moon.yml` — clone of `cli/moon.yml` (`--disable gomoddirectives`, `--allow-parallel-runners`, `/`-prefixed goSources so core edits invalidate bench tasks).
- `.github/workflows/ci.yml` — add `bench/go.sum` to the Go cache `hashFiles(...)` keys.
- `.github/dependabot.yml` — gomod entry for `/bench`.
- `release-please-config.json` — `exclude-paths: ["cli", "bench"]`.
- `.gitignore` — `bench/results/` and `bench/latitude/hosts.env`.

### Measurement session (between PRs, journaled)
Follow the runbook: smoke locally → provision → stages 1–4 (GHCR stage uses a throwaway private package + PAT Josh provides at run time) → summarize each stage → sanity-check → destroy servers, delete GHCR package. Raw JSONL + summaries + operator notes land in `.journal/007/` on the journal branch. Server teardown is verified with `lsh servers list` before the session ends.

### PR 2 — `chore: set measured defaults`
- `options.go` — `DefaultPartSize`/`DefaultWorkers` set to measured values (or kept, if data confirms) with godocs rewritten from "provisional" to "measured on <hardware>, <date>, see the benchmarks reference".
- `docs/docs/reference/benchmarks.md` — new reference page: methodology (hardware, link measurement, unique-content rule, page-cache and hash-bound caveats), summarize-generated median grids, chosen defaults, date. Plain Language, Diátaxis reference conventions.
- `docs/mkdocs.yml` — nav entry under Reference.
- `docs/docs/explanation/design.md` — Defaults table reasoning updated to cite measurement; Open questions: adaptive-worker-count answered with the GHCR status counts + scaling data (expected verdict shape: "not warranted for v1, `WithWorkers` is the escape hatch" or "warranted, filed as follow-up").

### Journal (journal branch, session 007)
`.journal/002/PLAN.md` — phase-6 boxes checked with dated evidence; the nightly-job criterion amended with a dated note ("manual-only by decision, 2026-08-09"). NOTES.md checkpoints throughout; DESIGN preserved.

## Guardrails (what is NOT built)
No plotting, no stats libraries, no benchstat, no `testing.B`, no build tags, no adaptive scheduling in the harness, no registry lifecycle in Go (docker via scripts only), no scheduled/nightly CI of any kind, no cache-drop SSH gymnastics, no benchmarking of internals.

## Verification
- **PR 1:** `mise x -- env -u GOROOT moon run root:check` green (includes new `bench:check`); `bench run -spec specs/smoke.json` against a local docker zot completes and `bench summarize` renders the grid; unit tests cover spec parsing, matrix expansion, fixture determinism, summarize output.
- **Gates (PLAN.md phase 6):** harness reproduced on real hardware outside CI (the Latitude run) with numbers recorded and sanity-checked against the measured link speed; chosen defaults demonstrably beat the naive alternatives in the recorded matrix; adaptive-concurrency decision recorded with data in design.md.
- **PR 2:** docs build green; golden e2e/unit suites still green under any changed defaults.
- **Cost check:** `lsh servers list` empty after the session.
