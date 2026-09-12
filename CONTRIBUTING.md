# Contributing

Use Go 1.26.8 or newer. Run `make test check audit` before submitting changes (Python 3 is required for installer tests). Changes to service management, proxy settings, subscriptions, or recovery also require `make e2e` with Docker.

Keep the CLI and interactive menu on the same implementation path. Persist settings through atomic writes; preserve existing proxy values and unrelated user changes. New subscription formats must be tested against the pinned mihomo binary, not just a format detector.

Never commit real subscriptions, proxy credentials, API secrets, generated runtime configurations, or raw logs from private providers. Use deterministic local fixtures. Document actual test failures and platform limitations.

Dependency upgrades must update the pinned versions and SHA-256 digests and pass the Linux suite. Release tags use `vX.Y.Z`; the release workflow builds both architectures and uploads checksums.
