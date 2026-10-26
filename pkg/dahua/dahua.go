package dahua

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"dahua_companion/pkg/config"

	"github.com/icholy/digest"
)

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
	// snapshotConnected tracks the event-snapshot stream separately: it is
	// best effort and never affects IsConnected or availability.
	snapshotConnected atomic.Bool
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

// setConnected updates the stream state and notifies OnConnectionChange.
func (c *Client) setConnected(up bool) {
	c.connected.Store(up)
	if c.OnConnectionChange != nil {
		c.OnConnectionChange(up)
	}
}
