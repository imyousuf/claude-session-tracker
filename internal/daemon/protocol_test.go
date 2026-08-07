package daemon

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestEventRoundTrip(t *testing.T) {
	cases := []Event{
		{Type: EventSnapshotRequest},
		{Type: EventPreexec, MuxSocket: "/run/mux", PaneID: 5, Command: "tomoe start", CWD: "/tmp", Timestamp: 1000},
		{Type: EventPrecmd, MuxSocket: "/run/mux", PaneID: 5, CWD: "/tmp/sub", Timestamp: 2000},
		{Type: EventSessionStart, MuxSocket: "/run/mux", PaneID: 7, Provider: "codex", SessionID: "thr_abc", PID: 12345, CWD: "/tmp/proj"},
		{Type: EventSessionEnd, MuxSocket: "/run/mux", PaneID: 7, Provider: "codex", SessionID: "thr_abc"},
	}
	for _, ev := range cases {
		t.Run(string(ev.Type), func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteEvent(&buf, ev); err != nil {
				t.Fatalf("WriteEvent: %v", err)
			}
			next := ReadEvents(&buf)
			got, err := next()
			if err != nil {
				t.Fatalf("ReadEvents: %v", err)
			}
			if got != ev {
				t.Errorf("round-trip mismatch:\ngot:  %+v\nwant: %+v", got, ev)
			}
			// Second call should return EOF.
			_, err = next()
			if err != io.EOF {
				t.Errorf("expected EOF, got %v", err)
			}
		})
	}
}

func TestReadEventsHandlesMultipleEvents(t *testing.T) {
	stream := `{"type":"snapshot_request"}
{"type":"preexec","mux_socket":"/run/mux","pane_id":3,"command":"ls"}
{"type":"precmd","mux_socket":"/run/mux","pane_id":3}
`
	next := ReadEvents(strings.NewReader(stream))
	var got []Event
	for {
		ev, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].Type != EventSnapshotRequest {
		t.Errorf("first event type = %s", got[0].Type)
	}
	if got[1].Command != "ls" || got[1].PaneID != 3 {
		t.Errorf("second event = %+v", got[1])
	}
}

func TestReadEventsMalformedReturnsError(t *testing.T) {
	next := ReadEvents(strings.NewReader("not-json\n"))
	_, err := next()
	if err == nil {
		t.Fatal("expected parse error")
	}
}
