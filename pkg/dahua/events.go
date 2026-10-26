package dahua

import (
	"strconv"
	"strings"
	"time"

	"dahua_companion/pkg/metrics"

	"github.com/rs/zerolog/log"
)

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

// label is the event's metrics label.
func (e EventKind) label() string {
	if e == HumanDetected {
		return "human"
	}
	return "press"
}

// Event is one camera event: what happened and on which video channel (the
// event's index, 0 for a single-camera doorbell).
type Event struct {
	Kind    EventKind
	Channel int
}

// report applies the human cooldown and hands ev to onEvent.
func (c *Client) report(ev Event, onEvent func(Event)) {
	if ev.Kind == HumanDetected && c.humanCoolingDown(time.Now()) {
		log.Debug().Int("channel", ev.Channel).Msg("Human detection within cooldown; suppressed")
		metrics.EventsSuppressed.Inc(ev.Kind.label())
		return
	}
	metrics.EventsReceived.Inc(ev.Kind.label())
	log.Info().Stringer("event", ev.Kind).Int("channel", ev.Channel).Msg("Camera event received")
	onEvent(ev)
}

// reportIVS reports a completed IVS block as a human detection when its data
// names a human; other objects (vehicles, animals) are only debug-logged.
func (c *Client) reportIVS(b *ivsBlock, onEvent func(Event)) {
	human, err := b.human()
	if err != nil || !human {
		log.Debug().Err(err).Str("code", b.code).Str("data", b.data.String()).Msg("IVS event without a human")
		return
	}
	c.report(Event{Kind: HumanDetected, Channel: b.channel}, onEvent)
}

// humanCoolingDown reports whether a human detection at now falls within
// HumanCooldown of the last reported one, recording now when it does not.
func (c *Client) humanCoolingDown(now time.Time) bool {
	return coolingDown(c.HumanCooldown, &c.lastHuman, now)
}

// coolingDown reports whether now falls within window of *last, recording now
// in *last when it does not (so suppressed events never extend the window).
func coolingDown(window time.Duration, last *time.Time, now time.Time) bool {
	if window > 0 && !last.IsZero() && now.Sub(*last) < window {
		return true
	}
	*last = now
	return false
}

// logUnhandledLine records, at debug level, stream lines that are not a
// reported event: other event codes, Stop actions, and the JSON data blocks
// some events carry. It is the raw material for supporting a camera that
// names its events differently. Heartbeats and multipart framing are skipped
// so the log stays readable.
func logUnhandledLine(line string) {
	if line == "" || strings.EqualFold(line, "Heartbeat") || strings.HasPrefix(line, "--") ||
		strings.HasPrefix(line, "Content-Type:") || strings.HasPrefix(line, "Content-Length:") {
		return
	}
	log.Debug().Str("line", line).Msg("Unhandled camera stream line")
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
