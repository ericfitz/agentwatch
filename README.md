# agentwatch

agentwatch runs an interactive program, usually a coding agent such as `claude`, inside a pseudo-terminal (PTY) on one host. A person can then watch and drive that program from a browser anywhere. The agent host only dials out. A relay on a public HTTPS address pairs agent hosts with browsers. Every session is recorded as an [asciicast v2](https://docs.asciinema.org/manual/asciicast/v2/) file.

**A viewer token is full control of every wrapped process.** Anyone who holds it can type into every session on the relay. If you wrap `bash`, the viewer token is a shell on that host as your user. Guard the viewer token the way you guard an SSH private key, and never share it with someone you would not give that shell.

The wrapper runs on Linux and macOS only, because it needs a Unix PTY. The relay ships in the same binary, so it runs on the same systems.

## How the pieces fit

agentwatch is one binary with three subcommands.

```
agent host                          relay host                      any browser
agentwatch run -- claude  --wss-->  agentwatch relay  <--wss--  the embedded page
  PTY, local terminal,                session list, fan-out,        xterm.js terminal
  recording                           replay, auth
```

- `agentwatch run -- COMMAND` is the wrapper. It starts `COMMAND` in a PTY, keeps your local terminal working as if you ran `COMMAND` directly, writes the recording, and holds one outbound WebSocket to the relay.
- `agentwatch relay` serves the page, lists sessions, and routes frames between one wrapper and any number of browsers per session. It reads the first byte of each frame to route it and decodes no payload except the wrapper's `Hello`.
- `agentwatch token` prints a new random token.

Go is the language because one static binary carries the wrapper, the relay, and the page. The page is compiled into the binary with `go:embed`, and a build with `CGO_ENABLED=0` links no C library, so a host needs only the one file. `github.com/creack/pty` provides the PTY, and the standard library provides TLS, HTTP routing, SHA-256 for session IDs, and logging. The module targets Go 1.22.2, so the dependencies are pinned to versions that still support it: `github.com/creack/pty` v1.1.24, `github.com/coder/websocket` v1.8.13, and `golang.org/x/term` v0.29.0.

### Packages

| Path | Owns |
| --- | --- |
| `cmd/agentwatch` | Subcommands. Turns flags, environment variables, and files into the types below. |
| `internal/wire` | The v1 protocol: the frame-kind table, `Encode`, `Decode`, IDs, tokens, close codes, and `ClientJS`, which renders the table as `window.AW` for the browser. No I/O. |
| `internal/record` | asciicast v2 encoding and no-clobber file naming. |
| `internal/wrapper` | The PTY session: the local terminal, the reconnecting relay link, and size arbitration. |
| `internal/relay` | HTTP routes, auth, the session registry, fan-out, and the replay tail. |
| `internal/e2e` | Tests that run a real shell through a real relay with a Go viewer. |
| `web` | `index.html`, `app.js`, `app.css`, `codec.js`, and the vendored xterm.js 5.3.0 and xterm-addon-fit 0.8.0. |
| `scripts` | `demo.sh` and `gencert`, which makes a self-signed certificate for `localhost`. |

### Inside the wrapper

One session loop owns the PTY size, the recorder, and the relay link. Every event reaches it through a channel: PTY output, local keystrokes, local terminal resizes, frames from the relay, link up and link down, and the child's exit. One writer goroutine is the only writer of PTY input, so keystrokes from your terminal and keystrokes from browsers never interleave inside one write.

Several parties want to set one terminal size. The PTY follows whichever side typed or resized last: your local terminal, or the browsers as a group. Among browsers, the relay picks the one that typed or resized last. When the relay link drops, the PTY returns to your local size. A browser never resizes its terminal on its own. It requests a size, and it draws at whatever size the wrapper reports.

When the child exits, the wrapper drains the remaining output, writes an `exit N` marker, closes the recording, sends `Exit` to the relay, and restores your terminal. `agentwatch run` exits with the child's code, or with 128 plus the signal number if a signal killed the child.

## Protocol reference

### Frames

Every WebSocket message is one binary frame. Byte 0 is the kind, and the rest is the payload. The payload of `Output` and `Input` is raw terminal bytes. Every other payload is UTF-8 JSON.

| Kind | Byte | Sent by, to | Payload |
| --- | --- | --- | --- |
| `Hello` | `0x01` | wrapper to relay, first frame only, never forwarded | `{name, host, command, started, secret}` |
| `Output` | `0x02` | wrapper to every viewer | PTY output |
| `SizeChanged` | `0x03` | wrapper to every viewer | `{cols, rows}`, the PTY's actual size |
| `Exit` | `0x04` | wrapper to every viewer | `{code, signal}` |
| `Input` | `0x10` | viewer to wrapper | keystrokes, at most 64 KiB per frame |
| `ResizeRequest` | `0x11` | viewer to wrapper | `{cols, rows}`, the size that fits the viewer's window |
| `Presence` | `0x20` | relay to viewer | `{wrapperOnline, viewers}` |

The table in `internal/wire/wire.go` is the only list of kinds. The relay serves it to the browser as `/protocol.js`, and `web/codec.js` encodes and decodes frames from that copy. A Go test runs `codec.js` in an embedded JavaScript engine and round-trips every kind against `wire.Encode`.

### HTTP routes and authentication

| Route | Credential | Purpose |
| --- | --- | --- |
| `GET /` and the page files | none | The page. It carries no data. |
| `GET /protocol.js` | none | `window.AW`, generated from `internal/wire`. |
| `GET /healthz` | none | `200 ok`. |
| `GET /api/sessions` | viewer token in `Authorization: Bearer` | `{"sessions": [...]}`. The page also uses this route to check a pasted token. |
| `GET /api/sessions/{id}/attach` | viewer token as the `Sec-WebSocket-Protocol` entry `agentwatch.bearer.<token>` | Viewer WebSocket. |
| `GET /api/wrapper` | wrapper token in `Authorization: Bearer` | Wrapper WebSocket. |

Browsers cannot set headers on a WebSocket, so the viewer offers two subprotocols, `agentwatch.v1` and `agentwatch.bearer.<token>`. The relay selects `agentwatch.v1`. The relay checks the token before the upgrade and answers HTTP 401 to a missing or wrong token, including a token for the other role.

A session ID is the first 10 bytes of the SHA-256 digest of a 32-byte secret, written as 16 lowercase base32 characters. The wrapper makes the secret at start, keeps it in memory, and sends it in `Hello`. A wrapper that reconnects with the same secret gets the same session back.

### Close codes

| Code | Sent when | The peer then |
| --- | --- | --- |
| 1000 | The wrapper closes after `Exit`. | |
| 1001 | The relay is shutting down. | Reconnects. |
| 4400 | A frame has an unknown kind or goes the wrong direction, a wrapper's first frame is not a valid `Hello` within 10 s, or the subprotocol is missing. | Stops. The wrapper stops redialing, and the page shows an error. |
| 4404 | A viewer attaches to an unknown session. | Returns to the list. After a drop, the page first retries for 60 s from the first 4404, because the relay may have restarted before the wrapper registered again. |
| 4408 | A viewer's send queue overflowed. | Reconnects at once and gets a fresh replay. |
| 4409 | A newer wrapper connection presented the same secret. | Nothing. The old connection is already dead from the wrapper's view. |
| 4410 | The process exited, or the wrapper stayed away longer than 2 minutes. | Shows the exit and stops. |

### Reconnects

| Link | Behavior |
| --- | --- |
| Wrapper, first dial | Happens before the child starts, with a 10 s timeout. HTTP 401, HTTP 403, or a missing subprotocol make `agentwatch run` exit with code 2 before the child starts. Any other failure logs a warning, and the child starts anyway. |
| Wrapper, redial | Waits 1 s, then doubles up to 30 s, with 20 % jitter. The wait resets after a connection stays up 30 s. HTTP 401, HTTP 403, and close code 4400 stop the redials, and the session continues locally. |
| Wrapper, health | Pings every 15 s and drops the link after 10 s without a pong. A full send queue (1024 frames) also drops the link, so a slow network never blocks the agent. |
| Wrapper, offline | Output still reaches your terminal and the recording. Browsers miss it. |
| Relay, wrapper gone | Keeps the session listed as offline for 2 minutes. Attached viewers see `Presence{wrapperOnline: false}`. A reconnect within that window resumes the session for them. |
| Relay, restart | Loses all state. Wrappers redial and register again under the same session ID. |
| Relay, health | Pings every peer every 15 s. |
| Browser, drop | Waits 1 s, then doubles up to 10 s. Each attach resets the terminal, and the relay replays the size, the last 128 KiB of output, and the exit status if there is one. |

## Security model

- **Transport.** The relay serves TLS. It serves plain HTTP only with `--plain-http`, and only on a loopback address, which is the case behind a TLS-terminating reverse proxy on the same host. The wrapper dials `https://`, or `http://` only to a loopback host.
- **Two roles.** The wrapper token and the viewer token are separate, and the relay refuses to start unless both are set and differ. A wrapper token can register sessions but cannot list or attach to them.
- **Viewer power.** A viewer token is full control of every wrapped process on the relay. There is no view-only role in v1.
- **Token handling.** Tokens come from environment variables or files, never from flags, which other users can read in the process list. The relay compares tokens in constant time. A token must be at least 32 characters of `A-Z`, `a-z`, `0-9`, `-`, and `_`. Go code prints a token as `[redacted]`.
- **The child cannot read the tokens.** The wrapper removes `AGENTWATCH_WRAPPER_TOKEN` and `AGENTWATCH_VIEWER_TOKEN` from the child's environment. Without that step, an agent could read a token and drive other sessions through the relay.
- **Session ownership.** Taking over a session ID requires the secret behind it. The relay never shows the secret to viewers.
- **The relay is trusted.** It sees terminal traffic in plaintext. v1 has no end-to-end encryption.
- **Limits.** Every WebSocket message is at most 1 MiB. The wrapper must send `Hello` within 10 s of connecting. The relay's HTTP server waits at most 10 s for request headers.
- **Browser.** The Content Security Policy allows scripts from the relay only, so the page runs no inline or third-party script. Inline styles are allowed because the xterm.js renderer injects them. The token travels in a header or a subprotocol, never in a cookie, so other sites cannot make requests with it. The WebSocket `Origin` must match the relay's host.
- **Recordings.** The wrapper creates each recording with mode 0600, and any missing parent directories with mode 0700. It does not record keystrokes unless you pass `--record-input`, because keystrokes can contain passwords.

## Run agentwatch

### Build

```sh
CGO_ENABLED=0 go build -o agentwatch ./cmd/agentwatch
```

To build for another host, also set `GOOS` to `linux` or `darwin` and `GOARCH` to that host's architecture, such as `amd64` or `arm64`.

### Try it on one machine

`scripts/demo.sh` builds the binary, mints both tokens, makes a self-signed certificate, starts a relay on `127.0.0.1:8443`, and wraps an interactive `bash` in your terminal.

```sh
scripts/demo.sh
```

The script prints the URL, the viewer token, and where the recordings go. Open the URL, accept the certificate warning, and paste the viewer token. Exit the shell to stop the demo. To listen on another address, set `AGENTWATCH_DEMO_ADDR`.

### Mint the tokens

```sh
export AGENTWATCH_WRAPPER_TOKEN=$(./agentwatch token)
export AGENTWATCH_VIEWER_TOKEN=$(./agentwatch token)
```

The relay needs both tokens. Every agent host needs the wrapper token. Give the viewer token only to people who may control every wrapped process.

### Run the relay

```sh
./agentwatch relay --listen :8443 --tls-cert cert.pem --tls-key key.pem
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--listen ADDR` | `:8443` | Listen address. |
| `--tls-cert PATH`, `--tls-key PATH` | none, required without `--plain-http` | Certificate and private key. |
| `--plain-http` | off | Serve plain HTTP. Refused unless `--listen` is a loopback address such as `127.0.0.1:8080`. |
| `--wrapper-token-file PATH` | `$AGENTWATCH_WRAPPER_TOKEN` | File that holds the token wrappers present. |
| `--viewer-token-file PATH` | `$AGENTWATCH_VIEWER_TOKEN` | File that holds the token browsers present. |

To run behind a reverse proxy that terminates TLS, listen on loopback with `--plain-http` and have the proxy pass WebSocket upgrades and the `Host` header through unchanged.

### Run the wrapper

```sh
./agentwatch run --relay https://relay.example.com:8443 --name api-agent -- claude
```

Your terminal behaves as if you ran `claude` directly. When the session starts, the wrapper prints where the recording goes. Leave out `--relay` to record a session without sharing it.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--relay URL` | none | Relay base URL. It must be `https://`, or `http://` to a loopback host. |
| `--token-file PATH` | `$AGENTWATCH_WRAPPER_TOKEN` | File that holds the wrapper token. |
| `--ca-file PATH` | system roots | Extra CA certificate to trust for the relay, such as a self-signed one. |
| `--name NAME` | `COMMAND@HOSTNAME` | Name in the session list. |
| `--record PATH` | `$XDG_STATE_HOME/agentwatch/sessions/` | Recording destination. A path that ends in `/` is a directory that gets one new file per session. |
| `--no-record` | | Turn recording off. |
| `--record-input` | off | Also record keystrokes as `i` events. |
| `--log PATH` | stderr when stdin is not a terminal, discarded otherwise | Diagnostics. |

The child sees `AGENTWATCH_SESSION` (its session ID). The PTY is viewed in the browser, so the child always gets `TERM=xterm-256color` and `COLORTERM=truecolor`. A `NO_COLOR` value on the wrapper is passed through, and programs that honor it draw without color.

If stdin is not a terminal, the wrapper does not read it, and the PTY starts at 80x24 until a browser asks for another size.

To play a recording back, run `asciinema play FILE.cast`.

### Use the browser

1. Open the relay's URL, such as `https://relay.example.com:8443/`.
2. Paste the viewer token and click **Connect**. The page keeps the token in the browser's local storage until you click **Sign out**.
3. Click a session's name in the list. The page asks the wrapper to resize the PTY to fit your window, and what you type goes to the program.

The status line in the header shows whether the agent host is online, how many viewers are attached, and the exit code once the program ends. Full-screen programs such as vim redraw in the browser terminal, including the alternate screen, color, mouse reporting, and resize.

## Run the tests

```sh
GOTOOLCHAIN=local go test ./...
```

`internal/e2e` wraps `/bin/sh` behind a relay on a local TLS server and drives it with a Go viewer. It checks resizing, echo, a relay restart, a wrong token, and the exit code.

## License

agentwatch is under the license in `LICENSE`. The vendored xterm.js and xterm-addon-fit files are under the MIT license in `web/static/LICENSE-xterm.txt`.
