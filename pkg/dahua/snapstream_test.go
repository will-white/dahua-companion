package dahua

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseSnapshotEvent(t *testing.T) {
	testCases := []struct {
		name string
		text string
		want Event
		ok   bool
	}{
		{
			name: "press",
			text: "Events[0].Channel=0\nEvents[0].EventBaseInfo.Code=AlarmLocal\nEvents[0].EventBaseInfo.Action=Start\nEvents[0].EventBaseInfo.Index=0\n",
			want: Event{Kind: DoorbellPressed},
			ok:   true,
		},
		{
			name: "smart motion human, pulse, CRLF, channel from Channel",
			text: "Events[0].Channel=1\r\nEvents[0].EventBaseInfo.Code=SmartMotionHuman\r\nEvents[0].EventBaseInfo.Action=Pulse\r\n",
			want: Event{Kind: HumanDetected, Channel: 1},
			ok:   true,
		},
		{
			name: "intrusion naming a human",
			text: "Events[0].EventBaseInfo.Code=CrossRegionDetection\nEvents[0].EventBaseInfo.Action=Start\nEvents[0].Object.ObjectType=Human\n",
			want: Event{Kind: HumanDetected},
			ok:   true,
		},
		{
			name: "intrusion naming a vehicle",
			text: "Events[0].EventBaseInfo.Code=CrossRegionDetection\nEvents[0].EventBaseInfo.Action=Start\nEvents[0].Object.ObjectType=Vehicle\n",
		},
		{
			name: "stop",
			text: "Events[0].EventBaseInfo.Code=AlarmLocal\nEvents[0].EventBaseInfo.Action=Stop\n",
		},
		{
			name: "other code",
			text: "Events[0].EventBaseInfo.Code=VideoMotion\nEvents[0].EventBaseInfo.Action=Start\n",
		},
		{
			name: "only the first event counts",
			text: "Events[0].EventBaseInfo.Code=VideoMotion\nEvents[1].EventBaseInfo.Code=AlarmLocal\n",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSnapshotEvent(tc.text)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("parseSnapshotEvent() = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// snapPart renders one multipart part the way the camera frames it.
func snapPart(contentType, body string) string {
	return fmt.Sprintf("--myboundary\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s\r\n", contentType, len(body), body)
}

func TestListenSnapshots(t *testing.T) {
	var query string
	stream := snapPart("text/plain", "Heartbeat") +
		// A press and its image, with a heartbeat in between.
		snapPart("text/plain", "Events[0].EventBaseInfo.Code=AlarmLocal\nEvents[0].EventBaseInfo.Action=Start\nEvents[0].EventBaseInfo.Index=0\n") +
		snapPart("text/plain", "heartbeat") +
		snapPart("image/jpeg", "press-jpeg") +
		// A Stop's image is discarded.
		snapPart("text/plain", "Events[0].EventBaseInfo.Code=AlarmLocal\nEvents[0].EventBaseInfo.Action=Stop\n") +
		snapPart("image/jpeg", "stop-jpeg") +
		// Two human detections in a row: the second falls in the cooldown.
		snapPart("text/plain", "Events[0].EventBaseInfo.Code=SmartMotionHuman\nEvents[0].EventBaseInfo.Action=Start\n") +
		snapPart("image/jpeg", "human-jpeg") +
		snapPart("text/plain", "Events[0].EventBaseInfo.Code=SmartMotionHuman\nEvents[0].EventBaseInfo.Action=Start\n") +
		snapPart("image/jpeg", "human-jpeg-2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
		fmt.Fprint(w, stream)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.HumanCooldown = time.Minute
	type got struct {
		ev  Event
		img string
	}
	snaps := make(chan got, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.ListenSnapshots(ctx, []EventKind{DoorbellPressed, HumanDetected}, func(ev Event, img []byte) {
			snaps <- got{ev, string(img)}
		})
		close(done)
	}()

	want := []got{{Event{Kind: DoorbellPressed}, "press-jpeg"}, {Event{Kind: HumanDetected}, "human-jpeg"}}
	for _, w := range want {
		select {
		case g := <-snaps:
			if g != w {
				t.Errorf("snapshot = %+v, want %+v", g, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("snapshot %+v never arrived", w)
		}
	}
	select {
	case g := <-snaps:
		t.Errorf("unexpected snapshot %+v (Stop image or cooldown not applied)", g)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	<-done

	if !strings.Contains(query, "action=attachFileProc") || !strings.Contains(query, "Events=[AlarmLocal%2CSmartMotionHuman%2CCrossLineDetection%2CCrossRegionDetection]") {
		t.Errorf("snapshot stream query = %q, want the event codes subscribed", query)
	}
	if c.IsConnected() {
		t.Error("the snapshot stream must not mark the event stream connected")
	}
}

func TestReadSnapshotPartsNeedsBoundary(t *testing.T) {
	if err := readSnapshotParts(strings.NewReader(""), "multipart/x-mixed-replace", func(Event, []byte) {}); err == nil {
		t.Error("readSnapshotParts accepted a content type without a boundary")
	}
}
