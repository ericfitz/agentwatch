package wrapper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/ericfitz/agentwatch/internal/wire"
)

const (
	dialWait   = 10 * time.Second
	writeWait  = 10 * time.Second
	pingEvery  = 15 * time.Second
	pongWait   = 10 * time.Second
	minRedial  = time.Second
	maxRedial  = 30 * time.Second
	stableLink = 30 * time.Second
	linkQueue  = 1024
)

// errRejected marks a relay answer that retrying cannot fix: HTTP 401 or 403,
// a missing subprotocol, or close code 4400.
var errRejected = errors.New("relay rejected this wrapper")

// uplink keeps one WebSocket to the relay alive. It owns reconnect policy.
type uplink struct {
	endpoint string
	opts     websocket.DialOptions
	hello    []byte // sent first on every connection
	log      *slog.Logger
	events   chan linkEvent // to the session loop, in arrival order
	cur      atomic.Pointer[link]
	ctx      context.Context // ends redialing; independent of Run's ctx
	stop     context.CancelFunc
	done     chan struct{} // closed when run returns
	rejected error         // written by run before done closes
}

type link struct {
	conn *websocket.Conn
	out  chan []byte   // never closed, so a late send cannot panic; nil asks for Close(1000)
	dead chan struct{} // closed once the reader has stopped
}

type linkEventKind uint8

const (
	linkUp linkEventKind = iota + 1
	linkFrame
	linkDown
)

type linkEvent struct {
	kind linkEventKind
	msg  wire.Message // linkFrame only
}

// startUplink makes the first dial before the child starts. A rejection is
// returned so the operator hears about a wrong token before the agent runs.
// Any other failure is logged, and the uplink keeps retrying in the background.
func startUplink(ctx context.Context, r *Relay, hello wire.Hello, log *slog.Logger) (*uplink, error) {
	if r.URL.Scheme != "https" && !(r.URL.Scheme == "http" && wire.PlaintextAllowed(r.URL.Hostname())) {
		return nil, fmt.Errorf("relay URL %s must be https, or http on a loopback host", r.URL.Redacted())
	}
	endpoint := *r.URL
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + wire.WrapperPath
	endpoint.RawQuery, endpoint.Fragment = "", ""
	client := http.DefaultClient
	if r.TLS != nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = r.TLS
		client = &http.Client{Transport: t}
	}
	u := &uplink{
		endpoint: endpoint.String(),
		opts: websocket.DialOptions{
			HTTPClient:   client,
			HTTPHeader:   http.Header{"Authorization": {r.Token.Bearer()}},
			Subprotocols: []string{wire.Subprotocol},
		},
		hello:  wire.Encode(hello),
		log:    log,
		events: make(chan linkEvent, 64),
		done:   make(chan struct{}),
	}
	u.ctx, u.stop = context.WithCancel(context.Background())

	dctx, cancel := context.WithTimeout(ctx, dialWait)
	first, err := u.dial(dctx)
	cancel()
	if errors.Is(err, errRejected) {
		u.stop()
		return nil, err
	}
	if err != nil {
		log.Warn("wrapper: relay unreachable; the session starts locally and keeps retrying", "relay", r.URL.Redacted(), "err", err)
	}
	go u.run(first)
	return u, nil
}

func (u *uplink) dial(ctx context.Context) (*link, error) {
	c, resp, err := websocket.Dial(ctx, u.endpoint, &u.opts)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("%w: HTTP %s; check the wrapper token", errRejected, resp.Status)
		}
		return nil, err
	}
	if c.Subprotocol() != wire.Subprotocol {
		c.CloseNow()
		return nil, fmt.Errorf("%w: it did not select subprotocol %s", errRejected, wire.Subprotocol)
	}
	c.SetReadLimit(wire.MaxMessage)
	wctx, cancel := context.WithTimeout(ctx, writeWait)
	defer cancel()
	if err := c.Write(wctx, websocket.MessageBinary, u.hello); err != nil {
		c.CloseNow()
		return nil, err
	}
	return &link{conn: c, out: make(chan []byte, linkQueue), dead: make(chan struct{})}, nil
}

func (u *uplink) run(l *link) {
	defer close(u.done)
	var wait backoff
	for {
		if l != nil {
			began := time.Now()
			err := u.serve(l)
			if errors.Is(err, errRejected) {
				u.log.Error("wrapper: relay closed the link for good; the session continues locally", "err", err)
				u.rejected = err
				return
			}
			if time.Since(began) >= stableLink {
				wait = backoff{}
			}
		}
		select {
		case <-u.ctx.Done():
			return
		case <-time.After(wait.next()):
		}
		dctx, cancel := context.WithTimeout(u.ctx, dialWait)
		next, err := u.dial(dctx)
		cancel()
		if errors.Is(err, errRejected) {
			u.log.Error("wrapper: relay refused to reconnect; the session continues locally", "err", err)
			u.rejected = err
			return
		}
		if err != nil && u.ctx.Err() == nil {
			u.log.Warn("wrapper: relay redial failed", "err", err)
		}
		l = next
	}
}

// serve publishes l as the current link and reads from it until it ends.
func (u *uplink) serve(l *link) error {
	u.cur.Store(l)
	if u.ctx.Err() != nil {
		u.cur.CompareAndSwap(l, nil)
		close(l.dead)
		l.conn.CloseNow()
		return u.ctx.Err()
	}
	u.emit(linkEvent{kind: linkUp})
	go l.write()
	go l.keepalive()
	var err error
	for {
		var msg []byte
		if _, msg, err = l.conn.Read(context.Background()); err != nil {
			break
		}
		m, derr := wire.Decode(msg)
		if derr != nil {
			u.log.Warn("wrapper: dropped a bad frame from the relay", "err", derr)
			continue
		}
		u.emit(linkEvent{kind: linkFrame, msg: m})
	}
	u.cur.CompareAndSwap(l, nil)
	close(l.dead)
	l.conn.CloseNow()
	u.emit(linkEvent{kind: linkDown})
	if websocket.CloseStatus(err) == websocket.StatusCode(wire.CloseProtocol) {
		return fmt.Errorf("%w: %v", errRejected, err)
	}
	u.log.Info("wrapper: relay link lost", "err", err)
	return err
}

func (u *uplink) emit(ev linkEvent) {
	select {
	case u.events <- ev:
	case <-u.ctx.Done():
	}
}

// eventsC is nil without a relay, so the session loop's select never fires.
func (u *uplink) eventsC() <-chan linkEvent {
	if u == nil {
		return nil
	}
	return u.events
}

// send enqueues without blocking. With no link the frame is dropped. A full
// queue means the link is unhealthy, so send closes it, and the reconnect
// makes the gap visible as an offline period instead of silently losing frames.
func (u *uplink) send(m wire.Message) {
	if u == nil {
		return
	}
	l := u.cur.Load()
	if l == nil {
		return
	}
	select {
	case l.out <- wire.Encode(m):
	default:
		u.log.Warn("wrapper: relay link fell behind; reconnecting")
		go l.conn.CloseNow()
	}
}

// shutdown stops redialing, lets the current link flush every queued frame and
// close with 1000, and waits up to wait for that. It returns the reason the
// relay refused this wrapper, if it did.
func (u *uplink) shutdown(wait time.Duration) error {
	if u == nil {
		return nil
	}
	u.stop()
	if l := u.cur.Load(); l != nil {
		select {
		case l.out <- nil:
		default:
			go l.conn.CloseNow()
		}
	}
	select {
	case <-u.done:
		return u.rejected
	case <-time.After(wait):
		if l := u.cur.Load(); l != nil {
			l.conn.CloseNow()
		}
		return nil
	}
}

func (l *link) write() {
	for {
		var msg []byte
		select {
		case <-l.dead:
			return
		case msg = <-l.out:
		}
		if msg == nil {
			l.conn.Close(websocket.StatusNormalClosure, "")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), writeWait)
		err := l.conn.Write(ctx, websocket.MessageBinary, msg)
		cancel()
		if err != nil {
			l.conn.CloseNow()
			return
		}
	}
}

// keepalive drops a link whose relay stops answering pings, so a half-open TCP
// connection turns into a reconnect instead of a silent outage.
func (l *link) keepalive() {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-l.dead:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), pongWait)
		err := l.conn.Ping(ctx)
		cancel()
		if err != nil {
			l.conn.CloseNow()
			return
		}
	}
}

// backoff starts at minRedial, doubles to maxRedial, and adds ±20 % jitter.
type backoff struct{ step time.Duration }

func (b *backoff) next() time.Duration {
	b.step = min(max(2*b.step, minRedial), maxRedial)
	return time.Duration(float64(b.step) * (0.8 + 0.4*rand.Float64()))
}
