package dahua

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dahua_companion/pkg/metrics"
)

func TestForceOnline(t *testing.T) {
	var path, query string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		if status != http.StatusOK {
			http.Error(w, "Error\r\nBad Request!", status)
			return
		}
		fmt.Fprint(w, "OK\r\n")
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	if err := c.ForceOnline(context.Background()); err != nil {
		t.Fatalf("ForceOnline() = %v, want nil", err)
	}
	if path != "/cgi-bin/configManager.cgi" || query != "action=setConfig&VSP_PaaS.Online=true" {
		t.Errorf("request = %s?%s, want the VSP_PaaS.Online setConfig", path, query)
	}

	status = http.StatusBadRequest
	err := c.ForceOnline(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "Bad Request!") {
		t.Errorf("ForceOnline() = %v, want the status and the camera's reply", err)
	}
}

func TestKeepOnlineRepeatsUntilCancelled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "OK\r\n")
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.KeepOnline(ctx, 20*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() < 3 {
		t.Fatalf("hits = %d, want the setting re-sent every interval", hits.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("KeepOnline did not stop on cancellation")
	}
	after := hits.Load()
	time.Sleep(60 * time.Millisecond)
	if hits.Load() != after {
		t.Error("KeepOnline kept writing to the camera after it returned")
	}
}

func TestKeepOnlineRetriesAfterFailure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			http.Error(w, "Error", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "OK\r\n")
	}))
	defer srv.Close()

	okBefore, failedBefore := metrics.ForceOnline.Value("ok"), metrics.ForceOnline.Value("failed")
	c := testClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.KeepOnline(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for metrics.ForceOnline.Value("ok") == okBefore && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := metrics.ForceOnline.Value("failed") - failedBefore; got != 2 {
		t.Errorf("failed attempts = %d, want 2", got)
	}
	if metrics.ForceOnline.Value("ok") == okBefore {
		t.Error("KeepOnline gave up instead of retrying after failures")
	}
}
