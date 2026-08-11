# Contributing

Thank you for contributing to bigoci.

bigoci has one purpose: efficient upload and download of large files to and from OCI registries.
Changes that add other use cases are out of scope; see the [design document](https://imgoci.github.io/bigoci/explanation/design/) before proposing features.
For private vulnerability reporting, use [SECURITY.md](SECURITY.md) instead of public channels.

## Reporting Bugs

Report non-security bugs through GitHub issues.
Include the following details when possible:

- version, commit, or environment details
- steps to reproduce
- expected behavior
- actual behavior
- logs, screenshots, or a minimal reproduction

If you are reporting a security issue, stop and follow [SECURITY.md](SECURITY.md) instead.

## Pull Requests

Contributors should:

1. Keep changes focused and scoped to a single problem.
2. Add or update tests when behavior changes.
3. Update documentation when user-facing behavior changes.
4. Use Conventional Commit subjects, such as `feat: add config loader` or `fix: handle empty input`.
5. Make sure `moon run root:check` passes before requesting review.

## Local Setup

```sh
mise install         # provision the pinned toolchain (Go, Moon, the dev CLIs)
moon run root:check
```

Useful project commands:

```sh
moon run root:format
moon run root:lint
moon run root:build
moon run root:test
moon run docs:serve
```

## Release Changes

Release Please reads Conventional Commit subjects to build changelogs and release PRs.
Keep release-impacting commits clear; routine docs, CI, and maintenance commits should use the appropriate non-release type.
