// app.js is the agentwatch browser client. Protocol constants come from
// window.AW (/protocol.js), and every frame goes through AW.encode and
// AW.decode (/codec.js).
(() => {
  "use strict";

  const TOKEN_KEY = "agentwatch.viewerToken";
  const LIST_POLL_MS = 2000;
  const REDIAL_MIN_MS = 1000;
  const REDIAL_MAX_MS = 10000;
  const NOT_FOUND_RETRY_MS = 60000;
  const TOAST_MS = 6000;
  const encoder = new TextEncoder();

  const $ = (id) => document.getElementById(id);

  let token = localStorage.getItem(TOKEN_KEY) || "";
  let session = null; // the attached session's state, see openSession
  let view = 0; // bumped on every route, so stale async work can tell
  let listTimer = 0;
  let screen = null; // { term, fit }, see terminal()

  function show(name) {
    for (const v of ["login", "list", "session"]) $(v).hidden = v !== name;
    $("signout").hidden = name === "login";
    $("status").hidden = name !== "session";
    if (name !== "session") $("title").textContent = "";
  }

  function route() {
    view++;
    closeSession();
    clearTimeout(listTimer);
    if (!token) {
      show("login");
      $("token").focus();
      return;
    }
    const m = /^#\/s\/([a-z2-7]{16})$/.exec(location.hash);
    if (m) {
      openSession(m[1]);
    } else {
      show("list");
      pollList(view);
    }
  }

  // Token and session list.

  const api = (path, tok) => fetch(path, { headers: { Authorization: `Bearer ${tok}` }, cache: "no-store" });

  async function signIn(e) {
    e.preventDefault();
    const candidate = $("token").value.trim();
    const error = $("login-error");
    error.textContent = "";
    let resp;
    try {
      resp = await api("/api/sessions", candidate);
    } catch (err) {
      error.textContent = `The relay did not answer: ${err.message}`;
      return;
    }
    if (resp.status === 401) {
      error.textContent = "The relay rejected that token.";
      return;
    }
    if (!resp.ok) {
      error.textContent = `The relay answered ${resp.status}.`;
      return;
    }
    token = candidate;
    localStorage.setItem(TOKEN_KEY, token);
    $("token").value = "";
    route();
  }

  function signOut(message) {
    token = "";
    localStorage.removeItem(TOKEN_KEY);
    history.replaceState(null, "", location.pathname);
    route();
    $("login-error").textContent = message || "";
  }

  // fetchSessions resolves to the session list, or to null after a rejected
  // token has sent the user back to the login form.
  async function fetchSessions() {
    const resp = await api("/api/sessions", token);
    if (resp.status === 401) {
      signOut("The relay rejected the saved token. Paste a current one.");
      return null;
    }
    if (!resp.ok) throw new Error(`The relay answered ${resp.status} for the session list.`);
    return (await resp.json()).sessions;
  }

  async function pollList(gen) {
    let sessions;
    try {
      sessions = await fetchSessions();
    } catch (err) {
      toast(err.message, "error");
    }
    if (gen !== view) return;
    if (sessions) renderList(sessions);
    listTimer = setTimeout(() => pollList(gen), LIST_POLL_MS);
  }

  function renderList(sessions) {
    $("rows").replaceChildren(...sessions.map(sessionRow));
    $("empty").hidden = sessions.length > 0;
  }

  function sessionRow(s) {
    const link = document.createElement("a");
    link.href = `#/s/${s.id}`;
    link.textContent = s.name;
    const command = document.createElement("code");
    command.textContent = (s.command || []).join(" ");
    const state = cell(s.online ? "online" : "offline");
    state.className = s.online ? "online" : "offline";
    const tr = document.createElement("tr");
    tr.append(cell(link), cell(s.host), cell(command), cell(new Date(s.started).toLocaleString()), state, cell(String(s.viewers)));
    return tr;
  }

  function cell(content) {
    const td = document.createElement("td");
    td.append(content);
    return td;
  }

  // The attached session.

  // terminal returns the page's one terminal. Sessions reset it instead of
  // disposing it, because xterm 5.3.0 throws from a pending viewport refresh
  // when a terminal is disposed in the same frame as a write or reset.
  function terminal() {
    if (!screen) {
      const term = new Terminal({
        cursorBlink: true,
        scrollback: 10000,
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
        fontSize: 14,
        theme: { background: "#000000" },
      });
      const fit = new FitAddon.FitAddon();
      term.loadAddon(fit);
      term.open($("terminal"));
      screen = { term, fit };
    }
    screen.term.reset();
    screen.term.options.disableStdin = false;
    return screen;
  }

  function openSession(id) {
    show("session");
    const { term, fit } = terminal();
    const s = {
      id,
      term,
      fit,
      info: null, // this session's row from GET /api/sessions
      ws: null,
      attached: false, // the current socket has received its first Presence
      everAttached: false,
      dropped: false, // an attached socket closed and none has attached since
      notFoundSince: 0, // the first 4404 since the drop
      redials: 0,
      timer: 0,
      wrapperOnline: false,
      viewers: 0,
      exit: null, // the Exit frame once the process has ended
      ended: false, // the relay closed the session for good
      cleanup: [],
      // The attach snapshot replays queries the app already sent. xterm answers
      // them, and that answer is a keystroke. Hold input until the snapshot is parsed.
      suppressInput: true,
      sawPresence: false,
      parsingSnapshot: false,
    };
    session = s;
    for (const sub of [
      term.onData((d) => sendInput(s, encoder.encode(d))),
      term.onBinary((d) => sendInput(s, Uint8Array.from(d, (c) => c.charCodeAt(0) & 0xff))),
    ]) {
      s.cleanup.push(() => sub.dispose());
    }
    let resizeTimer = 0;
    listen(s, window, "resize", () => {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => sendSize(s), 100);
    });
    term.focus();
    loadInfo(s);
    connect(s);
  }

  function closeSession() {
    const s = session;
    if (!s) return;
    session = null;
    clearTimeout(s.timer);
    for (const off of s.cleanup) off();
    if (s.ws) {
      const ws = s.ws;
      s.ws = null;
      ws.close(1000);
    }
  }

  function listen(s, target, type, fn) {
    target.addEventListener(type, fn);
    s.cleanup.push(() => target.removeEventListener(type, fn));
  }

  async function loadInfo(s) {
    let sessions;
    try {
      sessions = await fetchSessions();
    } catch {
      return;
    }
    if (session !== s || !sessions) return;
    s.info = sessions.find((x) => x.id === s.id) || null;
    if (s.info) $("title").textContent = `${s.info.name} on ${s.info.host}`;
  }

  function connect(s) {
    if (session !== s) return;
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    let ws;
    try {
      ws = new WebSocket(`${scheme}//${location.host}/api/sessions/${s.id}/attach`, [AW.subprotocol, AW.bearerPrefix + token]);
    } catch (err) {
      signOut(`The saved token cannot be used: ${err.message}`);
      return;
    }
    ws.binaryType = "arraybuffer";
    s.ws = ws;
    s.attached = false;
    let opened = false;
    ws.onopen = () => {
      if (s.ws !== ws) return;
      opened = true;
      s.suppressInput = true;
      s.sawPresence = false;
      s.parsingSnapshot = false;
      s.term.reset(); // the snapshot (size, replay, exit) repaints the screen
      sendSize(s);
    };
    ws.onmessage = (e) => {
      if (s.ws !== ws) return;
      let frame;
      try {
        frame = AW.decode(e.data);
      } catch (err) {
        console.warn("agentwatch: dropped a frame:", err);
        return;
      }
      const handle = frameHandlers[frame.kind];
      if (handle) handle(s, frame.value);
    };
    ws.onclose = (e) => {
      if (s.ws !== ws) return;
      if (s.attached) s.dropped = true;
      s.ws = null;
      s.attached = false;
      s.wrapperOnline = false;
      onClose(s, e.code, opened);
      renderStatus(s);
    };
    renderStatus(s);
  }

  function onClose(s, code, opened) {
    if (session !== s) return;
    if (code === AW.close.Ended) {
      endSession(s);
      return;
    }
    if (code === AW.close.Protocol) {
      endSession(s);
      toast("The relay closed the connection with a protocol error.", "error");
      return;
    }
    // After a drop, a 4404 may come from a restarted relay that the wrapper
    // has not redialed yet. The wrapper redials at least every 36 s, so the
    // retry window starts at the first 4404, not at the drop.
    if (code === AW.close.NotFound) {
      s.notFoundSince ||= Date.now();
      if (!s.dropped || Date.now() - s.notFoundSince >= NOT_FOUND_RETRY_MS) {
        toast("That session is not on the relay.", "error");
        location.hash = "#/";
        return;
      }
    }
    // A failed upgrade hides its HTTP status, so the list endpoint answers
    // whether the token still works.
    if (!opened) fetchSessions().catch(() => {});
    const delay = code === AW.close.TooSlow ? 0 : Math.min(REDIAL_MIN_MS * 2 ** s.redials, REDIAL_MAX_MS);
    s.redials++;
    s.timer = setTimeout(() => connect(s), delay);
  }

  function endSession(s) {
    s.ended = true;
    s.term.options.disableStdin = true;
  }

  const frameHandlers = {
    Output(s, bytes) {
      if (s.sawPresence) {
        s.term.write(bytes);
        return;
      }
      s.parsingSnapshot = true;
      s.term.write(bytes, () => {
        s.parsingSnapshot = false;
        if (s.sawPresence) s.suppressInput = false;
      });
    },
    SizeChanged(s, size) {
      s.term.resize(size.cols, size.rows);
    },
    Exit(s, exit) {
      s.exit = exit;
      s.term.options.disableStdin = true;
      renderStatus(s);
    },
    Presence(s, p) {
      s.sawPresence = true;
      if (!s.parsingSnapshot) s.suppressInput = false;
      s.attached = s.everAttached = true;
      s.redials = 0;
      s.dropped = false;
      s.notFoundSince = 0;
      s.wrapperOnline = p.wrapperOnline;
      s.viewers = p.viewers;
      renderStatus(s);
    },
  };

  function send(s, kind, value) {
    if (!s.ws || s.ws.readyState !== WebSocket.OPEN) return false;
    s.ws.send(AW.encode(kind, value));
    return true;
  }

  function sendInput(s, bytes) {
    if (s.suppressInput || s.exit || s.ended) return;
    for (let i = 0; i < bytes.length; i += AW.maxInput) {
      send(s, "Input", bytes.subarray(i, i + AW.maxInput));
    }
  }

  // sendSize asks for the size that fits this window. The wrapper decides,
  // and SizeChanged applies its answer, so fit.fit() is never called.
  function sendSize(s) {
    const d = s.fit.proposeDimensions();
    if (!d || !(d.cols > 0) || !(d.rows > 0)) return;
    const clamp = (n) => Math.max(1, Math.min(1000, n));
    send(s, "ResizeRequest", { cols: clamp(d.cols), rows: clamp(d.rows) });
  }

  function renderStatus(s) {
    let text = `Live, ${s.viewers} ${s.viewers === 1 ? "viewer" : "viewers"}`;
    let tone = "ok";
    if (s.exit) {
      text = `Exited with code ${s.exit.code}${s.exit.signal ? ` (${s.exit.signal})` : ""}`;
      tone = "ended";
    } else if (s.ended) {
      text = "Session ended";
      tone = "ended";
    } else if (!s.attached) {
      text = s.everAttached ? "Reconnecting\u2026" : "Connecting\u2026";
      tone = "warn";
    } else if (!s.wrapperOnline) {
      text = "Agent host offline";
      tone = "warn";
    }
    $("status").textContent = text;
    $("status").dataset.tone = tone;
  }

  function toast(message, tone) {
    const el = document.createElement("div");
    el.className = `toast ${tone || ""}`;
    el.textContent = message;
    $("toasts").append(el);
    setTimeout(() => el.remove(), TOAST_MS);
  }

  $("login").addEventListener("submit", signIn);
  $("signout").addEventListener("click", () => signOut());
  window.addEventListener("hashchange", route);
  route();
})();
