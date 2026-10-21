package inbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	partPrefix  = ".agentwatch-"
	partSuffix  = ".part"
	maxNameLen  = 120
	maxExtLen   = 16
	maxNumbered = 1000
)

func safeName(raw string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	name := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, raw)
	name = strings.TrimLeft(name, ".") // no hidden files, no "..", no ".agentwatch-*"
	if len(name) > maxNameLen {
		ext := filepath.Ext(name)
		if len(ext) > maxExtLen {
			ext = ""
		}
		name = truncate(strings.TrimSuffix(name, ext), maxNameLen-len(ext)) + ext
	}
	if name == "" {
		return "upload"
	}
	return name
}

// truncate cuts valid UTF-8 s to at most n bytes without splitting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// numbered inserts -i before the last extension, so "a.tar.gz" becomes "a.tar-1.gz".
func numbered(name string, i int) string {
	if i == 0 {
		return name
	}
	ext := filepath.Ext(name)
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext)
}

// placeNoClobber gives the verified temp file its final name without ever
// replacing an existing file. It tries name, name-1, name-2, and so on up to
// name-999. os.Link fails with EEXIST on any existing entry, a symlink
// included, so a planted link is never followed. Any other Link error is
// final. The caller fails the transfer rather than fall back to a rename that
// could clobber.
func placeNoClobber(tmp, dir, name string) (string, error) {
	for i := 0; i < maxNumbered; i++ {
		cand := filepath.Join(dir, numbered(name, i))
		err := os.Link(tmp, cand)
		if err == nil {
			os.Remove(tmp)
			return cand, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("%s and %d numbered variants exist", name, maxNumbered-1)
}

// sweepStale removes .part files whose writer is provably gone.
func sweepStale(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, partPrefix) || !strings.HasSuffix(name, partSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > staleAfter {
			os.Remove(filepath.Join(dir, name))
		}
	}
	return nil
}
