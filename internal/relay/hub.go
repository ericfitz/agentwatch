package relay

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/coder/websocket"

	"github.com/ericfitz/agentwatch/internal/wire"
)

const (
	tailBytes     = 128 << 10
	tailAlign     = 4 << 10
	viewerQueue   = 512
	offlineGrace  = 2 * time.Minute
	helloDeadline = 10 * time.Second
	wrapperWrite  = 10 * time.Second
	pingEvery     = 15 * time.Second
	pongWait      = 10 * time.Second

	closeGoingAway = wire.CloseCode(websocket.StatusGoingAway)
)

// hub is one session. It is online while wrapper is set, offline while expire
// is set, and ended once it leaves Relay.sessions. Relay.mu guards every field.
type hub struct {
	info    wire.SessionInfo // from Hello; list fills in Online and Viewers
	wrapper *peer
	viewers map[*peer]struct{}
	active  *peer      // viewer whose size the wrapper last received
	tail    tailBuffer // replay for late joiners
	size    []byte     // last SizeChanged frame, verbatim
	exit    []byte     // Exit frame once the process ended
	expire  *time.Timer
}

type peer struct {
	conn    *websocket.Conn
	send    chan []byte    // viewers only; never closed, a nil entry ends the writer
	closing wire.CloseCode // set under Relay.mu before the nil entry is queued
	resize  []byte         // viewers only: last ResizeRequest frame, verbatim
}

// bind makes p the wrapper of the session its Hello names, creating the
// session or replacing an older connection, which the caller closes. It
// returns a nil hub once the relay is closed.
func (rl *Relay) bind(hello wire.Hello, p *peer) (h *hub, old *peer) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.closed {
		return nil, nil
	}
	id := hello.Secret.ID()
	h = rl.sessions[id]
	if h == nil {
		h = &hub{viewers: map[*peer]struct{}{}}
		rl.sessions[id] = h
	}
	h.info = wire.SessionInfo{ID: id, Name: hello.Name, Host: hello.Host, Command: hello.Command, Started: hello.Started}
	// A new link starts at the wrapper's local size, so the next typist must
	// resend its own.
	old, h.wrapper, h.active = h.wrapper, p, nil
	if h.expire != nil {
		h.expire.Stop()
		h.expire = nil
	}
	h.fanout(h.presence())
	return h, old
}

// unbind runs when p's connection ends. A replaced connection changes nothing.
// After an Exit the session ends; otherwise it waits rl.grace for a reconnect.
func (rl *Relay) unbind(h *hub, p *peer) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if h.wrapper != p {
		return
	}
	h.wrapper = nil
	if h.exit != nil {
		rl.remove(h, wire.CloseEnded)
		return
	}
	h.fanout(h.presence())
	var t *time.Timer
	t = time.AfterFunc(rl.grace, func() {
		// Read t only under mu: unbind holds mu while it assigns t.
		rl.mu.Lock()
		defer rl.mu.Unlock()
		if h.expire == t {
			rl.cfg.Log.Info("relay: session expired", "session", h.info.ID)
			rl.remove(h, wire.CloseEnded)
		}
	})
	h.expire = t
}

// remove ends the session. Each viewer gets code after the frames already
// queued for it. A second call finds nothing left to do.
func (rl *Relay) remove(h *hub, code wire.CloseCode) {
	if rl.sessions[h.info.ID] == h {
		delete(rl.sessions, h.info.ID)
	}
	if h.expire != nil {
		h.expire.Stop()
		h.expire = nil
	}
	for v := range h.viewers {
		v.end(code)
	}
	h.viewers = nil
}

// attach queues the snapshot (size, replay, exit) for v and adds it to the
// session. On failure it returns the close code for v.
func (rl *Relay) attach(id wire.SessionID, v *peer) (*hub, wire.CloseCode) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.closed {
		return nil, closeGoingAway
	}
	h := rl.sessions[id]
	if h == nil {
		return nil, wire.CloseNotFound
	}
	for _, f := range [][]byte{h.size, h.tail.frame(), h.exit} {
		if f != nil {
			v.send <- f
		}
	}
	h.viewers[v] = struct{}{}
	h.fanout(h.presence())
	return h, 0
}

func (rl *Relay) detach(h *hub, v *peer) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(h.viewers, v)
	if h.active == v {
		h.active = nil
	}
	h.fanout(h.presence())
}

func (rl *Relay) publish(h *hub, from *peer, kind wire.Kind, msg []byte) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	// A replaced connection can still hand over a frame it read before bind.
	if h.wrapper != from {
		return
	}
	switch kind {
	case wire.KindOutput:
		h.tail.write(msg[1:])
	case wire.KindSizeChanged:
		h.size = msg
	case wire.KindExit:
		h.exit = msg
	}
	h.fanout(msg)
}

// claim turns many viewers into one remote source for the wrapper. Before a
// viewer's first keystroke after another viewer held the size, it returns that
// viewer's last ResizeRequest as pre. dst is nil while the wrapper is offline.
func (rl *Relay) claim(h *hub, v *peer, kind wire.Kind, msg []byte) (dst *peer, pre []byte) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	switch {
	case kind == wire.KindResizeRequest:
		v.resize = msg
		h.active = v
	case kind == wire.KindInput && h.active != v:
		pre = v.resize
		h.active = v
	}
	return h.wrapper, pre
}

func (rl *Relay) list() []wire.SessionInfo {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	out := make([]wire.SessionInfo, 0, len(rl.sessions))
	for _, h := range rl.sessions {
		info := h.info
		info.Online = h.wrapper != nil
		info.Viewers = len(h.viewers)
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b wire.SessionInfo) int {
		return cmp.Or(a.Started.Compare(b.Started), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// fanout queues msg for every viewer without blocking. A viewer whose queue
// is full is dropped with 4408, so one slow browser never stalls the wrapper
// or the other viewers.
func (h *hub) fanout(msg []byte) {
	for v := range h.viewers {
		select {
		case v.send <- msg:
		default:
			delete(h.viewers, v)
			v.end(wire.CloseTooSlow)
		}
	}
}

func (h *hub) presence() []byte {
	return wire.Encode(wire.Presence{WrapperOnline: h.wrapper != nil, Viewers: len(h.viewers)})
}

// end closes v with code after the frames already queued, or at once when its
// queue is full.
func (v *peer) end(code wire.CloseCode) {
	v.closing = code
	select {
	case v.send <- nil:
	default:
		go closeConn(v.conn, code, "")
	}
}

func (v *peer) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-v.send:
			if msg == nil {
				closeConn(v.conn, v.closing, "")
				return
			}
			if v.conn.Write(ctx, websocket.MessageBinary, msg) != nil {
				return
			}
		}
	}
}

// forward writes each non-nil frame to the wrapper. The timeout is not tied to
// the viewer's context, because a write context that ends mid-write closes the
// wrapper's connection. A real timeout closes it on purpose: the link is
// unhealthy and the wrapper will reconnect.
func (p *peer) forward(frames ...[]byte) {
	for _, f := range frames {
		if f == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), wrapperWrite)
		err := p.conn.Write(ctx, websocket.MessageBinary, f)
		cancel()
		if err != nil {
			return
		}
	}
}

// tailBuffer keeps the last tailBytes of output. A replay starts at a line
// when one begins within tailAlign bytes of the cut. The buffer compacts only
// after growing to twice the cap.
type tailBuffer struct{ buf []byte }

func (t *tailBuffer) write(p []byte) {
	t.buf = append(t.buf, p...)
	if len(t.buf) >= 2*tailBytes {
		t.buf = append(t.buf[:0], t.last()...)
	}
}

func (t *tailBuffer) last() []byte {
	cut := len(t.buf) - tailBytes
	if cut <= 0 {
		return t.buf
	}
	if i := bytes.IndexByte(t.buf[cut-1:cut-1+tailAlign], '\n'); i >= 0 {
		cut += i
	}
	return t.buf[cut:]
}

// frame returns the replay as one Output frame, or nil before any output.
func (t *tailBuffer) frame() []byte {
	if len(t.buf) == 0 {
		return nil
	}
	return append([]byte{byte(wire.KindOutput)}, t.last()...)
}
