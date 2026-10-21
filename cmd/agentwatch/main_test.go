package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericfitz/agentwatch/internal/wire"
)

func TestRunFlagsBecomeConfig(t *testing.T) {
	opts, err := parseRunFlags([]string{"--record-input", "--name", "api", "--", "claude", "--resume"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := opts.cfg
	if !cfg.RecordInput || cfg.Name != "api" || strings.Join(cfg.Command, " ") != "claude --resume" {
		t.Fatalf("config %+v", cfg)
	}
	if cfg.Record == "" || cfg.Relay != nil || !strings.HasSuffix(cfg.Record, "agentwatch/sessions/") {
		t.Fatalf("record %q, relay %v", cfg.Record, cfg.Relay)
	}

	opts, err = parseRunFlags([]string{"--no-record", "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.cfg.RecordInput || opts.cfg.Record != "" || !strings.HasPrefix(opts.cfg.Name, "sh@") {
		t.Fatalf("config %+v", opts.cfg)
	}

	for _, args := range [][]string{
		{},
		{"--no-record"},
	} {
		if _, err := parseRunFlags(args); err == nil {
			t.Errorf("parseRunFlags(%q) succeeded", args)
		}
	}
}

func TestWrapperTokenComesFromEnvOrFileNeverAFlag(t *testing.T) {
	tok := wire.NewToken()
	raw := strings.TrimPrefix(tok.Bearer(), "Bearer ")
	args := []string{"--relay", "https://relay.example:8443", "sh"}

	t.Setenv(wrapperTokenEnv, "")
	if _, err := parseRunFlags(args); err == nil {
		t.Fatal("a relay without a token was accepted")
	}

	t.Setenv(wrapperTokenEnv, raw)
	opts, err := parseRunFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.cfg.Relay.Token.Matches(raw) || opts.cfg.Relay.URL.Host != "relay.example:8443" {
		t.Fatalf("relay %+v", opts.cfg.Relay)
	}

	other := wire.NewToken()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(strings.TrimPrefix(other.Bearer(), "Bearer ")+"\n"), 0o600)
	opts, err = parseRunFlags(append([]string{"--token-file", file}, args...))
	if err != nil {
		t.Fatal(err)
	}
	if !opts.cfg.Relay.Token.Matches(strings.TrimPrefix(other.Bearer(), "Bearer ")) {
		t.Fatal("--token-file did not override the environment")
	}
}

func TestRelayFlags(t *testing.T) {
	t.Setenv(wrapperTokenEnv, strings.TrimPrefix(wire.NewToken().Bearer(), "Bearer "))
	t.Setenv(viewerTokenEnv, strings.TrimPrefix(wire.NewToken().Bearer(), "Bearer "))
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"--tls-cert", "c.pem", "--tls-key", "k.pem"}, true},
		{[]string{"--plain-http", "--listen", "127.0.0.1:8080"}, true},
		{[]string{"--plain-http", "--listen", "localhost:8080"}, true},
		{[]string{"--plain-http", "--listen", ":8080"}, false},
		{[]string{"--plain-http", "--listen", "0.0.0.0:8080"}, false},
		{[]string{"--plain-http", "--listen", "10.0.0.5:8080"}, false},
		{[]string{}, false},
	} {
		_, err := parseRelayFlags(tc.args)
		if (err == nil) != tc.ok {
			t.Errorf("parseRelayFlags(%q): err %v, want ok=%v", tc.args, err, tc.ok)
		}
	}

	t.Setenv(viewerTokenEnv, "")
	if _, err := parseRelayFlags([]string{"--plain-http", "--listen", "127.0.0.1:8080"}); err == nil {
		t.Error("a relay without a viewer token was accepted")
	}
}
