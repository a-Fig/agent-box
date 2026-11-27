// What the page knows of the PC, how it stays current, and the controls. The page polls nothing:
// one /api/events stream sends the whole state first, then each machine, box or job that changes,
// whole. When the stream drops, the page asks /api/state; when that fails too, the PC is
// unreachable, and it asks again every 10s (docs/ui.md).

import { clock } from "./dom.js";

export { S, api, post, applyUpdates, start, retryNow, changed, setChanged, toast, jobsOf, machineOf, boxOf, trayAlive, RETRY_MS };

const RETRY_MS = 10000;
const TRAY_STALE_MS = 90000; // dash.TrayStaleAfter

const S = {
  conn: "loading", // loading, live, down
  lastTry: 0,
  nextTry: 0,
  machines: new Map(),
  boxes: new Map(),
  gen: 0, // counts snapshots (a reconnect replaces the jobs, so what was fetched before may be stale)
  jobs: new Map(), // by key: running, paused and the last finished, from the state and events
  updates: null, // { latest, checkedAt } from boxd's last stable release check, or null
  toast: null,
};

let onChange = () => {};
function setChanged(fn) {
  onChange = fn;
}
function changed() {
  onChange();
}

class ApiError extends Error {
  constructor(status, code, message, warning) {
    super(message);
    this.status = status;
    this.code = code;
    this.warning = warning;
  }
}

async function api(path, opts = {}) {
  let res;
  try {
    res = await fetch(path, { ...opts, signal: opts.signal || AbortSignal.timeout(15000), headers: { Accept: "application/json", ...(opts.headers || {}) } });
  } catch {
    throw new ApiError(0, "unreachable", "The PC didn't answer.");
  }
  if (res.status === 204) return null;
  let body = null;
  try {
    body = await res.json();
  } catch {}
  if (!res.ok) {
    const fallback = res.status >= 502 ? "The PC didn't answer." : "Request failed (" + res.status + ").";
    throw new ApiError(res.status, body?.code || "http-" + res.status, body?.error || fallback, body?.warning);
  }
  return body;
}

// Controls are POSTs with a JSON body and the X-Agent-Box header, which another site can't send
// without a preflight that boxd never grants.
function post(path, body) {
  return api(path, { method: "POST", body: JSON.stringify(body || {}), headers: { "Content-Type": "application/json", "X-Agent-Box": "1" } });
}

function applyUpdates(info) {
  const at = info?.checkedAt;
  const last = S.updates?.checkedAt;
  // Both paths carry UTC timestamps from boxd. Drop the trailing Z so whole
  // seconds sort before fractional seconds, and keep precision past milliseconds.
  if (at && (!last || at.slice(0, -1) > last.slice(0, -1))) S.updates = info;
}

function applyState(st) {
  clock.skew = Date.parse(st.now) - Date.now();
  S.machines = new Map((st.machines || []).map((m) => [m.id, m]));
  S.boxes = new Map((st.boxes || []).map((b) => [b.machine + "/" + b.name, b]));
  S.jobs = new Map((st.jobs || []).map((j) => [j.key, j]));
  S.updates = st.updates || null;
  S.gen++;
  S.conn = "live";
}

let es = null;
let retryTimer = 0;

async function start() {
  clearTimeout(retryTimer);
  S.lastTry = Date.now();
  try {
    applyState(await api("/api/state"));
    changed();
    openStream();
  } catch {
    down();
  }
}

function down() {
  es?.close();
  es = null;
  S.conn = "down";
  S.nextTry = Date.now() + RETRY_MS;
  changed();
  clearTimeout(retryTimer);
  retryTimer = setTimeout(start, RETRY_MS);
}

function retryNow() {
  S.conn = "loading";
  changed();
  start();
}

function openStream() {
  es?.close();
  es = new EventSource("/api/events");
  const on = (name, fn) =>
    es.addEventListener(name, (e) => {
      let data;
      try {
        data = JSON.parse(e.data);
      } catch {
        return;
      }
      fn(data);
      changed();
    });
  on("state", applyState);
  on("machine", (m) => S.machines.set(m.id, m));
  on("box", (b) => S.boxes.set(b.machine + "/" + b.name, b));
  on("box-gone", (b) => S.boxes.delete(b.machine + "/" + b.name));
  on("job", (j) => S.jobs.set(j.key, j));
  on("job-gone", (j) => S.jobs.delete(j.key));
  on("updates", applyUpdates);
  // On a drop the page starts over from the state, after a breath, so a stream that keeps
  // failing doesn't spin.
  es.onerror = () => {
    es?.close();
    es = null;
    clearTimeout(retryTimer);
    retryTimer = setTimeout(start, 1500);
  };
}

let toastTimer = 0;
function toast(text, bad = false) {
  S.toast = { text, bad };
  changed();
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    S.toast = null;
    changed();
  }, 5000);
}

function jobsOf(pred = () => true) {
  return [...S.jobs.values()].filter(pred).sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt));
}

function machineOf(id) {
  return S.machines.get(id);
}

function boxOf(machine, name) {
  return S.boxes.get(machine + "/" + name);
}

function trayAlive(m) {
  return !!m.trayLastSeen && clock.now() - Date.parse(m.trayLastSeen) < TRAY_STALE_MS;
}
