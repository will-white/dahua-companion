package dahua

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"dahua_companion/pkg/config"

	"github.com/cenkalti/backoff/v7"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestMain(m *testing.M) {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	os.Exit(m.Run())
}

func TestParseEvent(t *testing.T) {
	testCases := []struct {
		name     string
		line     string
		expected bool
		event    EventKind
		channel  int
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
		{
			name:     "human detection start",
			line:     "Code=SmartMotionHuman;action=Start;index=0",
			expected: true,
			event:    HumanDetected,
		},
		{
			name:     "human detection start with data block",
			line:     "Code=SmartMotionHuman;action=Start;index=0;data={",
			expected: true,
			event:    HumanDetected,
		},
		{
			name:     "human detection stop",
			line:     "Code=SmartMotionHuman;action=Stop;index=0",
			expected: false,
		},
		{
			name:     "second channel",
			line:     "Code=AlarmLocal;action=Start;index=1",
			expected: true,
			channel:  1,
		},
		{
			name:     "vehicle detection",
			line:     "Code=SmartMotionVehicle;action=Start;index=0",
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			want := Event{Kind: tc.event, Channel: tc.channel}
			if want.Kind == 0 {
				want.Kind = DoorbellPressed
			}
			ev, ok := parseEvent(tc.line)
			if ok != tc.expected {
				t.Errorf("ok = %v, want %v", ok, tc.expected)
			}
			if ok && ev != want {
				t.Errorf("event = %+v, want %+v", ev, want)
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
		"--myboundary",
		"Content-Type: text/plain",
		"",
		"Code=SmartMotionHuman;action=Start;index=0;data={",
		`   "Object" : { "ObjectType" : "Human" }`,
		"}",
	}
	var codes string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codes = r.URL.Query().Get("codes")
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
		for _, line := range lines {
			fmt.Fprint(w, line+"\r\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	var transitions []bool
	c.OnConnectionChange = func(up bool) { transitions = append(transitions, up) }
	c.DetectHumans = true
	var events []Event
	connectedDuringEvent := false
	err := c.listen(context.Background(), backoff.NewExponentialBackOff(), func(ev Event) {
		events = append(events, ev)
		connectedDuringEvent = c.IsConnected()
	})

	if err == nil {
		t.Fatal("expected an error when the stream ends")
	}
	if codes != "[AlarmLocal,SmartMotionHuman]" {
		t.Errorf("subscribed codes = %q, want human detection included", codes)
	}
	if len(events) != 2 || events[0].Kind != DoorbellPressed || events[1].Kind != HumanDetected {
		t.Errorf("events = %v, want [doorbell_pressed human_detected]", events)
	}
	if !connectedDuringEvent {
		t.Error("IsConnected() = false while the stream was open")
	}
	if c.IsConnected() {
		t.Error("IsConnected() = true after the stream ended")
	}
	if len(transitions) != 2 || !transitions[0] || transitions[1] {
		t.Errorf("connection transitions = %v, want [true false]", transitions)
	}
}

func TestLogUnhandledLine(t *testing.T) {
	var buf bytes.Buffer
	oldLogger, oldLevel := log.Logger, zerolog.GlobalLevel()
	log.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	t.Cleanup(func() {
		log.Logger = oldLogger
		zerolog.SetGlobalLevel(oldLevel)
	})

	for _, line := range []string{"", "Heartbeat", "--myboundary", "Content-Type: text/plain", "Content-Length: 39"} {
		logUnhandledLine(line)
	}
	if buf.Len() != 0 {
		t.Errorf("framing lines were logged: %s", buf.String())
	}
	logUnhandledLine("Code=CrossRegionDetection;action=Start;index=0;data={")
	if !strings.Contains(buf.String(), "CrossRegionDetection") {
		t.Errorf("event line not logged: %q", buf.String())
	}
}

func TestHumanCooldown(t *testing.T) {
	c := testClient("http://camera")
	c.HumanCooldown = 10 * time.Second
	start := time.Now()
	for _, tc := range []struct {
		at         time.Duration
		suppressed bool
	}{
		{0, false},
		{5 * time.Second, true},   // within the window of the reported one
		{9 * time.Second, true},   // suppressed detections don't extend it
		{10 * time.Second, false}, // window over: reported, window restarts
		{15 * time.Second, true},
	} {
		if got := c.humanCoolingDown(start.Add(tc.at)); got != tc.suppressed {
			t.Errorf("detection at +%v suppressed = %v, want %v", tc.at, got, tc.suppressed)
		}
	}

	c.HumanCooldown = 0
	if c.humanCoolingDown(start.Add(16 * time.Second)) {
		t.Error("detection suppressed with the cooldown disabled")
	}
}

func TestListenSubscribesDoorbellOnlyByDefault(t *testing.T) {
	var codes string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codes = r.URL.Query().Get("codes")
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	_ = c.listen(context.Background(), backoff.NewExponentialBackOff(), func(Event) {})
	if codes != "[AlarmLocal]" {
		t.Errorf("subscribed codes = %q, want only AlarmLocal without DetectHumans", codes)
	}
}

func TestListenNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	err := c.listen(context.Background(), backoff.NewExponentialBackOff(), func(Event) {
		t.Error("onEvent called for a failed connection")
	})
	if err == nil {
		t.Fatal("expected an error for a non-OK status")
	}
	if c.IsConnected() {
		t.Error("IsConnected() = true after a failed connection")
	}
}

func TestListenIdleStreamTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // go silent without closing, like a dead connection
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.idleTimeout = 100 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- c.listen(context.Background(), backoff.NewExponentialBackOff(), func(Event) {})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no data from camera") {
			t.Errorf("listen() = %v, want idle-timeout error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listen did not abort a silent stream")
	}
}

func TestListenHeartbeatsHoldOffWatchdog(t *testing.T) {
	// Heartbeats arrive at a fraction of the idle timeout: the stream must
	// outlive several timeout periods and end cleanly, not by watchdog.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
		for i := 0; i < 12; i++ {
			fmt.Fprint(w, "Heartbeat\r\n")
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.idleTimeout = 300 * time.Millisecond
	err := c.listen(context.Background(), backoff.NewExponentialBackOff(), func(Event) {})
	if err == nil || strings.Contains(err.Error(), "no data from camera") {
		t.Errorf("listen() = %v, want clean stream end without a watchdog trip", err)
	}
}

func TestListenUnresponsiveConnectIsBounded(t *testing.T) {
	// The camera accepts the TCP connection but never answers the request; the
	// watchdog must bound this phase too, since the client has no timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.idleTimeout = 100 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- c.listen(context.Background(), backoff.NewExponentialBackOff(), func(Event) {})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no data from camera") {
			t.Errorf("listen() = %v, want idle-timeout error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listen did not abort an unresponsive connection")
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
