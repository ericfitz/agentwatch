// Package wire is the agentwatch v1 protocol.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode"
)

const (
	Subprotocol  = "agentwatch.v1"
	BearerPrefix = "agentwatch.bearer."
	WrapperPath  = "/api/wrapper"

	MaxMessage = 1 << 20
	MaxInput   = 64 << 10

	maxTermSide  = 1000
	maxHelloName = 128
	maxHelloArgs = 64
)

type Kind byte

const (
	KindHello         Kind = 0x01
	KindOutput        Kind = 0x02
	KindSizeChanged   Kind = 0x03
	KindExit          Kind = 0x04
	KindInput         Kind = 0x10
	KindResizeRequest Kind = 0x11
	KindPresence      Kind = 0x20
)

// Route says who may send a kind and where the relay sends it next.
type Route uint8

const (
	RouteHello Route = iota + 1
	RouteUp
	RouteDown
	RouteRelay
)

// payloadForm is how the bytes after the kind byte are laid out. ClientJS
// exports it per kind, so codec.js never keeps its own list of raw kinds.
type payloadForm string

const (
	payloadJSON payloadForm = "json"
	payloadRaw  payloadForm = "raw"
)

type kindSpec struct {
	name    string // JS name in window.AW.kind
	route   Route
	payload payloadForm
	encode  func(Message) []byte
	decode  func(payload []byte) (Message, error)
}

// kinds is the only list of frame kinds. Encode, Decode, PeekKind, and
// ClientJS read it. A kind that is not here does not exist on the wire.
var kinds = map[Kind]kindSpec{
	KindHello:         jsonKind("Hello", RouteHello, Hello.validate),
	KindOutput:        rawKind[Output]("Output", RouteUp, nil),
	KindSizeChanged:   jsonKind("SizeChanged", RouteUp, SizeChanged.validate),
	KindExit:          jsonKind[Exit]("Exit", RouteUp, nil),
	KindInput:         rawKind("Input", RouteDown, Input.validate),
	KindResizeRequest: jsonKind("ResizeRequest", RouteDown, ResizeRequest.validate),
	KindPresence:      jsonKind[Presence]("Presence", RouteRelay, nil),
}

func jsonKind[T Message](name string, route Route, check func(T) error) kindSpec {
	return kindSpec{
		name:    name,
		route:   route,
		payload: payloadJSON,
		encode: func(m Message) []byte {
			b, err := json.Marshal(m)
			if err != nil {
				panic(fmt.Sprintf("wire: encode %s: %v", name, err))
			}
			return b
		},
		decode: func(p []byte) (Message, error) {
			var m T
			if err := json.Unmarshal(p, &m); err != nil {
				return nil, err
			}
			return checked(m, check)
		},
	}
}

func rawKind[T interface {
	Message
	~[]byte
}](name string, route Route, check func(T) error) kindSpec {
	return kindSpec{
		name:    name,
		route:   route,
		payload: payloadRaw,
		encode:  func(m Message) []byte { return []byte(m.(T)) },
		decode:  func(p []byte) (Message, error) { return checked(T(p), check) },
	}
}

func checked[T Message](m T, check func(T) error) (Message, error) {
	if check != nil {
		if err := check(m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Message is one decoded frame. Only this package implements it.
type Message interface{ kind() Kind }

type Hello struct {
	Secret  SessionSecret `json:"secret"`
	Name    string        `json:"name"` // 1..128 bytes, valid UTF-8, no control characters
	Host    string        `json:"host"`
	Command []string      `json:"command"` // at most 64 elements
	Started time.Time     `json:"started"`
}

type Output []byte
type Input []byte

// TermSize is valid when 1 <= Cols <= 1000 and 1 <= Rows <= 1000. Decode enforces it.
type TermSize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type SizeChanged struct{ TermSize }   // published only by the wrapper, the size's owner
type ResizeRequest struct{ TermSize } // a viewer's wish; the wrapper decides

type Exit struct {
	Code   int    `json:"code"`             // 128+n when killed by signal n
	Signal string `json:"signal,omitempty"` // "SIGKILL"
}

type Presence struct {
	WrapperOnline bool `json:"wrapperOnline"`
	Viewers       int  `json:"viewers"`
}

func (Hello) kind() Kind         { return KindHello }
func (Output) kind() Kind        { return KindOutput }
func (SizeChanged) kind() Kind   { return KindSizeChanged }
func (Exit) kind() Kind          { return KindExit }
func (Input) kind() Kind         { return KindInput }
func (ResizeRequest) kind() Kind { return KindResizeRequest }
func (Presence) kind() Kind      { return KindPresence }

func (h Hello) validate() error {
	if len(h.Name) < 1 || len(h.Name) > maxHelloName || strings.IndexFunc(h.Name, unicode.IsControl) >= 0 {
		return fmt.Errorf("session name %q is not 1..%d printable bytes", h.Name, maxHelloName)
	}
	if len(h.Command) > maxHelloArgs {
		return fmt.Errorf("command has %d arguments, limit %d", len(h.Command), maxHelloArgs)
	}
	return nil
}

func (s TermSize) validate() error {
	if s.Cols < 1 || s.Cols > maxTermSide || s.Rows < 1 || s.Rows > maxTermSide {
		return fmt.Errorf("terminal size %dx%d outside 1..%d", s.Cols, s.Rows, maxTermSide)
	}
	return nil
}

func (in Input) validate() error {
	if len(in) > MaxInput {
		return fmt.Errorf("input is %d bytes, limit %d", len(in), MaxInput)
	}
	return nil
}

// SessionInfo is one row of GET /api/sessions. The relay builds it from Hello
// and never stores the secret.
type SessionInfo struct {
	ID      SessionID `json:"id"`
	Name    string    `json:"name"`
	Host    string    `json:"host"`
	Command []string  `json:"command"`
	Started time.Time `json:"started"`
	Online  bool      `json:"online"`
	Viewers int       `json:"viewers"`
}

type CloseCode uint16

const (
	CloseProtocol CloseCode = 4400
	CloseNotFound CloseCode = 4404
	CloseTooSlow  CloseCode = 4408
	CloseReplaced CloseCode = 4409
	CloseEnded    CloseCode = 4410
)

var closeNames = map[string]CloseCode{
	"Protocol": CloseProtocol,
	"NotFound": CloseNotFound,
	"TooSlow":  CloseTooSlow,
	"Replaced": CloseReplaced,
	"Ended":    CloseEnded,
}

// ErrProtocol wraps every Decode failure.
var ErrProtocol = errors.New("agentwatch: protocol violation")

// Encode frames m as one WebSocket message.
func Encode(m Message) []byte {
	k := m.kind()
	return append([]byte{byte(k)}, kinds[k].encode(m)...)
}

// Decode parses and validates one frame. Output and Input alias msg, so the
// caller must not reuse msg while the message is in use.
func Decode(msg []byte) (Message, error) {
	_, spec, err := lookup(msg)
	if err != nil {
		return nil, err
	}
	m, err := spec.decode(msg[1:])
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrProtocol, spec.name, err)
	}
	return m, nil
}

// PeekKind reads only the kind byte, which is all the relay needs to route.
func PeekKind(msg []byte) (Kind, Route, error) {
	k, spec, err := lookup(msg)
	return k, spec.route, err
}

func lookup(msg []byte) (Kind, kindSpec, error) {
	if len(msg) == 0 {
		return 0, kindSpec{}, fmt.Errorf("%w: empty frame", ErrProtocol)
	}
	k := Kind(msg[0])
	spec, ok := kinds[k]
	if !ok {
		return 0, kindSpec{}, fmt.Errorf("%w: unknown kind 0x%02x", ErrProtocol, msg[0])
	}
	return k, spec, nil
}

// PlaintextAllowed reports whether host may be reached over http or ws, which
// is true only for a loopback IP or "localhost". host carries no port or brackets.
func PlaintextAllowed(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
