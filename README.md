# Dahua Companion

The Dahua Companion allows you to integrate your [Dahua](https://www.dahuasecurity.com/) and [Amcrest](https://amcrest.com/wifi-video-doorbell-cameras.html) doorbell's button/ring/buzzer with any home automation that can listen to queue / events.

The Dahua Companion will listen for the button/ring/buzzer and then send a message/event/topic via MQTT e.g. `doorbell/pressed`, with no payload by default.

## How it works

Dahua cameras report events via a long polling HTTP connection. And what you subscribe to affects what events the camera reports back with.

Dahua's API is included in the documentation folder.

For this project we publish two kinds of event:

- **Doorbell presses** — `AlarmLocal`, the buzzer/ringer/button pressed event. Published to `doorbell/pressed`.
- **Human detections** — `SmartMotionHuman`, the camera's human smart motion detection (SMD), and the IVS rules `CrossLineDetection` (tripwire) and `CrossRegionDetection` (intrusion) when their event data names a `Human` object. Published to `doorbell/human`. The camera needs smart motion detection with the *Human* object type, or an IVS rule with *Human* as a target, enabled; set `MQTT_HUMAN_TOPIC=` (empty) to turn this off entirely. Smart motion detection fires repeatedly while someone lingers, so detections within `HUMAN_COOLDOWN` (10s) of the last published one are suppressed.

More specifically we publish only the *started* events, since we don't care when Dahua decides an alarm or detection is "over". By default the message has no payload — the event is the signal. Set `MQTT_PAYLOAD_FORMAT=json` to get a JSON body instead:

```json
{"event_type":"press","timestamp":"2026-10-26T06:00:00.123Z","channel":0}
```

`event_type` is `press` or `detected` (human), `timestamp` is when the camera event arrived (UTC), and `channel` is the camera's event index (always `0` on a single-camera doorbell). With HA discovery, these fields show up as attributes on the event entity.

This project also has wrappers around the HTTP subscription and MQTT broker to always make sure it's connected: the camera stream runs under a heartbeat watchdog so a dead connection is rebuilt within seconds, and publishes are acknowledged by the broker. Events are buffered briefly while the broker reconnects, but anything older than 30 seconds is dropped instead of delivered late — a doorbell press only matters while someone might still be at the door. Presses are queued separately from, and always delivered ahead of, human detections, so a burst of detections can never delay or crowd out a ring.

### Snapshots

Set `SNAPSHOT=press,human` (or just one of them) and each event is followed by a JPEG still from the camera on `<event topic>/snapshot` — `doorbell/pressed/snapshot` and `doorbell/human/snapshot` by default. With discovery, Home Assistant gets two image entities, **Last ring** and **Last person**, so a ring's picture is never overwritten by someone walking past afterwards.

The event itself is published first and never waits on the camera. By default (`SNAPSHOT_SOURCE=request`) the still is fetched right after the event, about a second later, so the person may have moved slightly. `SNAPSHOT_SOURCE=event` instead opens the camera's event-snapshot stream, which pushes the still the camera captured *at the moment of* the event — better timing, but not every camera firmware supports it (if yours doesn't, the log shows the stream failing to connect; switch back to `request`). In that mode a still can occasionally arrive just before its event. Only one snapshot is fetched at a time: while one is in flight further detections get none, but a press takes over from a detection's fetch. Peak memory stays around one image (≈100–400 KB) however busy the doorway, and snapshots are never retained on the broker or held for a reconnect — if the broker is down, the still is dropped.

Availability is published retained on `doorbell/availability` (`online`/`offline`): it goes `offline` when the camera stream drops and an MQTT Last Will covers the process dying outright, so your automation can tell you the doorbell is *down* instead of just going quiet. Point your Home Assistant entity/trigger availability at that topic.

Set `MQTT_DISCOVERY_PREFIX=homeassistant` and the doorbell announces itself via [MQTT discovery](https://www.home-assistant.io/integrations/mqtt/#mqtt-discovery): Home Assistant auto-creates a device with a doorbell **Event entity** (plus a motion Event entity for human detection), events and availability already wired — no YAML needed.

A `/metrics` endpoint on the same port serves Prometheus metrics: rings and detections received, published and dropped (with the reason), suppressed detections, average camera-to-broker latency, snapshot outcomes, camera stream reconnects, and whether the camera stream and broker are up right now. With `SNAPSHOT_SOURCE=event`, the snapshot stream gets its own up gauge and connect/failure counters, so a camera that refuses it shows up on a dashboard rather than only in the logs.

A `/health` endpoint (port `8080` by default) returns 200 only when the MQTT connection is up, the event stream is attached, and the camera answers a live read-only probe. A successful probe is reused for 60 seconds so frequent health checks don't load the camera; a failure is re-probed every time.

## Running it

Multi-arch images (amd64/arm64) are published to `ghcr.io/will-white/dahua-companion`, tagged `vX.Y.Z` and `latest`, cosign-signed with SLSA provenance and SBOM attestations.

```yaml
# docker-compose.yml
services:
  dahua-companion:
    image: ghcr.io/will-white/dahua-companion:latest
    restart: unless-stopped
    env_file: .env
    ports:
      - "8080:8080" # /health, optional
```

The image ships a `HEALTHCHECK` that probes `/health`. Note Docker only *marks* the container unhealthy — plain `docker run`/compose never restarts it for that. In-process reconnection already recovers from camera and broker outages on its own; if you also want automatic restarts on a wedged process, pair the healthcheck with [autoheal](https://github.com/willfarrell/docker-autoheal) or an orchestrator that acts on health status.

## Capturing your camera's traffic

Dahua and Amcrest models don't all report events the same way. To see exactly what yours sends — or to help add support for it — record its raw streams:

```sh
docker run --rm --env-file .env -v "$PWD:/out" ghcr.io/will-white/dahua-companion:latest \
  /app/main -capture 2m -capture-dir /out
```

(The image's default command is `./main`, so the binary path is needed when passing flags. Outside Docker: `./main -capture 2m`.)

While it records, ring the doorbell and walk past it. It subscribes to *every* event code (not just the ones this app handles), needs only the camera settings (no MQTT), and prints which event codes it saw when it finishes. Add `-capture-snapshots` to also record the event-snapshot stream used by `SNAPSHOT_SOURCE=event` — that file contains pictures of your doorway, so it's off by default. Review the recordings before sharing them.

## Configuration

Configuration is via environment variables. A `.env` file in the working directory is also read (copy `.env.example` to get started); real environment variables win over `.env.local`, which wins over `.env`.

| Variable | Required | Description |
| --- | --- | --- |
| `MQTT_BROKER_URL` | yes | e.g. `tcp://mqtt-server:1883` (`ssl://` works too) |
| `MQTT_CLIENT_ID` | yes | MQTT client id |
| `MQTT_USERNAME` / `MQTT_PASSWORD` | yes | MQTT credentials |
| `MQTT_TOPIC` | no | defaults to `doorbell/pressed` |
| `MQTT_AVAILABILITY_TOPIC` | no | retained `online`/`offline` state, defaults to `doorbell/availability` |
| `MQTT_HUMAN_TOPIC` | no | human detection events, defaults to `doorbell/human`; set to empty to disable human detection |
| `MQTT_DISCOVERY_PREFIX` | no | set to `homeassistant` to enable HA MQTT discovery; empty (default) disables it |
| `HOSTNAME_OR_IP` | yes | camera hostname or IP |
| `DAHUA_SCHEME` | no | `http` (default) or `https` for the camera API |
| `DAHUA_CA_FILE` | no | with `https`: PEM file to verify the camera's certificate against (export the camera's self-signed certificate) instead of the system roots |
| `DAHUA_TLS_INSECURE` | no | with `https`: `true` skips certificate verification — still encrypted, but not protected against impersonation on your network |
| `DAHUA_USERNAME` / `DAHUA_PASSWORD` | yes | camera credentials (legacy `USERNAME`/`PASSWORD` still work, with a deprecation warning) |
| `*_FILE` | no | `MQTT_USERNAME_FILE`, `MQTT_PASSWORD_FILE`, `DAHUA_USERNAME_FILE`, `DAHUA_PASSWORD_FILE`: read the value from a file instead, e.g. a Docker secret under `/run/secrets/` (a trailing newline is ignored; the plain variable wins if both are set) |
| `HEALTH_PORT` | no | `/health` listen port, defaults to `8080` |
| `MQTT_PAYLOAD_FORMAT` | no | `empty` (default) or `json` — see above |
| `HUMAN_COOLDOWN` | no | minimum gap between published human detections, defaults to `10s`; `0` publishes every one |
| `SNAPSHOT_SOURCE` | no | `request` (default: fetch a still right after the event) or `event` (the camera's own still from the moment of the event, if supported) |
| `SNAPSHOT` | no | comma-separated events that also publish a camera still: `press`, `human`, or both; empty (default) disables |
| `LOG_LEVEL` | no | `info` (default), `debug`, `warn`, … — `debug` logs every camera stream line the app doesn't handle, useful for supporting a new camera model |
| `APP_ENV` | no | `development` switches to human-readable console logs |
| `GOMEMLIMIT` | no | standard Go runtime soft memory limit (e.g. `32MiB`) for very small hosts; the app idles at ~9 MB |

## Home Assistant Example

With discovery enabled (see above) none of this is needed — the device appears on its own. Without it, a plain MQTT trigger works:

```
alias: Doorbell buzzer
triggers:
  - trigger: mqtt
    topic: doorbell/pressed
actions:
  - action: notify.echos
    metadata: {}
    data:
      message: Someone is at the front door
      data:
        type: tts
mode: single
```
