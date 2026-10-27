// Package metrics keeps the app's counters and renders them in the
// Prometheus text exposition format. It is deliberately tiny and
// dependency-free: a dozen counters do not justify the Prometheus client
// library and its module tree in a scratch image.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Counter is a monotonically increasing count, optionally split by labels.
type Counter struct {
	name, help string
	labels     []string

	mu     sync.Mutex
	values map[string]uint64 // rendered label set -> count
}

// Inc adds one for the given label values, in the order the labels were
// declared.
func (c *Counter) Inc(labelValues ...string) {
	key := renderLabels(c.labels, labelValues)
	c.mu.Lock()
	c.values[key]++
	c.mu.Unlock()
}

// Value returns the current count for the label values (for tests).
func (c *Counter) Value(labelValues ...string) uint64 {
	key := renderLabels(c.labels, labelValues)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key]
}

func (c *Counter) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.labels) == 0 && len(c.values) == 0 {
		fmt.Fprintf(w, "%s 0\n", c.name)
		return
	}
	for _, key := range sortedKeys(c.values) {
		fmt.Fprintf(w, "%s%s %d\n", c.name, key, c.values[key])
	}
}

// Summary tracks the count and sum of observed durations, per label set:
// enough for an average and a rate, without histogram buckets.
type Summary struct {
	name, help string
	labels     []string

	mu    sync.Mutex
	count map[string]uint64
	sum   map[string]float64
}

// Observe records one duration for the given label values.
func (s *Summary) Observe(d time.Duration, labelValues ...string) {
	key := renderLabels(s.labels, labelValues)
	s.mu.Lock()
	s.count[key]++
	s.sum[key] += d.Seconds()
	s.mu.Unlock()
}

func (s *Summary) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s summary\n", s.name, s.help, s.name)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range sortedKeys(s.count) {
		fmt.Fprintf(w, "%s_sum%s %s\n", s.name, key, formatFloat(s.sum[key]))
		fmt.Fprintf(w, "%s_count%s %d\n", s.name, key, s.count[key])
	}
}

// gaugeFunc is a gauge read at scrape time.
type gaugeFunc struct {
	name, help string
	read       func() float64
}

func (g *gaugeFunc) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", g.name, g.help, g.name, g.name, formatFloat(g.read()))
}

type metric interface{ write(io.Writer) }

var (
	registryMu sync.Mutex
	registry   []metric
)

func register(m metric) {
	registryMu.Lock()
	registry = append(registry, m)
	registryMu.Unlock()
}

// NewCounter registers a counter.
func NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{name: name, help: help, labels: labels, values: map[string]uint64{}}
	register(c)
	return c
}

// NewSummary registers a duration summary.
func NewSummary(name, help string, labels ...string) *Summary {
	s := &Summary{name: name, help: help, labels: labels, count: map[string]uint64{}, sum: map[string]float64{}}
	register(s)
	return s
}

// GaugeFunc registers a gauge whose value is read from fn at every scrape.
func GaugeFunc(name, help string, fn func() float64) {
	register(&gaugeFunc{name: name, help: help, read: fn})
}

// BoolGauge adapts a connection check to GaugeFunc's 0/1.
func BoolGauge(fn func() bool) func() float64 {
	return func() float64 {
		if fn() {
			return 1
		}
		return 0
	}
}

// Handler serves every registered metric.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		registryMu.Lock()
		metrics := append([]metric(nil), registry...)
		registryMu.Unlock()
		for _, m := range metrics {
			m.write(w)
		}
	})
}

func renderLabels(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		v := ""
		if i < len(values) {
			v = values[i]
		}
		fmt.Fprintf(&b, "%s=%q", name, v)
	}
	b.WriteByte('}')
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatFloat(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return "NaN"
	}
	return fmt.Sprintf("%g", f)
}

// The app's metrics. Event labels are "press" and "human".
var (
	CameraConnects = NewCounter("dahua_companion_camera_stream_connects_total",
		"Event stream connections established.")
	CameraFailures = NewCounter("dahua_companion_camera_stream_failures_total",
		"Event stream attempts that failed or connections that dropped.")
	SnapshotStreamConnects = NewCounter("dahua_companion_snapshot_stream_connects_total",
		"Event-snapshot stream connections established (SNAPSHOT_SOURCE=event).")
	SnapshotStreamFailures = NewCounter("dahua_companion_snapshot_stream_failures_total",
		"Event-snapshot stream attempts that failed or connections that dropped (SNAPSHOT_SOURCE=event).")
	ForceOnline = NewCounter("dahua_companion_force_online_total",
		"DAHUA_FORCE_ONLINE attempts to mark the camera's cloud connection up, by result (ok, failed).", "result")
	EventsReceived = NewCounter("dahua_companion_events_received_total",
		"Camera events reported for publishing.", "event")
	EventsSuppressed = NewCounter("dahua_companion_events_suppressed_total",
		"Human detections suppressed by HUMAN_COOLDOWN.", "event")
	EventsPublished = NewCounter("dahua_companion_events_published_total",
		"Events acknowledged by the MQTT broker.", "event")
	EventsDropped = NewCounter("dahua_companion_events_dropped_total",
		"Events not delivered, by reason (queue_full, stale, publish_failed, shutdown).", "event", "reason")
	PublishLatency = NewSummary("dahua_companion_event_publish_latency_seconds",
		"Time from camera event to broker acknowledgement.", "event")
	Snapshots = NewCounter("dahua_companion_snapshots_total",
		"Snapshot outcomes (published, dropped, failed, skipped, superseded).", "result")
)
