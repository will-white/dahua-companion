package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestMain(m *testing.M) {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	os.Exit(m.Run())
}

func TestHealthCheckHandler(t *testing.T) {
	testCases := []struct {
		name     string
		mqtt     bool
		dahua    bool
		probeErr error
		wantCode int
		wantBody string
	}{
		{
			name:     "all healthy",
			mqtt:     true,
			dahua:    true,
			wantCode: http.StatusOK,
			wantBody: "MQTT: connected, HTTP: connected, Doorbell: okay",
		},
		{
			name:     "mqtt disconnected",
			mqtt:     false,
			dahua:    true,
			wantCode: http.StatusServiceUnavailable,
			wantBody: "MQTT: disconnected",
		},
		{
			name:     "event stream disconnected",
			mqtt:     true,
			dahua:    false,
			wantCode: http.StatusServiceUnavailable,
			wantBody: "HTTP: disconnected",
		},
		{
			name:     "camera unreachable",
			mqtt:     true,
			dahua:    true,
			probeErr: errors.New("dial timeout"),
			wantCode: http.StatusServiceUnavailable,
			wantBody: "Doorbell: unreachable",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(":0",
				func() bool { return tc.mqtt },
				func() bool { return tc.dahua },
				func(context.Context) error { return tc.probeErr },
			)
			rec := httptest.NewRecorder()
			s.healthCheckHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want substring %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestProbeGetsBoundedContext(t *testing.T) {
	var deadlineSet bool
	s := New(":0",
		func() bool { return true },
		func() bool { return true },
		func(ctx context.Context) error {
			_, deadlineSet = ctx.Deadline()
			return nil
		},
	)
	rec := httptest.NewRecorder()
	s.healthCheckHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if !deadlineSet {
		t.Error("probe context has no deadline")
	}
}

func TestProbeCache(t *testing.T) {
	calls := 0
	var probeErr error
	s := New(":0", func() bool { return true }, func() bool { return true }, func(context.Context) error {
		calls++
		return probeErr
	})
	get := func() int {
		rec := httptest.NewRecorder()
		s.healthCheckHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		return rec.Code
	}

	get()
	get()
	if calls != 1 {
		t.Errorf("probe calls = %d, want 1: a fresh success should be reused", calls)
	}

	s.lastProbeOK = time.Now().Add(-2 * s.probeTTL) // the cached success expires
	probeErr = errors.New("camera gone")
	if code := get(); code != http.StatusServiceUnavailable {
		t.Errorf("status = %d after the cache expired on a dead camera, want 503", code)
	}
	if code := get(); code != http.StatusServiceUnavailable || calls != 3 {
		t.Errorf("status = %d, probe calls = %d; want 503 and a fresh probe (failures are not cached)", code, calls)
	}

	probeErr = nil
	if code := get(); code != http.StatusOK {
		t.Errorf("status = %d after the camera recovered, want 200 immediately", code)
	}
}
