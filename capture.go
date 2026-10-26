package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"dahua_companion/pkg/config"
	"dahua_companion/pkg/dahua"
)

// codePattern finds event codes in both stream formats: "Code=AlarmLocal;" on
// the event stream and "Events[0].EventBaseInfo.Code=AlarmLocal" on the
// snapshot stream.
var codePattern = regexp.MustCompile(`Code=([A-Za-z0-9_]+)`)

// captureResult is one recorded stream.
type captureResult struct {
	name, path string
	bytes      int64
	err        error
}

// runCapture records the camera's raw streams for d to files in dir for the
// given duration (Ctrl+C ends it early), then prints where they went and which
// event codes showed up. It needs only the camera settings. The snapshot
// stream holds pictures of the doorway, so it is recorded only on request.
// It returns the process exit code.
func runCapture(d *config.Dahua, duration time.Duration, dir string, snapshots bool, out io.Writer) int {
	client, err := dahua.New(d)
	if err != nil {
		fmt.Fprintln(out, "capture:", err)
		return 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(out, "capture:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	stamp := time.Now().UTC().Format("20060102T150405Z")
	streams := []struct {
		name, file string
		run        func(context.Context, io.Writer) (int64, error)
	}{
		{"event stream", "capture-" + stamp + "-events.txt", client.CaptureEvents},
	}
	if snapshots {
		streams = append(streams, struct {
			name, file string
			run        func(context.Context, io.Writer) (int64, error)
		}{"snapshot stream", "capture-" + stamp + "-snapshots.multipart", client.CaptureSnapshots})
	}

	fmt.Fprintf(out, "Recording the camera at %s for %s (Ctrl+C to stop early). Ring the doorbell and walk past it now.\n", d.Host, duration)
	results := make([]captureResult, len(streams))
	var wg sync.WaitGroup
	for i, s := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := filepath.Join(dir, s.file)
			r := captureResult{name: s.name, path: path}
			f, err := os.Create(path)
			if err != nil {
				r.err = err
				results[i] = r
				return
			}
			r.bytes, r.err = s.run(ctx, f)
			if err := f.Close(); err != nil && r.err == nil {
				r.err = err
			}
			results[i] = r
		}()
	}
	wg.Wait()

	code := 0
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(out, "\n%s: FAILED: %v\n", r.name, r.err)
			code = 1
			continue
		}
		fmt.Fprintf(out, "\n%s: %d bytes -> %s\n", r.name, r.bytes, r.path)
		data, err := os.ReadFile(r.path)
		if err != nil {
			continue
		}
		fmt.Fprintf(out, "  event codes seen: %s\n", summarizeCodes(data))
	}
	fmt.Fprintln(out, "\nReview the files before sharing: they hold your camera's event data, and a snapshot recording holds pictures of your doorway.")
	return code
}

// summarizeCodes lists the event codes in a recording with their counts,
// most frequent first.
func summarizeCodes(data []byte) string {
	counts := map[string]int{}
	for _, m := range codePattern.FindAllSubmatch(data, -1) {
		counts[string(m[1])]++
	}
	if len(counts) == 0 {
		return "none (only heartbeats?)"
	}
	codes := make([]string, 0, len(counts))
	for c := range counts {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool {
		if counts[codes[i]] != counts[codes[j]] {
			return counts[codes[i]] > counts[codes[j]]
		}
		return codes[i] < codes[j]
	})
	parts := make([]string, len(codes))
	for i, c := range codes {
		parts[i] = fmt.Sprintf("%s ×%d", c, counts[c])
	}
	return strings.Join(parts, ", ")
}
