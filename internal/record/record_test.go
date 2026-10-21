package record_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ericfitz/agentwatch/internal/record"
)

var t0 = time.Date(2026, 10, 21, 5, 3, 0, 0, time.UTC)

var header = record.Header{
	Width:   100,
	Height:  30,
	Started: t0,
	Title:   "api-agent",
	Command: "claude --resume",
	Env:     map[string]string{"TERM": "xterm-256color", "SHELL": "/bin/bash"},
}

var errSink = errors.New("disk full")

type memSink struct {
	lines    []string
	attempts int
	failFrom int // 1-based Write attempt that starts failing; 0 never fails
	closed   bool
	closeErr error
}

func (s *memSink) Write(p []byte) (int, error) {
	s.attempts++
	if s.failFrom > 0 && s.attempts >= s.failFrom {
		return 0, errSink
	}
	s.lines = append(s.lines, string(p))
	return len(p), nil
}

func (s *memSink) Close() error {
	s.closed = true
	return s.closeErr
}

type event struct {
	Seconds float64
	Code    string
	Data    string
}

func at(d time.Duration) time.Time { return t0.Add(d) }

// events parses every line after the header as an asciicast v2 event, and
// checks that each Write carried exactly one newline-terminated line.
func events(t *testing.T, s *memSink) []event {
	t.Helper()
	var out []event
	for i, line := range s.lines {
		if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
			t.Fatalf("write %d is not one line: %q", i, line)
		}
		if i == 0 {
			continue
		}
		var raw []json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil || len(raw) != 3 {
			t.Fatalf("event %q is not a 3-element JSON array: %v", line, err)
		}
		var e event
		if json.Unmarshal(raw[0], &e.Seconds) != nil || json.Unmarshal(raw[1], &e.Code) != nil || json.Unmarshal(raw[2], &e.Data) != nil {
			t.Fatalf("event %q is not [float, string, string]", line)
		}
		out = append(out, e)
	}
	return out
}

func newRecorder(t *testing.T, s *memSink) *record.Recorder {
	t.Helper()
	r, err := record.New(s, header)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHeaderLine(t *testing.T) {
	s := &memSink{}
	newRecorder(t, s)
	var h struct {
		Version   int               `json:"version"`
		Width     int               `json:"width"`
		Height    int               `json:"height"`
		Timestamp int64             `json:"timestamp"`
		Title     string            `json:"title"`
		Command   string            `json:"command"`
		Env       map[string]string `json:"env"`
	}
	if len(s.lines) != 1 {
		t.Fatalf("New wrote %d lines, want the header only", len(s.lines))
	}
	if err := json.Unmarshal([]byte(s.lines[0]), &h); err != nil {
		t.Fatal(err)
	}
	if h.Version != 2 || h.Width != 100 || h.Height != 30 || h.Timestamp != t0.Unix() {
		t.Errorf("header = %+v", h)
	}
	if h.Title != "api-agent" || h.Command != "claude --resume" || !reflect.DeepEqual(h.Env, header.Env) {
		t.Errorf("header metadata = %+v", h)
	}
}

func TestEvents(t *testing.T) {
	s := &memSink{}
	r := newRecorder(t, s)
	r.Output(at(250*time.Millisecond), []byte("hi <b>&"))
	r.Input(at(500*time.Millisecond), []byte("x\r"))
	r.Resize(at(time.Second), 100, 30)
	r.Marker(at(1500*time.Millisecond), "relay connected")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	want := []event{
		{0.25, "o", "hi <b>&"},
		{0.5, "i", "x\r"},
		{1, "r", "100x30"},
		{1.5, "m", "relay connected"},
	}
	if got := events(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("events\n got %v\nwant %v", got, want)
	}
	if !s.closed {
		t.Error("Close did not close the sink")
	}
}

func TestRuneSplitAcrossWrites(t *testing.T) {
	s := &memSink{}
	r := newRecorder(t, s)
	r.Output(at(time.Second), []byte("a\xe2\x82"))
	r.Input(at(1500*time.Millisecond), []byte("k"))
	r.Output(at(2*time.Second), []byte("\xac b"))
	r.Close()
	want := []event{{1, "o", "a"}, {1.5, "i", "k"}, {2, "o", "€ b"}}
	if got := events(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("events\n got %v\nwant %v", got, want)
	}
}

func TestInvalidBytesBecomeReplacementChar(t *testing.T) {
	s := &memSink{}
	r := newRecorder(t, s)
	r.Output(at(time.Second), []byte("x\xffy"))
	r.Output(at(2*time.Second), []byte("z\xf0\x9f"))
	r.Close()
	got := events(t, s)
	if len(got) != 3 {
		t.Fatalf("events = %v, want 3", got)
	}
	if got[0] != (event{1, "o", "x\uFFFDy"}) || got[1] != (event{2, "o", "z"}) {
		t.Errorf("events = %v", got)
	}
	tail := got[2]
	if tail.Seconds != 2 || tail.Code != "o" || tail.Data == "" || strings.Trim(tail.Data, "\uFFFD") != "" {
		t.Errorf("held-back bytes flushed at Close as %v, want U+FFFD at 2s", tail)
	}
}

func TestOutOfOrderTimesAreClamped(t *testing.T) {
	s := &memSink{}
	r := newRecorder(t, s)
	r.Output(at(-time.Second), []byte("before start"))
	r.Marker(at(2*time.Second), "late")
	r.Resize(at(time.Second), 80, 24)
	r.Close()
	want := []event{{0, "o", "before start"}, {2, "m", "late"}, {2, "r", "80x24"}}
	if got := events(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("events\n got %v\nwant %v", got, want)
	}
}

func TestNilRecorderRecordsNothing(t *testing.T) {
	r, path, err := record.Open("", header)
	if r != nil || path != "" || err != nil {
		t.Fatalf("Open(\"\") = %v, %q, %v; want nil recorder", r, path, err)
	}
	r.Output(t0, []byte("x"))
	r.Input(t0, []byte("x"))
	r.Resize(t0, 1, 1)
	r.Marker(t0, "x")
	if err := r.Close(); err != nil {
		t.Fatalf("nil Close = %v", err)
	}
}

func TestOpenDirectoryNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	taken := filepath.Join(dir, "20261021T050300Z-api-agent.cast")
	if err := os.WriteFile(taken, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	var paths []string
	for _, target := range []string{dir, dir + string(filepath.Separator)} {
		r, path, err := record.Open(target, header)
		if err != nil {
			t.Fatal(err)
		}
		r.Marker(at(time.Second), "hello")
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	want := []string{
		filepath.Join(dir, "20261021T050300Z-api-agent-1.cast"),
		filepath.Join(dir, "20261021T050300Z-api-agent-2.cast"),
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	if b, _ := os.ReadFile(taken); string(b) != "keep" {
		t.Errorf("existing recording overwritten: %q", b)
	}
	for _, p := range paths {
		if got := events(t, fileSink(t, p)); !reflect.DeepEqual(got, []event{{1, "m", "hello"}}) {
			t.Errorf("%s events = %v", p, got)
		}
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, %v; want 0600", p, fi.Mode(), err)
		}
	}
}

func TestOpenSanitizesTitleAndCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "sessions") + string(filepath.Separator)
	h := header
	h.Title = "my agent/../x"
	r, path, err := record.Open(dir, h)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if want := filepath.Join(dir, "20261021T050300Z-my_agent_.._x.cast"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("created dir mode = %v, %v; want 0700", fi.Mode(), err)
	}
}

func TestOpenFilePathNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "run.cast")
	r, got, err := record.Open(path, header)
	if err != nil || got != path {
		t.Fatalf("Open(%q) = %q, %v", path, got, err)
	}
	r.Close()
	if _, _, err := record.Open(path, header); err == nil {
		t.Fatal("second Open of the same file path succeeded")
	}
	if got := fileSink(t, path).lines; len(got) != 1 {
		t.Errorf("file rewritten: %q", got)
	}
}

// fileSink reads a recording back as the lines a memSink would have seen.
func fileSink(t *testing.T, path string) *memSink {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &memSink{}
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if line != "" {
			s.lines = append(s.lines, line)
		}
	}
	var h struct{ Version int }
	if len(s.lines) == 0 || json.Unmarshal([]byte(s.lines[0]), &h) != nil || h.Version != 2 {
		t.Fatalf("%s does not start with an asciicast v2 header: %q", path, b)
	}
	return s
}

func TestFailingSinkSurfacesFromClose(t *testing.T) {
	s := &memSink{failFrom: 2}
	r := newRecorder(t, s)
	r.Output(at(time.Second), []byte("lost"))
	r.Marker(at(2*time.Second), "also lost")
	if err := r.Close(); !errors.Is(err, errSink) {
		t.Fatalf("Close = %v, want %v", err, errSink)
	}
	if s.attempts != 2 {
		t.Errorf("sink saw %d writes; want none after the first failure", s.attempts)
	}

	closing := &memSink{closeErr: errSink}
	if err := newRecorder(t, closing).Close(); !errors.Is(err, errSink) {
		t.Errorf("Close = %v, want the sink's Close error", err)
	}

	if _, err := record.New(&memSink{failFrom: 1}, header); !errors.Is(err, errSink) {
		t.Errorf("New with a failing sink = %v, want %v", err, errSink)
	}
}
