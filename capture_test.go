package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dahua_companion/pkg/config"
)

func TestRunCapture(t *testing.T) {
	var eventQuery, snapQuery string
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "eventManager"):
			eventQuery = r.URL.RawQuery
			fmt.Fprint(w, "--myboundary\r\nContent-Type: text/plain\r\n\r\nCode=VideoMotion;action=Start;index=0\r\n")
			fmt.Fprint(w, "Code=AlarmLocal;action=Start;index=0\r\nCode=VideoMotion;action=Stop;index=0\r\n")
		case strings.Contains(r.URL.Path, "snapManager"):
			snapQuery = r.URL.RawQuery
			fmt.Fprint(w, "--myboundary\r\nContent-Type: text/plain\r\n\r\nEvents[0].EventBaseInfo.Code=AlarmLocal\r\n")
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stream until the capture window closes
	}))
	defer cam.Close()

	dir := t.TempDir()
	var out bytes.Buffer
	d := &config.Dahua{Scheme: "http", Host: strings.TrimPrefix(cam.URL, "http://"), Username: "admin", Password: "secret"}
	if code := runCapture(d, 300*time.Millisecond, dir, true, &out); code != 0 {
		t.Fatalf("runCapture() = %d, want 0; output:\n%s", code, out.String())
	}

	if !strings.Contains(eventQuery, "codes=[All]") || !strings.Contains(snapQuery, "Events=[All]") {
		t.Errorf("queries = %q / %q, want every event code subscribed", eventQuery, snapQuery)
	}
	events, _ := filepath.Glob(filepath.Join(dir, "capture-*-events.txt"))
	snaps, _ := filepath.Glob(filepath.Join(dir, "capture-*-snapshots.multipart"))
	if len(events) != 1 || len(snaps) != 1 {
		t.Fatalf("recordings = %v / %v, want one of each", events, snaps)
	}
	data, _ := os.ReadFile(events[0])
	if !strings.Contains(string(data), "Code=AlarmLocal;action=Start;index=0\r\n") {
		t.Errorf("event recording is not the raw stream: %q", data)
	}
	if got := out.String(); !strings.Contains(got, "event codes seen: VideoMotion ×2, AlarmLocal ×1") || !strings.Contains(got, "Review the files before sharing") {
		t.Errorf("summary missing code counts or the privacy note:\n%s", got)
	}
}

func TestRunCaptureSkipsSnapshotsByDefault(t *testing.T) {
	snapRequested := false
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "snapManager") {
			snapRequested = true
		}
		<-r.Context().Done()
	}))
	defer cam.Close()

	var out bytes.Buffer
	d := &config.Dahua{Scheme: "http", Host: strings.TrimPrefix(cam.URL, "http://"), Username: "admin", Password: "secret"}
	runCapture(d, 100*time.Millisecond, t.TempDir(), false, &out)
	if snapRequested {
		t.Error("the snapshot stream (pictures of the doorway) was recorded without -capture-snapshots")
	}
}

func TestRunCaptureReportsRefusal(t *testing.T) {
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Error\r\nBad Request!", http.StatusBadRequest)
	}))
	defer cam.Close()

	var out bytes.Buffer
	d := &config.Dahua{Scheme: "http", Host: strings.TrimPrefix(cam.URL, "http://"), Username: "admin", Password: "secret"}
	if code := runCapture(d, time.Second, t.TempDir(), false, &out); code != 1 {
		t.Errorf("runCapture() = %d, want 1 for a refused stream", code)
	}
	if !strings.Contains(out.String(), "status 400") || !strings.Contains(out.String(), "Bad Request!") {
		t.Errorf("output should name the status and the camera's reply:\n%s", out.String())
	}
}
