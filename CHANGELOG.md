# Changelog

## [0.1.0](https://github.com/imgoci/bigoci/compare/v0.0.1...v0.1.0) (2026-08-10)


### Features

* **api:** progress reporting option ([#44](https://github.com/imgoci/bigoci/issues/44)) ([8a2ba0d](https://github.com/imgoci/bigoci/commit/8a2ba0df1e0b8f9adb5278d936979ca64f010566))
* **api:** transfer orchestrator and public Push/Pull API ([#17](https://github.com/imgoci/bigoci/issues/17)) ([829a861](https://github.com/imgoci/bigoci/commit/829a86105c965681caa054caa27d49def090cc22))
* **auth:** oras-go credentials adapter behind the Auth port ([#29](https://github.com/imgoci/bigoci/issues/29)) ([9944738](https://github.com/imgoci/bigoci/commit/9944738b710d1bfdae09e2a5333935149ddbb716))
* **bench:** throughput harness against local registries ([#32](https://github.com/imgoci/bigoci/issues/32)) ([92e7d52](https://github.com/imgoci/bigoci/commit/92e7d52f0bf62758c458648f211bad424d1eea5c))
* **core:** split planner and manifest codec ([#15](https://github.com/imgoci/bigoci/issues/15)) ([7c40d85](https://github.com/imgoci/bigoci/commit/7c40d85f35e8722c8bb7de5ce9273f1bdb9f4308))
* **oci:** classify a refused request as unauthorized ([#28](https://github.com/imgoci/bigoci/issues/28)) ([7c71aea](https://github.com/imgoci/bigoci/commit/7c71aeac83f596f9e4eb33ea05de7d37b29dcaef))
* **oci:** classify registry failures for retry ([#21](https://github.com/imgoci/bigoci/issues/21)) ([cb02877](https://github.com/imgoci/bigoci/commit/cb02877471cf8404a3abba547a10df5413557d49))
* **oci:** presigned redirect handling ([#30](https://github.com/imgoci/bigoci/issues/30)) ([521753d](https://github.com/imgoci/bigoci/commit/521753de4f6bc1779434b9b35eff7309f12e293e))
* **oci:** report the offset a blob read starts at ([#25](https://github.com/imgoci/bigoci/issues/25)) ([2b06ecf](https://github.com/imgoci/bigoci/commit/2b06ecfb0a2e05d581445213304bd7532fbb08f6))
* **ports:** port definitions and adapters for OCI and file I/O ([#16](https://github.com/imgoci/bigoci/issues/16)) ([a1e423d](https://github.com/imgoci/bigoci/commit/a1e423d4c5e9fda6d4026bbfd47d4ad805aeafce))
* **transfer:** per-part retry with backoff ([#23](https://github.com/imgoci/bigoci/issues/23)) ([4dca285](https://github.com/imgoci/bigoci/commit/4dca285dfaa371b15f8c186a9486cc6ccd61a4e1))
* **transfer:** pull resume from partial files ([#26](https://github.com/imgoci/bigoci/issues/26)) ([d789849](https://github.com/imgoci/bigoci/commit/d789849f0de325f2e8675d6e4041b7e67b30da34))


### Bug Fixes

* **bench:** preserve measurement validity and correct defaults ([#40](https://github.com/imgoci/bigoci/issues/40)) ([3b351c6](https://github.com/imgoci/bigoci/commit/3b351c65d6d5047b48702b14a2d1c371f6bb19d7))
* **docs:** bump pymdown-extensions to 11.0.1 ([#10](https://github.com/imgoci/bigoci/issues/10)) ([ea895a8](https://github.com/imgoci/bigoci/commit/ea895a8b2e88880e9f58b8bd4643bd542b55fc62))
* **file:** reject unsafe pull partials ([#43](https://github.com/imgoci/bigoci/issues/43)) ([8e3b2c7](https://github.com/imgoci/bigoci/commit/8e3b2c7ae16c812a9087984076919e6e98772f01))
* **manifest:** bound decoding allocations ([#46](https://github.com/imgoci/bigoci/issues/46)) ([cab0dd4](https://github.com/imgoci/bigoci/commit/cab0dd42a27346e858d939e00c2ef559649eee0a))
* **oci:** isolate off-origin upload sessions ([#41](https://github.com/imgoci/bigoci/issues/41)) ([f8e8389](https://github.com/imgoci/bigoci/commit/f8e8389ceddefe1f784f4c7a627c659e9db343ff))
* **transfer:** make resume verification cancellable ([#42](https://github.com/imgoci/bigoci/issues/42)) ([f89e3c4](https://github.com/imgoci/bigoci/commit/f89e3c47fd6941a9be935d40feaa171325c2f3ed))

## Changelog
