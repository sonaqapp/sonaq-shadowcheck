# ShadowCheck

ShadowCheck audits any Twitch channel through the official Helix API and
returns a 0–100 score plus five findings:

- **Account & status** — account age and Partner/Affiliate standing
- **Search & discovery** — whether the channel is indexed and at which position
- **Metadata** — category, language and tag completeness
- **VOD archiving** — recent broadcasts and how fresh the archive is
- **Live health** — uptime, viewers, category consistency and mature flag

The UI is embedded into the Go binary with `go:embed`. One binary, no
database, no static web root, no external assets.

## Quickstart

```bash
docker build -t shadowcheck .
docker run --rm -p 8080:8080 -e TWITCH_CLIENT_ID -e TWITCH_CLIENT_SECRET shadowcheck
# then open http://localhost:8080
```

Create a Twitch application at <https://dev.twitch.tv/console/apps> and use its
client ID and secret. The container is a distroless image with CA certificates
for the TLS calls to Twitch.

## CLI usage

The same binary works without the web server. Credentials are still read from
the environment:

```bash
export TWITCH_CLIENT_ID=your_client_id
export TWITCH_CLIENT_SECRET=your_client_secret

./shadowcheck --channel redalertdj
./shadowcheck --channel redalertdj --json
```

`--json` prints the complete report shape that the HTTP API returns, which is
handy for scripts and pipes. `--version` prints the release version.

## Configuration

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `TWITCH_CLIENT_ID` | yes | — | Twitch application client ID |
| `TWITCH_CLIENT_SECRET` | yes | — | Twitch application client secret |
| `PORT` | no | `8080` | HTTP port for web mode |

Without valid credentials the server still starts and serves the UI, but
`/api/shadowcheck` answers `503` with a configuration hint.

## HTTP API

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/` | Embedded single-page UI |
| `POST` | `/api/shadowcheck` | Audit a channel, body `{"channel":"name"}` |
| `GET` | `/api/shadowcheck?channel=name` | Same audit, share-link friendly |
| `GET` | `/api/health` | Version and credential status |

Example:

```bash
curl -s -X POST http://localhost:8080/api/shadowcheck \
  -H 'Content-Type: application/json' \
  -d '{"channel":"redalertdj"}' | jq
```

Reports are cached for 60 seconds and limited to 15 audits per minute per
client IP. Request bodies are capped at 2 KB and every audit has a 12 second
timeout.

## Development

```bash
go test ./...
go vet ./...
go build -o shadowcheck .
```

Layout:

```
main.go               audit engine, CLI, HTTP server, embedded UI
shadowcheck_test.go   unit and handler tests
web/index.html        single-page UI, compiled into the binary
Dockerfile            multi-stage build (static binary + distroless)
```

## License

MIT — see [LICENSE](LICENSE). Copyright (c) 2026 sonaq.app.

Maintained by [sonaq.app](https://sonaq.app) · contact@sonaq.app
