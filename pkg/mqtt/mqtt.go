package mqtt

import (
	"context"
	"time"

	"dahua_companion/pkg/config"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog/log"
)

// maxEventAge is how long a doorbell event stays worth delivering: a press
// only matters while someone might still be at the door.
const maxEventAge = 30 * time.Second

// reconnectPoll is how often to re-check the connection while holding an
// undelivered event.
const reconnectPoll = time.Second

type event struct {
	payload  string
	received time.Time
}

type Client struct {
	client mqtt.Client
	cfg    *config.Mqtt
	queue  chan event
}

func New(cfg *config.Mqtt) *Client {
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
	opts.OnConnect = connectHandler
	opts.OnConnectionLost = connectLostHandler
	client := mqtt.NewClient(opts)
	token := client.Connect()
	go func() {
		// With ConnectRetry the token only fails on non-retryable errors, e.g.
		// no valid broker URL.
		if token.Wait() && token.Error() != nil {
			log.Error().Err(token.Error()).Msg("Failed to connect to MQTT broker")
		}
	}()
	return &Client{client: client, cfg: cfg, queue: make(chan event, 100)}
}

// IsConnected reports whether the client currently has an open broker connection.
func (c *Client) IsConnected() bool {
	return c.client.IsConnectionOpen()
}

// Publish queues a doorbell event for delivery. It never blocks: if the queue
// is full the event is dropped.
func (c *Client) Publish(message string) {
	select {
	case c.queue <- event{payload: message, received: time.Now()}:
		log.Info().Msg("Message queued")
	default:
		log.Warn().Msg("Queue is full; dropping event")
	}
}

// ProcessQueue delivers queued events until ctx is cancelled. While the broker
// is unreachable it holds the current event and waits instead of spinning, and
// events older than maxEventAge are dropped rather than delivered late.
func (c *Client) ProcessQueue(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c.queue:
			c.deliver(ctx, ev)
		}
	}
}

func (c *Client) deliver(ctx context.Context, ev event) {
	for !c.client.IsConnectionOpen() {
		if ctx.Err() != nil {
			return
		}
		if time.Since(ev.received) > maxEventAge {
			log.Warn().Msg("Broker unreachable; dropping stale doorbell event")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectPoll):
		}
	}
	if time.Since(ev.received) > maxEventAge {
		log.Warn().Msg("Dropping stale doorbell event")
		return
	}
	if token := c.client.Publish(c.cfg.Topic, 0, false, ev.payload); token.Wait() && token.Error() != nil {
		log.Error().Err(token.Error()).Msg("Failed to publish message to MQTT broker")
		return
	}
	log.Info().Msg("Message published from queue")
}

func (c *Client) Disconnect() {
	c.client.Disconnect(250)
}

func connectHandler(client mqtt.Client) {
	log.Info().Msg("Connected to MQTT broker")
}

func connectLostHandler(client mqtt.Client, err error) {
	log.Error().Err(err).Msg("Connection lost to MQTT broker")
}
