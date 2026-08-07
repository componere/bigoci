# Technical Notes

- Use hexagonal architecture at all times. Keep business logic isolated from CLI, filesystem, network, storage, and other external adapters.
- Prefer functional testing before calling any feature complete. Unit tests are useful, but they do not prove the tool works the way the design intends.
- Take an agile approach to development. Avoid waterfall: underspecify when useful, prototype early, learn from the result, and refine from working behavior.
- The buildable spec is `docs/docs/explanation/design.md` (+ `docs/docs/reference/format.md` for the artifact contract). Read it before writing library code; implementation order is its "First slice" section.
- Rely only on universally supported registry operations (monolithic blob PUT, blob GET). Chunked upload, v1.1 resumable push, and Range support vary by registry; the design deliberately avoids depending on them.
- Release automation: release-please on plain GITHUB_TOKEN; pre-1.0 versioning is organic (feat -> minor). Do not set the manifest baseline to 0.0.0 (release-please bootstrap trap proposes 1.0.0) and do not re-enable bump-patch-for-minor-pre-major. The Actions "can create PRs" permission lives outside repository-settings.toml (Actions permissions API, org + repo).
