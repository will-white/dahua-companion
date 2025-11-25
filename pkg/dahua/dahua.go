package dahua

import (
	"bufio"
	"context"
	"dahua_companion/pkg/config"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/cenkalti/backoff/v5"
	"github.com/icholy/digest"
	"github.com/rs/zerolog/log"
)

var IsConnected int32

type Client struct {
	HttpClient *http.Client
	Cfg        *config.Dahua
}

func New(cfg *config.Dahua) *Client {
	httpClient := &http.Client{
		Transport: &digest.Transport{
			Username: cfg.Username,
			Password: cfg.Password,
		},
	}
	return &Client{HttpClient: httpClient, Cfg: cfg}
}

func (c *Client) Listen(shutdown chan struct{}, messageChan chan<- string) {
	bo := backoff.NewExponentialBackOff()

	for {
		select {
		case <-shutdown:
			return
		default:
			_, err := backoff.Retry(context.Background(), func() (struct{}, error) {
				return struct{}{}, c.listen(messageChan, shutdown)
			}, backoff.WithBackOff(bo), backoff.WithMaxElapsedTime(0))
			if err != nil {
				log.Error().Err(err).Msg("Dahua listen retry loop failed")
			}
		}
	}
}

func (c *Client) listen(messageChan chan<- string, shutdown chan struct{}) error {
	url := fmt.Sprintf("http://%s/cgi-bin/eventManager.cgi?action=attach&codes=[AlarmLocal]&heartbeat=30", c.Cfg.Host)
	resp, err := c.HttpClient.Get(url)
	if err != nil {
		log.Error().Err(err).Msg("Error fetching http stream")
		atomic.StoreInt32(&IsConnected, 0)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Error().Int("status_code", resp.StatusCode).Msg("Received non-OK HTTP status")
		atomic.StoreInt32(&IsConnected, 0)
		return fmt.Errorf("received non-OK HTTP status: %d", resp.StatusCode)
	}

	log.Info().Msg("Connected to HTTP stream and listening for events")
	atomic.StoreInt32(&IsConnected, 1)
	defer atomic.StoreInt32(&IsConnected, 0)

	scanner := bufio.NewScanner(resp.Body)
	for {
		select {
		case <-shutdown:
			return nil // permanent error to stop retry
		default:
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					log.Error().Err(err).Msg("Error reading the stream")
					return err
				}
				return fmt.Errorf("scanner finished without error")
			}
			line := scanner.Text()
			if isDoorbellPressed(line) {
				log.Info().Msg("Doorbell pressed")
				messageChan <- ""
			}
		}
	}
}

func isDoorbellPressed(line string) bool {
	if !strings.HasPrefix(line, "Code=AlarmLocal;") {
		return false
	}

	parts := strings.Split(strings.TrimRight(line, ";"), ";")
	eventData := make(map[string]string)
	for _, part := range parts {
		keyValue := strings.SplitN(part, "=", 2)
		if len(keyValue) == 2 {
			eventData[keyValue[0]] = keyValue[1]
		}
	}

	action, ok := eventData["action"]
	return eventData["Code"] == "AlarmLocal" && (!ok || action == "Start")
}
