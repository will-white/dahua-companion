package dahua

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"dahua_companion/pkg/config"

	"github.com/cenkalti/backoff/v5"
	"github.com/icholy/digest"
	"github.com/rs/zerolog/log"
)

// heartbeatSeconds asks the camera to send a "Heartbeat" line this often while
// no events are flowing (the API accepts 1-60 and defaults to 60).
const heartbeatSeconds = 5

// streamIdleTimeout is the default stream watchdog: three missed heartbeats
// and the connection is presumed dead.
const streamIdleTimeout = 3 * heartbeatSeconds * time.Second

type Client struct {
	HttpClient *http.Client
	Cfg        *config.Dahua
	// OnConnectionChange, when set before Listen starts, is called from the
	// listen goroutine on every event stream connect and disconnect.
	OnConnectionChange func(connected bool)
	// idleTimeout tears the event stream down when the camera has been silent
	// for this long; heartbeats keep a healthy stream well under it.
	idleTimeout time.Duration
	connected   atomic.Bool
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
	return &Client{HttpClient: httpClient, Cfg: cfg, idleTimeout: streamIdleTimeout}
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
	// The default 60s ceiling would grow the blind window during a long camera
	// outage; attempts against a LAN camera are cheap, so retry at least every
	// 10s.
	bo.MaxInterval = 10 * time.Second
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		return struct{}{}, c.listen(ctx, bo, onEvent)
	}, backoff.WithBackOff(bo), backoff.WithMaxElapsedTime(0))
	if err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("Dahua listen retry loop failed")
	}
}

func (c *Client) listen(ctx context.Context, bo *backoff.ExponentialBackOff, onEvent func()) error {
	url := fmt.Sprintf("http://%s/cgi-bin/eventManager.cgi?action=attach&codes=[AlarmLocal]&heartbeat=%d", c.Cfg.Host, heartbeatSeconds)

	// The camera speaks at least every heartbeatSeconds, so a silent stream
	// means the connection died underneath us (camera reboot, network drop)
	// even though the socket looks open - OS TCP keepalives take minutes to
	// notice. The watchdog aborts the request after idleTimeout of silence so
	// the backoff loop can rebuild the stream; arming it before Do also bounds
	// the connect and digest handshake, which the deliberately timeout-less
	// client leaves unbounded.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	watchdog := time.AfterFunc(c.idleTimeout, cancelStream)
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HttpClient.Do(req)
	if err != nil {
		return c.streamErr(ctx, streamCtx, err, "Error fetching http stream")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Error().Int("status_code", resp.StatusCode).Msg("Received non-OK HTTP status")
		return fmt.Errorf("received non-OK HTTP status: %d", resp.StatusCode)
	}

	log.Info().Msg("Connected to HTTP stream and listening for events")
	c.setConnected(true)
	defer c.setConnected(false)
	// The camera is reachable again: only consecutive failures should grow the
	// retry delay, so start the next reconnect from the shortest interval.
	bo.Reset()

	scanner := bufio.NewScanner(&watchdogReader{r: resp.Body, timer: watchdog, timeout: c.idleTimeout})
	for scanner.Scan() {
		if isDoorbellPressed(scanner.Text()) {
			log.Info().Msg("Doorbell pressed")
			onEvent()
		}
	}
	if err := scanner.Err(); err != nil {
		return c.streamErr(ctx, streamCtx, err, "Error reading the stream")
	}
	return fmt.Errorf("event stream ended")
}

// setConnected updates the stream state and notifies OnConnectionChange.
func (c *Client) setConnected(up bool) {
	c.connected.Store(up)
	if c.OnConnectionChange != nil {
		c.OnConnectionChange(up)
	}
}

// streamErr logs and returns err, naming the idle watchdog when it is what
// killed the stream. Shutdown cancellation stays quiet.
func (c *Client) streamErr(ctx, streamCtx context.Context, err error, msg string) error {
	if ctx.Err() != nil {
		return err
	}
	if streamCtx.Err() != nil {
		err = fmt.Errorf("no data from camera in %s: %w", c.idleTimeout, err)
	}
	log.Error().Err(err).Msg(msg)
	return err
}

// watchdogReader resets the stream watchdog on every chunk the camera sends.
// Resetting on raw reads rather than parsed lines means heartbeats hold the
// watchdog off no matter how the camera frames them.
type watchdogReader struct {
	r       io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (w *watchdogReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.timer.Reset(w.timeout)
	}
	return n, err
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
