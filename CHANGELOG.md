# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.0] - 2026-11-18

Standalone release: ShadowCheck is now a single-binary Twitch channel checker.

### Added

- `--channel <name>` CLI flag to run one audit directly in the terminal.
- `--json` flag to print the full report as JSON from the CLI.
- `--version` flag.
- Embedded web UI (`web/index.html`) compiled into the binary with `go:embed`,
  so web mode needs no static directory.
- `GET /api/shadowcheck?channel=<login>` for share links.
- `Dockerfile` with a multi-stage build and a distroless runtime image.
- Three-line Docker quickstart in the README.

### Changed

- Configuration reduced to `TWITCH_CLIENT_ID`, `TWITCH_CLIENT_SECRET` and the
  optional `PORT` (default `8080`).
- Health endpoint now reports the version and whether credentials are loaded.
- The UI is a compact audit app: channel input, score, five checks and a
  copyable share link.

### Removed

- The previous hosted-service signup backend and its local database,
  encryption and notification stack.
- The marketing landing page and signup form from the public UI.
- VPS-specific deployment assets (reverse proxy, service units, TLS scripts).

[0.3.0]: https://github.com/sonaqapp/sonaq-shadowcheck/releases/tag/v0.3.0
