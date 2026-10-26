package dahua

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"dahua_companion/pkg/config"

	"github.com/cenkalti/backoff/v7"
	"github.com/icholy/digest"
	"github.com/rs/zerolog/log"
)

// heartbeatSeconds asks the camera to send a "Heartbeat" line this often while
// no events are flowing (the API accepts 1-60 and defaults to 60).
const heartbeatSeconds = 5

// streamIdleTimeout is the default stream watchdog: three missed heartbeats
// and the connection is presumed dead.
const streamIdleTimeout = 3 * heartbeatSeconds * time.Second

// EventKind identifies a camera event reported to Listen's callback.
type EventKind int

const (
	// DoorbellPressed is the call button (AlarmLocal).
	DoorbellPressed EventKind = iota + 1
	// HumanDetected is the camera's human smart motion detection
	// (SmartMotionHuman). Only reported when DetectHumans is set.
	HumanDetected
)

func (e EventKind) String() string {
	switch e {
	case DoorbellPressed:
		return "doorbell_pressed"
	case HumanDetected:
		return "human_detected"
	default:
		return "unknown"
	}
}

// Event is one camera event: what happened and on which video channel (the
// event's index, 0 for a single-camera doorbell).
type Event struct {
	Kind    EventKind
	Channel int
}

type Client struct {
	HttpClient *http.Client
	Cfg        *config.Dahua
	// OnConnectionChange, when set before Listen starts, is called from the
	// listen goroutine on every event stream connect and disconnect.
	OnConnectionChange func(connected bool)
	// DetectHumans, when set before Listen starts, also subscribes to human
	// smart motion detection events.
	DetectHumans bool
	// HumanCooldown, when set before Listen starts, suppresses human
	// detections that follow a reported one within this window; smart motion
	// detection fires repeatedly while someone lingers.
	HumanCooldown time.Duration
	lastHuman     time.Time
	// idleTimeout tears the event stream down when the camera has been silent
	// for this long; heartbeats keep a healthy stream well under it.
	idleTimeout time.Duration
	connected   atomic.Bool
}

// New builds a camera client. It fails only on an unusable TLS setup (an
// unreadable or invalid DAHUA_CA_FILE).
func New(cfg *config.Dahua) (*Client, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Scheme == config.SchemeHTTPS {
		tlsConfig, err := cameraTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		base.TLSClientConfig = tlsConfig
	}
	// No client-wide timeout: the event stream is a long-poll that stays open
	// indefinitely. Bounded requests pass their own context deadline.
	httpClient := &http.Client{
		Transport: &digest.Transport{
			Username:  cfg.Username,
			Password:  cfg.Password,
			Transport: base,
		},
	}
	return &Client{HttpClient: httpClient, Cfg: cfg, idleTimeout: streamIdleTimeout}, nil
}

// cameraTLSConfig verifies the camera against DAHUA_CA_FILE when set (the
// usual way to trust a camera's self-signed certificate), the system roots
// otherwise, or not at all with DAHUA_TLS_INSECURE.
func cameraTLSConfig(cfg *config.Dahua) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLSInsecure {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // explicit opt-in for self-signed camera certificates
		return tlsConfig, nil
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading DAHUA_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("DAHUA_CA_FILE %s holds no PEM certificates", cfg.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	return tlsConfig, nil
}

// url builds a camera API URL for path (which includes its query).
func (c *Client) url(path string) string {
	return c.Cfg.Scheme + "://" + c.Cfg.Host + path
}

// IsConnected reports whether the event stream is currently established.
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// Probe makes a cheap read-only authenticated request against the camera.
func (c *Client) Probe(ctx context.Context) error {
	url := c.url("/cgi-bin/magicBox.cgi?action=getMachineName")
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

// maxSnapshotBytes caps a snapshot download. Doorbell JPEGs run 100-400 KB;
// the cap only keeps a misbehaving camera from ballooning memory.
const maxSnapshotBytes = 5 << 20

// maxLineBytes is the longest event stream line the scanner accepts. The
// bufio default of 64 KB is plenty for event lines, but a line past it is a
// fatal scan error - and since the camera would resend the same oversized
// line, the stream would reconnect forever. Lines are never buffered beyond
// their own length, so the high cap costs nothing in the normal case.
const maxLineBytes = 1 << 20

// Snapshot fetches a JPEG from the camera for an event's channel (the event
// index, 0-based; the API counts video channels from 1). Callers bound it with
// a context deadline, since the client itself has no timeout.
func (c *Client) Snapshot(ctx context.Context, channel int) ([]byte, error) {
	url := c.url(fmt.Sprintf("/cgi-bin/snapshot.cgi?channel=%d", channel+1))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, fmt.Errorf("camera returned status %d", res.StatusCode)
	}
	img, err := io.ReadAll(io.LimitReader(res.Body, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(img) > maxSnapshotBytes {
		return nil, fmt.Errorf("snapshot larger than %d bytes", maxSnapshotBytes)
	}
	if len(img) == 0 {
		return nil, fmt.Errorf("camera returned an empty snapshot")
	}
	return img, nil
}

// Listen maintains the event stream until ctx is cancelled, reconnecting with
// exponential backoff and calling onEvent for every doorbell press (and human
// detection, when DetectHumans is set).
func (c *Client) Listen(ctx context.Context, onEvent func(Event)) {
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

func (c *Client) listen(ctx context.Context, bo *backoff.ExponentialBackOff, onEvent func(Event)) error {
	codes := "AlarmLocal"
	if c.DetectHumans {
		codes += ",SmartMotionHuman"
	}
	url := c.url(fmt.Sprintf("/cgi-bin/eventManager.cgi?action=attach&codes=[%s]&heartbeat=%d", codes, heartbeatSeconds))

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
	scanner.Buffer(make([]byte, 0, 4096), maxLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		ev, ok := parseEvent(line)
		if !ok {
			logUnhandledLine(line)
			continue
		}
		if ev.Kind == HumanDetected && c.humanCoolingDown(time.Now()) {
			log.Debug().Int("channel", ev.Channel).Msg("Human detection within cooldown; suppressed")
			continue
		}
		log.Info().Stringer("event", ev.Kind).Int("channel", ev.Channel).Msg("Camera event received")
		onEvent(ev)
	}
	if err := scanner.Err(); err != nil {
		return c.streamErr(ctx, streamCtx, err, "Error reading the stream")
	}
	return fmt.Errorf("event stream ended")
}

// humanCoolingDown reports whether a human detection at now falls within
// HumanCooldown of the last reported one, recording now when it does not.
func (c *Client) humanCoolingDown(now time.Time) bool {
	if c.HumanCooldown > 0 && !c.lastHuman.IsZero() && now.Sub(c.lastHuman) < c.HumanCooldown {
		return true
	}
	c.lastHuman = now
	return false
}

// logUnhandledLine records, at debug level, stream lines that are not a
// reported event: other event codes, Stop actions, and the JSON data blocks
// some events carry. It is the raw material for supporting a camera that
// names its events differently. Heartbeats and multipart framing are skipped
// so the log stays readable.
func logUnhandledLine(line string) {
	if line == "" || line == "Heartbeat" || strings.HasPrefix(line, "--") ||
		strings.HasPrefix(line, "Content-Type:") || strings.HasPrefix(line, "Content-Length:") {
		return
	}
	log.Debug().Str("line", line).Msg("Unhandled camera stream line")
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

// parseEvent recognizes the start of a doorbell press or human detection in a
// "Code=...;action=...;" line. Stop events are ignored: only the moment it
// happens is worth announcing. Events with a data payload put "data={" at the
// end of the line and the JSON on the following lines, which never match.
func parseEvent(line string) (Event, bool) {
	var ev Event
	switch {
	case strings.HasPrefix(line, "Code=AlarmLocal;"):
		ev.Kind = DoorbellPressed
	case strings.HasPrefix(line, "Code=SmartMotionHuman;"):
		ev.Kind = HumanDetected
	default:
		return Event{}, false
	}

	parts := strings.Split(strings.TrimRight(line, ";"), ";")
	eventData := make(map[string]string)
	for _, part := range parts {
		keyValue := strings.SplitN(part, "=", 2)
		if len(keyValue) == 2 {
			eventData[keyValue[0]] = keyValue[1]
		}
	}

	// A missing or malformed index means the only channel there is.
	ev.Channel, _ = strconv.Atoi(eventData["index"])
	action, ok := eventData["action"]
	return ev, !ok || action == "Start"
}
