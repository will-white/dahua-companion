# Dahua Companion

The Dahua Companion allows you to integrate your [Dahua](https://www.dahuasecurity.com/) and [Amcrest](https://amcrest.com/wifi-video-doorbell-cameras.html) doorbell's button/ring/buzzer with any home automation that can listen to queue / events.

The Dahua Companion will listen for the button/ring/buzzer and then send a message/event/topic via MQTT e.g. `doorbell/pressed` with no payload.

## How it works

Dahua cameras report events via a long polling HTTP connection. And what you subscribe to affects what events the camera reports back with.

Dahua's API is included in the documentation folder.

For this project we're only concerned with the `AlarmLocal` event (This is the buzzer/ringer/button pressed event). And more specifically we publish only the `AlarmLocal` started events since we don't care when Dahua decides an alarm is "over".

After we receive an event we publish a `doorbell/pressed` to the MQTT broker.

This project also has wrappers around the HTTP subscription and MQTT broker to always make sure it's connected. Events are buffered briefly while the broker reconnects, but anything older than 30 seconds is dropped instead of delivered late — a doorbell press only matters while someone might still be at the door.

A `/health` endpoint (port `8080` by default) returns 200 only when the MQTT connection is up, the event stream is attached, and the camera answers a live read-only probe.

## Configuration

Configuration is via environment variables. A `.env` file in the working directory is also read (copy `.env.example` to get started); real environment variables win over `.env.local`, which wins over `.env`.

| Variable | Required | Description |
| --- | --- | --- |
| `MQTT_BROKER_URL` | yes | e.g. `tcp://mqtt-server:1883` (`ssl://` works too) |
| `MQTT_CLIENT_ID` | yes | MQTT client id |
| `MQTT_USERNAME` / `MQTT_PASSWORD` | yes | MQTT credentials |
| `MQTT_TOPIC` | no | defaults to `doorbell/pressed` |
| `HOSTNAME_OR_IP` | yes | camera hostname or IP |
| `DAHUA_USERNAME` / `DAHUA_PASSWORD` | yes | camera credentials (legacy `USERNAME`/`PASSWORD` still work, with a deprecation warning) |
| `HEALTH_PORT` | no | `/health` listen port, defaults to `8080` |
| `APP_ENV` | no | `development` switches to human-readable console logs |

## Home Assistant Example

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
