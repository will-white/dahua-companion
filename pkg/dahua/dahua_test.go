package dahua

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"dahua_companion/pkg/config"

	"github.com/cenkalti/backoff/v5"
	"github.com/rs/zerolog"
)

func TestMain(m *testing.M) {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	os.Exit(m.Run())
}

func TestIsDoorbellPressed(t *testing.T) {
	testCases := []struct {
		name     string
		line     string
		expected bool
	}{
		{
			name:     "valid doorbell press event",
			line:     "Code=AlarmLocal;action=Start;index=0",
			expected: true,
		},
		{
			name:     "valid doorbell press event with extra data",
			line:     "Code=AlarmLocal;action=Start;index=0;data=...",
			expected: true,
		},
		{
			name:     "stop event",
			line:     "Code=AlarmLocal;action=Stop;index=0",
			expected: false,
		},
		{
			name:     "different code",
			line:     "Code=VideoMotion;action=Start;index=0",
			expected: false,
		},
		{
			name:     "minimal event without index",
			line:     "Code=AlarmLocal;action=Start",
			expected: true,
		},
		{
			name:     "empty line",
			line:     "",
			expected: false,
		},
		{
			name:     "just code",
			line:     "Code=AlarmLocal;",
			expected: true,
		},
		{
			name:     "code without trailing semicolon",
			line:     "Code=AlarmLocal",
			expected: false,
		},
		{
			name:     "pulse action",
			line:     "Code=AlarmLocal;action=Pulse;index=0",
			expected: false,
		},
		{
			name:     "heartbeat line",
			line:     "Heartbeat",
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := isDoorbellPressed(tc.line)
			if actual != tc.expected {
				t.Errorf("expected %v, got %v", tc.expected, actual)
			}
		})
	}
}

func testClient(serverURL string) *Client {
	return New(&config.Dahua{
		Host:     strings.TrimPrefix(serverURL, "http://"),
		Username: "admin",
		Password: "secret",
	})
}

func TestListenStream(t *testing.T) {
	// Mimic the camera's multipart event stream, CRLF line endings included.
	lines := []string{
		"--myboundary",
		"Content-Type: text/plain",
		"",
		"Heartbeat",
		"--myboundary",
		"Content-Type: text/plain",
		"",
		"Code=VideoMotion;action=Start;index=0",
		"--myboundary",
		"Content-Type: text/plain",
		"",
		"Code=AlarmLocal;action=Start;index=0",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
		for _, line := range lines {
			fmt.Fprint(w, line+"\r\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	events := 0
	connectedDuringEvent := false
	err := c.listen(context.Background(), backoff.NewExponentialBackOff(), func() {
		events++
		connectedDuringEvent = c.IsConnected()
	})

	if err == nil {
		t.Fatal("expected an error when the stream ends")
	}
	if events != 1 {
		t.Errorf("events = %d, want 1", events)
	}
	if !connectedDuringEvent {
		t.Error("IsConnected() = false while the stream was open")
	}
	if c.IsConnected() {
		t.Error("IsConnected() = true after the stream ended")
	}
}

func TestListenNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	err := c.listen(context.Background(), backoff.NewExponentialBackOff(), func() {
		t.Error("onEvent called for a failed connection")
	})
	if err == nil {
		t.Fatal("expected an error for a non-OK status")
	}
	if c.IsConnected() {
		t.Error("IsConnected() = true after a failed connection")
	}
}

func TestProbe(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, "name=Doorbell")
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	if err := c.Probe(context.Background()); err != nil {
		t.Errorf("Probe() = %v, want nil", err)
	}

	status = http.StatusInternalServerError
	if err := c.Probe(context.Background()); err == nil {
		t.Error("Probe() = nil, want error for non-OK status")
	}
}
