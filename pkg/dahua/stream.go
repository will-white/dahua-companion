package dahua

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"dahua_companion/pkg/metrics"

	"github.com/cenkalti/backoff/v7"
	"github.com/rs/zerolog/log"
)

// heartbeatSeconds asks the camera to send a "Heartbeat" line this often while
// no events are flowing (the API accepts 1-60 and defaults to 60).
const heartbeatSeconds = 5

// streamIdleTimeout is the default stream watchdog: three missed heartbeats
// and the connection is presumed dead.
const streamIdleTimeout = 3 * heartbeatSeconds * time.Second

// maxLineBytes is the longest event stream line the scanner accepts. The
// bufio default of 64 KB is plenty for event lines, but a line past it is a
// fatal scan error - and since the camera would resend the same oversized
// line, the stream would reconnect forever. Lines are never buffered beyond
// their own length, so the high cap costs nothing in the normal case.
const maxLineBytes = 1 << 20

// Listen maintains the event stream until ctx is cancelled, reconnecting with
// exponential backoff and calling onEvent for every doorbell press (and human
// detection, when DetectHumans is set).
func (c *Client) Listen(ctx context.Context, onEvent func(Event)) {
	retryStream(ctx, "event stream", func(bo *backoff.ExponentialBackOff) error {
		err := c.listen(ctx, bo, onEvent)
		if err != nil && ctx.Err() == nil {
			metrics.CameraFailures.Inc()
		}
		return err
	})
}

// retryStream runs attempt until ctx is cancelled, reconnecting with
// exponential backoff. attempt resets the backoff once it connects, so only
// consecutive failures grow the delay.
func retryStream(ctx context.Context, name string, attempt func(*backoff.ExponentialBackOff) error) {
	bo := backoff.NewExponentialBackOff()
	// The default 60s ceiling would grow the blind window during a long camera
	// outage; attempts against a LAN camera are cheap, so retry at least every
	// 10s.
	bo.MaxInterval = 10 * time.Second
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		return struct{}{}, attempt(bo)
	}, backoff.WithBackOff(bo), backoff.WithMaxElapsedTime(0))
	if err != nil && ctx.Err() == nil {
		log.Error().Err(err).Str("stream", name).Msg("Dahua stream retry loop failed")
	}
}

func (c *Client) listen(ctx context.Context, bo *backoff.ExponentialBackOff, onEvent func(Event)) error {
	codes := "AlarmLocal"
	if c.DetectHumans {
		// Smart motion detection, plus the tripwire and intrusion IVS rules,
		// which report people through the object type in their data block.
		codes += ",SmartMotionHuman,CrossLineDetection,CrossRegionDetection"
	}
	url := c.url(fmt.Sprintf("/cgi-bin/eventManager.cgi?action=attach&codes=[%s]&heartbeat=%d", codes, heartbeatSeconds))
	return c.stream(ctx, bo, "event stream", url, true, func(body io.Reader, _ string) error {
		return c.scanEvents(body, onEvent)
	})
}

// stream runs one long-poll connection to url: it requests it under the idle
// watchdog and, once the camera answers 200, hands the body (read through the
// watchdog) to consume. track marks the connection in IsConnected and
// OnConnectionChange. It always returns an error: the stream ending is one.
func (c *Client) stream(ctx context.Context, bo *backoff.ExponentialBackOff, name, url string, track bool, consume func(body io.Reader, contentType string) error) error {
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
		return c.streamErr(ctx, streamCtx, err, name, "Error fetching http stream")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Error().Str("stream", name).Int("status_code", resp.StatusCode).Msg("Received non-OK HTTP status")
		return fmt.Errorf("received non-OK HTTP status: %d", resp.StatusCode)
	}

	log.Info().Str("stream", name).Msg("Connected to HTTP stream and listening for events")
	if track {
		metrics.CameraConnects.Inc()
		c.setConnected(true)
		defer c.setConnected(false)
	}
	// The camera is reachable again: only consecutive failures should grow the
	// retry delay, so start the next reconnect from the shortest interval.
	bo.Reset()

	body := &watchdogReader{r: resp.Body, timer: watchdog, timeout: c.idleTimeout}
	if err := consume(body, resp.Header.Get("Content-Type")); err != nil {
		return c.streamErr(ctx, streamCtx, err, name, "Error reading the stream")
	}
	return fmt.Errorf("%s ended", name)
}

// scanEvents reads event lines from the event stream body until it ends.
func (c *Client) scanEvents(body io.Reader, onEvent func(Event)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 4096), maxLineBytes)
	var block *ivsBlock // an IVS event whose data block is still arriving
	for scanner.Scan() {
		line := scanner.Text()
		if block != nil {
			done, err := block.feed(line)
			if !done {
				continue
			}
			if err != nil {
				log.Debug().Err(err).Msg("Abandoned IVS event data block")
			} else {
				c.reportIVS(block, onEvent)
			}
			block = nil
			continue
		}
		if b := startIVS(line); b != nil {
			if done, err := b.feed(b.first); done {
				if err != nil {
					log.Debug().Err(err).Str("line", line).Msg("Abandoned IVS event data block")
				} else {
					c.reportIVS(b, onEvent)
				}
			} else {
				block = b
			}
			continue
		}
		ev, ok := parseEvent(line)
		if !ok {
			logUnhandledLine(line)
			continue
		}
		c.report(ev, onEvent)
	}
	return scanner.Err()
}

// streamErr logs and returns err, naming the idle watchdog when it is what
// killed the stream. Shutdown cancellation stays quiet.
func (c *Client) streamErr(ctx, streamCtx context.Context, err error, name, msg string) error {
	if ctx.Err() != nil {
		return err
	}
	if streamCtx.Err() != nil {
		err = fmt.Errorf("no data from camera in %s: %w", c.idleTimeout, err)
	}
	log.Error().Err(err).Str("stream", name).Msg(msg)
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
