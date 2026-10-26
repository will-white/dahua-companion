package mqtt

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"dahua_companion/pkg/config"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog/log"
)

// maxEventAge is how long an event stays worth delivering: a press or a
// detection only matters while someone might still be at the door.
const maxEventAge = 30 * time.Second

// reconnectPoll is how often to re-check the connection while holding an
// undelivered event.
const reconnectPoll = time.Second

// publishTimeout bounds the wait for the broker's ack; without it a wedged
// connection would block the delivery loop indefinitely.
const publishTimeout = 5 * time.Second

// drainTimeout bounds the best-effort flush of already-queued events at
// shutdown.
const drainTimeout = 2 * time.Second

type event struct {
	topic    string
	payload  string
	received time.Time
}

type Client struct {
	client mqtt.Client
	cfg    *config.Mqtt
	// presses and detections are separate queues so a burst of detections
	// can never crowd a doorbell press out (a full queue drops new events),
	// and ProcessQueue always serves presses first.
	presses    chan event
	detections chan event
	// reconnected wakes a deliver loop that is holding an event, instead of
	// leaving it to the next reconnectPoll tick.
	reconnected chan struct{}

	// availabilityMu serializes availability publishes so the retained value
	// on the broker always converges to the latest state.
	availabilityMu sync.Mutex
	available      bool
}

func New(cfg *config.Mqtt) *Client {
	c := &Client{
		cfg:         cfg,
		presses:     make(chan event, 100),
		detections:  make(chan event, 100),
		reconnected: make(chan struct{}, 1),
	}
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	opts.SetUsername(cfg.Username)
	opts.SetPassword(cfg.Password)
	// Keep trying if the broker isn't up yet (e.g. compose boot order) instead
	// of crashing; /health reports the degraded state in the meantime.
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	// The default reconnect ceiling of 10 minutes is far too slow for a doorbell.
	opts.SetMaxReconnectInterval(30 * time.Second)
	// Likewise the default 30s keepalive: until a missed ping is noticed,
	// IsConnectionOpen still reports true and publishes vanish into the dead
	// socket, so shrink that window.
	opts.SetKeepAlive(10 * time.Second)
	opts.SetPingTimeout(5 * time.Second)
	// If the process dies without a clean disconnect, the broker announces it:
	// consumers can alert on the doorbell being down instead of missing
	// presses silently.
	opts.SetWill(cfg.AvailabilityTopic, "offline", 1, true)
	opts.OnConnect = func(mqtt.Client) {
		log.Info().Msg("Connected to MQTT broker")
		select {
		case c.reconnected <- struct{}{}:
		default:
		}
		// Re-announce the current state after every (re)connect; until the
		// event stream reports in, that is "offline".
		c.republishAvailability()
		c.publishDiscovery()
	}
	opts.OnConnectionLost = connectLostHandler
	// AddBroker silently drops URLs it cannot parse. With no valid broker the
	// connect token errors immediately and is never retried, which would leave
	// the process permanently degraded - that is a configuration error, so
	// fail fast instead.
	if len(opts.Servers) == 0 {
		log.Fatal().Str("broker", cfg.Broker).Msg("No valid MQTT broker URL configured")
	}
	c.client = mqtt.NewClient(opts)
	token := c.client.Connect()
	go func() {
		// With ConnectRetry, network failures retry forever; an error here is
		// a terminal one (e.g. Disconnect during the initial retry loop), so
		// log it and leave exiting to the shutdown path.
		if token.Wait() && token.Error() != nil {
			log.Error().Err(token.Error()).Msg("MQTT connect ended without a connection")
		}
	}()
	return c
}

// PublishAvailability records and announces whether the doorbell event stream
// is live, retained on the availability topic so consumers (e.g. Home
// Assistant) can mark the doorbell unavailable instead of missing presses
// silently. The last will covers the process dying outright.
func (c *Client) PublishAvailability(online bool) {
	c.availabilityMu.Lock()
	defer c.availabilityMu.Unlock()
	c.available = online
	c.publishAvailabilityLocked()
}

// republishAvailability re-announces the last recorded state, e.g. after a
// broker reconnect during which the will may have retained "offline".
func (c *Client) republishAvailability() {
	c.availabilityMu.Lock()
	defer c.availabilityMu.Unlock()
	c.publishAvailabilityLocked()
}

func (c *Client) publishAvailabilityLocked() {
	payload := "offline"
	if c.available {
		payload = "online"
	}
	// The ack wait happens off the lock path; delivery order is still the
	// call order, which the lock serializes.
	token := c.client.Publish(c.cfg.AvailabilityTopic, 1, true, payload)
	logPublishOutcome(token, "availability "+payload)
}

// publishDiscovery announces Home Assistant MQTT discovery configs: an event
// entity with device_class doorbell wired to the press topic and, when human
// detection is enabled, one with device_class motion wired to the human topic.
// Both share the availability topic and are retained so HA rediscovers them
// across restarts. No-op unless MQTT_DISCOVERY_PREFIX is set.
func (c *Client) publishDiscovery() {
	if c.cfg.DiscoveryPrefix == "" {
		return
	}
	c.publishEventDiscovery("doorbell", "Doorbell", c.cfg.Topic, pressEventType, "doorbell")
	if c.cfg.HumanTopic != "" {
		c.publishEventDiscovery("human", "Human detected", c.cfg.HumanTopic, humanEventType, "motion")
	}
	if c.cfg.SnapshotEnabled(config.SnapshotPress) {
		c.publishDiscoveryConfig("image", "doorbell_snapshot", map[string]any{
			"name":         "Last ring",
			"image_topic":  c.cfg.PressSnapshotTopic(),
			"content_type": "image/jpeg",
		})
	}
	if c.cfg.SnapshotEnabled(config.SnapshotHuman) {
		c.publishDiscoveryConfig("image", "human_snapshot", map[string]any{
			"name":         "Last person",
			"image_topic":  c.cfg.HumanSnapshotTopic(),
			"content_type": "image/jpeg",
		})
	}
}

func (c *Client) publishEventDiscovery(object, name, stateTopic, eventType, deviceClass string) {
	entity := map[string]any{
		"name":         name,
		"state_topic":  stateTopic,
		"event_types":  []string{eventType},
		"device_class": deviceClass,
	}
	// A JSON payload already carries event_type (its other fields become
	// event attributes); an empty one needs the template to supply it.
	if c.cfg.PayloadFormat != config.PayloadJSON {
		entity["value_template"] = `{"event_type":"` + eventType + `"}`
	}
	c.publishDiscoveryConfig("event", object, entity)
}

// publishDiscoveryConfig completes entity with the fields every entity of
// this device shares and publishes it, retained, as
// <prefix>/<component>/<id>/<object>/config.
func (c *Client) publishDiscoveryConfig(component, object string, entity map[string]any) {
	id := sanitizeTopicID(c.cfg.ClientID)
	entity["unique_id"] = id + "_" + object
	entity["availability_topic"] = c.cfg.AvailabilityTopic
	entity["device"] = map[string]any{
		"identifiers":  []string{id},
		"name":         "Dahua Companion",
		"manufacturer": "Dahua",
	}
	entity["origin"] = map[string]any{
		"name": "dahua-companion",
		"url":  "https://github.com/will-white/dahua-companion",
	}
	payload, err := json.Marshal(entity)
	if err != nil {
		log.Error().Err(err).Str("object", object).Msg("Failed to marshal discovery config")
		return
	}
	topic := c.cfg.DiscoveryPrefix + "/" + component + "/" + id + "/" + object + "/config"
	token := c.client.Publish(topic, 1, true, string(payload))
	logPublishOutcome(token, object+" discovery config")
}

// sanitizeTopicID reduces the client id to the [a-zA-Z0-9_-] set that
// discovery topic segments allow.
func sanitizeTopicID(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

// logPublishOutcome waits for the broker's ack off the caller's path and logs
// a failure; used for the retained state topics where delivery is not worth
// blocking for but silence would hide a problem.
func logPublishOutcome(token mqtt.Token, what string) {
	go func() {
		if !token.WaitTimeout(publishTimeout) {
			log.Error().Str("publish", what).Msg("Broker did not ack publish in time")
			return
		}
		if err := token.Error(); err != nil {
			log.Error().Err(err).Str("publish", what).Msg("Publish failed")
		}
	}()
}

// IsConnected reports whether the client currently has an open broker connection.
func (c *Client) IsConnected() bool {
	return c.client.IsConnectionOpen()
}

// Event types, shared by the JSON payload and the HA discovery config.
const (
	pressEventType = "press"
	humanEventType = "detected"
)

// PublishPress queues a doorbell press on channel for delivery to the press
// topic, ahead of any queued detections. It never blocks: if the queue is
// full the event is dropped.
func (c *Client) PublishPress(channel int) {
	c.enqueue(c.presses, c.cfg.Topic, pressEventType, channel)
}

// PublishHuman queues a human detection on channel for delivery to the human
// topic. It never blocks: if the queue is full the event is dropped.
func (c *Client) PublishHuman(channel int) {
	c.enqueue(c.detections, c.cfg.HumanTopic, humanEventType, channel)
}

func (c *Client) enqueue(queue chan event, topic, eventType string, channel int) {
	now := time.Now()
	ev := event{topic: topic, payload: c.payload(eventType, channel, now), received: now}
	select {
	case queue <- ev:
		log.Info().Str("topic", topic).Msg("Message queued")
	default:
		log.Warn().Str("topic", topic).Msg("Queue is full; dropping event")
	}
}

// payload renders an event in the configured format: empty (the event is the
// signal) or a JSON object with the event type, time and camera channel.
func (c *Client) payload(eventType string, channel int, at time.Time) string {
	if c.cfg.PayloadFormat != config.PayloadJSON {
		return ""
	}
	b, err := json.Marshal(struct {
		EventType string `json:"event_type"`
		Timestamp string `json:"timestamp"`
		Channel   int    `json:"channel"`
	}{eventType, at.UTC().Format(time.RFC3339Nano), channel})
	if err != nil {
		// Unreachable for this fixed shape; an empty payload still signals.
		log.Error().Err(err).Msg("Failed to marshal event payload")
		return ""
	}
	return string(b)
}

// ProcessQueue delivers queued events until ctx is cancelled, then makes a
// brief best-effort attempt to flush whatever is already queued - a press that
// arrived moments before shutdown should still ring. While the broker is
// unreachable it holds the current event and waits instead of spinning, and
// events older than maxEventAge are dropped rather than delivered late.
func (c *Client) ProcessQueue(ctx context.Context) {
	for {
		// Presses jump the line: check them alone first, so a backlog of
		// detections never delays a ring.
		select {
		case <-ctx.Done():
			c.drain()
			return
		case ev := <-c.presses:
			c.deliver(ctx, ev)
			continue
		default:
		}
		select {
		case <-ctx.Done():
			c.drain()
			return
		case ev := <-c.presses:
			c.deliver(ctx, ev)
		case ev := <-c.detections:
			c.deliver(ctx, ev)
		}
	}
}

// drain flushes queued fresh events at shutdown, bounded by drainTimeout. It
// never waits for a reconnect: if the broker is down the events are dropped.
func (c *Client) drain() {
	deadline := time.Now().Add(drainTimeout)
	// Presses first, as in ProcessQueue.
	c.drainQueue(c.presses, deadline)
	c.drainQueue(c.detections, deadline)
}

func (c *Client) drainQueue(queue chan event, deadline time.Time) {
	for {
		select {
		case ev := <-queue:
			remaining := time.Until(deadline)
			if remaining <= 0 || !c.client.IsConnectionOpen() {
				log.Warn().Str("topic", ev.topic).Msg("Dropping queued event at shutdown")
				continue
			}
			if time.Since(ev.received) > maxEventAge {
				log.Warn().Str("topic", ev.topic).Msg("Dropping stale event")
				continue
			}
			token := c.client.Publish(ev.topic, 1, false, ev.payload)
			if !token.WaitTimeout(remaining) || token.Error() != nil {
				log.Error().Str("topic", ev.topic).Msg("Failed to flush queued event at shutdown")
				continue
			}
			log.Info().Str("topic", ev.topic).Dur("latency_ms", time.Since(ev.received)).Msg("Message published during shutdown drain")
		default:
			return
		}
	}
}

func (c *Client) deliver(ctx context.Context, ev event) {
	for !c.client.IsConnectionOpen() {
		if ctx.Err() != nil {
			return
		}
		if time.Since(ev.received) > maxEventAge {
			log.Warn().Str("topic", ev.topic).Msg("Broker unreachable; dropping stale event")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.reconnected:
		case <-time.After(reconnectPoll):
		}
	}
	if time.Since(ev.received) > maxEventAge {
		log.Warn().Str("topic", ev.topic).Msg("Dropping stale event")
		return
	}
	// QoS 1: the broker acks the publish, so a half-open connection surfaces
	// as an error here instead of a silent drop.
	token := c.client.Publish(ev.topic, 1, false, ev.payload)
	if !token.WaitTimeout(publishTimeout) {
		log.Error().Str("topic", ev.topic).Dur("timeout_ms", publishTimeout).Msg("Broker did not ack publish in time; dropping event")
		return
	}
	if err := token.Error(); err != nil {
		log.Error().Err(err).Str("topic", ev.topic).Msg("Failed to publish message to MQTT broker")
		return
	}
	log.Info().Str("topic", ev.topic).Dur("latency_ms", time.Since(ev.received)).Msg("Message published from queue")
}

// PublishSnapshot sends a camera still to topic, not retained: a stale image
// is worse than none. Snapshots bypass the event queues - they are best
// effort, and holding images for a reconnect would defeat the memory bound -
// so a down broker simply drops them. It blocks until the broker acks or
// publishTimeout passes.
func (c *Client) PublishSnapshot(topic string, img []byte) {
	if !c.client.IsConnectionOpen() {
		log.Warn().Str("topic", topic).Msg("Broker unreachable; dropping snapshot")
		return
	}
	token := c.client.Publish(topic, 1, false, img)
	if !token.WaitTimeout(publishTimeout) {
		log.Error().Str("topic", topic).Msg("Broker did not ack snapshot in time")
		return
	}
	if err := token.Error(); err != nil {
		log.Error().Err(err).Str("topic", topic).Msg("Failed to publish snapshot")
		return
	}
	log.Info().Str("topic", topic).Int("bytes", len(img)).Msg("Snapshot published")
}

func (c *Client) Disconnect() {
	// A clean disconnect suppresses the will, so leave "offline" explicitly;
	// Disconnect's grace period lets it flush.
	c.PublishAvailability(false)
	c.client.Disconnect(250)
}

func connectLostHandler(client mqtt.Client, err error) {
	log.Error().Err(err).Msg("Connection lost to MQTT broker")
}
