package wrapper

import (
	"strings"
	"testing"

	"github.com/ericfitz/agentwatch/internal/wire"
)

func TestChildEnvAdvertisesTheBrowserTerminal(t *testing.T) {
	env := childEnv([]string{
		"TERM=dumb",
		"COLORTERM=no",
		"NO_COLOR=1",
		"SHELL=/bin/bash",
		"FOO=bar",
		"AGENTWATCH_WRAPPER_TOKEN=secret",
	}, wire.SessionID("abcdefghijklmnop"))
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Fatalf("duplicate %s", k)
		}
		got[k] = v
	}
	if got["TERM"] != "xterm-256color" || got["COLORTERM"] != "truecolor" {
		t.Fatalf("browser terminal is TERM=%s COLORTERM=%s", got["TERM"], got["COLORTERM"])
	}
	if got["NO_COLOR"] != "1" || got["FOO"] != "bar" || got["SHELL"] != "/bin/bash" {
		t.Fatalf("parent env was rewritten: %#v", got)
	}
	if _, ok := got["AGENTWATCH_WRAPPER_TOKEN"]; ok {
		t.Fatal("wrapper token leaked into the child")
	}
	if got["AGENTWATCH_SESSION"] != "abcdefghijklmnop" {
		t.Fatalf("session env = %#v", got)
	}
}
