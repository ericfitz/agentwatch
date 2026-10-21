// Package record writes asciicast v2.
package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Sink receives the encoded stream. The Recorder calls Write exactly once per
// complete line (the header or one event, '\n'-terminated), so a line-oriented
// sink can ship each call as one record and a truncated file still parses.
// *os.File satisfies Sink.
type Sink interface {
	Write(line []byte) (int, error)
	Close() error
}

type Header struct {
	Width, Height int
	Started       time.Time         // header "timestamp"; event times are relative to it
	Title         string            // session name
	Command       string            // argv joined for display
	Env           map[string]string // TERM and SHELL only
}

// stream is a text event stream whose writes may split a UTF-8 sequence.
type stream int

const (
	output stream = iota
	input
)

var streamCode = [...]string{output: "o", input: "i"}

// Recorder is owned by one goroutine. A nil *Recorder is valid and records
// nothing, so callers never branch on "recording off".
type Recorder struct {
	sink    Sink
	start   time.Time
	last    time.Duration // event times never decrease
	pending [2][]byte     // incomplete UTF-8 tail per stream
	err     error         // first sink error; later writes do nothing
}

const maxNumbered = 1000

// Open resolves target to a Sink and writes the header. target "" returns a nil
// Recorder. v1 knows one kind of target, the file system. If target is an
// existing directory or ends in a separator, the file is
// <target>/<Started UTC 20060102T150405Z>-<safe Title>.cast, with -1, -2...
// on collision. Otherwise target is the file path. Files are created
// O_CREATE|O_EXCL with mode 0600, parents 0700. It returns the resolved path.
func Open(target string, h Header) (*Recorder, string, error) {
	if target == "" {
		return nil, "", nil
	}
	f, path, err := create(target, h)
	if err != nil {
		return nil, "", fmt.Errorf("record: %w", err)
	}
	r, err := New(f, h)
	if err != nil {
		f.Close()
		os.Remove(path)
		return nil, "", fmt.Errorf("record: %w", err)
	}
	return r, path, nil
}

func create(target string, h Header) (*os.File, string, error) {
	if !isDir(target) {
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, "", err
		}
		f, err := createNew(target)
		return f, target, err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return nil, "", err
	}
	base := h.Started.UTC().Format("20060102T150405Z")
	if t := safeTitle(h.Title); t != "" {
		base += "-" + t
	}
	for i := 0; i < maxNumbered; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		path := filepath.Join(target, name+".cast")
		f, err := createNew(path)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, path, err
	}
	return nil, "", fmt.Errorf("%s and %d numbered variants exist", base, maxNumbered-1)
}

func isDir(target string) bool {
	if strings.HasSuffix(target, string(filepath.Separator)) {
		return true
	}
	fi, err := os.Stat(target)
	return err == nil && fi.IsDir()
}

func createNew(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func safeTitle(title string) string {
	t := strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, title)
	if len(t) > 64 {
		t = t[:64]
	}
	return t
}

// New writes the header to sink. Tests and future sinks use it directly.
func New(sink Sink, h Header) (*Recorder, error) {
	r := &Recorder{sink: sink, start: h.Started}
	line := appendHeader(nil, h)
	r.write(append(line, '\n'))
	if r.err != nil {
		return nil, r.err
	}
	return r, nil
}

func (r *Recorder) Output(at time.Time, p []byte) { r.text(at, output, p) } // "o"
func (r *Recorder) Input(at time.Time, p []byte)  { r.text(at, input, p) }  // "i"

// Resize writes an "r" event, "100x30".
func (r *Recorder) Resize(at time.Time, cols, rows int) {
	if r != nil {
		r.event(at, "r", fmt.Sprintf("%dx%d", cols, rows))
	}
}

// Marker writes an "m" event.
func (r *Recorder) Marker(at time.Time, label string) {
	if r != nil {
		r.event(at, "m", label)
	}
}

// Close writes any held-back bytes (as U+FFFD), closes the sink, and returns
// the first error seen during the whole recording.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	for s, p := range r.pending {
		if len(p) > 0 {
			r.event(r.start.Add(r.last), streamCode[s], string(p))
			r.pending[s] = nil
		}
	}
	if err := r.sink.Close(); r.err == nil {
		r.err = err
	}
	return r.err
}

func (r *Recorder) text(at time.Time, s stream, p []byte) {
	if r == nil {
		return
	}
	head, tail := completeUTF8(r.pending[s], p)
	r.pending[s] = append([]byte(nil), tail...)
	if len(head) > 0 {
		r.event(at, streamCode[s], string(head))
	}
}

// completeUTF8 joins carry and p and splits off a trailing partial rune.
// Invalid bytes stay in head; encoding/json turns them into U+FFFD.
func completeUTF8(carry, p []byte) (head, tail []byte) {
	b := p
	if len(carry) > 0 {
		b = append(carry, p...)
	}
	for i := len(b) - 1; i >= 0 && i > len(b)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if !utf8.FullRune(b[i:]) {
			return b[:i], b[i:]
		}
		break
	}
	return b, nil
}

// event writes [<seconds, 6 decimals>, "<code>", <JSON string>].
func (r *Recorder) event(at time.Time, code, data string) {
	d := at.Sub(r.start)
	if d < r.last {
		d = r.last
	}
	r.last = d
	us := d.Microseconds()
	line := fmt.Appendf(nil, "[%d.%06d, %q, ", us/1e6, us%1e6, code)
	line = appendString(line, data)
	r.write(append(line, "]\n"...))
}

func (r *Recorder) write(line []byte) {
	if r.err != nil {
		return
	}
	if _, err := r.sink.Write(line); err != nil {
		r.err = err
	}
}

func appendHeader(dst []byte, h Header) []byte {
	return encodeLine(dst, struct {
		Version   int               `json:"version"`
		Width     int               `json:"width"`
		Height    int               `json:"height"`
		Timestamp int64             `json:"timestamp"`
		Title     string            `json:"title,omitempty"`
		Command   string            `json:"command,omitempty"`
		Env       map[string]string `json:"env,omitempty"`
	}{2, h.Width, h.Height, h.Started.Unix(), h.Title, h.Command, h.Env})
}

func appendString(dst []byte, s string) []byte {
	return encodeLine(dst, s)
}

func encodeLine(dst []byte, v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("record: " + err.Error())
	}
	return append(dst, bytes.TrimSuffix(b.Bytes(), []byte{'\n'})...)
}
