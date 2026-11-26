// Building elements and formatting what they show. h makes elements from tags, attributes and
// text only, as Murmur's does, so nothing an agent wrote (a reason, a command, a log) can become
// markup: kids that are strings become text nodes.

export { h, s, icon, clock, ago, until, dur, bytes, pct, stamp, hhmm, shortRepo, plural, timeEl, tickTimes };

const NS = "http://www.w3.org/2000/svg";

function build(el, props, kids) {
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.setAttribute("class", v);
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false || kid === "") continue;
    el.append(kid instanceof Node ? kid : String(kid));
  }
  return el;
}

function h(tag, props, ...kids) {
  return build(document.createElement(tag), props, kids);
}

function s(tag, props, ...kids) {
  return build(document.createElementNS(NS, tag), props, kids);
}

// Icons from Lucide (ISC; see THIRD_PARTY_NOTICES), drawn at T3 Code's 1.75 stroke, as Murmur's.
const shapes = {
  grid: [["rect", { x: 3, y: 3, width: 7, height: 7, rx: 1 }], ["rect", { x: 14, y: 3, width: 7, height: 7, rx: 1 }], ["rect", { x: 14, y: 14, width: 7, height: 7, rx: 1 }], ["rect", { x: 3, y: 14, width: 7, height: 7, rx: 1 }]],
  box: [["path", { d: "M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z" }], ["path", { d: "m3.3 7 8.7 5 8.7-5" }], ["path", { d: "M12 22V12" }]],
  list: [["path", { d: "M3 6h.01M3 12h.01M3 18h.01M8 6h13M8 12h13M8 18h13" }]],
  chart: [["path", { d: "M3 3v16a2 2 0 0 0 2 2h16" }], ["path", { d: "M18 17V9M13 17V5M8 17v-3" }]],
  clock: [["circle", { cx: 12, cy: 12, r: 9 }], ["path", { d: "M12 7v5l3 2" }]],
  search: [["circle", { cx: 11, cy: 11, r: 8 }], ["path", { d: "m21 21-4.3-4.3" }]],
  x: [["path", { d: "M18 6 6 18" }], ["path", { d: "m6 6 12 12" }]],
  retry: [["path", { d: "M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8" }], ["path", { d: "M21 3v5h-5" }]],
  branch: [["path", { d: "M15 6a9 9 0 0 0-9 9V3" }], ["circle", { cx: 18, cy: 6, r: 3 }], ["circle", { cx: 6, cy: 18, r: 3 }]],
  laptop: [["rect", { x: 2, y: 3, width: 20, height: 14, rx: 2 }], ["path", { d: "M8 21h8M12 17v4" }]],
  offline: [["path", { d: "M12 20h.01M8.5 16.43a5 5 0 0 1 7 0M5 12.86a10 10 0 0 1 5.17-2.69M19 12.86a10 10 0 0 0-2-1.52M2 8.82a15 15 0 0 1 4.18-2.64M22 8.82a15 15 0 0 0-11.29-3.76" }], ["path", { d: "m2 2 20 20" }]],
  alert: [["path", { d: "m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3" }], ["path", { d: "M12 9v4M12 17h.01" }]],
  menu: [["path", { d: "M4 6h16M4 12h16M4 18h16" }]],
  back: [["path", { d: "m12 19-7-7 7-7" }], ["path", { d: "M19 12H5" }]],
  user: [["circle", { cx: 12, cy: 8, r: 4 }], ["path", { d: "M20 21a8 8 0 0 0-16 0" }]],
  hourglass: [["path", { d: "M5 22h14M5 2h14" }], ["path", { d: "M17 22v-4.172a2 2 0 0 0-.586-1.414L12 12l-4.414 4.414A2 2 0 0 0 7 17.828V22" }], ["path", { d: "M7 2v4.172a2 2 0 0 0 .586 1.414L12 12l4.414-4.414A2 2 0 0 0 17 6.172V2" }]],
  chevron: [["path", { d: "m9 18 6-6-6-6" }]],
  front: [["path", { d: "M5 3h14" }], ["path", { d: "m18 13-6-6-6 6" }], ["path", { d: "M12 7v14" }]],
};

function icon(name, cls = "") {
  return s(
    "svg",
    { viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", "stroke-width": 1.75, "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true", class: ("icon " + cls).trim() },
    shapes[name].map(([t, a]) => s(t, a)),
  );
}

// Time. The server's clock is the one that counts: clock.skew is its now minus ours, set from
// each state.
const clock = { skew: 0, now: () => Date.now() + clock.skew };

function ago(t) {
  const sec = Math.max(0, (clock.now() - Date.parse(t)) / 1000);
  if (sec < 45) return "just now";
  if (sec < 3600) return Math.round(sec / 60) + "m ago";
  if (sec < 86400) return Math.floor(sec / 3600) + "h ago";
  return Math.floor(sec / 86400) + "d ago";
}

function until(t) {
  const sec = (Date.parse(t) - clock.now()) / 1000;
  if (sec <= 0) return "expired";
  return "in " + dur(sec);
}

function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return sec + "s";
  const m = Math.floor(sec / 60);
  if (m < 60) return m + "m " + String(sec % 60).padStart(2, "0") + "s";
  const hr = Math.floor(m / 60);
  if (hr < 48) return hr + "h " + String(m % 60).padStart(2, "0") + "m";
  return Math.floor(hr / 24) + "d " + (hr % 24) + "h";
}

// A time that moves on by itself ("3m ago", "in 2h", a running job's "6m 12s"): the ticker
// rewrites only these spans each second, so nothing else is redrawn for the clock's sake.
function fmtTime(kind, t) {
  if (kind === "ago") return ago(t);
  if (kind === "until") return until(t);
  return dur((clock.now() - Date.parse(t)) / 1000);
}

function timeEl(kind, t, pre = "", post = "") {
  return h("span", { "data-time": kind, "data-t": t, "data-pre": pre, "data-post": post, title: stamp(t) }, pre + fmtTime(kind, t) + post);
}

function tickTimes(root) {
  for (const el of root.querySelectorAll("[data-time]")) {
    const text = el.dataset.pre + fmtTime(el.dataset.time, el.dataset.t) + el.dataset.post;
    if (el.textContent !== text) el.textContent = text;
  }
  // Something that shows from a time on, such as a queued job's nudged badge.
  for (const el of root.querySelectorAll("[data-show-after]")) {
    const hide = clock.now() < Date.parse(el.dataset.showAfter);
    if (el.hidden !== hide) el.hidden = hide;
  }
}

function stamp(t) {
  return new Date(t).toLocaleString(undefined, { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}

// A time of day, "16:02", for a time today; the stamp for one before.
function hhmm(t) {
  const d = new Date(t);
  if (d.toDateString() !== new Date(clock.now()).toDateString()) return stamp(t);
  return d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}

function bytes(n) {
  const gib = n / 2 ** 30;
  if (gib >= 1) return (gib >= 100 ? gib.toFixed(0) : gib.toFixed(1)) + " GiB";
  return Math.round(n / 2 ** 20) + " MiB";
}

function pct(a, b) {
  return b > 0 ? Math.min(100, Math.max(0, (a / b) * 100)) : 0;
}

function shortRepo(url) {
  return (url || "").replace(/\/+$/, "").replace(/\.git$/, "").split("/").pop() || "(no repo)";
}

function plural(n, word, many = word + "s") {
  return n + " " + (n === 1 ? word : many);
}
