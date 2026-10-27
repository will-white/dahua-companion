package dahua

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dahua_companion/pkg/metrics"

	"github.com/rs/zerolog/log"
)

// forceOnlinePath marks the camera's vendor-cloud (PaaS) connection as up.
// An Amcrest doorbell that cannot reach the Amcrest cloud - typically because
// it is firewalled off the internet - shows an "offline" pattern on its light
// ring; this setting clears it. It is not in the Dahua API PDFs, and the
// camera drops it again on its own, so it has to be re-sent periodically. The
// app's original health check sent it every 30s (Docker's healthcheck
// interval) until v0.2.0 replaced that probe with a read-only one.
const forceOnlinePath = "/cgi-bin/configManager.cgi?action=setConfig&VSP_PaaS.Online=true"

// forceOnlineTimeout bounds one attempt; the client has no timeout of its own.
const forceOnlineTimeout = 5 * time.Second

// ForceOnline tells the camera its cloud connection is up, once.
func (c *Client) ForceOnline(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(forceOnlinePath), nil)
	if err != nil {
		return err
	}
	res, err := c.HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode != http.StatusOK {
		// The camera's reply (usually "Error" plus a reason) says why, e.g. a
		// model without this setting.
		return fmt.Errorf("camera returned status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// KeepOnline calls ForceOnline now and then every interval until ctx is
// cancelled. Failures are retried on the next tick; the first one is logged
// as a warning and the rest at debug, so a camera that rejects the setting
// does not flood the log, and a recovery is logged once.
func (c *Client) KeepOnline(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failing := false
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, forceOnlineTimeout)
		err := c.ForceOnline(attemptCtx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			metrics.ForceOnline.Inc("failed")
			if !failing {
				log.Warn().Err(err).Msg("Failed to mark the camera online (DAHUA_FORCE_ONLINE); retrying every interval")
			} else {
				log.Debug().Err(err).Msg("Failed to mark the camera online")
			}
			failing = true
		default:
			metrics.ForceOnline.Inc("ok")
			if failing {
				log.Info().Msg("Marking the camera online works again")
			}
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
