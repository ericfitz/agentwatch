package wire_test

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"

	"github.com/ericfitz/agentwatch/internal/wire"
)

func samples() []wire.Message {
	return []wire.Message{
		wire.Hello{
			Secret:  wire.SessionSecret{1, 2, 3},
			Name:    "api-agent",
			Host:    "devbox",
			Command: []string{"claude", "--resume"},
			Started: time.Date(2026, 10, 21, 5, 3, 0, 0, time.UTC),
		},
		wire.Output("\x1b[31m\xff\x00\xe2\x82"),
		wire.SizeChanged{TermSize: wire.TermSize{Cols: 100, Rows: 30}},
		wire.Exit{Code: 137, Signal: "SIGKILL"},
		wire.Input("ls -la\r"),
		wire.ResizeRequest{TermSize: wire.TermSize{Cols: 1000, Rows: 1}},
		wire.Presence{WrapperOnline: true, Viewers: 2},
	}
}

// tableKinds probes every byte, so it sees exactly the kinds the table defines.
func tableKinds() map[wire.Kind]wire.Route {
	out := map[wire.Kind]wire.Route{}
	for b := 0; b < 256; b++ {
		if k, r, err := wire.PeekKind([]byte{byte(b)}); err == nil {
			out[k] = r
		}
	}
	return out
}

func TestEveryKindRoundTrips(t *testing.T) {
	covered := map[wire.Kind]bool{}
	for _, m := range samples() {
		frame := wire.Encode(m)
		covered[wire.Kind(frame[0])] = true
		got, err := wire.Decode(frame)
		if err != nil {
			t.Errorf("Decode(Encode(%T)): %v", m, err)
			continue
		}
		if !reflect.DeepEqual(got, m) {
			t.Errorf("round trip of %T\n got %#v\nwant %#v", m, got, m)
		}
	}
	for k := range tableKinds() {
		if !covered[k] {
			t.Errorf("kind 0x%02x is in the table but has no sample", byte(k))
		}
	}
}

func TestDecodeAcceptsLimits(t *testing.T) {
	for name, m := range map[string]wire.Message{
		"Input of MaxInput bytes": wire.Input(make([]byte, wire.MaxInput)),
		"TermSize 1x1":            wire.ResizeRequest{TermSize: wire.TermSize{Cols: 1, Rows: 1}},
		"TermSize 1000x1000":      wire.SizeChanged{TermSize: wire.TermSize{Cols: 1000, Rows: 1000}},
		"Hello of 128-byte name":  helloWithName(strings.Repeat("n", 128)),
	} {
		if _, err := wire.Decode(wire.Encode(m)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func helloWithName(name string) wire.Hello {
	return wire.Hello{Name: name, Host: "h", Command: []string{"sh"}, Started: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func frame(k wire.Kind, payload string) []byte { return append([]byte{byte(k)}, payload...) }

func jsonFrame(k wire.Kind, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return frame(k, string(b))
}

func helloFrame(name string, args int) []byte {
	return jsonFrame(wire.KindHello, map[string]any{
		"secret":  strings.Repeat("A", 43),
		"name":    name,
		"host":    "h",
		"command": make([]string, args),
		"started": "2026-10-21T05:03:00Z",
	})
}

func TestDecodeRejects(t *testing.T) {
	for name, msg := range map[string][]byte{
		"valid Hello": helloFrame("api-agent", 64),
	} {
		if _, err := wire.Decode(msg); err != nil {
			t.Fatalf("control case %s must decode: %v", name, err)
		}
	}

	cases := map[string][]byte{
		"empty frame":                   nil,
		"unknown kind":                  {0x7f},
		"kind 0x05":                     {0x05},
		"TermSize of 0":                 frame(wire.KindResizeRequest, `{"cols":0,"rows":24}`),
		"TermSize of 1001":              frame(wire.KindSizeChanged, `{"cols":80,"rows":1001}`),
		"empty Hello name":              helloFrame("", 1),
		"Hello name over 128 bytes":     helloFrame(strings.Repeat("n", 129), 1),
		"Hello name with escape":        helloFrame("evil\x1b[2J", 1),
		"Hello name with newline":       helloFrame("two\nlines", 1),
		"Hello with 65 command args":    helloFrame("api-agent", 65),
		"Hello secret wrong length":     frame(wire.KindHello, `{"secret":"AAAA","name":"x","host":"h","command":[],"started":"2026-10-21T05:03:00Z"}`),
		"Input over MaxInput":           frame(wire.KindInput, strings.Repeat("a", wire.MaxInput+1)),
		"JSON kind with non-JSON":       frame(wire.KindExit, "nope"),
		"JSON kind with trailing bytes": frame(wire.KindPresence, `{"viewers":1} {}`),
	}
	for name, msg := range cases {
		m, err := wire.Decode(msg)
		if !errors.Is(err, wire.ErrProtocol) {
			t.Errorf("%s: Decode = %#v, %v; want ErrProtocol", name, m, err)
		}
	}
}

func TestPeekKindRoutesMatchTable(t *testing.T) {
	want := map[wire.Kind]wire.Route{
		0x01: wire.RouteHello,
		0x02: wire.RouteUp,
		0x03: wire.RouteUp,
		0x04: wire.RouteUp,
		0x10: wire.RouteDown,
		0x11: wire.RouteDown,
		0x20: wire.RouteRelay,
	}
	if got := tableKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("routes\n got %v\nwant %v", got, want)
	}
	if _, _, err := wire.PeekKind(nil); !errors.Is(err, wire.ErrProtocol) {
		t.Fatalf("PeekKind(empty) = %v, want ErrProtocol", err)
	}
	k, r, err := wire.PeekKind(wire.Encode(wire.Input("x")))
	if err != nil || k != wire.KindInput || r != wire.RouteDown {
		t.Fatalf("PeekKind(Input frame) = %v, %v, %v", k, r, err)
	}
}

func TestClientJSNamesEveryKindAndCloseCode(t *testing.T) {
	vm := goja.New()
	if err := vm.Set("window", vm.GlobalObject()); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunScript("protocol.js", string(wire.ClientJS())); err != nil {
		t.Fatalf("ClientJS does not run: %v", err)
	}
	v, err := vm.RunString("JSON.stringify(window.AW)")
	if err != nil {
		t.Fatal(err)
	}
	var aw struct {
		Subprotocol  string
		BearerPrefix string
		Kind         map[string]int
		Payload      map[string]string
		Close        map[string]int
		MaxInput     int
	}
	if err := json.Unmarshal([]byte(v.String()), &aw); err != nil {
		t.Fatal(err)
	}

	wantKind := map[string]int{
		"Hello": 0x01, "Output": 0x02, "SizeChanged": 0x03, "Exit": 0x04,
		"Input": 0x10, "ResizeRequest": 0x11, "Presence": 0x20,
	}
	if !reflect.DeepEqual(aw.Kind, wantKind) {
		t.Errorf("AW.kind = %v, want %v", aw.Kind, wantKind)
	}
	wantPayload := map[string]string{}
	for name := range wantKind {
		wantPayload[name] = "json"
	}
	wantPayload["Output"], wantPayload["Input"] = "raw", "raw"
	if !reflect.DeepEqual(aw.Payload, wantPayload) {
		t.Errorf("AW.payload = %v, want %v", aw.Payload, wantPayload)
	}
	wantClose := map[string]int{"Protocol": 4400, "NotFound": 4404, "TooSlow": 4408, "Replaced": 4409, "Ended": 4410}
	if !reflect.DeepEqual(aw.Close, wantClose) {
		t.Errorf("AW.close = %v, want %v", aw.Close, wantClose)
	}
	if aw.Subprotocol != "agentwatch.v1" || aw.BearerPrefix != "agentwatch.bearer." {
		t.Errorf("AW subprotocol = %q, bearerPrefix = %q", aw.Subprotocol, aw.BearerPrefix)
	}
	if aw.MaxInput != 64<<10 {
		t.Errorf("AW.maxInput = %d, want %d", aw.MaxInput, 64<<10)
	}
}

func TestParseToken(t *testing.T) {
	text := "abcdefghijklmnopqrstuvwxyzABCD-_"
	tok, err := wire.ParseToken(text)
	if err != nil {
		t.Fatalf("32-character token rejected: %v", err)
	}
	if !tok.Matches(text) || tok.Matches(text[:31]+"x") || tok.Matches("") {
		t.Errorf("Matches disagrees with the token text")
	}
	if tok.Bearer() != "Bearer "+text {
		t.Errorf("Bearer() = %q", tok.Bearer())
	}
	if (wire.Token{}).Matches("") {
		t.Errorf("zero Token matches the empty string")
	}

	minted := strings.TrimPrefix(wire.NewToken().Bearer(), "Bearer ")
	if len(minted) != 43 {
		t.Errorf("NewToken has %d characters, want 43", len(minted))
	}
	if _, err := wire.ParseToken(minted); err != nil {
		t.Errorf("NewToken output rejected: %v", err)
	}

	for name, bad := range map[string]string{
		"empty":         "",
		"31 characters": text[:31],
		"plus":          text[:31] + "+",
		"slash":         text[:31] + "/",
		"padding":       text[:31] + "=",
		"space":         text[:31] + " ",
		"non-ASCII":     text[:30] + "é",
	} {
		if _, err := wire.ParseToken(bad); err == nil {
			t.Errorf("%s token accepted", name)
		} else if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("%s token error quotes the token: %v", name, err)
		}
	}
}

func TestSecretsNeverPrint(t *testing.T) {
	text := "abcdefghijklmnopqrstuvwxyzABCD-_"
	tok, _ := wire.ParseToken(text)
	secret := wire.NewSessionSecret()
	secretText, _ := secret.MarshalText()
	secretHex := hex.EncodeToString(secret[:])

	type config struct {
		Token  wire.Token
		Secret wire.SessionSecret
	}
	for _, s := range []string{
		fmt.Sprint(tok),
		fmt.Sprintf("%v %+v %#v %s %q %x", tok, tok, tok, tok, tok, tok),
		fmt.Sprintf("%v %+v %#v %s %x", secret, secret, secret, secret, secret),
		fmt.Sprintf("%v %+v %#v", config{tok, secret}, config{tok, secret}, config{tok, secret}),
	} {
		for _, leak := range []string{text, string(secretText), secretHex} {
			if strings.Contains(s, leak) {
				t.Errorf("formatted output %q contains secret text", s)
			}
		}
	}
}

func TestSessionID(t *testing.T) {
	var secret wire.SessionSecret
	for i := range secret {
		secret[i] = byte(i)
	}
	sum := sha256.Sum256(secret[:])
	want := strings.ToLower(base32.StdEncoding.EncodeToString(sum[:10]))

	got := secret.ID()
	if string(got) != want || len(want) != 16 {
		t.Fatalf("ID() = %q, want %q", got, want)
	}
	if parsed, err := wire.ParseSessionID(want); err != nil || parsed != got {
		t.Errorf("ParseSessionID(%q) = %q, %v", want, parsed, err)
	}
	for _, bad := range []string{"", want[:15], want + "a", strings.ToUpper(want), "0000000000000000", "abcdefgh/jklmnop"} {
		if _, err := wire.ParseSessionID(bad); err == nil {
			t.Errorf("ParseSessionID(%q) accepted", bad)
		}
	}
}

func TestPlaintextAllowed(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":             true,
		"LocalHost":             true,
		"127.0.0.1":             true,
		"127.8.9.10":            true,
		"::1":                   true,
		"":                      false,
		"0.0.0.0":               false,
		"::":                    false,
		"10.0.0.1":              false,
		"example.com":           false,
		"localhost.example.com": false,
		"[::1]":                 false,
	} {
		if got := wire.PlaintextAllowed(host); got != want {
			t.Errorf("PlaintextAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}
