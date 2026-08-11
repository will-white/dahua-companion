# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

See also `AGENTS.md` for persona/boundary conventions (conventional commits are required — the release agent parses them for SemVer bumps).

## Commands

```bash
go build ./...                       # build
go test ./...                        # all tests
go test -race -v ./...               # what CI runs
go test ./pkg/dahua -run TestIsDoorbellPressed   # single test
go vet ./...                         # static analysis
gofmt -l .                           # CI fails if this prints anything
go run .                             # run locally (needs env vars, see below)
go run . -healthcheck                # probe the local /health endpoint, exit 0/1
docker build -t dahua-companion .
```

CI (`.github/workflows/ci.yml`) runs: `scripts/check-go-version.sh`, `go mod verify`, build, vet, a `gofmt -l` check, then `go test -race`. `security.yml` runs `govulncheck ./...` on push/PR and weekly. `docker-publish.yml` also runs on PRs as a **build-only** validation — every step guards on `github.event_name != 'pull_request'`, so nothing is pushed or signed; it exists to catch a broken `Dockerfile` or action bump before a release.

`go` is **not installed** in the WSL dev environment, but `docker` is. To run the CI checks locally, use the official image (`-buildvcs=false` avoids a git-ownership error on the bind mount, and `-race` needs the non-alpine image for cgo; the named volume keeps the module cache warm between runs):

```bash
docker run --rm -v "$PWD":/app -w /app -v dahua-gomodcache:/go/pkg/mod \
  -e GOFLAGS=-buildvcs=false golang:1.26.5 \
  sh -c 'go build ./... && go vet ./... && gofmt -l . && go test -race ./...'
```

## Configuration

`config.Load()` calls `godotenv.Load(".env.local")` then `godotenv.Load(".env")`. godotenv never overwrites an already-set variable, so **`.env.local` wins over `.env`, and real environment variables win over both**. `.env.example` is committed; `.env` and `.env.local` are gitignored — copy the example and fill in real values.

Required (`envconfig`, fatal if missing): `MQTT_BROKER_URL`, `MQTT_CLIENT_ID`, `MQTT_USERNAME`, `MQTT_PASSWORD`, `HOSTNAME_OR_IP`, `DAHUA_USERNAME`, `DAHUA_PASSWORD`. Optional: `MQTT_TOPIC` (default `doorbell/pressed`), `HEALTH_PORT` (default `8080`), `APP_ENV=development` for human-readable console logging instead of JSON.

The camera credentials are deliberately prefixed `DAHUA_` — bare `USERNAME`/`PASSWORD` collide with variables the surrounding environment may already export, and real env vars beat `.env` files.

## Architecture

Single Go binary. `main.go` wires the pieces with a signal-cancelled `context.Context` and waits on a `WaitGroup` for shutdown:

```
dahua.Listen ──onEvent()──> mqtt.Publish ──queue──> mqtt.ProcessQueue ──> broker
                                       health.Server (:HEALTH_PORT/health)
```

- **`pkg/dahua`** — opens a long-poll HTTP GET to `/cgi-bin/eventManager.cgi?action=attach&codes=[AlarmLocal]&heartbeat=30` using digest auth (`icholy/digest` transport), built with `http.NewRequestWithContext` so cancelling the context aborts the stream immediately. Scans the response line-by-line; `isDoorbellPressed` parses `Code=...;action=...;` lines and only reports `AlarmLocal` with `action=Start` (or absent action). `onEvent` is called with no payload — the event *is* the signal. `Listen` wraps `listen` in `backoff/v5` retry with `MaxElapsedTime(0)` (retry until cancelled); the backoff **resets once a connection is established**, so only consecutive failures grow the reconnect delay. `Probe` is the read-only authenticated camera check (`magicBox.cgi?action=getMachineName`) that health uses; the shared `http.Client` intentionally has no client-wide timeout (the stream is endless), so bounded calls pass a context deadline.
- **`pkg/mqtt`** — `New()` connects with paho `ConnectRetry` enabled (a down broker at boot no longer kills the process) and the reconnect ceiling lowered to 30s. `Publish` is a non-blocking send onto a buffered channel of 100 (full queue = drop the new event). `ProcessQueue` delivers; while the broker is unreachable it holds the current event and polls once a second, and any event older than 30s (`maxEventAge`) is **dropped, not delivered late** — stale doorbell events are worthless by design. QoS 0, not retained.
- **`pkg/health`** — own `ServeMux` (not the default mux), `ReadHeaderTimeout`, listen port from `HEALTH_PORT`. `/health` writes the status code *before* the body and returns 503 unless MQTT is connected, the event stream is connected, and a `Probe` bounded to 5s succeeds. It takes two injected `func() bool`s and the probe func from `main.go` and imports neither `dahua` nor `mqtt`.
- Connection state lives on the clients as `IsConnected()` methods (`atomic.Bool` on dahua, paho's `IsConnectionOpen()` on mqtt) — there are no package-level flags.
- **`pkg/logger`** — zerolog global logger with caller info; Unix timestamps in production, `ConsoleWriter` when `APP_ENV=development`.
- `main -healthcheck` probes the local `/health` endpoint and exits 0/1. It backs the Docker `HEALTHCHECK` (scratch has no shell or curl) and deliberately reads only `HEALTH_PORT`, not the full config.

## Release flow

`release-agent.yml` runs `scripts/release-agent.sh` weekly (Mondays 09:00 UTC) or on dispatch: it diffs commits since the latest tag, picks major/minor/patch from `BREAKING CHANGE` / `feat` prefixes, and opens or updates an issue labeled `release-agent`. Adding the `release-approved` label to that issue triggers `release-publish.yml`, which cuts the GitHub release and calls `docker-publish.yml` to build, push, and cosign-sign the image to `ghcr.io`. Issue title/body reach the workflow shell via `env:` — never template `${{ github.event.issue.* }}` into a `run:` script; the body carries commit text.

Images publish **only** through this flow (there is no tag-push trigger) and are tagged `vX.Y.Z` + `latest`, built for linux/amd64 and linux/arm64.

## Docker

Multi-stage: a `golang:<ver>-alpine` builder → `scratch`, `CGO_ENABLED=0`, `-ldflags="-s -w"`, running as non-root `scratchuser` (only `/etc/passwd` is copied over). The CA bundle is copied from the builder, so an MQTT broker over TLS (`ssl://`) verifies. The `HEALTHCHECK` runs `/app/main -healthcheck` since scratch has no shell.

**The Go version is declared in three places** and must stay consistent — `toolchain` in `go.mod`, the `golang:<ver>-alpine` builder in `Dockerfile`, and the `devcontainers/go:<major>-<go-minor>-bookworm` image in `.devcontainer/devcontainer.json`. Renovate sees three unrelated dependencies, so `renovate.json` groups them into one "go version" PR, and `scripts/check-go-version.sh` fails CI if they drift regardless of how the drift happened. Run it directly after touching any of the three:

```bash
./scripts/check-go-version.sh
```

Note the dev container tag is `<image-definition-major>-<go-minor>-<debian>`; the `1-` definition family stopped at Go 1.24, so current Go requires the `2-` family.

`.dockerignore` is an allow-list (`*` then `!/main.go`, `!/go.mod`, `!/go.sum`, `!/pkg/`). A new top-level source directory is invisible to the build until it is added there.

## Reference

Dahua HTTP API PDFs live in `documentation/`; `AlarmLocal` is the only event this project subscribes to.
