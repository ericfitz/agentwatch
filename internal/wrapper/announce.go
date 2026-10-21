package wrapper

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ericfitz/agentwatch/internal/inbox"
)

const DefaultAnnounce = "{path} "

// Announce is a parsed --announce template. The default types the saved path
// at the prompt without pressing Enter, the way dropping a file onto a terminal
// window does. The zero value announces nothing.
type Announce struct{ parts []func(inbox.Saved) string }

var announceFields = map[string]func(inbox.Saved) string{
	"path":   func(f inbox.Saved) string { return f.Path },
	"name":   func(f inbox.Saved) string { return f.Name },
	"size":   func(f inbox.Saved) string { return strconv.FormatInt(f.Size, 10) },
	"sha256": func(f inbox.Saved) string { return f.SHA256.String() },
}

var announceEscapes = map[byte]byte{'r': '\r', 'n': '\n', 't': '\t', '\\': '\\', '{': '{'}

// ParseAnnounce rejects unknown placeholders and bad escapes.
func ParseAnnounce(tmpl string) (Announce, error) {
	var a Announce
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			s := lit.String()
			a.parts = append(a.parts, func(inbox.Saved) string { return s })
			lit.Reset()
		}
	}
	for i := 0; i < len(tmpl); i++ {
		switch c := tmpl[i]; c {
		case '\\':
			if i+1 == len(tmpl) {
				return Announce{}, fmt.Errorf("announce template ends in a lone backslash")
			}
			e, ok := announceEscapes[tmpl[i+1]]
			if !ok {
				return Announce{}, fmt.Errorf("announce template has unknown escape \\%c", tmpl[i+1])
			}
			lit.WriteByte(e)
			i++
		case '{':
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 {
				return Announce{}, fmt.Errorf("announce template has an unclosed {")
			}
			name := tmpl[i+1 : i+end]
			field, ok := announceFields[name]
			if !ok {
				return Announce{}, fmt.Errorf("announce template has unknown placeholder {%s}; use {path}, {name}, {size}, or {sha256}", name)
			}
			flush()
			a.parts = append(a.parts, field)
			i += end
		default:
			lit.WriteByte(c)
		}
	}
	flush()
	return a, nil
}

// Render fills the template from a verified file.
func (a Announce) Render(f inbox.Saved) []byte {
	var b []byte
	for _, part := range a.parts {
		b = append(b, part(f)...)
	}
	return b
}
