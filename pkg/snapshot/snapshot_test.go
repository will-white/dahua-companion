package snapshot

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestMain(m *testing.M) {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	os.Exit(m.Run())
}

// fakeCamera hands out fetches that block until released, so tests control
// exactly when each one finishes.
type fakeCamera struct {
	mu        sync.Mutex
	started   chan int // channel number of each fetch as it starts
	release   chan struct{}
	err       error
	published []string
}

func newFakeCamera() *fakeCamera {
	return &fakeCamera{started: make(chan int, 10), release: make(chan struct{})}
}

func (f *fakeCamera) fetch(ctx context.Context, channel int) ([]byte, error) {
	f.started <- channel
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	return []byte("jpeg"), nil
}

func (f *fakeCamera) publish(topic string, _ []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, topic)
}

func (f *fakeCamera) topics() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.published...)
}

func awaitStart(t *testing.T, f *fakeCamera) int {
	t.Helper()
	select {
	case ch := <-f.started:
		return ch
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never started")
		return 0
	}
}

func TestRequestPublishesFetchedImage(t *testing.T) {
	f := newFakeCamera()
	s := New(context.Background(), f.fetch, f.publish)
	s.Request("doorbell/pressed/snapshot", 2, true)
	if ch := awaitStart(t, f); ch != 2 {
		t.Errorf("fetched channel %d, want 2", ch)
	}
	close(f.release)
	s.Wait()
	if got := f.topics(); len(got) != 1 || got[0] != "doorbell/pressed/snapshot" {
		t.Errorf("published = %v, want the press snapshot", got)
	}
}

func TestRequestSkippedWhileInFlight(t *testing.T) {
	f := newFakeCamera()
	s := New(context.Background(), f.fetch, f.publish)
	s.Request("doorbell/human/snapshot", 0, false)
	awaitStart(t, f)
	s.Request("doorbell/human/snapshot", 0, false) // detection behind detection: skipped
	close(f.release)
	s.Wait()
	if got := f.topics(); len(got) != 1 {
		t.Errorf("published = %v, want exactly one snapshot", got)
	}

	// Once the fetch finishes, the next request runs again.
	s.Request("doorbell/human/snapshot", 0, false)
	awaitStart(t, f)
	s.Wait()
	if got := f.topics(); len(got) != 2 {
		t.Errorf("published = %v, want a second snapshot after the first finished", got)
	}
}

func TestPressReplacesDetection(t *testing.T) {
	f := newFakeCamera()
	s := New(context.Background(), f.fetch, f.publish)
	s.Request("doorbell/human/snapshot", 0, false)
	awaitStart(t, f)
	s.Request("doorbell/pressed/snapshot", 0, true)
	awaitStart(t, f)                               // the press fetch starts while the detection is cancelled
	s.Request("doorbell/human/snapshot", 0, false) // nothing replaces a press
	close(f.release)
	s.Wait()
	if got := f.topics(); len(got) != 1 || got[0] != "doorbell/pressed/snapshot" {
		t.Errorf("published = %v, want only the press snapshot", got)
	}
}

func TestPressDoesNotReplacePress(t *testing.T) {
	f := newFakeCamera()
	s := New(context.Background(), f.fetch, f.publish)
	s.Request("doorbell/pressed/snapshot", 0, true)
	awaitStart(t, f)
	s.Request("doorbell/pressed/snapshot", 0, true)
	close(f.release)
	s.Wait()
	select {
	case <-f.started:
		t.Error("second press started a fetch while the first was in flight")
	default:
	}
	if got := f.topics(); len(got) != 1 {
		t.Errorf("published = %v, want one snapshot", got)
	}
}

func TestFailedFetchPublishesNothing(t *testing.T) {
	f := newFakeCamera()
	f.err = errors.New("camera said no")
	s := New(context.Background(), f.fetch, f.publish)
	s.Request("doorbell/pressed/snapshot", 0, true)
	awaitStart(t, f)
	close(f.release)
	s.Wait()
	if got := f.topics(); len(got) != 0 {
		t.Errorf("published = %v, want nothing after a failed fetch", got)
	}
}

func TestShutdownCancelsAndRefuses(t *testing.T) {
	f := newFakeCamera()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, f.fetch, f.publish)
	s.Request("doorbell/pressed/snapshot", 0, true)
	awaitStart(t, f)
	cancel()
	done := make(chan struct{})
	go func() {
		s.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after cancellation; the fetch was not aborted")
	}
	s.Request("doorbell/pressed/snapshot", 0, true)
	select {
	case <-f.started:
		t.Error("fetch started after shutdown")
	default:
	}
	if got := f.topics(); len(got) != 0 {
		t.Errorf("published = %v, want nothing during shutdown", got)
	}
}
