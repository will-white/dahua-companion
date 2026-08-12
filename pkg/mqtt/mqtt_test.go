package mqtt

import (
	"context"
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

// fakePaho implements the parts of mqtt.Client that Client uses; the embedded
// interface panics on anything else.
type fakePaho struct {
	mqtt.Client
	connected atomic.Bool
	published []string
	qos       []byte
	token     fakeToken
}

func (f *fakePaho) IsConnectionOpen() bool { return f.connected.Load() }
func (f *fakePaho) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	f.published = append(f.published, payload.(string))
	f.qos = append(f.qos, qos)
	return &f.token
}

func newTestClient(connected bool) (*Client, *fakePaho) {
	f := &fakePaho{}
	f.connected.Store(connected)
	c := &Client{
		client: f,
		cfg:    &config.Mqtt{Topic: "doorbell/pressed"},
		queue:  make(chan event, 100),
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
	if len(f.qos) != 1 || f.qos[0] != 1 {
		t.Errorf("published with qos %v, want [1] so the broker acks delivery", f.qos)
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
