package web_test

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"

	"github.com/ericfitz/agentwatch/internal/wire"
)

const (
	idHex = "000102030405060708090a0b0c0d0e0f"
	// SHA-256("abc"), the FIPS 180-2 test vector.
	abcSHA = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
)

// harness is test-only JS. view renders what AW.decode hands app.js as JSON,
// with each Uint8Array as hex so a byte array cannot pass for a plain Array.
const utf8Polyfill = `
function TextEncoder() {}
TextEncoder.prototype.encode = function (s) {
  var out = [];
  for (var i = 0; i < s.length; i++) {
    var c = s.charCodeAt(i);
    if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
      var lo = s.charCodeAt(i + 1);
      if (lo >= 0xdc00 && lo <= 0xdfff) {
        c = 0x10000 + ((c - 0xd800) << 10) + (lo - 0xdc00);
        i++;
      }
    }
    if (c >= 0xd800 && c <= 0xdfff) c = 0xfffd;
    if (c < 0x80) out.push(c);
    else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 0x3f));
    else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f));
    else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 0x3f), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f));
  }
  return new Uint8Array(out);
};
function TextDecoder() {}
TextDecoder.prototype.decode = function (bytes) {
  var units = [];
  var i = 0;
  while (i < bytes.length) {
    var b = bytes[i++];
    if (b < 0x80) { units.push(b); continue; }
    var need, cp, lower = 0x80, upper = 0xbf;
    if (b >= 0xc2 && b <= 0xdf) { need = 1; cp = b & 0x1f; }
    else if (b >= 0xe0 && b <= 0xef) {
      need = 2; cp = b & 0x0f;
      if (b === 0xe0) lower = 0xa0;
      if (b === 0xed) upper = 0x9f;
    } else if (b >= 0xf0 && b <= 0xf4) {
      need = 3; cp = b & 0x07;
      if (b === 0xf0) lower = 0x90;
      if (b === 0xf4) upper = 0x8f;
    } else { units.push(0xfffd); continue; }
    for (; need > 0; need--) {
      if (i >= bytes.length || bytes[i] < lower || bytes[i] > upper) { cp = -1; break; }
      cp = (cp << 6) | (bytes[i++] & 0x3f);
      lower = 0x80; upper = 0xbf;
    }
    if (cp < 0) units.push(0xfffd);
    else if (cp >= 0x10000) {
      cp -= 0x10000;
      units.push(0xd800 + (cp >> 10), 0xdc00 + (cp & 0x3ff));
    } else units.push(cp);
  }
  var s = "";
  for (var j = 0; j < units.length; j += 4096) s += String.fromCharCode.apply(null, units.slice(j, j + 4096));
  return s;
};
`

const harness = `
function hex(b) {
  var s = "";
  for (var i = 0; i < b.length; i++) s += (b[i] < 16 ? "0" : "") + b[i].toString(16);
  return s;
}
function view(frame) {
  return JSON.stringify(AW.decode(frame), function (k, v) { return v instanceof Uint8Array ? hex(v) : v; });
}
function reencode(frame) {
  var m = AW.decode(frame);
  return AW.encode(m.kind, m.value);
}
`

func newCodec(t *testing.T) *goja.Runtime {
	t.Helper()
	codec, err := os.ReadFile("codec.js")
	if err != nil {
		t.Fatal(err)
	}
	vm := goja.New()
	if err := vm.Set("window", vm.GlobalObject()); err != nil {
		t.Fatal(err)
	}
	for _, script := range []struct{ name, src string }{
		{"protocol.js", string(wire.ClientJS())},
		{"utf8.js", utf8Polyfill},
		{"codec.js", string(codec)},
		{"harness.js", harness},
	} {
		if _, err := vm.RunScript(script.name, script.src); err != nil {
			t.Fatalf("%s: %v", script.name, err)
		}
	}
	return vm
}

func call(t *testing.T, vm *goja.Runtime, fn string, frame []byte) (goja.Value, error) {
	t.Helper()
	f, ok := goja.AssertFunction(vm.Get(fn))
	if !ok {
		t.Fatalf("%s is not a function", fn)
	}
	return f(goja.Undefined(), vm.ToValue(vm.NewArrayBuffer(append([]byte(nil), frame...))))
}

func TestCodecRoundTripsEveryKind(t *testing.T) {
	var id wire.TransferID
	for i := range id {
		id[i] = byte(i)
	}
	digest := wire.Digest(sha256.Sum256([]byte("abc")))
	var secret wire.SessionSecret
	for i := range secret {
		secret[i] = 0xff
	}
	fill := strings.NewReplacer("$ID", idHex, "$SHA", abcSHA, "$SECRET", strings.Repeat("_", 42)+"8").Replace

	cases := []struct {
		msg  wire.Message
		want string
	}{
		{
			wire.Hello{
				Secret:  secret,
				Name:    "api-agent",
				Host:    "devbox",
				Command: []string{"claude", "--resume"},
				Started: time.Date(2026, 10, 21, 5, 3, 0, 0, time.UTC),
				Inbox:   wire.InboxInfo{Enabled: true, MaxBytes: 100 << 20},
			},
			`{"kind":"Hello","value":{"secret":"$SECRET","name":"api-agent","host":"devbox","command":["claude","--resume"],"started":"2026-10-21T05:03:00Z","inbox":{"enabled":true,"maxBytes":104857600}}}`,
		},
		{wire.Output("\x1b[31m\xff\x00\xe2\x82"), `{"kind":"Output","value":"1b5b33316dff00e282"}`},
		{wire.SizeChanged{TermSize: wire.TermSize{Cols: 100, Rows: 30}}, `{"kind":"SizeChanged","value":{"cols":100,"rows":30}}`},
		{wire.Exit{Code: 137, Signal: "SIGKILL"}, `{"kind":"Exit","value":{"code":137,"signal":"SIGKILL"}}`},
		{
			wire.TransferStatus{ID: id, State: wire.TransferDone, Name: "a.txt", Size: 3, Received: 3, SHA256: &digest, Path: "/inbox/a.txt"},
			`{"kind":"TransferStatus","value":{"id":"$ID","state":"done","name":"a.txt","size":3,"received":3,"sha256":"$SHA","path":"/inbox/a.txt"}}`,
		},
		{wire.Input("ls -la\r"), `{"kind":"Input","value":"6c73202d6c610d"}`},
		{wire.ResizeRequest{TermSize: wire.TermSize{Cols: 1000, Rows: 1}}, `{"kind":"ResizeRequest","value":{"cols":1000,"rows":1}}`},
		{
			wire.Offer{ID: id, Name: "naïve <€> 😀.txt", Size: 3, SHA256: digest},
			`{"kind":"Offer","value":{"id":"$ID","name":"naïve <€> 😀.txt","size":3,"sha256":"$SHA"}}`,
		},
		{
			wire.Chunk{ID: id, Offset: 5<<32 + 7, Data: []byte("abc")},
			`{"kind":"Chunk","value":{"id":"$ID","offset":21474836487,"data":"616263"}}`,
		},
		{wire.Cancel{ID: id}, `{"kind":"Cancel","value":{"id":"$ID"}}`},
		{wire.Presence{WrapperOnline: false, Viewers: 2}, `{"kind":"Presence","value":{"wrapperOnline":false,"viewers":2}}`},
	}

	vm := newCodec(t)
	covered := map[byte]bool{}
	for _, c := range cases {
		frame := wire.Encode(c.msg)
		covered[frame[0]] = true

		got, err := call(t, vm, "view", frame)
		if err != nil {
			t.Errorf("AW.decode(%T): %v", c.msg, err)
			continue
		}
		var gotJSON, wantJSON any
		if err := json.Unmarshal([]byte(got.String()), &gotJSON); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(fill(c.want)), &wantJSON); err != nil {
			t.Fatalf("bad expectation for %T: %v", c.msg, err)
		}
		if !reflect.DeepEqual(gotJSON, wantJSON) {
			t.Errorf("AW.decode(%T)\n got %s\nwant %s", c.msg, got, fill(c.want))
		}

		back, err := call(t, vm, "reencode", frame)
		if err != nil {
			t.Errorf("AW.encode(%T): %v", c.msg, err)
			continue
		}
		bytes, ok := back.Export().([]byte)
		if !ok {
			t.Fatalf("AW.encode returned %T, want Uint8Array", back.Export())
		}
		decoded, err := wire.Decode(bytes)
		if err != nil {
			t.Errorf("wire.Decode(AW.encode(%T)) = %v; frame %x", c.msg, err, bytes)
			continue
		}
		if !reflect.DeepEqual(decoded, c.msg) {
			t.Errorf("Go -> JS -> Go of %T\n got %#v\nwant %#v", c.msg, decoded, c.msg)
		}
	}

	for b := 0; b < 256; b++ {
		if _, _, err := wire.PeekKind([]byte{byte(b)}); err == nil && !covered[byte(b)] {
			t.Errorf("kind 0x%02x has no codec case", b)
		}
	}
}

func TestCodecRejectsMalformedFrames(t *testing.T) {
	vm := newCodec(t)
	for name, frame := range map[string][]byte{
		"empty frame":        {},
		"unknown kind":       {0x7f},
		"short Chunk header": append([]byte{byte(wire.KindChunk)}, make([]byte, 23)...),
		"offset past 2^53":   append([]byte{byte(wire.KindChunk)}, append(make([]byte, 16), 0x00, 0x20, 0, 0, 0, 0, 0, 0, 'x')...),
	} {
		if _, err := call(t, vm, "view", frame); err == nil {
			t.Errorf("AW.decode accepted %s", name)
		}
	}
	for name, js := range map[string]string{
		"unknown kind":      `AW.encode("Nope", {})`,
		"short Chunk id":    `AW.encode("Chunk", {id: "0102", offset: 0, data: new Uint8Array(1)})`,
		"uppercase id":      `AW.encode("Chunk", {id: "` + strings.ToUpper(idHex) + `", offset: 0, data: new Uint8Array(1)})`,
		"fractional offset": `AW.encode("Chunk", {id: "` + idHex + `", offset: 1.5, data: new Uint8Array(1)})`,
		"negative offset":   `AW.encode("Chunk", {id: "` + idHex + `", offset: -1, data: new Uint8Array(1)})`,
	} {
		if _, err := vm.RunString(js); err == nil {
			t.Errorf("AW.encode accepted %s", name)
		}
	}
}
