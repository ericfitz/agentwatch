// codec.js turns agentwatch frames into {kind, value} and back. It reads every
// constant from window.AW, which /protocol.js defines, and uses no browser API
// beyond typed arrays, so a Go test can run it.
//
//   raw kinds (Output, Input): value is a Uint8Array
//   Chunk:                     value is {id: 32 hex chars, offset, data: Uint8Array}
//   every other kind:          value is the JSON payload as an object
(function (AW) {
  "use strict";

  var TWO_32 = 4294967296;
  var MAX_SAFE = 9007199254740991;
  var idBytes = AW.chunkIdBytes;

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
    },
    chunk: {
      encode: function (c) {
        if (typeof c.offset !== "number" || c.offset < 0 || c.offset > MAX_SAFE || Math.floor(c.offset) !== c.offset) {
          throw new Error("agentwatch: bad Chunk offset " + c.offset);
        }
        var body = new Uint8Array(AW.chunkHeader + c.data.length);
        var view = new DataView(body.buffer);
        body.set(hexToBytes(c.id, idBytes), 0);
        view.setUint32(idBytes, Math.floor(c.offset / TWO_32));
        view.setUint32(idBytes + 4, c.offset % TWO_32);
        body.set(c.data, AW.chunkHeader);
        return body;
      },
      decode: function (bytes) {
        if (bytes.length < AW.chunkHeader) {
          throw new Error("agentwatch: short Chunk header");
        }
        var view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
        var hi = view.getUint32(idBytes);
        if (hi >= 0x200000) {
          throw new Error("agentwatch: Chunk offset beyond 2^53");
        }
        return {
          id: bytesToHex(bytes.subarray(0, idBytes)),
          offset: hi * TWO_32 + view.getUint32(idBytes + 4),
          data: bytes.subarray(AW.chunkHeader)
        };
      }
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

  // decode accepts an ArrayBuffer or a Uint8Array. Raw values and Chunk data
  // are views into it.
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

  function hexToBytes(s, n) {
    if (typeof s !== "string" || s.length !== 2 * n || !/^[0-9a-f]*$/.test(s)) {
      throw new Error("agentwatch: bad id " + s);
    }
    var out = new Uint8Array(n);
    for (var i = 0; i < n; i++) {
      out[i] = parseInt(s.substr(2 * i, 2), 16);
    }
    return out;
  }

  function bytesToHex(bytes) {
    var s = "";
    for (var i = 0; i < bytes.length; i++) {
      s += (bytes[i] < 16 ? "0" : "") + bytes[i].toString(16);
    }
    return s;
  }

  function utf8Encode(s) {
    return new TextEncoder().encode(s);
  }

  function utf8Decode(bytes) {
    return new TextDecoder().decode(bytes);
  }
})(window.AW);
