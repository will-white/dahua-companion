# Dahua Companion

The Dahua Companion allows you to integrate your [Dahua](https://www.dahuasecurity.com/) and [Amcrest](https://amcrest.com/wifi-video-doorbell-cameras.html) doorbell's button/ring/buzzer with any home automation that can listen queue / events.

The Dahua Companion will listen for the button/ring/buzzer and then send a message/event/topic via MQTT e.g. `doorbell/pressed` with no payload.

## How it works

Dahua cameras report events via a long polling HTTP connection. And what you subscribe to effects what events the camera reports back with.

Dahua's API is included in the documentation folder.

For this project we're only concerned with the `AlarmLocal` event (This is the buzzer/ringer/button pressed event). And more specifically we publish only the `AlarmLocal` started events since we don't care when Dahua decides an alarm is "over".

After we receive an event we publish a `doorbell/pressed` to the MQTT broker.

This project also has wrappers around the HTTP subscription and MQTT broker to always make sure it's connected. The only thing we don't have is an internal queue to monitor in between MQTT outages. Since it doesn't make sense to send events that no longer matter.
