package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	NewCounter("test_plain_total", "A counter without labels.") // never incremented: renders 0
	labelled := NewCounter("test_labelled_total", "A counter with labels.", "event", "reason")
	latency := NewSummary("test_latency_seconds", "A summary.", "event")
	GaugeFunc("test_up", "A gauge.", BoolGauge(func() bool { return true }))

	labelled.Inc("press", "stale")
	labelled.Inc("press", "stale")
	labelled.Inc("human", "queue_full")
	latency.Observe(250*time.Millisecond, "press")
	latency.Observe(750*time.Millisecond, "press")

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE test_plain_total counter\ntest_plain_total 0\n",
		`test_labelled_total{event="human",reason="queue_full"} 1`,
		`test_labelled_total{event="press",reason="stale"} 2`,
		"# TYPE test_latency_seconds summary",
		`test_latency_seconds_sum{event="press"} 1`,
		`test_latency_seconds_count{event="press"} 2`,
		"# TYPE test_up gauge\ntest_up 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}
	if got := labelled.Value("press", "stale"); got != 2 {
		t.Errorf("Value() = %d, want 2", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
}
