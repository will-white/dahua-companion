package mqtt

import (
	"dahua_companion/pkg/config"
	"sync/atomic"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog/log"
)

var IsConnected int32

type Client struct {
	client mqtt.Client
	cfg    *config.Mqtt
	queue  chan string
}

func New(cfg *config.Mqtt) *Client {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	opts.SetUsername(cfg.Username)
	opts.SetPassword(cfg.Password)
	opts.OnConnect = connectHandler
	opts.OnConnectionLost = connectLostHandler
	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatal().Err(token.Error()).Msg("Failed to connect to MQTT broker")
	}
	return &Client{client: client, cfg: cfg, queue: make(chan string, 100)}
}

func (c *Client) Publish(message string) {
	select {
	case c.queue <- message:
		log.Info().Msg("Message queued")
	default:
		log.Warn().Msg("Queue is full. Dropping oldest message and adding new one.")
		// Remove the oldest message.
		<-c.queue
		// Add the new message.
		c.queue <- message
	}
}

func (c *Client) ProcessQueue(shutdown chan struct{}) {
	for {
		select {
		case <-shutdown:
			return
		case msg := <-c.queue:
			if atomic.LoadInt32(&IsConnected) == 1 {
				if token := c.client.Publish(c.cfg.Topic, 0, false, msg); token.Wait() && token.Error() != nil {
					log.Error().Err(token.Error()).Msg("Failed to publish message to MQTT broker")
				} else {
					log.Info().Msg("Message published from queue")
				}
			} else {
				// If not connected, requeue the message. This is a simple approach.
				// A more advanced implementation might use a different strategy.
				c.Publish(msg)
			}
		}
	}
}

func (c *Client) Disconnect() {
	c.client.Disconnect(250)
}

func connectHandler(client mqtt.Client) {
	atomic.StoreInt32(&IsConnected, 1)
	log.Info().Msg("Connected to MQTT broker")
}

func connectLostHandler(client mqtt.Client, err error) {
	atomic.StoreInt32(&IsConnected, 0)
	log.Error().Err(err).Msg("Connection lost to MQTT broker")
}
