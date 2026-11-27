// The Settings page, laid out as Murmur's: sections, a row per setting with a line under it saying
// what it does, saved at once. Only the theme and the read-only update check are interactive: the
// theme lives in this browser, and Check for updates asks boxd for the latest stable release on
// GitHub without touching the running builds. The rest is shown from what boxd reports
// (Machine.Config, the caps, the tray's heartbeat and build) and changes where it lives today: the
// mode on Overview or in the tray, the caps and times in the host's setup.

import { h, icon, bytes, dur, timeEl } from "./dom.js";
import { S, post, trayAlive, changed } from "./store.js";
import { hint } from "./tip.js";
import { section, modeLabel, MODE_HINTS } from "./views.js";
import { drift, releaseStatus } from "./versions.js";

export { settings, themeSwitch };

// The theme, as Murmur's: Dim by default, Dark and Light as options, chosen with a segmented
// radio group. theme.js applied the saved one before the page painted.
const THEMES = [
  ["dark", "Dark"],
  ["dim", "Dim"],
  ["light", "Light"],
];
const THEME_KEY = "agent-box:theme";
function theme() {
  return document.documentElement.dataset.theme || "dim";
}
function setTheme(t) {
  document.documentElement.dataset.theme = t;
  try {
    localStorage.setItem(THEME_KEY, t);
  } catch {}
  changed();
}

function themeSwitch() {
  return h(
    "div",
    { class: "segmented", role: "radiogroup", "aria-label": "Theme" },
    THEMES.map(([id, label]) =>
      h(
        "button",
        {
          role: "radio",
          "aria-checked": theme() === id ? "true" : "false",
          tabindex: theme() === id ? 0 : -1,
          "data-key": "theme-" + id,
          onclick: () => setTheme(id),
          onkeydown: (e) => {
            // One Tab stop; the arrow keys move the choice (WCAG 4.1.2).
            const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[e.key];
            if (!step) return;
            e.preventDefault();
            const i = THEMES.findIndex(([t]) => t === theme());
            const next = THEMES[(i + step + THEMES.length) % THEMES.length][0];
            setTheme(next);
            document.querySelector(`[data-key="theme-${next}"]`)?.focus();
          },
        },
        label,
      ),
    ),
  );
}

// A row: its title, one line saying what it is, and its value or control on the right.
function row(title, description, control, feedback) {
  return h(
    "div",
    { class: "row" },
    h("div", { class: "row-grid" }, h("div", null, h("div", { class: "row-title" }, title), description && h("p", { class: "row-description" }, description), feedback), control && h("div", { class: "row-control" }, control)),
  );
}

const value = (...kids) => h("span", { class: "num settings-value" }, ...kids);
const group = (rows) => h("div", { class: "group" }, rows);

function build(v) {
  if (!v || !v.commit) return value("unknown");
  const short = v.commit.slice(0, 7) + (v.modified ? "+" : "");
  return value(v.tag ? v.tag + " · " + short : short);
}

// A build and, under it, how long boxd has seen it run.
function buildCell(v, what) {
  return h("div", { class: "settings-stack" }, build(v), v?.since ? h("span", { class: "muted" }, timeEl("ago", v.since, what + " ")) : null);
}

let checking = false;
let checkErr = "";

async function checkUpdates() {
  if (checking) return;
  checking = true;
  checkErr = "";
  changed();
  try {
    const info = await post("/api/updates/check");
    // An event from a newer check in another tab can arrive before this response.
    if (!S.updates?.checkedAt || Date.parse(info.checkedAt) > Date.parse(S.updates.checkedAt)) S.updates = info;
  } catch (e) {
    checkErr = e.message || "Couldn't check for updates.";
  } finally {
    checking = false;
    changed();
  }
}

function statusBadge(st) {
  if (!st) return null;
  return h("span", hint(st.why, { class: "badge " + st.cls }, true), st.label);
}

function latestRow(machineId) {
  const u = S.updates;
  return row(
    "Latest stable release",
    [u?.checkedAt ? timeEl("ago", u.checkedAt, "Last checked ") : "Not checked yet", ". Running builds stay as they are."],
    [
      value(u?.checkedAt ? u.latest || "none published" : "unknown"),
      h(
        "button",
        {
          class: "btn btn-sm btn-outline settings-check",
          type: "button",
          "data-key": "check-updates-" + machineId,
          "aria-disabled": checking ? "true" : null,
          onclick: checkUpdates,
        },
        icon("retry", checking ? "spin" : ""),
        checking ? "Checking…" : "Check for updates",
      ),
    ],
    checkErr ? h("p", { class: "row-error settings-error", role: "alert" }, checkErr) : null,
  );
}

function updates(m) {
  const alive = trayAlive(m);
  const note = drift(m.boxd, m.tray, alive);
  const boxdStatus = releaseStatus(m.boxd, S.updates);
  const trayStatus = releaseStatus(m.tray, S.updates, { isTray: true, trayUp: alive });
  return [
    latestRow(m.id),
    row(
      ["boxd", statusBadge(boxdStatus)],
      m.boxd?.commit ? "The server behind this page." : "This server hasn't reported its build.",
      m.boxd?.commit ? buildCell(m.boxd, "running since") : h("span", { class: "badge badge-outline" }, "unknown"),
    ),
    row(
      ["Tray", statusBadge(trayStatus)],
      m.tray?.commit ? "Applies the mode on the PC." : "This host hasn't reported a tray build.",
      m.tray?.commit ? buildCell(m.tray, "this build since") : h("span", { class: "badge badge-outline" }, "not reporting"),
    ),
    note ? row(note.warn ? "Out of step" : "Different builds", note.why, h("span", { class: "badge " + (note.warn ? "badge-warn" : "") }, note.text)) : null,
  ];
}

function modeRows(m) {
  const alive = trayAlive(m);
  const who = m.enforcer === "headless" ? "Headless enforcer" : "Tray";
  const req = m.modeRequested;
  const rows = [
    row("Mode", "Who the PC is for right now. Switch it on Overview, or from the tray, which owns the setting: it can't be saved from here.", [
      h("span", hint(MODE_HINTS[m.mode] || "", { class: "badge " + (m.mode === "mypc" ? "" : "badge-on") }, true), modeLabel(m.mode)),
      req ? h("span", { class: "badge badge-outline" }, "→ " + modeLabel(req.mode) + " pending") : null,
    ]),
    row("Enforcer", "What applies the mode to the boxes. With none reporting, a mode change waits and boxes keep the CPUs they have.", h("span", { class: "badge " + (alive ? "badge-on" : "badge-outline") }, alive ? who : "none reporting")),
  ];
  for (const s of m.config?.sizes || []) {
    if (s.gpu && !m.config.gpu) continue;
    rows.push(row((s.gpu ? "GPU" : s.name[0].toUpperCase() + s.name.slice(1)) + " box", "CPUs a box of this size gets in Shared and in Full. It also holds " + bytes(s.memory) + " of memory and " + bytes(s.disk) + " of disk.", value(s.sharedCpus + " Shared · " + s.fullCpus + " Full")));
  }
  return rows;
}

function limitRows(m) {
  const c = m.caps || {};
  const k = m.config || {};
  // An unset cap doesn't limit; one set to 0 does, so it reads 0.
  const cap = (key, fmt = String) => value(k.capped?.[key] ? fmt(c[key]) : "no limit");
  return [
    row("Boxes", "Most boxes that may exist at once; a run that needs one more waits in the queue.", cap("instances")),
    row("CPUs", "Most CPUs all boxes together may hold, counting each at its size's Full share.", cap("cpu")),
    row("Memory", "Most memory all boxes together may be given. The tray sets it from the mode.", cap("memory", bytes)),
    row("Disk", "Most disk all boxes together, and the image they start from, may use.", cap("disk", bytes)),
    row("Idle expiry", "A box made for a run expires this long after it is made unless a job keeps it.", value(dur(k.idleExpiry))),
    row("Expiry during a job", "While a job runs, its box's expiry is pushed out to this long from the start.", value(dur(k.jobExpiry))),
    row("Kept after a job", "A throwaway box and its log stay up to this long once the job ends, so an agent can pull what it built. A queued job, or another launch that needs the room, takes it sooner.", value(dur(k.keptAfterJob))),
  ];
}

const gpuWhy = (why) =>
  ({
    device: "the PC's GPU isn't visible in the Linux distro",
    blocked: "the project blocks GPU devices",
    both: "the PC's GPU isn't visible in the Linux distro and the project blocks GPU devices",
  })[why] || "no device";

function gpuRows(m) {
  const on = m.config?.gpu;
  return [row("GPU boxes", on ? "This host can give a box a GPU. One runs at a time, always as a throwaway." : "This host can't give a box a GPU (" + gpuWhy(m.config?.gpuWhy) + "), so runs that ask for one are turned away.", h("span", { class: "badge " + (on ? "badge-on" : "badge-outline") }, on ? "Available" : "Not on this host"))];
}

const settings = {
  mount(el) {
    this.update(el);
  },
  leave() {
    if (!checking) checkErr = "";
  },
  update(el) {
    const ms = [...S.machines.values()];
    const many = ms.length > 1;
    const perMachine = (title, rows) => ms.map((m) => section(title, many ? m.name : null, group(rows(m).filter(Boolean))));
    el.replaceChildren(
      h(
        "div",
        { class: "page" },
        h("div", { class: "page-head" }, h("h2", null, "Settings"), h("p", null, "How the dashboard looks, and how this host is set up. Changes save as you make them.")),
        section("Appearance", null, group([row("Theme", "Dim is the default. Saved in this browser only, so each device keeps its own.", themeSwitch())])),
        perMachine("Updates", updates),
        perMachine("Mode", modeRows),
        perMachine("Limits", limitRows),
        perMachine("GPU", gpuRows),
      ),
    );
  },
};
