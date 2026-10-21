package wrapper_test

import (
	"crypto/sha256"
	"testing"

	"github.com/ericfitz/agentwatch/internal/inbox"
	"github.com/ericfitz/agentwatch/internal/wrapper"
)

func TestAnnounceRendersEveryPlaceholderAndEscape(t *testing.T) {
	saved := inbox.Saved{Path: "/in/a b.txt", Name: "a b.txt", Size: 3, SHA256: sha256.Sum256([]byte("abc"))}
	for tmpl, want := range map[string]string{
		wrapper.DefaultAnnounce:              "/in/a b.txt ",
		`echo got {name}\r`:                  "echo got a b.txt\r",
		`{size}\t{sha256}\n`:                 "3\tba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad\n",
		`\{path} \\ {path}`:                  "{path} \\ /in/a b.txt",
		`}{name}}`:                           "}a b.txt}",
		"":                                   "",
		`cat {path} | head; # {name} {size}`: "cat /in/a b.txt | head; # a b.txt 3",
	} {
		a, err := wrapper.ParseAnnounce(tmpl)
		if err != nil {
			t.Errorf("ParseAnnounce(%q): %v", tmpl, err)
			continue
		}
		if got := string(a.Render(saved)); got != want {
			t.Errorf("Render(%q) = %q, want %q", tmpl, got, want)
		}
	}
}

func TestAnnounceRejectsUnknownPlaceholdersAndEscapes(t *testing.T) {
	for _, tmpl := range []string{`{file}`, `{path`, `\x`, `trailing\`, `{}`} {
		if _, err := wrapper.ParseAnnounce(tmpl); err == nil {
			t.Errorf("ParseAnnounce(%q) succeeded", tmpl)
		}
	}
}
