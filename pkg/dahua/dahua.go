package dahua

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"dahua_companion/pkg/config"

	"github.com/cenkalti/backoff/v5"
	"github.com/icholy/digest"
	"github.com/rs/zerolog/log"
)

type Client struct {
	HttpClient *http.Client
	Cfg        *config.Dahua
	connected  atomic.Bool
}

func New(cfg *config.Dahua) *Client {
	// No client-wide timeout: the event stream is a long-poll that stays open
	// indefinitely. Bounded requests pass their own context deadline.
	httpClient := &http.Client{
		Transport: &digest.Transport{
			Username: cfg.Username,
			Password: cfg.Password,
		},
	}
	return &Client{HttpClient: httpClient, Cfg: cfg}
}

// IsConnected reports whether the event stream is currently established.
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// Probe makes a cheap read-only authenticated request against the camera.
func (c *Client) Probe(ctx context.Context) error {
	url := fmt.Sprintf("http://%s/cgi-bin/magicBox.cgi?action=getMachineName", c.Cfg.Host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := c.HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body) // drain so the connection can be reused
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("camera returned status %d", res.StatusCode)
	}
	return nil
}

// Listen maintains the event stream until ctx is cancelled, reconnecting with
// exponential backoff and calling onEvent for every doorbell press.
func (c *Client) Listen(ctx context.Context, onEvent func()) {
	bo := backoff.NewExponentialBackOff()
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		return struct{}{}, c.listen(ctx, bo, onEvent)
	}, backoff.WithBackOff(bo), backoff.WithMaxElapsedTime(0))
	if err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("Dahua listen retry loop failed")
	}
}

func (c *Client) listen(ctx context.Context, bo *backoff.ExponentialBackOff, onEvent func()) error {
	url := fmt.Sprintf("http://%s/cgi-bin/eventManager.cgi?action=attach&codes=[AlarmLocal]&heartbeat=30", c.Cfg.Host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HttpClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Msg("Error fetching http stream")
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Error().Int("status_code", resp.StatusCode).Msg("Received non-OK HTTP status")
		return fmt.Errorf("received non-OK HTTP status: %d", resp.StatusCode)
	}

	log.Info().Msg("Connected to HTTP stream and listening for events")
	c.connected.Store(true)
	defer c.connected.Store(false)
	// The camera is reachable again: only consecutive failures should grow the
	// retry delay, so start the next reconnect from the shortest interval.
	bo.Reset()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if isDoorbellPressed(scanner.Text()) {
			log.Info().Msg("Doorbell pressed")
			onEvent()
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Msg("Error reading the stream")
		}
		return err
	}
	return fmt.Errorf("event stream ended")
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
