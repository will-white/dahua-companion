package dahua

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// maxSnapshotBytes caps a snapshot download. Doorbell JPEGs run 100-400 KB;
// the cap only keeps a misbehaving camera from ballooning memory.
const maxSnapshotBytes = 5 << 20

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
