// History: charts over a day (hourly buckets) or a week (daily ones), drawn in plain SVG. Each
// chart is one job: columns for counts over time, a line for memory, bars for totals, and a strip
// for the mode timeline. Series colors are the reference palette's slots in a fixed order;
// results wear the status colors. Every mark has a <title>, the browser's own hover tooltip, and
// every chart with two or more series a legend, so nothing rests on color alone.

import { h, s, icon, dur, bytes, stamp, clock } from "./dom.js";
import { api, changed } from "./store.js";
import { empty, section, modeLabel } from "./views.js";

export { history };

const H = { range: "day", data: {}, loading: false, error: "", at: {} };

const RESULT_COLORS = { pass: "var(--success)", fail: "var(--destructive)", cancelled: "var(--muted-foreground)", lost: "var(--warning)" };
const SIZES = ["small", "medium", "large", "gpu"];
const KINDS = ["go-test", "go-build", "js-test", "py-test", "gradle", "build", "gpu", "other"];
const slot = (i) => "var(--s" + ((i % 8) + 1) + ")";
const MODE_COLORS = { full: "var(--s1)", shared: "var(--s3)", mypc: "var(--muted-foreground)", yield: "var(--s2)" };
const WHY = { yielding: "yielding to a game", mypc: "My PC", full: "caps full", "gpu-busy": "GPU busy" };

async function load(range, force) {
  if (!force && H.data[range] && Date.now() - H.at[range] < 60000) return;
  H.loading = true;
  H.error = "";
  changed();
  try {
    H.data[range] = await api("/api/history?range=" + range);
    H.at[range] = Date.now();
  } catch (e) {
    H.error = e.message;
  }
  H.loading = false;
  changed();
}

// Charts are drawn at the width they are shown at, so their 10px labels stay 10px: W is set before
// each chart is made, from the page's width.
let W = 640;
const sized = (w, make) => ((W = Math.max(260, Math.floor(w))), make());

function label(b, range) {
  const d = new Date(b.start);
  return range === "day" ? d.toLocaleTimeString(undefined, { hour: "numeric" }) : d.toLocaleDateString(undefined, { weekday: "short", day: "numeric", timeZone: "UTC" }); // a week's days are UTC days
}

function sum(o) {
  return Object.values(o || {}).reduce((a, b) => a + b, 0);
}

function nice(max) {
  if (max <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(max));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= max) return m * p;
  return 10 * p;
}

// Stacked columns, one per bucket; keys in a fixed order so a color always means one thing.
function columns(buckets, range, get, keys, color, fmt = String, height = 150) {
  const top = 8, bottom = 18, left = 30;
  const plotH = height - top - bottom;
  const max = nice(Math.max(0, ...buckets.map((b) => keys.reduce((a, k) => a + (get(b)[k] || 0), 0))));
  const step = (W - left) / buckets.length;
  const bw = Math.max(2, Math.min(28, step * 0.62));
  const kids = [];
  for (const f of [0, 0.5, 1]) {
    const y = top + plotH * (1 - f);
    kids.push(s("line", { class: "axis", x1: left, x2: W, y1: y, y2: y }), s("text", { x: left - 6, y: y + 3, "text-anchor": "end" }, fmt(max * f)));
  }
  const every = Math.ceil(buckets.length / (range === "day" ? 6 : 7));
  buckets.forEach((b, i) => {
    const x = left + step * i + (step - bw) / 2;
    let y = top + plotH;
    const vals = get(b);
    const lines = [label(b, range)];
    keys.forEach((k, ki) => {
      const v = vals[k] || 0;
      if (!v) return;
      lines.push(k + ": " + fmt(v));
      const hgt = (v / max) * plotH;
      // A 2px gap between stacked segments, in the surface's color, keeps them apart.
      kids.push(s("rect", { x, y: y - hgt + 1, width: bw, height: Math.max(1, hgt - 2), rx: 2, fill: color(k, ki) }));
      y -= hgt;
    });
    if (i % every === 0) kids.push(s("text", { x: x + bw / 2, y: height - 4, "text-anchor": "middle" }, label(b, range)));
    kids.push(s("rect", { class: "hit", x: left + step * i, y: top, width: step, height: plotH }, s("title", null, lines.length > 1 ? lines.join("\n") : lines[0] + ": none")));
  });
  return s("svg", { viewBox: `0 0 ${W} ${height}`, role: "img" }, kids);
}

function legend(keys, color, name = (k) => k) {
  return h("div", { class: "legend" }, keys.map((k, i) => h("span", null, h("i", { "data-bg": color(k, i) }), name(k))));
}

// Memory in use against the cap: a 2px line over a faint area, the cap dashed.
function memory(buckets, range) {
  const height = 150, top = 8, bottom = 18, left = 30;
  const plotH = height - top - bottom;
  const cap = Math.max(...buckets.map((b) => b.memCap), 1);
  const max = Math.max(cap, ...buckets.map((b) => b.memUsed));
  const step = (W - left) / buckets.length;
  const x = (i) => left + step * i + step / 2;
  const y = (v) => top + plotH * (1 - v / max);
  const pts = buckets.map((b, i) => x(i).toFixed(1) + "," + y(b.memUsed).toFixed(1));
  const kids = [
    s("line", { class: "axis", x1: left, x2: W, y1: top + plotH, y2: top + plotH }),
    s("line", { x1: left, x2: W, y1: y(cap), y2: y(cap), stroke: "var(--destructive-foreground)", "stroke-dasharray": "4 4", "stroke-width": 1 }),
    s("text", { x: left - 6, y: y(cap) + 3, "text-anchor": "end" }, bytes(cap).replace(" GiB", "G")),
    s("text", { x: left - 6, y: top + plotH + 3, "text-anchor": "end" }, "0"),
    s("polygon", { points: [x(0).toFixed(1) + "," + (top + plotH), ...pts, x(buckets.length - 1).toFixed(1) + "," + (top + plotH)].join(" "), fill: "var(--s1)", "fill-opacity": 0.14 }),
    s("polyline", { points: pts.join(" "), fill: "none", stroke: "var(--s1)", "stroke-width": 2, "stroke-linejoin": "round" }),
  ];
  const every = Math.ceil(buckets.length / (range === "day" ? 6 : 7));
  buckets.forEach((b, i) => {
    if (i % every === 0) kids.push(s("text", { x: x(i), y: height - 4, "text-anchor": "middle" }, label(b, range)));
    kids.push(s("rect", { class: "hit", x: left + step * i, y: top, width: step, height: plotH }, s("title", null, label(b, range) + ": " + bytes(b.memUsed) + " of " + bytes(b.memCap))));
  });
  return s("svg", { viewBox: `0 0 ${W} ${height}`, role: "img", "aria-label": "Memory in use against the cap" }, kids);
}

// The mode timeline: a strip of the effective mode, yields in their own color, and under it the
// runs turned away in each bucket, so it is plain what games cost agents.
function timeline(d, range) {
  const buckets = d.buckets;
  const t0 = Date.parse(buckets[0].start);
  const span = (range === "day" ? 3600e3 : 86400e3) * buckets.length;
  const left = 52;
  const x = (t) => left + ((Math.min(Math.max(t, t0), t0 + span) - t0) / span) * (W - left);
  const kids = [s("text", { x: left - 6, y: 19, "text-anchor": "end" }, "mode")];
  for (const seg of d.segments || []) {
    const a = x(Date.parse(seg.start)), b = x(Date.parse(seg.end));
    if (b - a < 0.5) continue;
    const key = seg.mode === "mypc" && seg.reason ? "yield" : seg.mode;
    const what = key === "yield" ? "Yielding: " + seg.reason.replace(/^gaming:\s*/, "") : modeLabel(seg.mode);
    kids.push(s("rect", { x: a + 1, y: 8, width: Math.max(1, b - a - 2), height: 16, rx: 3, fill: MODE_COLORS[key] }, s("title", null, what + "\n" + stamp(seg.start) + " – " + stamp(seg.end) + " (" + dur((Date.parse(seg.end) - Date.parse(seg.start)) / 1000) + ")")));
  }
  const max = Math.max(1, ...buckets.map((b) => sum(b.refusals.byWhy)));
  const step = (W - left) / buckets.length;
  kids.push(s("text", { x: left - 6, y: 52, "text-anchor": "end" }, "refused"), s("line", { class: "axis", x1: left, x2: W, y1: 62, y2: 62 }));
  buckets.forEach((b, i) => {
    const n = sum(b.refusals.byWhy);
    const bx = left + step * i + step * 0.2;
    if (n) kids.push(s("rect", { x: bx, y: 62 - (n / max) * 26, width: Math.max(2, step * 0.6), height: (n / max) * 26, rx: 2, fill: "var(--s8)" }));
    const why = Object.entries(b.refusals.byWhy || {}).map(([k, v]) => v + " " + (WHY[k] || k));
    kids.push(s("rect", { class: "hit", x: left + step * i, y: 32, width: step, height: 30 }, s("title", null, label(b, range) + ": " + (n ? n + " refused (" + why.join(", ") + ")" : "none refused"))));
  });
  const every = Math.ceil(buckets.length / (range === "day" ? 6 : 7));
  buckets.forEach((b, i) => i % every === 0 && kids.push(s("text", { x: left + step * i + step / 2, y: 78, "text-anchor": "middle" }, label(b, range))));
  return s("svg", { viewBox: `0 0 ${W} 82`, role: "img", "aria-label": "Mode timeline and refused runs" }, kids);
}

// Totals as horizontal bars, biggest first, labelled with their value.
function hbars(entries, color, fmt = String, limit = 6) {
  entries = entries.filter(([, v]) => v > 0).sort((a, b) => b[1] - a[1]);
  if (!entries.length) return h("p", { class: "row-empty" }, "Nothing in this range.");
  const max = entries[0][1];
  return h(
    "div",
    { class: "hbars" },
    entries.slice(0, limit).map(([k, v], i) => h("div", { class: "hbar", title: k + ": " + fmt(v) }, h("span", { class: "truncate" }, k), h("div", { class: "bar" }, h("i", { "data-w": (v / max) * 100, "data-bg": color(k, i) })), h("span", { class: "val" }, fmt(v)))),
  );
}

function totals(buckets, get) {
  const out = {};
  for (const b of buckets) for (const [k, v] of Object.entries(get(b) || {})) out[k] = (out[k] || 0) + v;
  return out;
}

function chart(title, sub, body, wide) {
  return h("div", { class: "group chart" + (wide ? " chart-wide" : "") }, h("h3", null, title), sub && h("p", { class: "sub" }, sub), body);
}

const history = {
  mount(el) {
    this.sig = "";
    this.update(el);
    load(H.range);
  },
  update(el) {
    // Only its own changes redraw it: the stream's events would otherwise redraw the charts every
    // few seconds and close a tooltip being read.
    const sig = [H.range, H.at[H.range], H.loading, H.error, el.clientWidth].join("|");
    if (sig === this.sig && el.firstChild) return;
    this.sig = sig;
    const seg = h(
      "div",
      { class: "segmented", role: "radiogroup", "aria-label": "Range" },
      [
        ["day", "Day"],
        ["week", "Week"],
      ].map(([id, l]) =>
        h(
          "button",
          {
            role: "radio",
            "aria-checked": H.range === id ? "true" : "false",
            onclick: () => {
              H.range = id;
              load(id);
              changed();
            },
          },
          l,
        ),
      ),
    );
    const d = H.data[H.range];
    const head = h("div", { class: "page-head machine-head" }, h("div", null, h("h2", null, "History"), h("p", null, "What agents used the PC for, and what games cost them.")), seg);
    let body;
    if (!d && H.loading) body = empty("retry", "Loading history…", null);
    else if (!d && H.error) body = empty("alert", "Couldn't load the history", H.error, h("button", { class: "btn btn-sm btn-outline", onclick: () => load(H.range, true) }, "Try again"));
    else if (!d) body = null;
    else body = this.charts(d, el.clientWidth);
    el.replaceChildren(h("div", { class: "page" }, head, body));
  },
  charts(d, avail) {
    const narrow = window.matchMedia("(max-width: 767px)").matches;
    const gutter = narrow ? 12 : 20;
    const wide = Math.min(avail, 1024) - 2 * gutter - 34;
    const half = narrow ? wide : (wide + 34 - 16) / 2 - 34;
    const B = d.buckets || [];
    if (!B.length) return empty("chart", "No history yet", "boxd records jobs, boxes and modes from when it starts.");
    const range = d.range || H.range;
    const byResult = totals(B, (b) => b.jobs.byResult);
    const jobsN = sum(byResult);
    const hours = sum(totals(B, (b) => b.boxHours.bySize));
    const refused = sum(totals(B, (b) => b.refusals.byWhy));
    const passRate = jobsN ? Math.round(((byResult.pass || 0) / Math.max(1, jobsN - (byResult.cancelled || 0))) * 100) + "%" : "—";
    // The median per repo over the range: each bucket's median weighted by its jobs, which is
    // close enough to read from and needs no second call.
    const med = {};
    const wt = {};
    for (const b of B)
      for (const [r, v] of Object.entries(b.medianSeconds.byRepo || {})) {
        const n = b.jobs.byRepo[r] || 1;
        med[r] = (med[r] || 0) + v * n;
        wt[r] = (wt[r] || 0) + n;
      }
    const medians = Object.keys(med).map((r) => [r, med[r] / wt[r]]);
    const sizesSeen = SIZES.filter((k) => B.some((b) => b.boxHours.bySize[k]));
    const modeKeys = ["full", "shared", "mypc", "yield"];
    const per = range === "day" ? "an hour" : "a day";
    return [
      h(
        "div",
        { class: "stats" },
        [
          [jobsN, "jobs"],
          [passRate, "passed, of those not cancelled"],
          [hours.toFixed(hours < 10 ? 1 : 0), "box-hours"],
          [refused, "runs turned away"],
        ].map(([v, l]) => h("div", { class: "group stat" }, h("b", null, String(v)), h("span", null, l))),
      ),
      h(
        "div",
        { class: "charts" },
        chart("Mode and yields", "The PC's mode, the games it yielded to, and the runs it turned away in each " + (range === "day" ? "hour" : "day") + ".", [sized(wide, () => timeline(d, range)), legend(modeKeys, (k) => MODE_COLORS[k], (k) => (k === "yield" ? "Yielding to a game" : modeLabel(k))), h("div", { class: "legend" }, h("span", null, h("i", { "data-bg": "var(--s8)" }), "Runs refused (hover for why)"))], true),
        chart("Jobs by result", "Jobs started in " + per + ".", [sized(wide, () => columns(B, range, (b) => b.jobs.byResult, ["pass", "fail", "cancelled", "lost"], (k) => RESULT_COLORS[k])), legend(["pass", "fail", "cancelled", "lost"], (k) => RESULT_COLORS[k])], true),
        chart("Box-hours by size", "How long boxes existed.", [sized(half, () => columns(B, range, (b) => b.boxHours.bySize, SIZES, (k) => slot(SIZES.indexOf(k)), (v) => (v >= 10 || v === 0 ? Math.round(v) : v.toFixed(1)) + "")), legend(sizesSeen.length ? sizesSeen : SIZES, (k) => slot(SIZES.indexOf(k)))]),
        chart("Memory in use", "Mean of the minute samples, against the " + bytes(Math.max(...B.map((b) => b.memCap))) + " cap (dashed).", sized(half, () => memory(B, range))),
        chart("What agents use boxes for", "Jobs by kind.", hbars(Object.entries(totals(B, (b) => b.jobs.byKind)), (k) => slot(KINDS.indexOf(k)))),
        chart("Median run time", "Per repo, finished jobs.", hbars(medians, () => "var(--s7)", dur)),
        chart("Busiest repos", "Jobs started.", hbars(Object.entries(totals(B, (b) => b.jobs.byRepo)), () => "var(--s1)")),
        chart("Busiest agents", "Jobs started, by session.", hbars(Object.entries(totals(B, (b) => b.jobs.byAgent)), () => "var(--s3)")),
      ),
      h("p", { class: "muted", "data-now": clock.now() }, H.loading ? "Refreshing…" : (range === "day" ? "Each bar is an hour. " : "Each bar is a day, midnight to midnight UTC. ") + (H.at[range] ? "Loaded " + new Date(H.at[range]).toLocaleTimeString() + "." : "")),
    ];
  },
};
