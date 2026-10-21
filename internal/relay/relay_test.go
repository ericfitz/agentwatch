package relay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ericfitz/agentwatch/internal/relay"
	"github.com/ericfitz/agentwatch/internal/wire"
)

const timeout = 10 * time.Second

type env struct {
	t       *testing.T
	srv     *httptest.Server
	wrapper wire.Token
	viewer  wire.Token
}

func start(t *testing.T, grace time.Duration) *env {
	t.Helper()
	e := &env{t: t, wrapper: wire.NewToken(), viewer: wire.NewToken()}
	rl, err := relay.New(relay.Config{
		WrapperToken: e.wrapper,
		ViewerToken:  e.viewer,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if grace > 0 {
		relay.SetGrace(rl, grace)
	}
	e.srv = httptest.NewTLSServer(rl)
	t.Cleanup(func() {
		rl.Close()
		e.srv.Close()
	})
	return e
}

// raw is the token text a browser pastes and offers as a subprotocol.
func raw(tok wire.Token) string { return strings.TrimPrefix(tok.Bearer(), "Bearer ") }

func (e *env) dial(path string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	opts.HTTPClient = e.srv.Client()
	c, resp, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(e.srv.URL, "https")+path, opts)
	if c != nil {
		c.SetReadLimit(wire.MaxMessage)
		e.t.Cleanup(func() { c.CloseNow() })
	}
	return c, resp, err
}

func (e *env) dialWrapper(header http.Header) (*websocket.Conn, *http.Response, error) {
	return e.dial(wire.WrapperPath, &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{wire.Subprotocol}})
}

func (e *env) dialViewer(id wire.SessionID, tok string, header http.Header) (*websocket.Conn, *http.Response, error) {
	protos := []string{wire.Subprotocol}
	if tok != "" {
		protos = append(protos, wire.BearerPrefix+tok)
	}
	return e.dial("/api/sessions/"+string(id)+"/attach", &websocket.DialOptions{HTTPHeader: header, Subprotocols: protos})
}

func hello(secret wire.SessionSecret, name string) wire.Hello {
	return wire.Hello{
		Secret:  secret,
		Name:    name,
		Host:    "build-01",
		Command: []string{"claude"},
		Started: time.Date(2026, 10, 21, 5, 3, 0, 0, time.UTC),
	}
}

// connect dials as a wrapper and sends Hello. The session is not
// necessarily registered when it returns.
func (e *env) connect(secret wire.SessionSecret) *websocket.Conn {
	e.t.Helper()
	c, _, err := e.dialWrapper(http.Header{"Authorization": {e.wrapper.Bearer()}})
	if err != nil {
		e.t.Fatalf("wrapper dial: %v", err)
	}
	send(e.t, c, hello(secret, "api-agent"))
	return c
}

// register connects a wrapper and waits until the session list shows it online.
func (e *env) register(secret wire.SessionSecret) *websocket.Conn {
	e.t.Helper()
	c := e.connect(secret)
	e.waitSessions(func(ss []wire.SessionInfo) bool {
		for _, s := range ss {
			if s.ID == secret.ID() && s.Online {
				return true
			}
		}
		return false
	})
	return c
}

// attach dials as a viewer and returns the snapshot frames that precede the
// first Presence, which the relay sends once the viewer is attached.
func (e *env) attach(id wire.SessionID) (*websocket.Conn, []wire.Message) {
	e.t.Helper()
	c, _, err := e.dialViewer(id, raw(e.viewer), nil)
	if err != nil {
		e.t.Fatalf("viewer dial: %v", err)
	}
	var snap []wire.Message
	for {
		m := recv(e.t, c)
		if _, ok := m.(wire.Presence); ok {
			return c, snap
		}
		snap = append(snap, m)
	}
}

func (e *env) get(path string, tok *wire.Token) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if tok != nil {
		req.Header.Set("Authorization", tok.Bearer())
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) sessions() []wire.SessionInfo {
	e.t.Helper()
	resp := e.get("/api/sessions", &e.viewer)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET /api/sessions: %s", resp.Status)
	}
	var body struct{ Sessions []wire.SessionInfo }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		e.t.Fatal(err)
	}
	return body.Sessions
}

func (e *env) waitSessions(ok func([]wire.SessionInfo) bool) []wire.SessionInfo {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ss := e.sessions()
		if ok(ss) {
			return ss
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("session list never matched; last %+v", ss)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func send(t *testing.T, c *websocket.Conn, m wire.Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, wire.Encode(m)); err != nil {
		t.Fatalf("write %T: %v", m, err)
	}
}

func recv(t *testing.T, c *websocket.Conn) wire.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m, err := wire.Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// recvData returns the next frame that is not a Presence.
func recvData(t *testing.T, c *websocket.Conn) wire.Message {
	t.Helper()
	for {
		m := recv(t, c)
		if _, ok := m.(wire.Presence); !ok {
			return m
		}
	}
}

func wantMessage(t *testing.T, got, want wire.Message) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s, want %s", brief(got), brief(want))
	}
}

func brief(m wire.Message) string {
	if o, ok := m.(wire.Output); ok {
		return fmt.Sprintf("Output of %d bytes %.40q", len(o), o)
	}
	return fmt.Sprintf("%T %+v", m, m)
}

func wantOutput(t *testing.T, c *websocket.Conn, want string) {
	t.Helper()
	wantMessage(t, recvData(t, c), wire.Output(want))
}

// wantClose reads until the connection ends and checks its close code.
func wantClose(t *testing.T, c *websocket.Conn, code wire.CloseCode) {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_, _, err := c.Read(ctx)
		cancel()
		if err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(code) {
				t.Fatalf("closed with %d, want %d: %v", got, code, err)
			}
			return
		}
	}
}

func TestNewRequiresTwoDistinctTokens(t *testing.T) {
	tok := wire.NewToken()
	for name, cfg := range map[string]relay.Config{
		"no tokens":        {},
		"no viewer token":  {WrapperToken: tok},
		"no wrapper token": {ViewerToken: tok},
		"same token":       {WrapperToken: tok, ViewerToken: tok},
	} {
		if _, err := relay.New(cfg); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	if _, err := relay.New(relay.Config{WrapperToken: tok, ViewerToken: wire.NewToken()}); err != nil {
		t.Errorf("distinct tokens: %v", err)
	}
}

func TestPublicRoutesCarrySecurityHeaders(t *testing.T) {
	e := start(t, 0)
	for path, want := range map[string]string{
		"/healthz":     "ok",
		"/protocol.js": string(wire.ClientJS()),
	} {
		resp := e.get(path, nil)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Errorf("GET %s: %s %q", path, resp.Status, body)
		}
	}
	for path, status := range map[string]int{
		"/codec.js":            http.StatusOK,
		"/static/xterm.min.js": http.StatusOK,
		"/api/sessions":        http.StatusUnauthorized,
	} {
		resp := e.get(path, nil)
		if resp.StatusCode != status {
			t.Errorf("GET %s: %s, want %d", path, resp.Status, status)
		}
		h := resp.Header
		if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self';") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("GET %s: missing security headers: %v", path, h)
		}
	}
}

func TestEveryEndpointRejectsMissingWrongAndOtherRoleTokens(t *testing.T) {
	e := start(t, 0)
	secret := wire.NewSessionSecret()
	e.register(secret)
	id := secret.ID()

	bearerHeader := func(tok string) http.Header {
		if tok == "" {
			return http.Header{}
		}
		return http.Header{"Authorization": {"Bearer " + tok}}
	}
	endpoints := []struct {
		name         string
		right, other wire.Token
		ok           int
		call         func(tok string) int
	}{
		{"GET /api/sessions", e.viewer, e.wrapper, http.StatusOK, func(tok string) int {
			req, _ := http.NewRequest("GET", e.srv.URL+"/api/sessions", nil)
			req.Header = bearerHeader(tok)
			resp, err := e.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}},
		{"viewer attach", e.viewer, e.wrapper, http.StatusSwitchingProtocols, func(tok string) int {
			_, resp, _ := e.dialViewer(id, tok, nil)
			return resp.StatusCode
		}},
		{"wrapper", e.wrapper, e.viewer, http.StatusSwitchingProtocols, func(tok string) int {
			_, resp, _ := e.dialWrapper(bearerHeader(tok))
			return resp.StatusCode
		}},
	}
	for _, ep := range endpoints {
		for name, tok := range map[string]string{
			"missing":    "",
			"wrong":      raw(wire.NewToken()),
			"other role": raw(ep.other),
		} {
			if got := ep.call(tok); got != http.StatusUnauthorized {
				t.Errorf("%s with %s token: status %d, want 401", ep.name, name, got)
			}
		}
		if got := ep.call(raw(ep.right)); got != ep.ok {
			t.Errorf("%s with the right token: status %d, want %d", ep.name, got, ep.ok)
		}
	}

	_, resp, err := e.dialViewer(id, raw(e.viewer), http.Header{"Origin": {"https://evil.example"}})
	if err == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin attach: err %v, status %v, want 403", err, resp.StatusCode)
	}
}

func TestFrameOnTheWrongRouteCloses4400(t *testing.T) {
	e := start(t, 0)
	secret := wire.NewSessionSecret()
	w := e.register(secret)

	for _, m := range []wire.Message{wire.Output("x"), hello(secret, "x"), wire.Presence{}} {
		v, _ := e.attach(secret.ID())
		send(t, v, m)
		wantClose(t, v, wire.CloseProtocol)
	}

	send(t, w, wire.Input("x"))
	wantClose(t, w, wire.CloseProtocol)
}

func TestOutputReachesEveryViewer(t *testing.T) {
	e := start(t, 0)
	secret := wire.NewSessionSecret()
	w := e.register(secret)
	v1, _ := e.attach(secret.ID())
	v2, _ := e.attach(secret.ID())

	send(t, w, wire.Output("hello\r\n"))
	wantOutput(t, v1, "hello\r\n")
	wantOutput(t, v2, "hello\r\n")
}

func TestLateJoinerGetsLastSizeAndLineAlignedTail(t *testing.T) {
	e := start(t, 0)
	secret := wire.NewSessionSecret()
	w := e.register(secret)
	early, _ := e.attach(secret.ID())

	send(t, w, wire.SizeChanged{TermSize: wire.TermSize{Cols: 80, Rows: 24}})
	var all strings.Builder
	const lineLen = 1000
	for i := 0; i < 300; i += 10 {
		var frame strings.Builder
		for j := i; j < i+10; j++ {
			line := fmt.Sprintf("line %04d ", j)
			frame.WriteString(line + strings.Repeat("x", lineLen-len(line)-2) + "\r\n")
		}
		all.WriteString(frame.String())
		send(t, w, wire.Output(frame.String()))
	}
	last := wire.SizeChanged{TermSize: wire.TermSize{Cols: 100, Rows: 30}}
	send(t, w, last)
	for {
		if _, ok := recvData(t, early).(wire.SizeChanged); ok {
			break
		}
	}

	_, snap := e.attach(secret.ID())
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d frames, want size and replay: %+v", len(snap), snap)
	}
	wantMessage(t, snap[0], last)
	// The longest run of whole lines that fits in 128 KiB.
	lines := (128 << 10) / lineLen
	wantMessage(t, snap[1], wire.Output(all.String()[all.Len()-lines*lineLen:]))
}

func TestTypingViewerResendsItsSizeAfterAnotherHeldIt(t *testing.T) {
	e := start(t, 0)
	secret := wire.NewSessionSecret()
	w := e.register(secret)
	a, _ := e.attach(secret.ID())
	b, _ := e.attach(secret.ID())
	sizeA := wire.ResizeRequest{TermSize: wire.TermSize{Cols: 80, Rows: 24}}
	sizeB := wire.ResizeRequest{TermSize: wire.TermSize{Cols: 120, Rows: 40}}

	send(t, a, sizeA)
	wantMessage(t, recv(t, w), sizeA)
	send(t, b, sizeB)
	wantMessage(t, recv(t, w), sizeB)

	send(t, a, wire.Input("x"))
	wantMessage(t, recv(t, w), sizeA)
	wantMessage(t, recv(t, w), wire.Input("x"))
	send(t, a, wire.Input("y"))
	wantMessage(t, recv(t, w), wire.Input("y"))

	send(t, b, wire.Input("z"))
	wantMessage(t, recv(t, w), sizeB)
	wantMessage(t, recv(t, w), wire.Input("z"))
}

func TestReconnectWithSameSecretKeepsViewersAttached(t *testing.T) {
	e := start(t, 0)
	secret := wire.NewSessionSecret()
	w1 := e.register(secret)
	v, _ := e.attach(secret.ID())
	size := wire.ResizeRequest{TermSize: wire.TermSize{Cols: 90, Rows: 20}}
	send(t, v, size)
	wantMessage(t, recv(t, w1), size)
	send(t, w1, wire.Output("one"))
	wantOutput(t, v, "one")

	w1.Close(websocket.StatusNormalClosure, "")
	wantMessage(t, recv(t, v), wire.Presence{WrapperOnline: false, Viewers: 1})
	if ss := e.sessions(); len(ss) != 1 || ss[0].ID != secret.ID() || ss[0].Online {
		t.Fatalf("while offline, sessions = %+v", ss)
	}

	w2 := e.connect(secret)
	wantMessage(t, recv(t, v), wire.Presence{WrapperOnline: true, Viewers: 1})
	send(t, w2, wire.Output("two"))
	wantOutput(t, v, "two")
	// The new link starts at the wrapper's local size, so the viewer's size goes first.
	send(t, v, wire.Input("k"))
	wantMessage(t, recv(t, w2), size)
	wantMessage(t, recv(t, w2), wire.Input("k"))

	// A newer connection wins even while the older one still looks alive,
	// and frames the older one sends after that never reach viewers.
	w3 := e.connect(secret)
	wantMessage(t, recv(t, v), wire.Presence{WrapperOnline: true, Viewers: 1})
	send(t, w2, wire.Output("stale"))
	wantClose(t, w2, wire.CloseReplaced)
	send(t, w3, wire.Output("three"))
	wantOutput(t, v, "three")
}

func TestDifferentSecretCannotTakeOverASession(t *testing.T) {
	e := start(t, 0)
	owner := wire.NewSessionSecret()
	w := e.register(owner)
	v, _ := e.attach(owner.ID())

	intruder := wire.NewSessionSecret()
	wi := e.register(intruder)
	if intruder.ID() == owner.ID() {
		t.Fatal("two secrets share an id")
	}
	if ss := e.sessions(); len(ss) != 2 {
		t.Fatalf("sessions = %+v, want the owner's and the intruder's", ss)
	}
	vi, _ := e.attach(intruder.ID())
	send(t, wi, wire.Output("intruder"))
	wantOutput(t, vi, "intruder")

	send(t, w, wire.Output("owner"))
	wantOutput(t, v, "owner")
	send(t, v, wire.Input("k"))
	wantMessage(t, recv(t, w), wire.Input("k"))
}

func TestGraceExpiryCloses4410(t *testing.T) {
	e := start(t, 200*time.Millisecond)
	secret := wire.NewSessionSecret()
	w := e.register(secret)
	v, _ := e.attach(secret.ID())

	w.CloseNow()
	wantMessage(t, recv(t, v), wire.Presence{WrapperOnline: false, Viewers: 1})
	wantClose(t, v, wire.CloseEnded)
	if ss := e.sessions(); len(ss) != 0 {
		t.Fatalf("after expiry, sessions = %+v", ss)
	}

	e.register(secret)
}

func TestExitReachesALaggingViewerBeforeClose4410(t *testing.T) {
	e := start(t, 0)
	secret := wire.NewSessionSecret()
	w := e.register(secret)
	v, _ := e.attach(secret.ID())

	// More bytes than the socket buffers hold, in fewer frames than the viewer
	// queue holds, so the Exit is still queued when the session ends.
	const frames = 64
	chunk := strings.Repeat("z", 512<<10)
	for i := 0; i < frames; i++ {
		send(t, w, wire.Output(chunk))
	}
	send(t, w, wire.Exit{Code: 3})
	w.Close(websocket.StatusNormalClosure, "")

	for i := 0; i < frames; i++ {
		wantOutput(t, v, chunk)
	}
	wantMessage(t, recvData(t, v), wire.Exit{Code: 3})
	wantClose(t, v, wire.CloseEnded)
}

func TestViewerThatStopsReadingGets4408WhileOthersKeepReceiving(t *testing.T) {
	e := start(t, 0)
	secret := wire.NewSessionSecret()
	w := e.register(secret)
	fast, _ := e.attach(secret.ID())
	stalled, _ := e.attach(secret.ID())

	payload := strings.Repeat("y", 32<<10)
	dropped := false
	for i := 0; i < 4000 && !dropped; i++ {
		frame := fmt.Sprintf("%06d%s", i, payload)
		send(t, w, wire.Output(frame))
		wantOutput(t, fast, frame)
		if i%16 == 15 {
			dropped = e.sessions()[0].Viewers == 1
		}
	}
	if !dropped {
		t.Fatal("the stalled viewer was never dropped")
	}

	wantClose(t, stalled, wire.CloseTooSlow)
	send(t, w, wire.Output("after"))
	wantOutput(t, fast, "after")
}
