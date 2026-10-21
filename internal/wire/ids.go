package wire

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	minTokenLen  = 32
	sessionIDLen = 16
	redacted     = "[redacted]"
)

var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Token is a role credential of at least 32 characters from [A-Za-z0-9_-].
// That charset is also legal inside a WebSocket subprotocol token.
type Token struct{ s string }

// ParseToken errors never quote s, so they are safe to log.
func ParseToken(s string) (Token, error) {
	if len(s) < minTokenLen {
		return Token{}, fmt.Errorf("token is %d characters, want at least %d", len(s), minTokenLen)
	}
	if i := strings.IndexFunc(s, func(r rune) bool { return !isTokenChar(r) }); i >= 0 {
		return Token{}, fmt.Errorf("token has a character outside [A-Za-z0-9_-] at offset %d", i)
	}
	return Token{s}, nil
}

// NewToken returns 32 random bytes as 43 characters of unpadded base64url.
func NewToken() Token { return Token{base64.RawURLEncoding.EncodeToString(random(32))} }

func (t Token) String() string   { return redacted } // %v never leaks it
func (t Token) GoString() string { return redacted } // nor does %#v
func (t Token) Bearer() string   { return "Bearer " + t.s }
func (t Token) IsZero() bool     { return t.s == "" }

// Matches compares in constant time. A zero Token matches nothing.
func (t Token) Matches(candidate string) bool {
	return !t.IsZero() && subtle.ConstantTimeCompare([]byte(t.s), []byte(candidate)) == 1
}

func isTokenChar(r rune) bool {
	return 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '-' || r == '_'
}

// SessionSecret is 32 random bytes the wrapper makes at start and keeps in
// memory. JSON form is unpadded base64url. String() is redacted.
type SessionSecret [32]byte

func NewSessionSecret() SessionSecret {
	var s SessionSecret
	copy(s[:], random(len(s)))
	return s
}

// ID is the first 10 bytes of sha256(secret) as 16 lowercase base32 characters.
func (s SessionSecret) ID() SessionID {
	sum := sha256.Sum256(s[:])
	return SessionID(lowerBase32.EncodeToString(sum[:10]))
}

func (s SessionSecret) String() string   { return redacted }
func (s SessionSecret) GoString() string { return redacted }

func (s SessionSecret) MarshalText() ([]byte, error) {
	return []byte(base64.RawURLEncoding.EncodeToString(s[:])), nil
}

func (s *SessionSecret) UnmarshalText(b []byte) error {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(string(b))
	if err != nil || len(raw) != len(s) {
		return errors.New("session secret is not 32 bytes of unpadded base64url")
	}
	copy(s[:], raw)
	return nil
}

type SessionID string

func ParseSessionID(s string) (SessionID, error) {
	if len(s) != sessionIDLen || strings.IndexFunc(s, func(r rune) bool { return !('a' <= r && r <= 'z' || '2' <= r && r <= '7') }) >= 0 {
		return "", fmt.Errorf("session id %q is not %d lowercase base32 characters", s, sessionIDLen)
	}
	return SessionID(s), nil
}

type TransferID [16]byte // 32 lowercase hex chars in JSON, raw 16 bytes inside Chunk
type Digest [32]byte     // 64 lowercase hex chars in JSON

func (id TransferID) String() string                { return hex.EncodeToString(id[:]) } // also names the .part file
func (id TransferID) MarshalText() ([]byte, error)  { return []byte(id.String()), nil }
func (id *TransferID) UnmarshalText(b []byte) error { return parseLowerHex(id[:], b) }

func (d Digest) String() string                { return hex.EncodeToString(d[:]) }
func (d Digest) MarshalText() ([]byte, error)  { return []byte(d.String()), nil }
func (d *Digest) UnmarshalText(b []byte) error { return parseLowerHex(d[:], b) }

func parseLowerHex(dst, src []byte) error {
	if len(src) != hex.EncodedLen(len(dst)) {
		return fmt.Errorf("want %d hex characters, got %d", hex.EncodedLen(len(dst)), len(src))
	}
	for _, c := range src {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return fmt.Errorf("%q is not lowercase hex", src)
		}
	}
	_, err := hex.Decode(dst, src)
	return err
}

func random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("wire: crypto/rand failed: " + err.Error())
	}
	return b
}
