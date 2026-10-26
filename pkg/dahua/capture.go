package dahua

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The capture streams subscribe to every event code ("All"), not just the
// ones the app handles: the point is to see what a camera actually sends.
var (
	captureEventsPath    = fmt.Sprintf("/cgi-bin/eventManager.cgi?action=attach&codes=[All]&heartbeat=%d", heartbeatSeconds)
	captureSnapshotsPath = fmt.Sprintf("/cgi-bin/snapManager.cgi?action=attachFileProc&heartbeat=%d&Flags[0]=Event&Events=[All]", heartbeatSeconds)
)

// CaptureEvents records the raw event stream, every event code included, to
// w until ctx ends. See capture.
func (c *Client) CaptureEvents(ctx context.Context, w io.Writer) (int64, error) {
	return c.capture(ctx, captureEventsPath, w)
}

// CaptureSnapshots records the raw event-snapshot stream (multipart text and
// JPEG parts) to w until ctx ends. See capture.
func (c *Client) CaptureSnapshots(ctx context.Context, w io.Writer) (int64, error) {
	return c.capture(ctx, captureSnapshotsPath, w)
}

// capture copies a camera stream to w byte for byte - no parsing, no
// watchdog, no reconnect - so the recording is exactly what the camera sent
// and can become a test fixture. The capture window closing (ctx ending) is
// success; a camera that refuses the stream is an error carrying the start
// of its reply, which usually says why (e.g. an unsupported API).
func (c *Client) capture(ctx context.Context, path string, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path), nil)
	if err != nil {
		return 0, err
	}
	res, err := c.HttpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return 0, fmt.Errorf("camera returned status %d: %s", res.StatusCode, strings.TrimSpace(string(excerpt)))
	}
	n, err := io.Copy(w, res.Body)
	if ctx.Err() != nil {
		return n, nil
	}
	return n, err
}
