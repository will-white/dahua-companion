package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
