// History: charts over a day (hourly buckets) or a week (daily ones), drawn in plain SVG. Each
// chart is one job: columns for counts over time, a line for memory, bars for totals, and a strip
// for the mode timeline. Series colors are the reference palette's slots in a fixed order;
// results wear the status colors. Every mark has a tooltip (tip.js), shown on hover, focus and tap,
// and every chart with two or more series a legend, so nothing rests on color alone.

import { h, s, icon, dur, bytes, stamp, clock } from "./dom.js";
import { api, changed } from "./store.js";
import { empty, section, modeLabel, MODE_HINTS, RESULT_HINTS } from "./views.js";
import { rove, hint } from "./tip.js";

export { history };

const H = { range: "day", data: {}, loading: {}, error: {}, at: {} };

const RESULT_COLORS = { pass: "var(--success)", fail: "var(--destructive)", cancelled: "var(--muted-foreground)", lost: "var(--warning)" };
const SIZES = ["small", "medium", "large", "gpu"];
const KINDS = ["go-test", "go-build", "js-test", "py-test", "gradle", "build", "gpu", "other"];
const slot = (i) => "var(--s" + (i < 0 ? 8 : (i % 8) + 1) + ")"; // -1, a kind the table lacks, is drawn as other
const MODE_COLORS = { full: "var(--s1)", shared: "var(--s3)", mypc: "var(--muted-foreground)" };
const WHY = { mypc: "My PC", full: "caps full", "gpu-busy": "GPU busy" };

async function load(range, force) {
  if (!force && H.data[range] && Date.now() - H.at[range] < 60000) return;
  if (H.loading[range]) return;
  H.loading[range] = true;
  H.error[range] = "";
  changed();
  try {
    H.data[range] = await api("/api/history?range=" + range);
    H.at[range] = Date.now();
  } catch (e) {
    H.error[range] = e.message;
  }
  H.loading[range] = false;
  changed();
}

// Charts are drawn at the width they are shown at, so their 10px labels stay 10px: W is set before
// each chart is made, from the page's width.
let W = 640;
const sized = (w, make) => ((W = Math.max(260, Math.floor(w))), make());

// A mark's tooltip text, for tip.js and, as one line, for screen readers. rove makes the first
// mark of each chart its tab stop.
const tip = (text, props) => ({ ...props, tabindex: -1, "data-tip": text, "aria-label": text.replace(/\n/g, "; ") });

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
    kids.push(s("rect", tip(lines.length > 1 ? lines.join("\n") : lines[0] + ": none", { class: "hit", x: left + step * i, y: top, width: step, height: plotH })));
  });
  return rove(s("svg", { viewBox: `0 0 ${W} ${height}`, role: "group" }, kids));
}

// A legend entry with a hint (about) explains its word, for someone who hasn't read the docs.
function legend(keys, color, name = (k) => k, about = () => null) {
  return h("div", { class: "legend" }, keys.map((k, i) => h("span", about(k) ? hint(about(k), {}, true) : null, h("i", { "data-bg": color(k, i) }), name(k))));
}

const SIZE_HINTS = {
  small: "Hours small boxes existed: the smallest size, for light runs.",
  medium: "Hours medium boxes existed: the default size.",
  large: "Hours large boxes existed: for big test suites and builds.",
  gpu: "Hours the GPU box existed: one at a time, with the PC's GPU.",
};
const REFUSED_HINT = "Runs box turned away: it exited 75 and nothing started, because of My PC, no room under the limits, or the GPU in use.";
const QUEUED_HINT = "Runs that waited in the queue for room, counted when they joined it. They start by themselves once their size fits.";

// Memory in use: a 2px line over a faint area. Use mostly sits far under the cap, so the scale
// is set by use (the larger of its peak x 1.25 and half the cap), with a gridline and label at
// each GiB step; the cap is a dashed line when it fits under the top, else a note in the corner.
function memory(buckets, range) {
  const height = 150, top = 8, bottom = 18, left = 36;
  const plotH = height - top - bottom;
  const GiB = 2 ** 30;
  const cap = Math.max(...buckets.map((b) => b.memCap), 1);
  const peak = Math.max(0, ...buckets.map((b) => b.memUsed));
  const max = Math.max(peak * 1.25, cap / 2);
  // Gridlines at a round number of GiB, at most five of them.
  const stepGiB = [0.5, 1, 2, 4, 8, 16, 32].find((g) => max / (g * GiB) <= 5) || 64;
  const step = (W - left) / buckets.length;
  const x = (i) => left + step * i + step / 2;
  const y = (v) => top + plotH * (1 - v / max);
  const pts = buckets.map((b, i) => x(i).toFixed(1) + "," + y(b.memUsed).toFixed(1));
  const gib = (v) => (v / GiB).toFixed(v % GiB ? 1 : 0) + "G";
  const kids = [];
  for (let v = 0; v <= max + 1; v += stepGiB * GiB) {
    kids.push(s("line", { class: "axis", x1: left, x2: W, y1: y(v), y2: y(v) }), s("text", { x: left - 6, y: y(v) + 3, "text-anchor": "end" }, v ? gib(v) : "0"));
  }
  if (cap <= max) {
    kids.push(s("line", tip("Cap: " + bytes(cap), { class: "mark", x1: left, x2: W, y1: y(cap), y2: y(cap), stroke: "var(--destructive-foreground)", "stroke-dasharray": "4 4", "stroke-width": 1 })));
  } else {
    kids.push(s("text", { x: W, y: top + 2, "text-anchor": "end" }, "cap " + bytes(cap) + " ↑"));
  }
  kids.push(
    s("polygon", { points: [x(0).toFixed(1) + "," + (top + plotH), ...pts, x(buckets.length - 1).toFixed(1) + "," + (top + plotH)].join(" "), fill: "var(--s1)", "fill-opacity": 0.14 }),
    s("polyline", { points: pts.join(" "), fill: "none", stroke: "var(--s1)", "stroke-width": 2, "stroke-linejoin": "round" }),
  );
  const every = Math.ceil(buckets.length / (range === "day" ? 6 : 7));
  buckets.forEach((b, i) => {
    if (i % every === 0) kids.push(s("text", { x: x(i), y: height - 4, "text-anchor": "middle" }, label(b, range)));
    kids.push(s("rect", tip(label(b, range) + ": " + bytes(b.memUsed) + " of " + bytes(b.memCap), { class: "hit", x: left + step * i, y: top, width: step, height: plotH })));
  });
  return rove(s("svg", { viewBox: `0 0 ${W} ${height}`, role: "group", "aria-label": "Memory in use against the cap" }, kids));
}

// The mode timeline: a strip of the mode, and under it the runs turned away and the runs queued in
// each bucket, so it is plain what My PC costs agents.
function timeline(d, range) {
  const buckets = d.buckets;
  const t0 = Date.parse(buckets[0].start);
  const span = (range === "day" ? 3600e3 : 86400e3) * buckets.length;
  const left = 52;
  const x = (t) => left + ((Math.min(Math.max(t, t0), t0 + span) - t0) / span) * (W - left);
  const kids = [s("text", { x: left - 6, y: 19, "text-anchor": "end" }, "mode")];
  // Each row's marks in a group of their own, so the arrow keys move along a row and up and down
  // between rows (tip.js).
  const segs = [], refusedHits = [], queuedHits = [];
  for (const seg of d.segments || []) {
    const a = x(Date.parse(seg.start)), b = x(Date.parse(seg.end));
    if (b - a < 0.5) continue;
    segs.push(s("rect", tip(modeLabel(seg.mode) + "\n" + stamp(seg.start) + " – " + stamp(seg.end) + " (" + dur((Date.parse(seg.end) - Date.parse(seg.start)) / 1000) + ")", { class: "mark", x: a + 1, y: 8, width: Math.max(1, b - a - 2), height: 16, rx: 3, fill: MODE_COLORS[seg.mode] })));
  }
  kids.push(s("g", { class: "row" }, segs));
  const max = Math.max(1, ...buckets.map((b) => sum(b.refusals.byWhy)));
  const step = (W - left) / buckets.length;
  kids.push(s("text", { x: left - 6, y: 52, "text-anchor": "end" }, "refused"), s("line", { class: "axis", x1: left, x2: W, y1: 62, y2: 62 }));
  buckets.forEach((b, i) => {
    const n = sum(b.refusals.byWhy);
    const bx = left + step * i + step * 0.2;
    if (n) kids.push(s("rect", { x: bx, y: 62 - (n / max) * 26, width: Math.max(2, step * 0.6), height: (n / max) * 26, rx: 2, fill: "var(--s8)" }));
    const why = Object.entries(b.refusals.byWhy || {}).map(([k, v]) => v + " " + (WHY[k] || k));
    refusedHits.push(s("rect", tip(label(b, range) + ": " + (n ? n + " refused (" + why.join(", ") + ")" : "none refused"), { class: "hit", x: left + step * i, y: 32, width: step, height: 30 })));
  });
  kids.push(s("g", { class: "row" }, refusedHits));
  // Runs queued, counted when they were queued; a bucket's tooltip also gives the waits of the
  // queued runs that started in it.
  const qmax = Math.max(1, ...buckets.map((b) => b.queued?.jobs || 0));
  kids.push(s("text", { x: left - 6, y: 90, "text-anchor": "end" }, "queued"), s("line", { class: "axis", x1: left, x2: W, y1: 100, y2: 100 }));
  buckets.forEach((b, i) => {
    const q = b.queued || {};
    const n = q.jobs || 0;
    const bx = left + step * i + step * 0.2;
    if (n) kids.push(s("rect", { x: bx, y: 100 - (n / qmax) * 26, width: Math.max(2, step * 0.6), height: (n / qmax) * 26, rx: 2, fill: "var(--s7)" }));
    const lines = [label(b, range) + ": " + (n ? n + " queued" : "none queued")];
    if (q.started) lines.push(q.started + " started after a median wait of " + dur(q.medianWaitSeconds));
    queuedHits.push(s("rect", tip(lines.join("\n"), { class: "hit", x: left + step * i, y: 70, width: step, height: 30 })));
  });
  kids.push(s("g", { class: "row" }, queuedHits));
  const every = Math.ceil(buckets.length / (range === "day" ? 6 : 7));
  buckets.forEach((b, i) => i % every === 0 && kids.push(s("text", { x: left + step * i + step / 2, y: 116, "text-anchor": "middle" }, label(b, range))));
  return rove(s("svg", { viewBox: `0 0 ${W} 120`, role: "group", "aria-label": "Mode timeline, refused runs and queued runs" }, kids));
}

// Totals as horizontal bars, biggest first, labelled with their value.
function hbars(entries, color, fmt = String, limit = 6, emptyLabel = "") {
  entries = entries.filter(([, v]) => v > 0).sort((a, b) => b[1] - a[1]);
  if (!entries.length) return h("p", { class: "row-empty" }, "Nothing in this range.");
  const max = entries[0][1];
  return rove(
    h(
      "div",
      { class: "hbars" },
      entries.slice(0, limit).map(([k, v], i) => {
        const name = k || emptyLabel || "(none)";
        return h("div", tip(name + ": " + fmt(v), { class: "hbar", role: "img" }), h("span", { class: "truncate" }, name), h("div", { class: "bar" }, h("i", { "data-w": (v / max) * 100, "data-bg": color(k, i) })), h("span", { class: "val" }, fmt(v)));
      }),
    ),
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
    const sig = [H.range, H.at[H.range], H.loading[H.range], H.error[H.range], el.clientWidth].join("|");
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
          hint(id === "day" ? "The last 24 hours, an hour per bar." : "The last 7 days, a day per bar, midnight to midnight UTC.", {
            role: "radio",
            "aria-checked": H.range === id ? "true" : "false",
            onclick: () => {
              H.range = id;
              load(id);
              changed();
            },
          }),
          l,
        ),
      ),
    );
    const d = H.data[H.range];
    const head = h("div", { class: "page-head machine-head" }, h("div", null, h("h2", null, "History"), h("p", null, "What agents used the PC for, and what My PC cost them.")), seg);
    let body;
    if (!d && H.loading[H.range]) body = empty("retry", "Loading history…", null);
    else if (!d && H.error[H.range]) body = empty("alert", "Couldn't load the history", H.error[H.range], h("button", { class: "btn btn-sm btn-outline", onclick: () => load(H.range, true) }, "Try again"));
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
    // Queued runs, and their median wait over the range: each bucket's median weighted by the
    // queued runs that started in it, as the run-time medians are below.
    const queuedN = B.reduce((a, b) => a + (b.queued?.jobs || 0), 0);
    const startedN = B.reduce((a, b) => a + (b.queued?.started || 0), 0);
    const wait = startedN ? B.reduce((a, b) => a + (b.queued?.medianWaitSeconds || 0) * (b.queued?.started || 0), 0) / startedN : 0;
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
    const modeKeys = ["full", "shared", "mypc"];
    const per = range === "day" ? "an hour" : "a day";
    return [
      h(
        "div",
        { class: "stats" },
        [
          [jobsN, "jobs"],
          [passRate, "passed, of those not cancelled"],
          [hours.toFixed(hours < 10 ? 1 : 0), "box-hours", "Hours boxes existed, added up over all boxes: two boxes for an hour are 2 box-hours."],
          [refused, "runs turned away", REFUSED_HINT],
          [queuedN, startedN ? "runs queued; median wait " + dur(wait) : "runs queued", QUEUED_HINT],
        ].map(([v, l, tip]) => h("div", { class: "group stat" }, h("b", null, String(v)), h("span", tip ? hint(tip, {}, true) : null, l))),
      ),
      h(
        "div",
        { class: "charts" },
        chart("Mode", "The PC's mode, and the runs it turned away or queued in each " + (range === "day" ? "hour" : "day") + ".", [sized(wide, () => timeline(d, range)), legend(modeKeys, (k) => MODE_COLORS[k], modeLabel, (k) => MODE_HINTS[k]), h("div", { class: "legend" }, h("span", hint(REFUSED_HINT, {}, true), h("i", { "data-bg": "var(--s8)" }), "Runs refused (hover for why)"), h("span", hint(QUEUED_HINT, {}, true), h("i", { "data-bg": "var(--s7)" }), "Runs queued (hover for waits)"))], true),
        chart("Jobs by result", "Jobs started in " + per + ".", [sized(wide, () => columns(B, range, (b) => b.jobs.byResult, ["pass", "fail", "cancelled", "lost"], (k) => RESULT_COLORS[k])), legend(["pass", "fail", "cancelled", "lost"], (k) => RESULT_COLORS[k], undefined, (k) => RESULT_HINTS[k])], true),
        chart("Box-hours by size", "How long boxes existed.", [sized(half, () => columns(B, range, (b) => b.boxHours.bySize, SIZES, (k) => slot(SIZES.indexOf(k)), (v) => String(Math.round(v * 10) / 10))), legend(sizesSeen.length ? sizesSeen : SIZES, (k) => slot(SIZES.indexOf(k)), undefined, (k) => SIZE_HINTS[k])]),
        chart("Memory in use", "Mean of the minute samples; the cap is " + bytes(Math.max(...B.map((b) => b.memCap))) + ".", sized(half, () => memory(B, range))),
        chart("What agents use boxes for", "Jobs by kind.", hbars(Object.entries(totals(B, (b) => b.jobs.byKind)), (k) => slot(KINDS.indexOf(k)))),
        chart("Median run time", "Per repo, finished jobs.", hbars(medians, () => "var(--s7)", dur, 6, "(no repo)")),
        chart("Busiest repos", "Jobs started.", hbars(Object.entries(totals(B, (b) => b.jobs.byRepo)), () => "var(--s1)", String, 6, "(no repo)")),
        chart("Busiest agents", "Jobs started, by session.", hbars(Object.entries(totals(B, (b) => b.jobs.byAgent)), () => "var(--s3)", String, 6, "unknown agent")),
      ),
      h("p", { class: "muted", "data-now": clock.now() }, H.loading[range] ? "Refreshing…" : (range === "day" ? "Each bar is an hour. " : "Each bar is a day, midnight to midnight UTC. ") + (H.at[range] ? "Loaded " + new Date(H.at[range]).toLocaleTimeString() + "." : "")),
    ];
  },
};
