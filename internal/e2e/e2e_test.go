// Package e2e_test runs a real shell in a PTY through wrapper.Run, a relay on a
// TLS server, and a Go viewer that speaks the browser's protocol.
package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ericfitz/agentwatch/internal/relay"
	"github.com/ericfitz/agentwatch/internal/wire"
	"github.com/ericfitz/agentwatch/internal/wrapper"
)

const (
	timeout = 15 * time.Second
	prompt  = "e2e$ "
)

type result struct {
	code int
	err  error
}

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	relay   atomic.Pointer[relay.Relay]
	wrapTok wire.Token
	viewTok wire.Token
	cast    string
	done    chan result
	logs    lockedBuffer
}

// start serves a relay over TLS and wraps /bin/sh with it. The handler reads
// the relay through a pointer so a test can restart the relay at the same URL.
func start(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, wrapTok: wire.NewToken(), viewTok: wire.NewToken(), done: make(chan result, 1)}
	h.relay.Store(h.newRelay())
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.relay.Load().ServeHTTP(w, r)
	}))
	h.srv.StartTLS()
	t.Cleanup(func() {
		h.relay.Load().Close()
		h.srv.Close()
	})

	dir := t.TempDir()
	h.cast = filepath.Join(dir, "session.cast")
	base, err := url.Parse(h.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(h.srv.Certificate())
	stdin, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTWATCH_WRAPPER_TOKEN", raw(h.wrapTok))
	t.Setenv("AGENTWATCH_VIEWER_TOKEN", raw(h.viewTok))
	t.Setenv("PS1", prompt)
	t.Setenv("ENV", "")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		code, err := wrapper.Run(ctx, wrapper.Config{
			Command: []string{"/bin/sh"},
			Name:    "e2e",
			Relay:   &wrapper.Relay{URL: base, Token: h.wrapTok, TLS: &tls.Config{RootCAs: roots}},
			Record:  h.cast,
			Stdin:   stdin,
			Log:     slog.New(slog.NewTextHandler(&h.logs, nil)),
		})
		h.done <- result{code, err}
	}()
	t.Cleanup(func() {
		cancel()
		stdin.Close()
		stdinW.Close()
		if t.Failed() {
			t.Logf("wrapper log:\n%s", h.logs.String())
		}
	})
	return h
}

func (h *harness) newRelay() *relay.Relay {
	rl, err := relay.New(relay.Config{
		WrapperToken: h.wrapTok,
		ViewerToken:  h.viewTok,
		Log:          slog.New(slog.NewTextHandler(&h.logs, nil)),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return rl
}

// restartRelay replaces the relay at the same URL. The old one closes every
// socket with 1001, as a relay restart does. Close waits for each peer's
// close reply, and the test's viewer replies only once it reads again.
func (h *harness) restartRelay() {
	go h.relay.Swap(h.newRelay()).Close()
}

func raw(tok wire.Token) string { return strings.TrimPrefix(tok.Bearer(), "Bearer ") }

// session waits until the session list shows the wrapper online.
func (h *harness) session() wire.SessionID {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", h.srv.URL+"/api/sessions", nil)
		req.Header.Set("Authorization", h.viewTok.Bearer())
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			h.t.Fatal(err)
		}
		var body struct{ Sessions []wire.SessionInfo }
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			h.t.Fatal(err)
		}
		for _, s := range body.Sessions {
			if s.Name == "e2e" && s.Online {
				return s.ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatal("the wrapper never came online at the relay")
	return ""
}

func (h *harness) wait() result {
	h.t.Helper()
	select {
	case r := <-h.done:
		return r
	case <-time.After(timeout):
		h.t.Fatal("wrapper.Run did not return")
		return result{}
	}
}

// viewer is a Go stand-in for app.js. It keeps every byte of output it has
// read, and mark is where the next expectOutput starts looking.
type viewer struct {
	t    *testing.T
	c    *websocket.Conn
	out  strings.Builder
	mark int
}

func (h *harness) attach(id wire.SessionID) *viewer {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	wsURL := "wss" + strings.TrimPrefix(h.srv.URL, "https") + "/api/sessions/" + string(id) + "/attach"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient:   h.srv.Client(),
		Subprotocols: []string{wire.Subprotocol, wire.BearerPrefix + raw(h.viewTok)},
	})
	if err != nil {
		h.t.Fatalf("viewer attach: %v", err)
	}
	c.SetReadLimit(wire.MaxMessage)
	h.t.Cleanup(func() { c.CloseNow() })
	return &viewer{t: h.t, c: c}
}

func (v *viewer) send(m wire.Message) {
	v.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := v.c.Write(ctx, websocket.MessageBinary, wire.Encode(m)); err != nil {
		v.t.Fatalf("viewer write %T: %v", m, err)
	}
}

func (v *viewer) next() (wire.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, b, err := v.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	m, err := wire.Decode(b)
	if err != nil {
		v.t.Fatalf("viewer decode: %v", err)
	}
	if o, ok := m.(wire.Output); ok {
		v.out.Write(o)
	}
	return m, nil
}

// expectOutput reads until the output after the last match contains s.
func (v *viewer) expectOutput(s string) {
	v.t.Helper()
	for {
		if i := strings.Index(v.out.String()[v.mark:], s); i >= 0 {
			v.mark += i + len(s)
			return
		}
		if _, err := v.next(); err != nil {
			v.t.Fatalf("waiting for %q: %v; output so far:\n%s", s, err, v.out.String())
		}
	}
}

// run types cmd once the shell shows its next prompt. Typing earlier is
// typeahead: the terminal echoes it before the prompt and the output lands
// after the prompt instead of on a new line.
func (v *viewer) run(cmd string) {
	v.t.Helper()
	v.expectOutput(prompt)
	v.send(wire.Input(cmd + "\r"))
}

// expect reads until a frame of type T satisfies ok.
func expect[T wire.Message](v *viewer, ok func(T) bool) T {
	v.t.Helper()
	for {
		m, err := v.next()
		if err != nil {
			var zero T
			v.t.Fatalf("waiting for %T: %v", zero, err)
		}
		if m, is := m.(T); is && ok(m) {
			return m
		}
	}
}

func (v *viewer) expectClose(code wire.CloseCode) {
	v.t.Helper()
	for {
		if _, err := v.next(); err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(code) {
				v.t.Fatalf("closed with %d, want %d: %v", got, code, err)
			}
			return
		}
	}
}

func TestShellInPTYThroughRelay(t *testing.T) {
	h := start(t)
	id := h.session()
	v := h.attach(id)

	v.send(wire.ResizeRequest{TermSize: wire.TermSize{Cols: 100, Rows: 30}})
	expect(v, func(m wire.SizeChanged) bool { return m.TermSize == wire.TermSize{Cols: 100, Rows: 30} })
	v.run("stty size")
	v.expectOutput("stty size")
	v.expectOutput("\n30 100")

	v.run("echo typed''-by-viewer")
	v.expectOutput("echo typed''-by-viewer")
	v.expectOutput("\ntyped-by-viewer")

	v.run("echo env:${AGENTWATCH_WRAPPER_TOKEN:-unset}:${AGENTWATCH_VIEWER_TOKEN:-unset}:$AGENTWATCH_SESSION")
	v.expectOutput("\nenv:unset:unset:" + string(id))

	v.run("exit 3")
	if x := expect(v, func(wire.Exit) bool { return true }); x.Code != 3 {
		t.Fatalf("viewer got %+v, want code 3", x)
	}
	v.expectClose(wire.CloseEnded)
	if r := h.wait(); r.code != 3 || r.err != nil {
		t.Fatalf("Run returned %d, %v; want 3, nil", r.code, r.err)
	}

	codes, output := readCast(t, h.cast)
	for _, want := range []string{"o", "r 100x30", "m exit 3"} {
		if !codes[want] {
			t.Errorf("recording has no %q event; has %v", want, codes)
		}
	}
	if codes["i"] {
		t.Error("recording has input events without RecordInput")
	}
	if !strings.Contains(output, "\n30 100") || !strings.Contains(output, "\ntyped-by-viewer") {
		t.Errorf("recorded output lacks the session's output:\n%s", output)
	}
}

func TestRelayRestartKeepsTheSession(t *testing.T) {
	h := start(t)
	id := h.session()
	v := h.attach(id)

	v.run("echo before''-restart")
	v.expectOutput("\nbefore-restart")

	h.restartRelay()
	v.expectClose(wire.CloseCode(websocket.StatusGoingAway))
	if again := h.session(); again != id {
		t.Fatalf("after the relay restart the session is %s, want %s", again, id)
	}
	v = h.attach(id)
	// The new relay has an empty replay, and the shell is already at its prompt.
	v.send(wire.Input("echo after''-restart\r"))
	v.expectOutput("\nafter-restart")

	v.run("exit 0")
	v.expectClose(wire.CloseEnded)
	if r := h.wait(); r.code != 0 || r.err != nil {
		t.Fatalf("Run returned %d, %v", r.code, r.err)
	}
	codes, _ := readCast(t, h.cast)
	if !codes["m relay disconnected"] || !codes["m relay connected"] {
		t.Errorf("recording does not mark the relay outage; has %v", codes)
	}
}

func TestWrongWrapperTokenFailsBeforeTheChildStarts(t *testing.T) {
	h := &harness{t: t, wrapTok: wire.NewToken(), viewTok: wire.NewToken()}
	srv := httptest.NewTLSServer(h.newRelay())
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	marker := filepath.Join(t.TempDir(), "started")
	for name, r := range map[string]*wrapper.Relay{
		"wrong token":             {URL: base, Token: wire.NewToken(), TLS: &tls.Config{RootCAs: roots}},
		"viewer token":            {URL: base, Token: h.viewTok, TLS: &tls.Config{RootCAs: roots}},
		"plain http off loopback": {URL: &url.URL{Scheme: "http", Host: "relay.example:8080"}, Token: h.wrapTok},
	} {
		code, err := wrapper.Run(context.Background(), wrapper.Config{
			Command: []string{"/bin/sh", "-c", "touch " + marker},
			Name:    "e2e",
			Relay:   r,
		})
		if code != 2 || err == nil {
			t.Errorf("%s: Run returned %d, %v; want 2 and an error", name, code, err)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("%s: the child started", name)
		}
	}
}

// readCast returns the set of event codes, plus "r <size>" and "m <label>" for
// resizes and markers, and the concatenated output.
func readCast(t *testing.T, path string) (map[string]bool, string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	if !sc.Scan() {
		t.Fatal("recording is empty")
	}
	var header struct{ Version, Width, Height int }
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil || header.Version != 2 {
		t.Fatalf("header %s: %v", sc.Bytes(), err)
	}
	codes := map[string]bool{}
	var output strings.Builder
	for sc.Scan() {
		var ev [3]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("event %s: %v", sc.Bytes(), err)
		}
		code, data := ev[1].(string), ev[2].(string)
		codes[code] = true
		switch code {
		case "o":
			output.WriteString(data)
		case "r", "m":
			codes[code+" "+data] = true
		}
	}
	return codes, output.String()
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
