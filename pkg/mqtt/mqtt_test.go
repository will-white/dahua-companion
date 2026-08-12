package mqtt

import (
	"context"
	"encoding/json"
	"os"
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
	published []pub
	token     fakeToken
}

func (f *fakePaho) IsConnectionOpen() bool { return f.connected.Load() }
func (f *fakePaho) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	f.published = append(f.published, pub{topic: topic, qos: qos, retained: retained, payload: payload.(string)})
	return &f.token
}

func newTestClient(connected bool) (*Client, *fakePaho) {
	f := &fakePaho{}
	f.connected.Store(connected)
	c := &Client{
		client:      f,
		cfg:         &config.Mqtt{Topic: "doorbell/pressed", AvailabilityTopic: "doorbell/availability"},
		queue:       make(chan event, 100),
		reconnected: make(chan struct{}, 1),
	}
	return c, f
}

func TestPublishDropsWhenFull(t *testing.T) {
	c, _ := newTestClient(true)
	for i := 0; i < cap(c.queue)+10; i++ {
		c.Publish("") // must never block, even beyond capacity
	}
	if got := len(c.queue); got != cap(c.queue) {
		t.Errorf("queue length = %d, want %d", got, cap(c.queue))
	}
}

func TestDeliverPublishesFreshEvent(t *testing.T) {
	c, f := newTestClient(true)
	c.deliver(context.Background(), event{payload: "", received: time.Now()})
	if len(f.published) != 1 {
		t.Errorf("published %d messages, want 1", len(f.published))
	}
	if p := f.published[0]; p.qos != 1 {
		t.Errorf("published with qos %d, want 1 so the broker acks delivery", p.qos)
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
	c.Publish("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.ProcessQueue(ctx)
	if len(f.published) != 1 {
		t.Errorf("published %d messages, want the queued event flushed", len(f.published))
	}
}

func TestDrainDropsWhenBrokerUnreachable(t *testing.T) {
	c, f := newTestClient(false)
	c.Publish("")
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
	c.deliver(context.Background(), event{payload: "", received: time.Now()})
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
	c.deliver(context.Background(), event{payload: "", received: time.Now()})
	if len(f.published) != 1 {
		t.Errorf("publish attempts = %d, want exactly 1 (no retry loop)", len(f.published))
	}
}

func TestDeliverDropsStaleEvent(t *testing.T) {
	c, f := newTestClient(true)
	stale := event{payload: "", received: time.Now().Add(-maxEventAge - time.Second)}
	c.deliver(context.Background(), stale)
	if len(f.published) != 0 {
		t.Errorf("published %d messages, want 0", len(f.published))
	}
}

func TestDeliverDropsStaleEventWhileDisconnected(t *testing.T) {
	c, f := newTestClient(false)
	stale := event{payload: "", received: time.Now().Add(-maxEventAge - time.Second)}
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
	c.deliver(ctx, event{payload: "", received: time.Now()})
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
	c.deliver(context.Background(), event{payload: "", received: time.Now()})
	if len(f.published) != 1 {
		t.Errorf("published %d messages, want 1", len(f.published))
	}
}
