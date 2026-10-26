package mqtt

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dahua_companion/pkg/config"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog"
)

func TestMain(m *testing.M) {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	os.Exit(m.Run())
}

type fakeToken struct {
	err      error
	timedOut bool
}

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return !t.timedOut }
func (t *fakeToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (t *fakeToken) Error() error { return t.err }

type pub struct {
	topic    string
	qos      byte
	retained bool
	payload  string
}

// fakePaho implements the parts of mqtt.Client that Client uses; the embedded
// interface panics on anything else.
type fakePaho struct {
	mqtt.Client
	connected atomic.Bool
	mu        sync.Mutex // guards published for tests that publish from a goroutine
	published []pub
	token     fakeToken
}

func (f *fakePaho) IsConnectionOpen() bool { return f.connected.Load() }
func (f *fakePaho) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, pub{topic: topic, qos: qos, retained: retained, payload: payload.(string)})
	return &f.token
}

func (f *fakePaho) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

func (f *fakePaho) snapshot() []pub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pub(nil), f.published...)
}

func newTestClient(connected bool) (*Client, *fakePaho) {
	f := &fakePaho{}
	f.connected.Store(connected)
	c := &Client{
		client:      f,
		cfg:         &config.Mqtt{Topic: "doorbell/pressed", HumanTopic: "doorbell/human", AvailabilityTopic: "doorbell/availability", PayloadFormat: config.PayloadEmpty},
		presses:     make(chan event, 100),
		detections:  make(chan event, 100),
		reconnected: make(chan struct{}, 1),
	}
	return c, f
}

func TestPublishDropsWhenFull(t *testing.T) {
	c, _ := newTestClient(true)
	for i := 0; i < cap(c.detections)+10; i++ {
		c.PublishHuman(0) // must never block, even beyond capacity
	}
	if got := len(c.detections); got != cap(c.detections) {
		t.Errorf("queue length = %d, want %d", got, cap(c.detections))
	}
	// A full detection queue must not cost a press its place.
	c.PublishPress(0)
	if got := len(c.presses); got != 1 {
		t.Errorf("press queue length = %d, want 1 despite full detection queue", got)
	}
}

func TestProcessQueueDeliversPressesFirst(t *testing.T) {
	c, f := newTestClient(true)
	c.PublishHuman(0)
	c.PublishHuman(0)
	c.PublishPress(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.ProcessQueue(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	pubs := f.snapshot()
	if len(pubs) != 3 || pubs[0].topic != "doorbell/pressed" {
		t.Errorf("published = %+v, want the press delivered ahead of queued detections", pubs)
	}
}

func TestPayloadFormats(t *testing.T) {
	c, _ := newTestClient(true)
	at := time.Date(2026, 10, 26, 6, 0, 0, 0, time.UTC)
	if got := c.payload(pressEventType, 0, at); got != "" {
		t.Errorf("empty format payload = %q, want empty", got)
	}
	c.cfg.PayloadFormat = config.PayloadJSON
	got := c.payload(humanEventType, 1, at)
	want := `{"event_type":"detected","timestamp":"2026-10-26T06:00:00Z","channel":1}`
	if got != want {
		t.Errorf("json payload = %s, want %s", got, want)
	}
}

func TestDeliverPublishesFreshEvent(t *testing.T) {
	c, f := newTestClient(true)
	c.deliver(context.Background(), event{topic: "doorbell/pressed", payload: "", received: time.Now()})
	if len(f.published) != 1 {
		t.Errorf("published %d messages, want 1", len(f.published))
	}
	if p := f.published[0]; p.qos != 1 {
		t.Errorf("published with qos %d, want 1 so the broker acks delivery", p.qos)
	}
}

func TestDeliverUsesEventTopic(t *testing.T) {
	c, f := newTestClient(true)
	c.PublishPress(0)
	c.PublishHuman(0)
	c.deliver(context.Background(), <-c.presses)
	c.deliver(context.Background(), <-c.detections)
	if len(f.published) != 2 || f.published[0].topic != "doorbell/pressed" || f.published[1].topic != "doorbell/human" {
		t.Errorf("published = %+v, want each event on its own topic in order", f.published)
	}
}

func TestPublishAvailabilityRetainsState(t *testing.T) {
	c, f := newTestClient(true)
	c.PublishAvailability(true)
	c.PublishAvailability(false)
	if len(f.published) != 2 {
		t.Fatalf("published %d messages, want 2", len(f.published))
	}
	for i, want := range []string{"online", "offline"} {
		p := f.published[i]
		if p.topic != "doorbell/availability" || p.payload != want || !p.retained || p.qos != 1 {
			t.Errorf("publish %d = %+v, want retained qos-1 %q on doorbell/availability", i, p, want)
		}
	}
}

func TestPublishDiscovery(t *testing.T) {
	c, f := newTestClient(true)
	c.cfg.ClientID = "dahua companion!" // exercises topic-id sanitization
	c.cfg.DiscoveryPrefix = "homeassistant"
	c.cfg.HumanTopic = "" // human detection disabled: the doorbell entity only
	c.publishDiscovery()
	if len(f.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(f.published))
	}
	p := f.published[0]
	if p.topic != "homeassistant/event/dahua_companion_/doorbell/config" || !p.retained || p.qos != 1 {
		t.Errorf("discovery publish = %+v, want retained qos-1 config under the prefix", p)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(p.payload), &cfg); err != nil {
		t.Fatalf("discovery payload is not JSON: %v", err)
	}
	if cfg["state_topic"] != "doorbell/pressed" || cfg["availability_topic"] != "doorbell/availability" || cfg["device_class"] != "doorbell" {
		t.Errorf("discovery config = %v, want press and availability topics wired", cfg)
	}
	if cfg["value_template"] != `{"event_type":"press"}` {
		t.Errorf("value_template = %v, want it to supply the event for empty payloads", cfg["value_template"])
	}
}

func TestPublishDiscoveryIncludesHumanEntity(t *testing.T) {
	c, f := newTestClient(true)
	c.cfg.ClientID = "companion"
	c.cfg.DiscoveryPrefix = "homeassistant"
	c.cfg.HumanTopic = "doorbell/human"
	c.publishDiscovery()
	if len(f.published) != 2 {
		t.Fatalf("published %d messages, want doorbell and human configs", len(f.published))
	}
	p := f.published[1]
	if p.topic != "homeassistant/event/companion/human/config" || !p.retained {
		t.Errorf("human discovery publish = %+v, want retained config under the prefix", p)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(p.payload), &cfg); err != nil {
		t.Fatalf("discovery payload is not JSON: %v", err)
	}
	if cfg["state_topic"] != "doorbell/human" || cfg["device_class"] != "motion" || cfg["unique_id"] != "companion_human" {
		t.Errorf("human discovery config = %v, want the human topic wired as a motion event", cfg)
	}
}

func TestPublishDiscoveryJSONPayloadDropsTemplate(t *testing.T) {
	c, f := newTestClient(true)
	c.cfg.DiscoveryPrefix = "homeassistant"
	c.cfg.PayloadFormat = config.PayloadJSON
	c.publishDiscovery()
	var cfg map[string]any
	if err := json.Unmarshal([]byte(f.published[0].payload), &cfg); err != nil {
		t.Fatalf("discovery payload is not JSON: %v", err)
	}
	if _, ok := cfg["value_template"]; ok {
		t.Error("value_template set with JSON payloads; it would hide the payload's attributes")
	}
}

func TestPublishDiscoveryDisabledByDefault(t *testing.T) {
	c, f := newTestClient(true)
	c.publishDiscovery()
	if len(f.published) != 0 {
		t.Errorf("published %d messages, want 0 with no discovery prefix", len(f.published))
	}
}

func TestProcessQueueDrainsQueuedEventAtShutdown(t *testing.T) {
	c, f := newTestClient(true)
	c.PublishPress(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.ProcessQueue(ctx)
	if len(f.published) != 1 {
		t.Errorf("published %d messages, want the queued event flushed", len(f.published))
	}
}

func TestDrainDropsWhenBrokerUnreachable(t *testing.T) {
	c, f := newTestClient(false)
	c.PublishPress(0)
	done := make(chan struct{})
	go func() {
		c.drain()
		close(done)
	}()
	select {
	case <-done: // drain must not wait for a reconnect
	case <-time.After(time.Second):
		t.Fatal("drain blocked; it must drop events while the broker is down")
	}
	if len(f.published) != 0 {
		t.Errorf("published %d messages, want 0", len(f.published))
	}
}

func TestDeliverWakesOnReconnect(t *testing.T) {
	c, f := newTestClient(false)
	start := time.Now()
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.connected.Store(true)
		c.reconnected <- struct{}{}
	}()
	c.deliver(context.Background(), event{topic: "doorbell/pressed", payload: "", received: time.Now()})
	if len(f.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(f.published))
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("deliver took %v, want a prompt wake instead of the poll tick", elapsed)
	}
}

func TestRepublishAvailabilityAnnouncesLastState(t *testing.T) {
	c, f := newTestClient(true)
	c.PublishAvailability(true)
	c.republishAvailability() // what OnConnect runs after a broker reconnect
	if len(f.published) != 2 || f.published[1].payload != "online" {
		t.Fatalf("published = %+v, want the reconnect to re-announce online", f.published)
	}
}

func TestDeliverGivesUpOnUnackedPublish(t *testing.T) {
	c, f := newTestClient(true)
	f.token.timedOut = true
	c.deliver(context.Background(), event{topic: "doorbell/pressed", payload: "", received: time.Now()})
	if len(f.published) != 1 {
		t.Errorf("publish attempts = %d, want exactly 1 (no retry loop)", len(f.published))
	}
}

func TestDeliverDropsStaleEvent(t *testing.T) {
	c, f := newTestClient(true)
	stale := event{topic: "doorbell/pressed", payload: "", received: time.Now().Add(-maxEventAge - time.Second)}
	c.deliver(context.Background(), stale)
	if len(f.published) != 0 {
		t.Errorf("published %d messages, want 0", len(f.published))
	}
}

func TestDeliverDropsStaleEventWhileDisconnected(t *testing.T) {
	c, f := newTestClient(false)
	stale := event{topic: "doorbell/pressed", payload: "", received: time.Now().Add(-maxEventAge - time.Second)}
	done := make(chan struct{})
	go func() {
		c.deliver(context.Background(), stale)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliver did not return promptly for a stale event")
	}
	if len(f.published) != 0 {
		t.Errorf("published %d messages, want 0", len(f.published))
	}
}

func TestDeliverStopsOnShutdown(t *testing.T) {
	c, f := newTestClient(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.deliver(ctx, event{topic: "doorbell/pressed", payload: "", received: time.Now()})
	if len(f.published) != 0 {
		t.Errorf("published %d messages, want 0", len(f.published))
	}
}

func TestDeliverWaitsForReconnect(t *testing.T) {
	c, f := newTestClient(false)
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.connected.Store(true)
	}()
	c.deliver(context.Background(), event{topic: "doorbell/pressed", payload: "", received: time.Now()})
	if len(f.published) != 1 {
		t.Errorf("published %d messages, want 1", len(f.published))
	}
}
