// trace-worker.js — the /ws/trace consumer.
//
// It owns the trace WebSocket so every event line stays off the main thread.
// Two jobs (DESIGN §13):
//   1. Durable capture: append the full, unabridged raw stream to OPFS via a
//      sync access handle (only available inside a Worker), batched.
//   2. Display feed: forward a throttled, formatted tail to the main thread for
//      the ring-buffer DOM, coalesced to ~30 fps.
// HEARTBEAT events are excluded from the display feed and instead drive the
// status row; trace_meta envelopes update the drop counters (DESIGN §7).

const FLUSH_MS = 200;
const FLUSH_BYTES = 64 * 1024;
const FORWARD_MS = 33; // ~30 fps
const TICK_MS = 150; // residual flush/forward when the stream goes quiet
const SIZE_CAP = 512 * 1024 * 1024; // OPFS ceiling before rotation (oldest dropped)
const MAX_BACKOFF_MS = 10000;

const encoder = new TextEncoder();

let ws = null;
let currentId = null;
let closedByMain = false;
let backoff = 500;

let handle = null; // OPFS sync access handle
let writeOffset = 0;
let pending = [];
let pendingBytes = 0;
let lastFlush = 0;

let displayBatch = [];
let lastForward = 0;

// Status (status row): liveness + cumulative counters.
let alive = false;
let emitted = 0; // sum of HEARTBEAT events_emitted_delta
let tracerDrops = 0; // bloodhound's own drop_count_total
let relayDrops = 0; // GW backpressure drops, from trace_meta

self.onmessage = (e) => {
  const m = e.data;
  if (m.type === "open") openStream(m.id);
  else if (m.type === "close") closeStream();
  else if (m.type === "export") exportData();
};

self.setInterval(() => {
  forwardBatch();
  flush();
}, TICK_MS);

async function openStream(id) {
  closeStream(); // tear down any previous subscription first
  closedByMain = false;
  currentId = id;
  emitted = 0;
  tracerDrops = 0;
  relayDrops = 0;
  alive = false;
  postStatus();
  await openHandle(id);
  connect();
}

async function openHandle(id) {
  try {
    const root = await navigator.storage.getDirectory();
    const f = await root.getFileHandle(`trace-${id}.ndjson`, { create: true });
    handle = await f.createSyncAccessHandle();
    handle.truncate(0);
    writeOffset = 0;
    post({ type: "capture", enabled: true });
  } catch (err) {
    handle = null; // persistence unavailable; the live display still works
    post({ type: "capture", enabled: false, reason: String(err) });
  }
}

function connect() {
  const proto = self.location.protocol === "https:" ? "wss" : "ws";
  ws = new WebSocket(`${proto}://${self.location.host}/ws/trace?id=${encodeURIComponent(currentId)}`);
  ws.onopen = () => {
    backoff = 500;
    post({ type: "conn", state: "open" });
  };
  ws.onmessage = (ev) => handleLine(ev.data);
  ws.onclose = () => {
    post({ type: "conn", state: "closed" });
    alive = false;
    postStatus();
    // Auto-reconnect with backoff; tail -F -n0 resumes from the file end, so the
    // only loss is the brief reconnection window (DESIGN §9).
    if (!closedByMain) {
      self.setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
    }
  };
  ws.onerror = () => {};
}

function handleLine(text) {
  if (typeof text !== "string") return; // trace channel is text-only
  persist(text); // durable record keeps everything, heartbeats included

  let obj;
  try {
    obj = JSON.parse(text);
  } catch {
    queueDisplay({ formatted: text, raw: text, etype: "?" });
    return;
  }

  if (obj.type === "trace_meta") {
    relayDrops = obj.dropped | 0;
    postStatus();
    return;
  }

  const etype = obj.event && obj.event.type;
  if (etype === "HEARTBEAT") {
    alive = true;
    const a = obj.args || {};
    if (typeof a.events_emitted_delta === "number") emitted += a.events_emitted_delta;
    if (typeof a.drop_count_total === "number") tracerDrops = a.drop_count_total;
    postStatus();
    return; // excluded from the log stream
  }

  const h = obj.header || {};
  const ev = obj.event || {};
  queueDisplay({
    raw: text,
    etype: etype || "?",
    name: ev.name != null ? String(ev.name) : "",
    comm: h.comm || "",
    pid: h.pid != null ? h.pid : null,
    rc: obj.return_code != null ? obj.return_code : null,
    detail: formatArgs(etype, ev.name, obj.args || {}),
    cmd: commandLine(ev.name, obj.args || {}),
    t: new Date().toTimeString().slice(0, 8),
    // Plain one-liner retained for substring filtering and the raw-off fallback.
    formatted: formatEvent(obj),
  });
}

// commandLine reconstructs the launched command for execve/execveat anchors.
function commandLine(name, a) {
  if (name === "execve" || name === "execveat") {
    if (Array.isArray(a.argv) && a.argv.length) return a.argv.join(" ");
    return a.filename || a.pathname || "";
  }
  return "";
}

// ---- Durable capture (OPFS) ------------------------------------------------

function persist(text) {
  if (!handle) return;
  const bytes = encoder.encode(text + "\n");
  pending.push(bytes);
  pendingBytes += bytes.length;
  if (pendingBytes >= FLUSH_BYTES || performance.now() - lastFlush >= FLUSH_MS) flush();
}

function flush() {
  if (!handle || pending.length === 0) return;
  // Size cap with rotation: drop the whole capture once the ceiling is hit
  // (the GW drop policy, not this, is the live-stream backstop — §11).
  if (writeOffset + pendingBytes > SIZE_CAP) {
    handle.truncate(0);
    writeOffset = 0;
    post({ type: "rotated" });
  }
  for (const b of pending) {
    handle.write(b, { at: writeOffset });
    writeOffset += b.length;
  }
  handle.flush();
  pending = [];
  pendingBytes = 0;
  lastFlush = performance.now();
}

// ---- Display feed ----------------------------------------------------------

function queueDisplay(item) {
  displayBatch.push(item);
  if (performance.now() - lastForward >= FORWARD_MS) forwardBatch();
}

function forwardBatch() {
  if (displayBatch.length === 0) return;
  post({ type: "batch", lines: displayBatch });
  displayBatch = [];
  lastForward = performance.now();
}

function postStatus() {
  post({ type: "status", alive, emitted, tracerDrops, relayDrops });
}

// ---- Export ----------------------------------------------------------------

function exportData() {
  if (!handle) {
    post({ type: "export-data", bytes: null });
    return;
  }
  flush();
  const size = handle.getSize();
  const buf = new Uint8Array(size);
  if (size > 0) handle.read(buf, { at: 0 });
  post({ type: "export-data", bytes: buf.buffer, filename: `trace-${currentId}.ndjson` }, [buf.buffer]);
}

function closeStream() {
  closedByMain = true;
  if (ws) {
    try {
      ws.close();
    } catch {}
    ws = null;
  }
  flush();
  if (handle) {
    try {
      handle.flush();
      handle.close();
    } catch {}
    handle = null;
  }
}

// ---- One-line formatting (DESIGN §7) ---------------------------------------

function formatEvent(o) {
  const h = o.header || {};
  const ev = o.event || {};
  const type = ev.type || "?";
  let name = ev.name != null ? String(ev.name) : "";
  if (/^\d+$/.test(name)) name = "syscall #" + name; // numeric Tier-1 syscall id
  const detail = formatArgs(type, ev.name, o.args || {});
  const rc = o.return_code != null ? "→ " + o.return_code : "";
  const proc = `(pid ${h.pid != null ? h.pid : "?"}${h.comm ? " " + h.comm : ""})`;
  // Displayed time is the browser's receive time; header.timestamp is not a wall
  // clock (two clocks across event types), so it is not rendered here (§7).
  const time = new Date().toTimeString().slice(0, 8);
  return [time, pad(type, 10), pad(name, 12), pad(detail, 26), pad(rc, 6), proc].join("  ");
}

function formatArgs(type, name, a) {
  switch (type) {
    case "SYSCALL":
    case "TRACEPOINT":
      if (a.filename) return a.filename;
      if (a.pathname) return a.pathname;
      if (a.syscall_nr != null) return "nr=" + a.syscall_nr;
      return "";
    case "TTY":
    case "PACKET":
      if (typeof a.data === "string") return `"…" (b64 ${b64len(a.data)}B)`;
      return "";
    case "LIFECYCLE":
      return a.filename || a.executable || a.comm || "";
    case "LSM":
      return a.filename || a.path || (name ? String(name) : "");
    default:
      return "";
  }
}

function b64len(s) {
  const n = s.length;
  const pad = s.endsWith("==") ? 2 : s.endsWith("=") ? 1 : 0;
  return Math.max(0, Math.floor((n * 3) / 4) - pad);
}

function pad(s, n) {
  s = String(s);
  return s.length >= n ? s : s + " ".repeat(n - s.length);
}

function post(msg, transfer) {
  self.postMessage(msg, transfer || []);
}
