//go:build !windows

package main

import (
	"fmt"
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
// broker and a fake camera: the press on the fake event stream must arrive on
// the press topic, availability and HA discovery must be announced, /health
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

	// Fake camera: answers health probes, streams one press, then heartbeats
	// until the client hangs up.
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "magicBox") {
			fmt.Fprint(w, "name=Doorbell")
			return
		}
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
		fmt.Fprint(w, "Code=AlarmLocal;action=Start;index=0\r\n")
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
	t.Setenv("HOSTNAME_OR_IP", strings.TrimPrefix(cam.URL, "http://"))
	t.Setenv("DAHUA_USERNAME", "admin")
	t.Setenv("DAHUA_PASSWORD", "secret")
	t.Setenv("HEALTH_PORT", healthPort)

	// Observe what main publishes.
	presses := make(chan struct{}, 16)
	availability := make(chan string, 16)
	discovery := make(chan string, 16)
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
	subscribe("doorbell/pressed", func(_ mqtt.Client, _ mqtt.Message) { presses <- struct{}{} })
	subscribe("doorbell/availability", func(_ mqtt.Client, m mqtt.Message) { availability <- string(m.Payload()) })
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
	case <-presses:
	case <-time.After(10 * time.Second):
		t.Fatal("press never arrived on doorbell/pressed")
	}
	select {
	case topic := <-discovery:
		if topic != "homeassistant/event/e2e-companion/doorbell/config" {
			t.Errorf("discovery config topic = %q", topic)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("discovery config never arrived")
	}

	// /health must report 200 once broker, stream, and probe are all up.
	healthDeadline := time.Now().Add(10 * time.Second)
	for {
		res, err := http.Get("http://127.0.0.1:" + healthPort + "/health")
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
