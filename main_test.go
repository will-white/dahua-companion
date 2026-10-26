//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// freePort reserves an ephemeral localhost port and returns its number.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestEndToEndDoorbellPress boots the real main() against an embedded MQTT
// broker and a fake camera: the press and the human detection on the fake
// event stream must arrive on their topics, availability and HA discovery must be announced, /health
// must report 200, and SIGTERM must shut everything down cleanly. It exercises
// the wiring in main.go that the package tests cannot, so it is the only test
// allowed to call main() (the -healthcheck flag registration is once-only).
func TestEndToEndDoorbellPress(t *testing.T) {
	broker := mochi.New(nil)
	if err := broker.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatal(err)
	}
	brokerAddr := "127.0.0.1:" + freePort(t)
	if err := broker.AddListener(listeners.NewTCP(listeners.Config{ID: "tcp", Address: brokerAddr})); err != nil {
		t.Fatal(err)
	}
	go func() { _ = broker.Serve() }()
	t.Cleanup(func() { _ = broker.Close() })

	// Fake camera: answers health probes, streams one press and one human
	// detection, then heartbeats
	// until the client hangs up.
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "magicBox") {
			fmt.Fprint(w, "name=Doorbell")
			return
		}
		if strings.Contains(r.URL.Path, "snapshot") {
			fmt.Fprint(w, "jpeg-"+r.URL.Query().Get("channel"))
			return
		}
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
		fmt.Fprint(w, "Code=AlarmLocal;action=Start;index=0\r\n")
		w.(http.Flusher).Flush()
		// Let the press snapshot finish first: one in flight at a time means
		// a detection arriving mid-fetch would (by design) get none.
		time.Sleep(500 * time.Millisecond)
		fmt.Fprint(w, "Code=SmartMotionHuman;action=Start;index=0\r\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
				fmt.Fprint(w, "Heartbeat\r\n")
				w.(http.Flusher).Flush()
			}
		}
	}))
	t.Cleanup(cam.Close)

	healthPort := freePort(t)
	t.Setenv("MQTT_BROKER_URL", "tcp://"+brokerAddr)
	t.Setenv("MQTT_CLIENT_ID", "e2e-companion")
	t.Setenv("MQTT_USERNAME", "u")
	t.Setenv("MQTT_PASSWORD", "p")
	t.Setenv("MQTT_TOPIC", "doorbell/pressed")
	t.Setenv("MQTT_AVAILABILITY_TOPIC", "doorbell/availability")
	t.Setenv("MQTT_DISCOVERY_PREFIX", "homeassistant")
	t.Setenv("MQTT_PAYLOAD_FORMAT", "json")
	t.Setenv("SNAPSHOT", "press,human")
	t.Setenv("HOSTNAME_OR_IP", strings.TrimPrefix(cam.URL, "http://"))
	t.Setenv("DAHUA_USERNAME", "admin")
	t.Setenv("DAHUA_PASSWORD", "secret")
	t.Setenv("HEALTH_PORT", healthPort)

	// Observe what main publishes.
	presses := make(chan string, 16)
	humans := make(chan struct{}, 16)
	availability := make(chan string, 16)
	discovery := make(chan string, 16)
	snapshots := make(chan string, 16)
	obsOpts := mqtt.NewClientOptions().AddBroker("tcp://" + brokerAddr).SetClientID("e2e-observer")
	obsOpts.SetConnectRetry(true)
	obsOpts.SetConnectRetryInterval(100 * time.Millisecond)
	sub := mqtt.NewClient(obsOpts)
	if token := sub.Connect(); !token.WaitTimeout(10*time.Second) || token.Error() != nil {
		t.Fatalf("observer connect: %v", token.Error())
	}
	t.Cleanup(func() { sub.Disconnect(100) })
	subscribe := func(topic string, cb mqtt.MessageHandler) {
		t.Helper()
		if token := sub.Subscribe(topic, 1, cb); !token.WaitTimeout(5*time.Second) || token.Error() != nil {
			t.Fatalf("subscribe %s: %v", topic, token.Error())
		}
	}
	subscribe("doorbell/pressed", func(_ mqtt.Client, m mqtt.Message) { presses <- string(m.Payload()) })
	subscribe("doorbell/human", func(_ mqtt.Client, _ mqtt.Message) { humans <- struct{}{} })
	subscribe("doorbell/availability", func(_ mqtt.Client, m mqtt.Message) { availability <- string(m.Payload()) })
	subscribe("doorbell/+/snapshot", func(_ mqtt.Client, m mqtt.Message) { snapshots <- m.Topic() + "=" + string(m.Payload()) })
	subscribe("homeassistant/#", func(_ mqtt.Client, m mqtt.Message) { discovery <- m.Topic() })

	// main() parses flags; hide go test's own flags from it.
	oldArgs := os.Args
	os.Args = []string{oldArgs[0]}
	t.Cleanup(func() { os.Args = oldArgs })

	done := make(chan struct{})
	go func() {
		main()
		close(done)
	}()

	// The event stream connecting flips availability online; the press follows.
	awaitPayload(t, availability, "online")
	select {
	case payload := <-presses:
		var press struct {
			EventType string `json:"event_type"`
			Channel   int    `json:"channel"`
		}
		if err := json.Unmarshal([]byte(payload), &press); err != nil || press.EventType != "press" {
			t.Errorf("press payload = %q, want JSON with event_type press", payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("press never arrived on doorbell/pressed")
	}
	select {
	case <-humans:
	case <-time.After(10 * time.Second):
		t.Fatal("human detection never arrived on doorbell/human")
	}
	wantSnapshots := map[string]bool{
		"doorbell/pressed/snapshot=jpeg-1": true,
		"doorbell/human/snapshot=jpeg-1":   true,
	}
	for len(wantSnapshots) > 0 {
		select {
		case got := <-snapshots:
			if !wantSnapshots[got] {
				t.Errorf("unexpected snapshot %q", got)
			}
			delete(wantSnapshots, got)
		case <-time.After(10 * time.Second):
			t.Fatalf("snapshots never arrived: %v", wantSnapshots)
		}
	}
	wantDiscovery := map[string]bool{
		"homeassistant/event/e2e-companion/doorbell/config":          true,
		"homeassistant/event/e2e-companion/human/config":             true,
		"homeassistant/image/e2e-companion/doorbell_snapshot/config": true,
		"homeassistant/image/e2e-companion/human_snapshot/config":    true,
	}
	for len(wantDiscovery) > 0 {
		select {
		case topic := <-discovery:
			if !wantDiscovery[topic] {
				t.Errorf("unexpected discovery config topic %q", topic)
			}
			delete(wantDiscovery, topic)
		case <-time.After(10 * time.Second):
			t.Fatalf("discovery configs never arrived: %v", wantDiscovery)
		}
	}

	// One connection per request: a pooled client can leave a dialed-but-unused
	// connection open, which http.Server.Shutdown waits out for 5s.
	httpc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	// /health must report 200 once broker, stream, and probe are all up.
	healthDeadline := time.Now().Add(10 * time.Second)
	for {
		res, err := httpc.Get("http://127.0.0.1:" + healthPort + "/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(healthDeadline) {
			t.Fatal("/health never reported 200")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// /metrics reflects what just happened.
	res, err := httpc.Get("http://127.0.0.1:" + healthPort + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{
		`dahua_companion_events_published_total{event="press"} 1`,
		`dahua_companion_events_published_total{event="human"} 1`,
		"dahua_companion_camera_stream_up 1",
		"dahua_companion_mqtt_connected 1",
	} {
		if !strings.Contains(string(metricsBody), want) {
			t.Errorf("/metrics missing %q:\n%s", want, metricsBody)
		}
	}

	// Graceful shutdown: availability flips offline and main returns.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	awaitPayload(t, availability, "offline")
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("main did not shut down after SIGTERM")
	}
}

// awaitPayload consumes ch until want arrives, tolerating intermediate states
// (e.g. the initial "offline" published before the stream connects).
func awaitPayload(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("never observed availability %q", want)
		}
	}
}
