// Package wrapper runs a command in a PTY, records it, and links it to a relay
// so browsers can watch it, type into it, resize it, and send it files.
package wrapper

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/ericfitz/agentwatch/internal/inbox"
	"github.com/ericfitz/agentwatch/internal/record"
	"github.com/ericfitz/agentwatch/internal/wire"
)

type Config struct {
	Command      []string     // argv; Command[0] is resolved with exec.LookPath
	Name         string       // session list name
	Relay        *Relay       // nil: no remoting
	Record       string       // record.Open target; "" disables
	RecordInput  bool         // also write "i" events
	Inbox        string       // absolute receive dir; "" refuses uploads
	MaxFileBytes int64        // > 0 when Inbox is set
	Announce     Announce     // typed into the PTY after each verified file
	Stdin        *os.File     // local terminal; nil or not a TTY means headless
	Stdout       io.Writer    // local copy of PTY output; nil discards
	Log          *slog.Logger // nil discards
}

type Relay struct {
	URL   *url.URL // base URL: https, or http on a loopback host
	Token wire.Token
	TLS   *tls.Config // nil uses system roots
}

const (
	expireEvery  = 5 * time.Second
	drainWait    = time.Second // a background grandchild can hold the PTY open forever
	hangupWait   = 5 * time.Second
	shutdownWait = 2 * time.Second
	maxHelloArgs = 64
)

// session is the single owner of all mutable session state. Every other
// goroutine only turns blocking I/O into channel values.
type session struct {
	cfg    Config
	log    *slog.Logger
	stdout io.Writer
	cmd    *exec.Cmd
	ptmx   *os.File
	pty    *ptyWriter // the only writer to PTY input
	rec    *record.Recorder
	inbox  *inbox.Inbox
	up     *uplink    // nil without a relay
	local  *localTerm // nil when headless
	size   sizeArbiter

	ptyOut     chan []byte
	localIn    chan []byte
	localWinch chan wire.TermSize
	exited     chan wire.Exit
	quit       chan struct{} // closed by finish so I/O goroutines stop delivering

	exit    *wire.Exit // set once the child has exited
	drained bool       // PTY output reached EOF or EIO
}

// Run starts Command in a PTY and returns when it has exited and the session
// has been flushed. exitCode is the child's status as a process exit code
// (128+n for signal n), or 2 when setup failed before the child started,
// including a relay that rejected the token on the first dial. A non-nil err
// is something the operator must hear about. It is a setup failure, a relay
// that started rejecting the wrapper mid-session, or an incomplete recording.
// The child's code is still returned in the last two cases.
func Run(ctx context.Context, cfg Config) (exitCode int, err error) {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	if len(cfg.Command) == 0 {
		return 2, errors.New("no command to run")
	}
	path, err := exec.LookPath(cfg.Command[0])
	if err != nil {
		return 2, err
	}
	in, err := inbox.Open(inbox.Config{Dir: cfg.Inbox, MaxBytes: cfg.MaxFileBytes})
	if err != nil {
		return 2, err
	}
	secret := wire.NewSessionSecret()
	started := time.Now()
	host, _ := os.Hostname()
	hello := wire.Hello{
		Secret:  secret,
		Name:    cfg.Name,
		Host:    host,
		Command: cfg.Command[:min(len(cfg.Command), maxHelloArgs)],
		Started: started,
		Inbox:   in.Info(),
	}
	// The relay applies the same check, so a bad name fails here instead of
	// as a close code after the agent has started.
	if _, err := wire.Decode(wire.Encode(hello)); err != nil {
		return 2, err
	}

	var up *uplink
	if cfg.Relay != nil {
		if up, err = startUplink(ctx, cfg.Relay, hello, log); err != nil {
			return 2, err
		}
	}
	fail := func(err error) (int, error) {
		up.shutdown(0)
		return 2, err
	}

	local, err := openLocal(cfg.Stdin)
	if err != nil {
		return fail(err)
	}
	s := &session{
		cfg:        cfg,
		log:        log,
		stdout:     stdout,
		inbox:      in,
		up:         up,
		local:      local,
		size:       newSizeArbiter(local.size()),
		ptyOut:     make(chan []byte),
		localIn:    make(chan []byte),
		localWinch: make(chan wire.TermSize),
		exited:     make(chan wire.Exit, 1),
		quit:       make(chan struct{}),
	}
	initial := s.size.applied
	rec, recPath, err := record.Open(cfg.Record, record.Header{
		Width:   int(initial.Cols),
		Height:  int(initial.Rows),
		Started: started,
		Title:   cfg.Name,
		Command: strings.Join(cfg.Command, " "),
		Env:     map[string]string{"TERM": browserTERM, "SHELL": os.Getenv("SHELL")},
	})
	if err != nil {
		local.restore()
		return fail(err)
	}
	s.rec = rec

	s.cmd = &exec.Cmd{Path: path, Args: cfg.Command, Env: childEnv(os.Environ(), secret.ID(), cfg.Inbox)}
	if s.ptmx, err = pty.StartWithSize(s.cmd, winsize(initial)); err != nil {
		rec.Close()
		os.Remove(recPath)
		local.restore()
		return fail(err)
	}
	if recPath != "" {
		fmt.Fprintf(stdout, "agentwatch: recording to %s\r\n", recPath)
	}
	if cfg.Inbox != "" {
		fmt.Fprintf(stdout, "agentwatch: uploads go to %s\r\n", cfg.Inbox)
	}

	s.pty = startPTYWriter(s.ptmx)
	go readPTY(s.ptmx, s.ptyOut, s.quit)
	go func() {
		s.cmd.Wait()
		s.exited <- exitStatus(s.cmd.ProcessState)
	}()
	if local != nil {
		go local.readInput(s.localIn, s.quit)
		go local.watchSize(s.localWinch, s.quit)
	}
	return s.loop(ctx)
}

func (s *session) loop(ctx context.Context) (int, error) {
	expire := time.NewTicker(expireEvery)
	defer expire.Stop()
	done := ctx.Done()
	var drain, kill <-chan time.Time
	for {
		select {
		case b, ok := <-s.ptyOut:
			if !ok {
				s.ptyOut, s.drained = nil, true
				if s.exit != nil {
					return s.finish()
				}
				continue
			}
			now := time.Now()
			s.stdout.Write(b)
			s.rec.Output(now, b)
			s.up.send(wire.Output(b))
		case b, ok := <-s.localIn:
			if !ok {
				s.localIn = nil
				continue
			}
			s.applySize(s.size.observe(sourceLocal, nil))
			s.input(b)
		case sz := <-s.localWinch:
			s.applySize(s.size.observe(sourceLocal, &sz))
		case ev := <-s.up.eventsC():
			s.link(ev)
		case now := <-expire.C:
			for _, st := range s.inbox.Expire(now) {
				s.up.send(st)
			}
		case x := <-s.exited:
			s.exit = &x
			if s.drained {
				return s.finish()
			}
			drain = time.After(drainWait)
		case <-drain:
			return s.finish()
		case <-done:
			done = nil
			s.signal(syscall.SIGHUP)
			kill = time.After(hangupWait)
		case <-kill:
			s.signal(syscall.SIGKILL)
		}
	}
}

func (s *session) link(ev linkEvent) {
	now := time.Now()
	switch ev.kind {
	case linkUp:
		s.up.send(wire.SizeChanged{TermSize: s.size.applied})
		s.rec.Marker(now, "relay connected")
	case linkDown:
		// The statuses have nowhere to go; a sender restarts by offering again.
		s.inbox.AbortAll(wire.FailCancelled)
		s.applySize(s.size.remoteLost())
		s.rec.Marker(now, "relay disconnected")
	case linkFrame:
		s.handle(ev.msg)
	}
}

func (s *session) handle(m wire.Message) {
	switch m := m.(type) {
	case wire.Input:
		s.applySize(s.size.observe(sourceRemote, nil))
		s.input(m)
	case wire.ResizeRequest:
		s.applySize(s.size.observe(sourceRemote, &m.TermSize))
	case wire.TransferMessage:
		now := time.Now()
		st, saved := s.inbox.Handle(m, now)
		s.up.send(st)
		if saved != nil {
			s.pty.enqueue(s.cfg.Announce.Render(*saved))
			s.rec.Marker(now, "received "+saved.Path+" sha256="+saved.SHA256.String())
		}
	default:
		s.log.Warn("wrapper: relay sent a frame only viewers receive", "type", fmt.Sprintf("%T", m))
	}
}

func (s *session) input(b []byte) {
	if s.cfg.RecordInput {
		s.rec.Input(time.Now(), b)
	}
	s.pty.enqueue(b)
}

func (s *session) applySize(size wire.TermSize, changed bool) {
	if !changed {
		return
	}
	if err := pty.Setsize(s.ptmx, winsize(size)); err != nil {
		s.log.Warn("wrapper: resize failed", "err", err)
	}
	s.rec.Resize(time.Now(), int(size.Cols), int(size.Rows))
	s.up.send(wire.SizeChanged{TermSize: size})
}

// signal reaches the child's whole process group; the child leads its own
// session, so its group id is its pid.
func (s *session) signal(sig syscall.Signal) {
	if err := syscall.Kill(-s.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.log.Warn("wrapper: signal failed", "signal", sig, "err", err)
	}
}

// finish runs once the child has exited. Output frames, the final transfer
// statuses, and Exit share one queue, so Exit is the last frame the relay sees.
func (s *session) finish() (int, error) {
	close(s.quit)
	s.pty.close()
	s.ptmx.Close()
	now := time.Now()
	for _, st := range s.inbox.AbortAll(wire.FailCancelled) {
		s.up.send(st)
	}
	s.rec.Marker(now, fmt.Sprintf("exit %d", s.exit.Code))
	recErr := s.rec.Close()
	s.up.send(*s.exit)
	linkErr := s.up.shutdown(shutdownWait)
	s.local.restore()
	return s.exit.Code, errors.Join(recErr, linkErr)
}
