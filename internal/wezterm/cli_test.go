package wezterm

import (
	"fmt"
	"reflect"
	"testing"
)

// sampleListJSON is a minimal, generic `wezterm cli list --format json` payload
// used by the tests below. Constructed inline (no testdata file on disk) so
// tests have no dependency on the developer's machine or filesystem layout.
//
// Shape mirrors real wezterm output: 4 panes across 2 windows. Window 0 has
// one tab (1 pane). Window 0 also has tab 6 with two panes (a split, different
// left_col). Window 1 has one pane.
const sampleListJSON = `[
  {
    "window_id": 0, "tab_id": 1, "pane_id": 1, "workspace": "default",
    "size": {"rows": 59, "cols": 254, "pixel_width": 2540, "pixel_height": 1298, "dpi": 96},
    "title": "user@host: /home/user/projects/foo",
    "cwd": "file://host/home/user/projects/foo/",
    "cursor_x": 80, "cursor_y": 13, "cursor_shape": "Default", "cursor_visibility": "Visible",
    "left_col": 0, "top_row": 0, "tab_title": "",
    "window_title": "user@host: /home/user/projects/foo",
    "is_active": true, "is_zoomed": false, "tty_name": "/dev/pts/15"
  },
  {
    "window_id": 0, "tab_id": 6, "pane_id": 12, "workspace": "default",
    "size": {"rows": 59, "cols": 127, "pixel_width": 1270, "pixel_height": 1298, "dpi": 96},
    "title": "claude",
    "cwd": "file://host/home/user/projects/bar/",
    "cursor_x": 2, "cursor_y": 31, "cursor_shape": "Default", "cursor_visibility": "Hidden",
    "left_col": 0, "top_row": 0, "tab_title": "",
    "window_title": "user@host: /home/user/projects/bar",
    "is_active": true, "is_zoomed": false, "tty_name": "/dev/pts/14"
  },
  {
    "window_id": 0, "tab_id": 6, "pane_id": 13, "workspace": "default",
    "size": {"rows": 59, "cols": 126, "pixel_width": 1260, "pixel_height": 1298, "dpi": 96},
    "title": "user@host: /home/user/projects/bar",
    "cwd": "file://host/home/user/projects/bar/",
    "cursor_x": 71, "cursor_y": 0, "cursor_shape": "Default", "cursor_visibility": "Visible",
    "left_col": 128, "top_row": 0, "tab_title": "",
    "window_title": "user@host: /home/user/projects/bar",
    "is_active": false, "is_zoomed": false, "tty_name": "/dev/pts/19"
  },
  {
    "window_id": 1, "tab_id": 8, "pane_id": 20, "workspace": "default",
    "size": {"rows": 50, "cols": 200, "pixel_width": 2000, "pixel_height": 1000, "dpi": 96},
    "title": "tomoe",
    "cwd": "file://host/home/user/audio/",
    "cursor_x": 0, "cursor_y": 0, "cursor_shape": "Default", "cursor_visibility": "Visible",
    "left_col": 0, "top_row": 0, "tab_title": "",
    "window_title": "tomoe daemon",
    "is_active": true, "is_zoomed": false, "tty_name": "/dev/pts/22"
  }
]`

// SampleListJSON exposes the inline test payload to other packages' tests
// (e.g. internal/snapshot) so they don't need to re-define it or read from disk.
func SampleListJSON() []byte { return []byte(sampleListJSON) }

// fakeRunner records args and returns canned output.
type fakeRunner struct {
	calls       [][]string
	stdinByCall []string          // stdin passed to each call ("" for plain Run)
	response    map[string][]byte // keyed by joined args; default if unmatched
	err         error
}

func (f *fakeRunner) Run(args ...string) ([]byte, error) {
	return f.record("", args)
}

func (f *fakeRunner) RunStdin(stdin string, args ...string) ([]byte, error) {
	return f.record(stdin, args)
}

func (f *fakeRunner) record(stdin string, args []string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	f.stdinByCall = append(f.stdinByCall, stdin)
	if f.err != nil {
		return nil, f.err
	}
	key := joinKey(args)
	if out, ok := f.response[key]; ok {
		return out, nil
	}
	// Default response for matchers that don't care.
	return f.response[""], nil
}

func joinKey(args []string) string {
	s := ""
	for i, a := range args {
		if i > 0 {
			s += " "
		}
		s += a
	}
	return s
}

func TestParseListInlineSample(t *testing.T) {
	panes, err := ParseList([]byte(sampleListJSON))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if len(panes) != 4 {
		t.Fatalf("expected 4 panes, got %d", len(panes))
	}

	// First pane: window 0, tab 1, pane 1.
	p := panes[0]
	if p.WindowID != 0 || p.TabID != 1 || p.PaneID != 1 {
		t.Fatalf("first pane ids = (%d, %d, %d)", p.WindowID, p.TabID, p.PaneID)
	}
	if p.Workspace != "default" {
		t.Fatalf("workspace = %q", p.Workspace)
	}
	if p.Size.Cols != 254 || p.Size.Rows != 59 {
		t.Fatalf("size = %+v", p.Size)
	}
	if p.TTYName != "/dev/pts/15" {
		t.Fatalf("tty = %q", p.TTYName)
	}
	if !p.IsActive {
		t.Fatal("first pane should be active")
	}

	// Third pane is split off the second (same tab, different left_col).
	p2, p3 := panes[1], panes[2]
	if p2.TabID != p3.TabID || p2.TabID != 6 {
		t.Fatalf("panes 12/13 should share tab 6: %d/%d", p2.TabID, p3.TabID)
	}
	if p3.LeftCol == p2.LeftCol {
		t.Fatal("split panes should have different left_col")
	}

	// Last pane in a different window.
	if panes[3].WindowID != 1 {
		t.Fatalf("fourth pane window = %d", panes[3].WindowID)
	}
}

func TestParseCWD(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"file://host/home/u/proj/", "/home/u/proj"},
		{"file://host/home/u/proj", "/home/u/proj"},
		{"file://host/", "/"},
		{"/already/local", "/already/local"},
		{"", ""},
	}
	for _, c := range cases {
		got := ParseCWD(c.in)
		if got != c.want {
			t.Errorf("ParseCWD(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestListInjectedRunner(t *testing.T) {
	data := []byte(sampleListJSON)
	f := &fakeRunner{response: map[string][]byte{"list --format json": data}}
	restore := SetRunner(f)
	defer restore()

	raw, panes, err := ListWithRaw()
	if err != nil {
		t.Fatalf("ListWithRaw: %v", err)
	}
	if !reflect.DeepEqual(raw, data) {
		t.Fatal("raw bytes mismatch — should be exactly what runner returned")
	}
	if len(panes) != 4 {
		t.Fatalf("len = %d", len(panes))
	}
	if len(f.calls) != 1 || f.calls[0][0] != "list" {
		t.Fatalf("unexpected calls: %+v", f.calls)
	}
}

func TestListPropagatesError(t *testing.T) {
	f := &fakeRunner{err: fmt.Errorf("boom")}
	restore := SetRunner(f)
	defer restore()

	_, err := List()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSpawnArgsBuilding(t *testing.T) {
	cases := []struct {
		name     string
		args     SpawnArgs
		wantArgs []string
		response string
		wantID   int64
	}{
		{
			name:     "default shell, new window, cwd",
			args:     SpawnArgs{CWD: "/tmp/x", NewWindow: true},
			wantArgs: []string{"spawn", "--new-window", "--cwd", "/tmp/x"},
			response: "42\n",
			wantID:   42,
		},
		{
			name:     "new tab in existing window with command",
			args:     SpawnArgs{WindowID: 3, CWD: "/tmp/y", Command: []string{"claude", "--resume", "abc"}},
			wantArgs: []string{"spawn", "--window-id", "3", "--cwd", "/tmp/y", "--", "claude", "--resume", "abc"},
			response: "99",
			wantID:   99,
		},
		{
			name:     "workspace specified",
			args:     SpawnArgs{Workspace: "dev", NewWindow: true},
			wantArgs: []string{"spawn", "--new-window", "--workspace", "dev"},
			response: "7\n",
			wantID:   7,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{response: map[string][]byte{"": []byte(c.response)}}
			restore := SetRunner(f)
			defer restore()

			id, err := Spawn(c.args)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if id != c.wantID {
				t.Errorf("id = %d, want %d", id, c.wantID)
			}
			if !reflect.DeepEqual(f.calls[0], c.wantArgs) {
				t.Errorf("args = %v, want %v", f.calls[0], c.wantArgs)
			}
		})
	}
}

func TestSplitPaneArgsBuilding(t *testing.T) {
	f := &fakeRunner{response: map[string][]byte{"": []byte("55\n")}}
	restore := SetRunner(f)
	defer restore()

	id, err := SplitPane(SplitArgs{
		PaneID:    10,
		Direction: SplitRight,
		CWD:       "/tmp/proj",
		Command:   []string{"tomoe", "start"},
	})
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if id != 55 {
		t.Errorf("id = %d", id)
	}
	want := []string{"split-pane", "--pane-id", "10", "--right", "--cwd", "/tmp/proj", "--", "tomoe", "start"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("args = %v, want %v", f.calls[0], want)
	}
}

func TestSplitPaneInvalidDirection(t *testing.T) {
	_, err := SplitPane(SplitArgs{PaneID: 1, Direction: "diagonal"})
	if err == nil {
		t.Fatal("expected error for invalid direction")
	}
}

func TestSendText(t *testing.T) {
	f := &fakeRunner{response: map[string][]byte{"": nil}}
	restore := SetRunner(f)
	defer restore()

	if err := SendText(SendTextArgs{PaneID: 7, Text: "echo hi\r", NoPaste: true}); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	want := []string{"send-text", "--pane-id", "7", "--no-paste"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("args = %v, want %v", f.calls[0], want)
	}
	// Text must be delivered via stdin, not as an argument.
	if f.stdinByCall[0] != "echo hi\r" {
		t.Errorf("stdin = %q, want %q", f.stdinByCall[0], "echo hi\r")
	}
}

func TestSendTextWithoutNoPaste(t *testing.T) {
	f := &fakeRunner{response: map[string][]byte{"": nil}}
	restore := SetRunner(f)
	defer restore()

	if err := SendText(SendTextArgs{PaneID: 3, Text: "ls\r"}); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	want := []string{"send-text", "--pane-id", "3"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("args = %v, want %v", f.calls[0], want)
	}
}

func TestSendTextError(t *testing.T) {
	f := &fakeRunner{err: fmt.Errorf("boom")}
	restore := SetRunner(f)
	defer restore()

	if err := SendText(SendTextArgs{PaneID: 1, Text: "x\r", NoPaste: true}); err == nil {
		t.Fatal("expected error from SendText")
	}
}

func TestSocketHelpers(t *testing.T) {
	t.Setenv("WEZTERM_UNIX_SOCKET", "/run/wezterm/mux")
	t.Setenv("WEZTERM_PANE", "42")

	if got := MuxSocket(); got != "/run/wezterm/mux" {
		t.Errorf("MuxSocket = %q", got)
	}
	if got := PaneID(); got != 42 {
		t.Errorf("PaneID = %d", got)
	}
	if !InWezterm() {
		t.Error("InWezterm should be true")
	}

	t.Setenv("WEZTERM_PANE", "")
	if InWezterm() {
		t.Error("InWezterm should be false without WEZTERM_PANE")
	}
}
