package dahua

import (
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"
	"time"

	"dahua_companion/pkg/metrics"

	"github.com/cenkalti/backoff/v7"
	"github.com/rs/zerolog/log"
)

// ListenSnapshots maintains the camera's event-snapshot stream
// (snapManager.cgi?action=attachFileProc) until ctx is cancelled, calling
// onSnapshot with each still the camera captured at the moment of an event of
// one of the given kinds. Unlike fetching snapshot.cgi after the event, the
// image shows the instant the event fired. Human stills honour HumanCooldown,
// as the events do. The stream reconnects with the same backoff and idle
// watchdog as the event stream, but does not affect IsConnected: snapshots
// are best effort.
func (c *Client) ListenSnapshots(ctx context.Context, kinds []EventKind, onSnapshot func(Event, []byte)) {
	var codes []string
	for _, kind := range kinds {
		switch kind {
		case DoorbellPressed:
			codes = append(codes, "AlarmLocal")
		case HumanDetected:
			codes = append(codes, "SmartMotionHuman", "CrossLineDetection", "CrossRegionDetection")
		}
	}
	url := c.url(fmt.Sprintf("/cgi-bin/snapManager.cgi?action=attachFileProc&heartbeat=%d&Flags[0]=Event&Events=[%s]",
		heartbeatSeconds, strings.Join(codes, "%2C")))

	var lastHuman time.Time // only this goroutine touches it
	deliver := func(ev Event, img []byte) {
		if ev.Kind == HumanDetected && coolingDown(c.HumanCooldown, &lastHuman, time.Now()) {
			log.Debug().Msg("Human snapshot within cooldown; suppressed")
			return
		}
		onSnapshot(ev, img)
	}
	retryStream(ctx, "snapshot stream", func(bo *backoff.ExponentialBackOff) error {
		err := c.stream(ctx, bo, "snapshot stream", url, false, func(body io.Reader, contentType string) error {
			return readSnapshotParts(body, contentType, deliver)
		})
		if err != nil && ctx.Err() == nil {
			metrics.SnapshotStreamFailures.Inc()
		}
		return err
	})
}

// SnapshotStreamConnected reports whether the event-snapshot stream
// (ListenSnapshots) is currently established.
func (c *Client) SnapshotStreamConnected() bool {
	return c.snapshotConnected.Load()
}

// readSnapshotParts reads the multipart snapshot stream: a text/plain part
// describing an event (or a heartbeat), followed for events by an image/jpeg
// part holding the still. Each image is delivered with the event described
// just before it; images after anything unrecognized are discarded.
func readSnapshotParts(body io.Reader, contentType string, deliver func(Event, []byte)) error {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("snapshot stream content type %q: %w", contentType, err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Errorf("snapshot stream content type %q has no boundary", contentType)
	}
	mr := multipart.NewReader(body, boundary)
	var pending *Event
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(part.Header.Get("Content-Type"), "image/") {
			img, err := io.ReadAll(io.LimitReader(part, maxSnapshotBytes+1))
			if err != nil {
				return err
			}
			switch {
			case pending == nil:
				// An image for an event we don't publish (a Stop, a vehicle).
			case len(img) == 0 || len(img) > maxSnapshotBytes:
				log.Warn().Int("bytes", len(img)).Msg("Discarding event snapshot of unusable size")
			default:
				deliver(*pending, img)
			}
			pending = nil
			continue
		}
		text, err := io.ReadAll(io.LimitReader(part, maxIVSDataBytes))
		if err != nil {
			return err
		}
		// The API docs spell it both "heartbeat" and "Heartbeat".
		if strings.EqualFold(strings.TrimSpace(string(text)), "Heartbeat") {
			continue // heartbeats don't separate an event from its image
		}
		if ev, ok := parseSnapshotEvent(string(text)); ok {
			pending = &ev
		} else {
			logUnhandledLine(strings.TrimSpace(string(text)))
			pending = nil
		}
	}
}

// parseSnapshotEvent reads the first event of a snapshot stream text part:
//
//	Events[0].EventBaseInfo.Code=AlarmLocal
//	Events[0].EventBaseInfo.Action=Start
//	Events[0].EventBaseInfo.Index=0
//
// It recognizes the same events as the event stream: presses, smart motion
// human detections, and tripwire/intrusion events whose object is a human.
func parseSnapshotEvent(text string) (Event, bool) {
	fields := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.HasPrefix(key, "Events[0].") {
			fields[strings.TrimPrefix(key, "Events[0].")] = value
		}
	}
	if action, ok := fields["EventBaseInfo.Action"]; ok &&
		!strings.EqualFold(action, "Start") && !strings.EqualFold(action, "Pulse") {
		return Event{}, false
	}
	var ev Event
	switch fields["EventBaseInfo.Code"] {
	case "AlarmLocal":
		ev.Kind = DoorbellPressed
	case "SmartMotionHuman":
		ev.Kind = HumanDetected
	case "CrossLineDetection", "CrossRegionDetection":
		human := false
		for key, value := range fields {
			if strings.HasSuffix(key, ".ObjectType") && strings.EqualFold(value, "Human") {
				human = true
				break
			}
		}
		if !human {
			return Event{}, false
		}
		ev.Kind = HumanDetected
	default:
		return Event{}, false
	}
	index, ok := fields["EventBaseInfo.Index"]
	if !ok {
		index = fields["Channel"]
	}
	ev.Channel, _ = strconv.Atoi(index)
	return ev, true
}
