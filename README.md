# template-go

`template-go` is the reusable Go repository starter for Meigma projects.
It includes a small Go CLI skeleton, Moon tasks, pinned CI, Dependabot, baseline repository security settings, and an enabled Release Please plus GoReleaser release layer.

## Local Bootstrap

Prerequisites:

- [mise](https://mise.jdx.dev) — provisions every pinned tool from `mise.toml` +
  `mise.lock`: Go, Moon, Python + uv (for the MkDocs docs project), the
  `golangci-lint` CLI, and `melange`/`apko`/`cosign` for releases. Run
  `mise install` once; there is nothing else to install by hand.

Tool versions live in `mise.toml`; `mise.lock` records a per-platform download URL
and checksum for each (and, for the aqua-backed CLIs, cosign/SLSA/GitHub-attestation
verification). `mise install` runs with `locked = true`, so it **fails closed** if a
tool lacks a pre-resolved, checksummed entry for the current platform. Moon runs every
task against these tools as `system` binaries on PATH and manages no toolchain itself.
To bump a tool, edit its version in `mise.toml`, run
`mise lock --platform linux-x64,linux-arm64,macos-x64,macos-arm64`, and commit
`mise.toml` + `mise.lock`.

After creating a new repository from this template, replace the placeholder names before doing feature work:

```sh
go mod edit -module github.com/meigma/YOUR_REPO
mv cmd/template-go cmd/YOUR_BINARY
```

Then update `template-go` references in the Moon tasks, GoReleaser config, `ghd.toml`, README, and package docs.

## Common Tasks

Moon is the standard task front door:

```sh
moon run root:format
moon run root:lint
moon run root:build
moon run root:test
moon run root:check
```

CI runs the same aggregate check:

```sh
moon ci --summary minimal
```

The starter CLI is intentionally small:

```sh
go run ./cmd/template-go --version
go run ./cmd/template-go --message "hello from cobra"
go test ./...
```

The CLI entrypoint uses Cobra and Viper in the same shape as other Meigma CLIs: `cmd/template-go` stays thin, `internal/cli` owns command construction, and Viper-backed flags can also be supplied through `TEMPLATE_GO_*` environment variables.

## Container Image

The image is built **without a Dockerfile**:
[melange](https://github.com/chainguard-dev/melange) compiles the binary into a
signed [Wolfi](https://github.com/wolfi-dev) apk (`melange.yaml`), and
[apko](https://github.com/chainguard-dev/apko) assembles it into a minimal,
multi-arch, non-root runtime image (`apko.yaml`) — the modern equivalent of the
former distroless image (uid 65532, ca-certificates, tzdata, no shell). Each
architecture builds natively (no QEMU). Build and run it locally with the bundled
mise task (it uses melange's Docker runner, so Docker must be running):

```sh
mise run image-local              # build the host-arch image, load as template-go:dev
docker run --rm template-go:dev --version
docker run --rm template-go:dev --message "hello from container"
```

The Wolfi base intentionally floats to the latest packages (fresh CA bundle and
timezones, low CVE surface); the exact resolved versions are recorded in the
per-build SBOM and provenance attestation rather than pinned. `version`, `commit`,
and `date` are stamped into the binary via melange `--vars-file` — the release
workflow supplies the real values, and `mise run image-local` uses `dev`.

## CI and Security

The default CI workflow keeps permissions minimal, pins external actions, disables checkout credential persistence, and delegates checks to Moon.
It uses GitHub-hosted dependency caches for Go, golangci-lint, and uv download artifacts while leaving Moon remote caching as an optional follow-up for repositories that need a shared task-output cache.
The docs workflow builds the MkDocs site on pull requests and deploys `docs/build` to GitHub Pages from the default branch.
The scheduled security scan workflow builds the local container image weekly, scans it for high/critical fixed vulnerabilities, and uploads SARIF results to GitHub code scanning.
Dependabot covers GitHub Actions, the root Go module, and the docs uv project.

Repository settings live in `.github/repository-settings.toml`.
They default to immutable releases, private vulnerability reporting, signed commits, squash-only merges, GitHub Pages workflow publishing, and protected tags.

## Release Layer

Release automation is enabled for the template application so this repository proves the full binary and container release lifecycle before generated projects inherit it.
Repositories generated from the template should update the release app credentials, package names, asset patterns, container image name, and `ghd.toml` signer workflow before cutting their first release.

The release path is:

- Release Please opens and maintains the release PR.
- Release Please creates a draft GitHub release and tag after merge.
- Release Dry Run rehearses the GoReleaser binary path and the native-runner melange/apko container build path on pull requests.
- GoReleaser builds binaries, checksums, and SBOMs without publishing directly.
- The release workflow uploads assets to the draft release; a separate, isolated reusable workflow (`attest.yml`) generates the GitHub-hosted provenance attestation for the binary checksums.
- The release workflow builds amd64 and arm64 apks with melange on native GitHub-hosted runners, assembles and publishes `ghcr.io/meigma/template-go:vX.Y.Z` as a multi-platform manifest with apko, signs it with keyless cosign, and attaches a syft SBOM attestation; the isolated `attest.yml` workflow then creates the GitHub-native provenance attestation for the manifest digest.
- Generating both provenance attestations in the isolated `attest.yml` reusable workflow (not in the build job) keeps the signing identity unreachable by build steps — the SLSA Build L3 isolation requirement — while staying on GitHub's attestation API (verify with `gh attestation verify --signer-workflow …/attest.yml`).
- A human inspects the draft release before publication.

The root `ghd.toml` matches the default GoReleaser output so generated projects can be installed with `ghd` once the release workflow runs.
After cloning this template, update `provenance.signer_workflow`, package names, asset patterns, binary paths, and image names to match the new repository and binary name.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines, local setup expectations, and pull request workflow.

## Security

See [SECURITY.md](SECURITY.md) for supported versions and the private vulnerability reporting path.

## License

Add the repository license before publishing a project generated from this template.
