// The whole machine, split (#294): of all a host's CPUs, memory and GPU, what Windows and the
// owner's apps use, what WSL holds beyond its boxes, what boxes use, what they are promised but
// leave idle, what is open to agents under the mode, and what the mode keeps back. The tray reads
// the machine as Windows sees it (Machine.host); boxd adds what it knows from inside WSL. A tray from
// before #294 reports no host, and the page then shows nothing of this.

import { h, bytes, clock, timeEl } from "./dom.js";
import { S } from "./store.js";
import { hint } from "./tip.js";

export { useSection, fleetLine };

// Segments in the order they stack, left to right: what is used, then what is free.
const SEGS = {
  windows: { label: "Windows and apps", about: "What Windows and the owner's apps and games use. For CPU it also holds WSL's own work beside the boxes (Linux, Incus, boxd)." },
  wsl: { label: "WSL", about: "What the agents distro holds beyond its boxes: Linux, Incus, boxd and its page cache. Windows gets it back when the boxes' memory is released." },
  boxes: { label: "Boxes", about: "What the boxes use now." },
  held: { label: "Promised to boxes", about: "Free memory within the limits the boxes that exist were given: they may take it at any time." },
  open: { label: "Open to agents", about: "Free, and room the mode lets new boxes have." },
  kept: { label: "Kept for you", about: "Free, and the mode keeps it for the owner: boxes can't have it. In My PC that is all that is free." },
};

// Boxes that hold memory: running ones, and frozen ones, which keep theirs until they stop. Only
// running ones use CPU.
const boxesOn = (m, frozen = false) => [...S.boxes.values()].filter((b) => b.machine === m.id && (b.state === "running" || (frozen && b.state === "frozen")));

// A reading is current while the enforcer that writes it beats (dash.TrayStaleAfter, 90s). An old
// one would be set against boxd's live numbers, which is worse than none.
const fresh = (m) => !!m.host && m.reachable && clock.now() - Date.parse(m.host.at) < 90000;

// Splits a resource of size total. used is what the machine uses in all; inBoxes what boxes use of
// it; wsl, for memory, what the distro holds (boxes included). held and cap are what boxes count
// against the mode's cap and the cap (capped false: no cap, so all that is free is open). promise
// shows the boxes' unused limits as held: memory, which a box may take at any time. A CPU limit
// isn't held, so for CPU it only counts against the cap. In My PC nothing is open.
function split({ total, used, inBoxes, wsl = null, held, cap, capped, promise, mypc }) {
  const out = {};
  if (wsl != null) {
    out.windows = Math.max(0, used - wsl);
    out.wsl = Math.max(0, wsl - inBoxes);
  } else out.windows = Math.max(0, used - inBoxes);
  out.boxes = inBoxes;
  let free = Math.max(0, total - out.windows - (out.wsl || 0) - inBoxes);
  out.held = promise ? Math.min(free, Math.max(0, held - inBoxes)) : 0;
  free -= out.held;
  out.open = mypc ? 0 : capped ? Math.min(free, Math.max(0, cap - held)) : free;
  out.kept = free - out.open;
  return out;
}

function memSplit(m) {
  const u = m.host;
  const inBoxes = boxesOn(m, true).reduce((a, b) => a + (b.memUsed || 0), 0);
  return split({ total: u.memTotal, used: u.memUsed, inBoxes, wsl: Math.max(m.wslMemory || 0, inBoxes), held: m.allocated?.memory || 0, cap: m.caps?.memory || 0, capped: m.config?.capped?.memory, promise: true, mypc: m.mode === "mypc" });
}

function cpuSplit(m) {
  const u = m.host;
  const inBoxes = boxesOn(m).reduce((a, b) => a + ((b.cpuPct || 0) / 100) * (b.cpus || 0), 0);
  return split({ total: u.cpus, used: (u.cpuPct / 100) * u.cpus, inBoxes, held: m.allocated?.cpu || 0, cap: m.caps?.cpu || 0, capped: m.config?.capped?.cpu, mypc: m.mode === "mypc" });
}

const cores = (n) => (n < 10 ? n.toFixed(1) : Math.round(n)) + " CPUs";

// One resource: its name and total, a stacked bar, and a legend that gives each segment's amount.
function bar(title, total, parts, fmt, totalFmt = fmt) {
  const keys = Object.keys(SEGS).filter((k) => parts[k] > 0);
  const pct = (v) => (total > 0 ? (v / total) * 100 : 0);
  const summary = title + " of " + totalFmt(total) + ": " + keys.map((k) => SEGS[k].label + " " + fmt(parts[k])).join(", ");
  return h(
    "div",
    { class: "use" },
    h("div", { class: "meter-label" }, h("span", null, title), h("strong", { class: "num" }, totalFmt(total))),
    h("div", { class: "use-bar", role: "img", "aria-label": summary }, keys.map((k) => h("i", hint(SEGS[k].label + ": " + fmt(parts[k]) + " (" + Math.round(pct(parts[k])) + "%)\n" + SEGS[k].about, { class: "use-" + k, "data-w": pct(parts[k]) })))),
    h("div", { class: "legend use-legend" }, keys.map((k) => h("span", hint(SEGS[k].about, {}, "use-" + title + "-" + k), h("i", { class: "use-" + k }), SEGS[k].label + " ", h("b", { class: "num" }, fmt(parts[k]))))),
  );
}

function gpuBar(m) {
  const g = m.host.gpu;
  const box = boxesOn(m).find((b) => b.gpu);
  const used = { label: box ? "In use, with " + box.name + " holding it" : "In use", about: box ? "The GPU's memory in use. A GPU box (" + box.name + ") holds the GPU now, beside Windows and the owner's apps." : "The GPU's memory in use, by Windows and the owner's apps: no box holds the GPU now." };
  const free = g.memTotal - g.memUsed;
  const pct = (v) => (v / g.memTotal) * 100;
  return h(
    "div",
    { class: "use" },
    h("div", { class: "meter-label" }, h("span", hint("Its memory, and how busy it is: " + Math.round(g.pct) + "%.", {}, "use-gpu"), g.name || "GPU"), h("strong", { class: "num" }, bytes(g.memTotal) + " · " + Math.round(g.pct) + "% busy")),
    h("div", { class: "use-bar", role: "img", "aria-label": "GPU memory of " + bytes(g.memTotal) + ": " + bytes(g.memUsed) + " in use" }, h("i", hint(used.label + ": " + bytes(g.memUsed) + "\n" + used.about, { class: box ? "use-boxes" : "use-windows", "data-w": pct(g.memUsed) })), h("i", hint("Free: " + bytes(free), { class: "use-open", "data-w": pct(free) }))),
  );
}

// The host page's section: one bar per resource, or nothing without the tray's reading.
function useSection(m) {
  if (!m.host) return null;
  const u = m.host;
  // A reading that has gone stale says when it was taken and shows nothing else.
  if (!fresh(m)) return h("div", { class: "group" }, h("p", { class: "row-empty" }, "The tray last read the whole machine ", timeEl("ago", u.at), ". It isn't reporting now, so there is nothing current to show."));
  return h(
    "div",
    { class: "group" },
    h(
      "div",
      { class: "use-stack" },
      bar("CPU", u.cpus, cpuSplit(m), cores, (n) => n + " CPUs"),
      bar("Memory", u.memTotal, memSplit(m), bytes),
      u.gpu ? gpuBar(m) : null,
      h("p", { class: "muted use-note" }, "CPU is how busy each part is, in CPUs' worth; memory is what each part holds. The tray reads Windows every 30 seconds while boxes are live, so Windows' share can lag by that much."),
    ),
  );
}

// Overview's one line: the fleet's machines added up.
function fleetLine(ms) {
  const hs = ms.filter(fresh);
  if (!hs.length) return null;
  const sum = (f) => hs.reduce((a, m) => a + f(m), 0);
  const cpu = hs.map(cpuSplit), mem = hs.map(memSplit);
  const add = (xs, k) => xs.reduce((a, x) => a + x[k], 0);
  return h(
    "p",
    { class: "fleet-line" },
    h("span", null, sum((m) => m.host.cpus) + " CPUs and " + bytes(sum((m) => m.host.memTotal)) + " in all"),
    h("span", null, "boxes use " + cores(add(cpu, "boxes")) + " and " + bytes(add(mem, "boxes"))),
    h("span", null, cores(add(cpu, "open")) + " and " + bytes(add(mem, "open")) + " open to agents"),
  );
}
