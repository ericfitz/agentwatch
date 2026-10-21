// Command agentwatch runs a program in a PTY that browsers can drive through a
// relay, and serves that relay.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/ericfitz/agentwatch/internal/relay"
	"github.com/ericfitz/agentwatch/internal/wire"
	"github.com/ericfitz/agentwatch/internal/wrapper"
)

const (
	wrapperTokenEnv = "AGENTWATCH_WRAPPER_TOKEN"
	viewerTokenEnv  = "AGENTWATCH_VIEWER_TOKEN"

	runArgs   = "[flags] -- COMMAND [ARGS...]"
	relayArgs = "[flags]"
	tokenArgs = ""
)

var commands = []struct {
	name, args, summary string
	run                 func(args []string) int
}{
	{"run", runArgs, "Run COMMAND in a PTY, record it, and link it to a relay.", cmdRun},
	{"relay", relayArgs, "Serve the relay and the browser client.", cmdRelay},
	{"token", tokenArgs, "Print a new random token.", cmdToken},
}

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(args[1:])
		}
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(os.Stdout)
		return 0
	}
	fmt.Fprintf(os.Stderr, "agentwatch: unknown command %q\n\n", args[0])
	usage(os.Stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %s\n      %s\n", synopsis(c.name, c.args), c.summary)
	}
	fmt.Fprintln(w, "\nRun 'agentwatch COMMAND -h' for a command's flags.")
}

func synopsis(name, args string) string {
	return strings.TrimSpace("agentwatch " + name + " " + args)
}

func newFlagSet(name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet("agentwatch "+name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage:", synopsis(name, args))
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags prints usage to stdout and returns flag.ErrHelp on -h. Other
// errors are left for the caller to print once.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(os.Stdout)
		fs.Usage()
	}
	return err
}

func usageError(cmd string, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
	return 2
}

func cmdToken(args []string) int {
	if err := parseFlags(newFlagSet("token", tokenArgs), args); err != nil {
		return usageError("agentwatch token", err)
	}
	fmt.Println(strings.TrimPrefix(wire.NewToken().Bearer(), "Bearer "))
	return 0
}

type runOptions struct {
	cfg     wrapper.Config // cmdRun sets Stdin, Stdout, and Log
	logPath string
}

func parseRunFlags(args []string) (runOptions, error) {
	fs := newFlagSet("run", runArgs)
	relayURL := fs.String("relay", "", "relay base `URL`: https, or http on a loopback host. Without it the session is local and recorded.")
	tokenFile := fs.String("token-file", "", "`PATH` of a file holding the wrapper token (default $"+wrapperTokenEnv+")")
	caFile := fs.String("ca-file", "", "extra CA certificate `PATH` to trust for the relay")
	name := fs.String("name", "", "session `NAME` in the session list (default COMMAND@HOSTNAME)")
	record := fs.String("record", defaultRecordDir(), "asciicast destination `PATH`; a directory gets one new file per session")
	noRecord := fs.Bool("no-record", false, "turn recording off")
	recordInput := fs.Bool("record-input", false, "also record keystrokes as \"i\" events; keystrokes can contain secrets")
	logPath := fs.String("log", "", "diagnostics `PATH` (default stderr when headless, discarded when interactive)")
	if err := parseFlags(fs, args); err != nil {
		return runOptions{}, err
	}

	cfg := wrapper.Config{Command: fs.Args(), Name: *name, RecordInput: *recordInput}
	if len(cfg.Command) == 0 {
		return runOptions{}, errors.New("no command given; usage: " + synopsis("run", runArgs))
	}
	if cfg.Name == "" {
		host, _ := os.Hostname()
		cfg.Name = filepath.Base(cfg.Command[0]) + "@" + host
	}
	if !*noRecord {
		if *record == "" {
			return runOptions{}, errors.New("no recording directory: set --record or pass --no-record")
		}
		cfg.Record = *record
	}
	var err error
	if *relayURL != "" {
		if cfg.Relay, err = relayTarget(*relayURL, *tokenFile, *caFile); err != nil {
			return runOptions{}, err
		}
	}
	return runOptions{cfg: cfg, logPath: *logPath}, nil
}

// relayTarget leaves the http-on-loopback rule to wrapper.Run, which enforces
// it for every caller.
func relayTarget(rawURL, tokenFile, caFile string) (*wrapper.Relay, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("--relay: %w", err)
	}
	tok, err := loadToken(tokenFile, wrapperTokenEnv)
	if err != nil {
		return nil, err
	}
	r := &wrapper.Relay{URL: u, Token: tok}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("--ca-file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--ca-file %s: no PEM certificates found", caFile)
		}
		r.TLS = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return r, nil
}

func cmdRun(args []string) int {
	opts, err := parseRunFlags(args)
	if err != nil {
		return usageError("agentwatch run", err)
	}
	log, err := openLog(opts.logPath, term.IsTerminal(int(os.Stdin.Fd())))
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentwatch run: --log:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cfg := opts.cfg
	cfg.Stdin, cfg.Stdout, cfg.Log = os.Stdin, os.Stdout, log
	code, err := wrapper.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentwatch:", err)
	}
	return code
}

// openLog keeps diagnostics off an interactive terminal, where they would
// garble the agent's screen.
func openLog(path string, interactive bool) (*slog.Logger, error) {
	var w io.Writer = os.Stderr
	switch {
	case path != "":
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		w = f
	case interactive:
		w = io.Discard
	}
	return slog.New(slog.NewTextHandler(w, nil)), nil
}

type relayOptions struct {
	listen, cert, key string
	plain             bool
	cfg               relay.Config
}

func parseRelayFlags(args []string) (relayOptions, error) {
	fs := newFlagSet("relay", relayArgs)
	listen := fs.String("listen", ":8443", "listen `ADDR`")
	cert := fs.String("tls-cert", "", "TLS certificate `PATH`")
	key := fs.String("tls-key", "", "TLS private key `PATH`")
	plain := fs.Bool("plain-http", false, "serve plain HTTP; allowed only when --listen is a loopback address, behind a TLS-terminating proxy")
	wrapperFile := fs.String("wrapper-token-file", "", "`PATH` of a file holding the token wrappers present (default $"+wrapperTokenEnv+")")
	viewerFile := fs.String("viewer-token-file", "", "`PATH` of a file holding the token browsers present (default $"+viewerTokenEnv+")")
	if err := parseFlags(fs, args); err != nil {
		return relayOptions{}, err
	}

	o := relayOptions{listen: *listen, cert: *cert, key: *key, plain: *plain}
	if o.plain {
		host, _, err := net.SplitHostPort(o.listen)
		if err != nil || !wire.PlaintextAllowed(host) {
			return relayOptions{}, fmt.Errorf("--plain-http needs a loopback --listen address such as 127.0.0.1:8080, not %q", o.listen)
		}
	} else if o.cert == "" || o.key == "" {
		return relayOptions{}, errors.New("--tls-cert and --tls-key are required; --plain-http is allowed only on a loopback address")
	}
	var err error
	if o.cfg.WrapperToken, err = loadToken(*wrapperFile, wrapperTokenEnv); err != nil {
		return relayOptions{}, err
	}
	if o.cfg.ViewerToken, err = loadToken(*viewerFile, viewerTokenEnv); err != nil {
		return relayOptions{}, err
	}
	return o, nil
}

func cmdRelay(args []string) int {
	opts, err := parseRelayFlags(args)
	if err != nil {
		return usageError("agentwatch relay", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	opts.cfg.Log = log
	rl, err := relay.New(opts.cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentwatch relay:", err)
		return 2
	}
	srv := &http.Server{Addr: opts.listen, Handler: rl, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		rl.Close()
		srv.Shutdown(context.Background())
	}()
	log.Info("relay: listening", "addr", opts.listen, "tls", !opts.plain)
	if opts.plain {
		err = srv.ListenAndServe()
	} else {
		err = srv.ListenAndServeTLS(opts.cert, opts.key)
	}
	if !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "agentwatch relay:", err)
		return 1
	}
	<-stopped
	return 0
}

// loadToken reads a token from path, or from env when path is empty. Tokens
// never come from flags, which other users can read in the process list.
func loadToken(path, env string) (wire.Token, error) {
	text, source := os.Getenv(env), "$"+env
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return wire.Token{}, err
		}
		text, source = strings.TrimSpace(string(b)), path
	}
	if text == "" {
		return wire.Token{}, fmt.Errorf("no token in $%s; set it or pass a token file (mint one with 'agentwatch token')", env)
	}
	tok, err := wire.ParseToken(text)
	if err != nil {
		return wire.Token{}, fmt.Errorf("%s: %w", source, err)
	}
	return tok, nil
}

// defaultRecordDir ends in a separator, which tells record.Open to make one
// new file per session in it.
func defaultRecordDir() string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "agentwatch", "sessions") + string(filepath.Separator)
}
