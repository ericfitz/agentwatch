package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericfitz/agentwatch/internal/wire"
)

func TestRunFlagsBecomeConfig(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	opts, err := parseRunFlags([]string{"--record-input", "--max-file-size", "2MiB", "--name", "api", "--", "claude", "--resume"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := opts.cfg
	if !cfg.RecordInput || cfg.MaxFileBytes != 2<<20 || cfg.Name != "api" || strings.Join(cfg.Command, " ") != "claude --resume" {
		t.Fatalf("config %+v", cfg)
	}
	if cfg.Inbox != filepath.Join(dir, "agentwatch-inbox") || cfg.Record == "" || cfg.Relay != nil {
		t.Fatalf("inbox %q, record %q, relay %v", cfg.Inbox, cfg.Record, cfg.Relay)
	}

	opts, err = parseRunFlags([]string{"--no-record", "--no-inbox", "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.cfg.RecordInput || opts.cfg.Record != "" || opts.cfg.Inbox != "" || !strings.HasPrefix(opts.cfg.Name, "sh@") {
		t.Fatalf("config %+v", opts.cfg)
	}

	for _, args := range [][]string{
		{},
		{"--max-file-size", "10MB", "sh"},
		{"--announce", "{file}", "sh"},
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

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"100MiB": 100 << 20, "1GiB": 1 << 30, "64KiB": 64 << 10, "512": 512} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1KiB", "10MB", "MiB", "1.5GiB", "99999999999GiB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) succeeded", in)
		}
	}
}
