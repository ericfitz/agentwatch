package wrapper

import (
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/ericfitz/agentwatch/internal/wire"
)

// localTerm is the operator's terminal. It exists only when Stdin is a TTY,
// which is the only case where the wrapper changes terminal modes.
type localTerm struct {
	f     *os.File
	saved *term.State
}

func openLocal(f *os.File) (*localTerm, error) {
	if f == nil || !term.IsTerminal(int(f.Fd())) {
		return nil, nil
	}
	saved, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	return &localTerm{f: f, saved: saved}, nil
}

// size is zero when headless or when the terminal reports nothing usable.
func (l *localTerm) size() wire.TermSize {
	if l == nil {
		return wire.TermSize{}
	}
	cols, rows, err := term.GetSize(int(l.f.Fd()))
	if err != nil || cols < 1 || rows < 1 {
		return wire.TermSize{}
	}
	return wire.TermSize{Cols: uint16(min(cols, 1000)), Rows: uint16(min(rows, 1000))}
}

func (l *localTerm) restore() {
	if l != nil {
		term.Restore(int(l.f.Fd()), l.saved)
	}
}

// readInput closes out when the terminal reaches EOF.
func (l *localTerm) readInput(out chan<- []byte, quit <-chan struct{}) {
	defer close(out)
	buf := make([]byte, 4<<10)
	for {
		n, err := l.f.Read(buf)
		if n > 0 && !deliver(out, append([]byte(nil), buf[:n]...), quit) {
			return
		}
		if err != nil {
			return
		}
	}
}

func (l *localTerm) watchSize(out chan<- wire.TermSize, quit <-chan struct{}) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	for {
		select {
		case <-quit:
			return
		case <-winch:
		}
		if sz := l.size(); sz != (wire.TermSize{}) && !deliver(out, sz, quit) {
			return
		}
	}
}

// readPTY closes out at EOF or EIO, which is when every holder of the PTY's
// terminal side has exited.
func readPTY(ptmx io.Reader, out chan<- []byte, quit <-chan struct{}) {
	defer close(out)
	buf := make([]byte, 32<<10)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 && !deliver(out, append([]byte(nil), buf[:n]...), quit) {
			return
		}
		if err != nil {
			return
		}
	}
}

func deliver[T any](out chan<- T, v T, quit <-chan struct{}) bool {
	select {
	case out <- v:
		return true
	case <-quit:
		return false
	}
}

// ptyWriter serializes keyboard input, viewer input, and announcements onto the
// PTY. enqueue never blocks: a blocked PTY write must not stall the loop that
// drains PTY output, or a child that stops reading stdin deadlocks the session.
type ptyWriter struct {
	mu     sync.Mutex
	ready  sync.Cond
	queue  [][]byte
	closed bool
}

func startPTYWriter(dst io.Writer) *ptyWriter {
	w := &ptyWriter{}
	w.ready.L = &w.mu
	go w.run(dst)
	return w
}

// enqueue takes ownership of p.
func (w *ptyWriter) enqueue(p []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.queue = append(w.queue, p)
		w.ready.Signal()
	}
}

// close drops anything still queued.
func (w *ptyWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.queue = nil
	w.ready.Signal()
}

func (w *ptyWriter) run(dst io.Writer) {
	for {
		w.mu.Lock()
		for len(w.queue) == 0 && !w.closed {
			w.ready.Wait()
		}
		if w.closed {
			w.mu.Unlock()
			return
		}
		p := w.queue[0]
		w.queue = w.queue[1:]
		w.mu.Unlock()
		if _, err := dst.Write(p); err != nil {
			w.close()
			return
		}
	}
}

func winsize(s wire.TermSize) *pty.Winsize {
	return &pty.Winsize{Cols: s.Cols, Rows: s.Rows}
}

// exitStatus reports a signal death as 128+n, the way a shell does.
func exitStatus(ps *os.ProcessState) wire.Exit {
	if ps == nil {
		return wire.Exit{Code: 1}
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return wire.Exit{Code: 128 + int(ws.Signal()), Signal: unix.SignalName(ws.Signal())}
	}
	return wire.Exit{Code: ps.ExitCode()}
}

var scrubbedEnv = map[string]bool{
	"AGENTWATCH_WRAPPER_TOKEN": true,
	"AGENTWATCH_VIEWER_TOKEN":  true,
	"AGENTWATCH_SESSION":       true,
	"AGENTWATCH_INBOX":         true,
}

// The PTY is a new terminal whose viewer is xterm.js. TERM and COLORTERM
// describe that viewer, not the terminal that launched the wrapper.
const (
	browserTERM      = "xterm-256color"
	browserCOLORTERM = "truecolor"
)

// childEnv removes both role tokens, so a wrapped agent cannot read a
// credential that drives other hosts' agents through the relay.
func childEnv(base []string, id wire.SessionID, inboxDir string) []string {
	env := make([]string, 0, len(base)+4)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if scrubbedEnv[k] || k == "TERM" || k == "COLORTERM" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "AGENTWATCH_SESSION="+string(id))
	if inboxDir != "" {
		env = append(env, "AGENTWATCH_INBOX="+inboxDir)
	}
	env = append(env, "TERM="+browserTERM, "COLORTERM="+browserCOLORTERM)
	return env
}
