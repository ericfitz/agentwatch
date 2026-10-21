// Package relay is the meeting point between wrappers and browsers. Wrappers
// dial in and register sessions. Viewers list sessions and attach. The relay
// routes frames by kind and decodes no payload except Hello.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ericfitz/agentwatch/internal/wire"
	"github.com/ericfitz/agentwatch/web"
)

type Config struct {
	WrapperToken wire.Token
	ViewerToken  wire.Token
	Log          *slog.Logger // nil uses slog.Default()
}

type Relay struct {
	cfg   Config
	mux   *http.ServeMux
	grace time.Duration

	mu       sync.Mutex
	sessions map[wire.SessionID]*hub
	closed   bool
}

// style-src allows inline style because the xterm.js DOM renderer injects
// <style> elements. Scripts stay strict.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// New fails when either token is zero or the two are equal.
func New(cfg Config) (*Relay, error) {
	if cfg.WrapperToken.IsZero() || cfg.ViewerToken.IsZero() {
		return nil, errors.New("relay: both a wrapper token and a viewer token are required")
	}
	if cfg.WrapperToken == cfg.ViewerToken {
		return nil, errors.New("relay: the wrapper token and the viewer token must differ")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	rl := &Relay{cfg: cfg, grace: offlineGrace, sessions: map[wire.SessionID]*hub{}}
	clientJS := wire.ClientJS()
	rl.mux = http.NewServeMux()
	rl.mux.Handle("GET /", http.FileServerFS(web.FS))
	rl.mux.HandleFunc("GET /protocol.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(clientJS)
	})
	rl.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	rl.mux.HandleFunc("GET /api/sessions", rl.serveSessions)
	rl.mux.HandleFunc("GET /api/sessions/{id}/attach", rl.serveViewer)
	rl.mux.HandleFunc("GET "+wire.WrapperPath, rl.serveWrapper)
	return rl, nil
}

func (rl *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	rl.mux.ServeHTTP(w, r)
}

// Close ends every WebSocket with 1001 and stops grace timers. http.Server's
// Shutdown does not reach hijacked connections, so the CLI calls both.
func (rl *Relay) Close() error {
	rl.mu.Lock()
	rl.closed = true
	var conns []*websocket.Conn
	for id, h := range rl.sessions {
		if h.wrapper != nil {
			conns = append(conns, h.wrapper.conn)
			h.wrapper = nil
		}
		for v := range h.viewers {
			conns = append(conns, v.conn)
		}
		h.viewers = nil
		if h.expire != nil {
			h.expire.Stop()
			h.expire = nil
		}
		delete(rl.sessions, id)
	}
	rl.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			closeConn(c, closeGoingAway, "relay shutting down")
		}()
	}
	wg.Wait()
	return nil
}

func (rl *Relay) serveSessions(w http.ResponseWriter, r *http.Request) {
	if !authorized(w, r, rl.cfg.ViewerToken) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(struct {
		Sessions []wire.SessionInfo `json:"sessions"`
	}{rl.list()})
}

func (rl *Relay) serveWrapper(w http.ResponseWriter, r *http.Request) {
	if !authorized(w, r, rl.cfg.WrapperToken) {
		return
	}
	c := accept(w, r)
	if c == nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hello, err := readHello(ctx, c)
	if err != nil {
		rl.cfg.Log.Warn("relay: rejected wrapper", "remote", r.RemoteAddr, "err", err)
		closeConn(c, wire.CloseProtocol, "bad or late hello")
		return
	}
	id := hello.Secret.ID()
	p := &peer{conn: c}
	h, old := rl.bind(hello, p)
	if h == nil {
		closeConn(c, closeGoingAway, "relay shutting down")
		return
	}
	if old != nil {
		go closeConn(old.conn, wire.CloseReplaced, "replaced by a newer connection")
	}
	rl.cfg.Log.Info("relay: session online", "session", id, "name", hello.Name, "host", hello.Host)
	defer rl.cfg.Log.Info("relay: wrapper disconnected", "session", id)
	defer rl.unbind(h, p)

	go keepalive(ctx, c)
	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		kind, route, err := wire.PeekKind(msg)
		if typ != websocket.MessageBinary || err != nil || route != wire.RouteUp {
			rl.cfg.Log.Warn("relay: wrapper sent a frame it may not send", "session", id, "kind", kind, "err", err)
			closeConn(c, wire.CloseProtocol, "wrappers send only up-route frames")
			return
		}
		rl.publish(h, p, kind, msg)
	}
}

func (rl *Relay) serveViewer(w http.ResponseWriter, r *http.Request) {
	if !authorized(w, r, rl.cfg.ViewerToken) {
		return
	}
	id, err := wire.ParseSessionID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c := accept(w, r)
	if c == nil {
		return
	}
	defer c.CloseNow()
	v := &peer{conn: c, send: make(chan []byte, viewerQueue)}
	h, code := rl.attach(id, v)
	if h == nil {
		closeConn(c, code, "")
		return
	}
	defer rl.detach(h, v)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go v.writeLoop(ctx)
	go keepalive(ctx, c)
	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		kind, route, err := wire.PeekKind(msg)
		if typ != websocket.MessageBinary || err != nil || route != wire.RouteDown {
			rl.cfg.Log.Warn("relay: viewer sent a frame it may not send", "session", id, "kind", kind, "err", err)
			closeConn(c, wire.CloseProtocol, "viewers send only down-route frames")
			return
		}
		if dst, pre := rl.claim(h, v, kind, msg); dst != nil {
			dst.forward(pre, msg)
		}
	}
}

func authorized(w http.ResponseWriter, r *http.Request, role wire.Token) bool {
	if role.Matches(bearer(r)) {
		return true
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// bearer returns the token from "Authorization: Bearer <t>" or from a
// Sec-WebSocket-Protocol entry with wire.BearerPrefix. Browsers cannot set
// headers on a WebSocket, so the token has two carriers.
func bearer(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return t
	}
	for _, line := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(line, ",") {
			if t, ok := strings.CutPrefix(strings.TrimSpace(p), wire.BearerPrefix); ok {
				return t
			}
		}
	}
	return ""
}

// accept upgrades r with the default Origin check. It returns nil once it
// has already answered or closed the connection.
func accept(w http.ResponseWriter, r *http.Request) *websocket.Conn {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
	if err != nil {
		return nil
	}
	c.SetReadLimit(wire.MaxMessage)
	if c.Subprotocol() != wire.Subprotocol {
		closeConn(c, wire.CloseProtocol, "subprotocol "+wire.Subprotocol+" required")
		return nil
	}
	return c
}

// readHello closes with 4400 itself when no frame arrives in time, because a
// Read whose context expires drops the connection without a close code.
func readHello(ctx context.Context, c *websocket.Conn) (wire.Hello, error) {
	late := time.AfterFunc(helloDeadline, func() { closeConn(c, wire.CloseProtocol, "no hello") })
	defer late.Stop()
	typ, msg, err := c.Read(ctx)
	if err != nil {
		return wire.Hello{}, err
	}
	if typ != websocket.MessageBinary {
		return wire.Hello{}, errors.New("hello is not a binary frame")
	}
	m, err := wire.Decode(msg)
	if err != nil {
		return wire.Hello{}, err
	}
	hello, ok := m.(wire.Hello)
	if !ok {
		return wire.Hello{}, errors.New("first frame is not a hello")
	}
	return hello, nil
}

func keepalive(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pongWait)
		err := c.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.CloseNow()
			}
			return
		}
	}
}

func closeConn(c *websocket.Conn, code wire.CloseCode, reason string) {
	c.Close(websocket.StatusCode(code), reason)
}
