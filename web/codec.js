// codec.js turns agentwatch frames into {kind, value} and back. It reads every
// constant from window.AW, which /protocol.js defines, and uses no browser API
// beyond typed arrays, so a Go test can run it.
//
//   raw kinds (Output, Input): value is a Uint8Array
//   every other kind:          value is the JSON payload as an object
(function (AW) {
  "use strict";

  var kindName = {};
  for (var name in AW.kind) {
    if (Object.prototype.hasOwnProperty.call(AW.kind, name)) {
      kindName[AW.kind[name]] = name;
    }
  }

  var forms = {
    raw: {
      encode: function (bytes) { return bytes; },
      decode: function (bytes) { return bytes; }
    },
    json: {
      encode: function (value) { return utf8Encode(JSON.stringify(value)); },
      decode: function (bytes) { return JSON.parse(utf8Decode(bytes)); }
    }
  };

  AW.encode = function (kind, value) {
    var form = forms[AW.payload[kind]];
    if (!form) {
      throw new Error("agentwatch: unknown kind " + kind);
    }
    var body = form.encode(value);
    var frame = new Uint8Array(1 + body.length);
    frame[0] = AW.kind[kind];
    frame.set(body, 1);
    return frame;
  };

  // decode accepts an ArrayBuffer or a Uint8Array. Raw values are views into it.
  AW.decode = function (data) {
    var bytes = data instanceof Uint8Array ? data : new Uint8Array(data);
    if (bytes.length === 0) {
      throw new Error("agentwatch: empty frame");
    }
    var kind = kindName[bytes[0]];
    if (kind === undefined) {
      throw new Error("agentwatch: unknown kind " + bytes[0]);
    }
    return { kind: kind, value: forms[AW.payload[kind]].decode(bytes.subarray(1)) };
  };

  function utf8Encode(s) {
    return new TextEncoder().encode(s);
  }

  function utf8Decode(bytes) {
    return new TextDecoder().decode(bytes);
  }
})(window.AW);
