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
    $("upload").hidden = name !== "session";
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
      transfers: new Map(), // id -> upload, see addUpload
      cleanup: [],
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
    for (const t of s.transfers.values()) {
      if (t.offered) send(s, "Cancel", { id: t.id });
    }
    $("transfers").replaceChildren();
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
      endSession(s, "The session ended before the upload finished.");
      return;
    }
    if (code === AW.close.Protocol) {
      endSession(s, "The relay closed the connection with a protocol error.");
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

  function endSession(s, message) {
    s.ended = true;
    s.term.options.disableStdin = true;
    failTransfers(s, message);
  }

  const frameHandlers = {
    Output(s, bytes) {
      s.term.write(bytes);
    },
    SizeChanged(s, size) {
      s.term.resize(size.cols, size.rows);
    },
    Exit(s, exit) {
      s.exit = exit;
      s.term.options.disableStdin = true;
      failTransfers(s, "The process exited before the upload finished.");
      renderStatus(s);
    },
    TransferStatus(s, st) {
      onTransferStatus(s, st);
    },
    Presence(s, p) {
      const wasReady = ready(s);
      s.attached = s.everAttached = true;
      s.redials = 0;
      s.dropped = false;
      s.notFoundSince = 0;
      s.wrapperOnline = p.wrapperOnline;
      s.viewers = p.viewers;
      if (!wasReady && ready(s)) {
        for (const t of s.transfers.values()) {
          if (t.offer) offer(s, t);
        }
      }
      renderStatus(s);
    },
  };

  // ready means frames sent now reach the wrapper.
  function ready(s) {
    return s.attached && s.wrapperOnline && !s.exit && !s.ended && s.ws !== null && s.ws.readyState === WebSocket.OPEN;
  }

  function send(s, kind, value) {
    if (!s.ws || s.ws.readyState !== WebSocket.OPEN) return false;
    s.ws.send(AW.encode(kind, value));
    return true;
  }

  function sendInput(s, bytes) {
    if (s.exit || s.ended) return;
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

  // Uploads. Each upload is
  //   {id, name, data, offer, offered, awaitingCancel, sent, acked, row}
  // sent and acked are byte counts. acked comes from receiving statuses and
  // is the flow-control credit.

  async function addUpload(s, file) {
    const inbox = s.info && s.info.inbox;
    if (inbox && !inbox.enabled) {
      toast(AW.failText.disabled, "error");
      return;
    }
    if (inbox && file.size > inbox.maxBytes) {
      toast(`${file.name}: ${AW.failText.too_large}`, "error");
      return;
    }
    if (s.exit || s.ended) {
      toast("The session has ended.", "error");
      return;
    }
    const t = {
      id: hex(crypto.getRandomValues(new Uint8Array(AW.chunkIdBytes))),
      name: file.name,
      data: null,
      offer: null,
      offered: false,
      awaitingCancel: false,
      sent: 0,
      acked: 0,
      row: null,
    };
    t.row = transferRow(s, t);
    s.transfers.set(t.id, t);
    setProgress(t, "hashing");
    try {
      // WebCrypto has no streaming digest, so the whole file is read first.
      const data = new Uint8Array(await file.arrayBuffer());
      const sha256 = hex(new Uint8Array(await crypto.subtle.digest("SHA-256", data)));
      t.data = data;
      t.offer = { id: t.id, name: file.name, size: data.length, sha256 };
    } catch (err) {
      finishTransfer(s, t, `${file.name}: could not read the file: ${err.message}`, "error");
      return;
    }
    if (s.transfers.get(t.id) !== t) return;
    if (ready(s)) {
      offer(s, t);
    } else {
      setProgress(t, "waiting for the agent host");
    }
  }

  // offer starts t from byte 0. After a drop the wrapper may still hold the
  // earlier attempt, so a repeat offer cancels it first and ignores statuses
  // until that cancel is answered. If the repeat offer comes back done, the
  // file was saved before the drop and is not sent again.
  function offer(s, t) {
    t.sent = 0;
    t.acked = 0;
    t.awaitingCancel = t.offered;
    if (t.offered) send(s, "Cancel", { id: t.id });
    send(s, "Offer", t.offer);
    t.offered = true;
    setProgress(t, "offered");
  }

  function onTransferStatus(s, st) {
    const t = s.transfers.get(st.id);
    if (!t || !t.offer) return;
    if (t.awaitingCancel) {
      if (st.state === "failed" && st.reason === "cancelled") t.awaitingCancel = false;
      return;
    }
    if (st.state === "receiving") {
      t.acked = st.received;
      setProgress(t);
      pump(s, t);
    } else if (st.state === "done") {
      finishTransfer(s, t, `Saved ${st.name} to ${st.path}`, "ok");
    } else {
      finishTransfer(s, t, `${t.name}: ${AW.failText[st.reason] || st.reason}`, "error");
    }
  }

  function pump(s, t) {
    const windowBytes = AW.maxChunk * AW.uploadWindow;
    while (ready(s) && t.sent < t.data.length && t.sent - t.acked < windowBytes) {
      const end = Math.min(t.sent + AW.maxChunk, t.data.length);
      send(s, "Chunk", { id: t.id, offset: t.sent, data: t.data.subarray(t.sent, end) });
      t.sent = end;
    }
  }

  function cancelTransfer(s, t) {
    if (t.offered) send(s, "Cancel", { id: t.id });
    finishTransfer(s, t, `${AW.failText.cancelled}: ${t.name}`, "");
  }

  function finishTransfer(s, t, message, tone) {
    s.transfers.delete(t.id);
    t.row.remove();
    toast(message, tone);
  }

  function failTransfers(s, message) {
    for (const t of [...s.transfers.values()]) finishTransfer(s, t, `${t.name}: ${message}`, "error");
  }

  function transferRow(s, t) {
    const li = document.createElement("li");
    const label = document.createElement("span");
    label.className = "name";
    const bar = document.createElement("progress");
    bar.max = 1;
    bar.value = 0;
    const cancel = document.createElement("button");
    cancel.type = "button";
    cancel.className = "link";
    cancel.textContent = "Cancel";
    cancel.addEventListener("click", () => cancelTransfer(s, t));
    li.append(label, bar, cancel);
    $("transfers").append(li);
    return li;
  }

  function setProgress(t, phase) {
    const label = t.row.querySelector(".name");
    const bar = t.row.querySelector("progress");
    if (phase) {
      label.textContent = `${t.name} (${phase})`;
      return;
    }
    bar.value = t.offer.size > 0 ? t.acked / t.offer.size : 0;
    label.textContent = `${t.name} (${Math.floor(100 * bar.value)}%)`;
  }

  const hex = (bytes) => Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");

  function toast(message, tone) {
    const el = document.createElement("div");
    el.className = `toast ${tone || ""}`;
    el.textContent = message;
    $("toasts").append(el);
    setTimeout(() => el.remove(), TOAST_MS);
  }

  const hasFiles = (e) => e.dataTransfer && [...e.dataTransfer.types].includes("Files");

  function uploadAll(files) {
    for (const f of files) {
      if (session) addUpload(session, f);
    }
  }

  $("login").addEventListener("submit", signIn);
  $("signout").addEventListener("click", () => signOut());
  $("upload").addEventListener("click", () => $("file").click());
  $("file").addEventListener("change", (e) => {
    uploadAll(e.target.files);
    e.target.value = "";
  });
  $("session").addEventListener("dragover", (e) => {
    if (session && hasFiles(e)) {
      e.preventDefault();
      $("dropzone").hidden = false;
    }
  });
  $("session").addEventListener("dragleave", (e) => {
    if (!$("session").contains(e.relatedTarget)) $("dropzone").hidden = true;
  });
  $("session").addEventListener("drop", (e) => {
    e.preventDefault();
    $("dropzone").hidden = true;
    uploadAll(e.dataTransfer.files);
  });
  window.addEventListener("hashchange", route);
  route();
})();
