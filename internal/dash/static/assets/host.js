// A host's page (#fleet/<id>, #289): everything the dashboard knows about one host of the fleet, in
// one place. Its mode and what fits now (the card Overview shows), its running jobs and boxes, its
// builds and whether an update is out, and how it is set up. Before, the builds and setup were on
// Settings, which then held mostly things that couldn't be changed (#290).
//
// Every host is a PC running boxes in WSL today. Cloud hosts (Cloudflare Workers #277, Modal #278)
// will report a kind of their own; hostKind is the one place that knows the kinds, so the sidebar
// and this page say what a host is without assuming it has a tray.

import { h, icon, bytes, dur, plural, timeEl } from "./dom.js";
import { S, post, applyUpdates, trayAlive, changed, jobsOf } from "./store.js";
import { hint } from "./tip.js";
import { section, empty, machineCard, banners, runningRow, boxRow, queued, queueSection } from "./views.js";
import { row, group } from "./settings.js";
import { drift, releaseStatus } from "./versions.js";
import { useSection } from "./use.js";

export { host, hostKind, hostLink };

const KINDS = {
  pc: { icon: "monitor", label: "PC", about: "A Windows PC that runs boxes in WSL." },
};

function hostKind(m) {
  return KINDS[m.kind] || KINDS.pc;
}

function hostLink(m) {
  return "#fleet/" + encodeURIComponent(m.id);
}

const value = (...kids) => h("span", { class: "num settings-value" }, ...kids);

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
    applyUpdates(await post("/api/updates/check"));
  } catch (e) {
    checkErr = e.message || "Couldn't check for updates.";
  } finally {
    checking = false;
    changed();
  }
}

function statusBadge(st, key) {
  if (!st) return null;
  return h("span", hint(st.why, { class: "badge " + st.cls }, key), st.label);
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

function buildRows(m) {
  const alive = trayAlive(m);
  const note = drift(m.boxd, m.tray, alive);
  const who = m.enforcer === "headless" ? "Headless enforcer" : "Tray";
  return [
    latestRow(m.id),
    row(
      ["boxd", statusBadge(releaseStatus(m.boxd, S.updates), m.id + "/boxd/release-status")],
      m.boxd?.commit ? "The server behind this page." : "This server hasn't reported its build.",
      m.boxd?.commit ? buildCell(m.boxd, "running since") : h("span", { class: "badge badge-outline" }, "unknown"),
    ),
    row(
      ["Tray", statusBadge(releaseStatus(m.tray, S.updates), m.id + "/tray/release-status")],
      m.tray?.commit ? "Applies the mode on the PC." : "This host hasn't reported a tray build.",
      m.tray?.commit ? buildCell(m.tray, "this build since") : h("span", { class: "badge badge-outline" }, "not reporting"),
    ),
    note ? row(note.warn ? "Out of step" : "Different builds", note.why, h("span", { class: "badge " + (note.warn ? "badge-warn" : "") }, note.text)) : null,
    row(
      "Enforcer",
      "What applies the mode to the boxes: the tray while someone is logged on, a headless task while nobody is. With none reporting, a mode change waits and boxes keep the CPUs they have.",
      h("div", { class: "settings-stack" }, h("span", { class: "badge " + (alive ? "badge-on" : "badge-outline") }, alive ? who : "none reporting"), m.trayLastSeen ? h("span", { class: "muted" }, timeEl("ago", m.trayLastSeen, "seen ")) : null),
    ),
  ];
}

const gpuWhy = (why) =>
  ({
    device: "the PC's GPU isn't visible in the Linux distro",
    blocked: "the project blocks GPU devices",
    both: "the PC's GPU isn't visible in the Linux distro and the project blocks GPU devices",
  })[why] || "no device";

function setupRows(m) {
  const k = m.config || {};
  const rows = [];
  for (const s of k.sizes || []) {
    if (s.gpu && !k.gpu) continue;
    rows.push(row((s.gpu ? "GPU" : s.name[0].toUpperCase() + s.name.slice(1)) + " box", "CPUs a box of this size gets in Shared and in Full. It also holds " + bytes(s.memory) + " of memory and " + bytes(s.disk) + " of disk.", value(s.sharedCpus + " Shared · " + s.fullCpus + " Full")));
  }
  rows.push(
    row("GPU boxes", k.gpu ? "This host can give a box a GPU. One runs at a time, always as a throwaway." : "This host can't give a box a GPU (" + gpuWhy(k.gpuWhy) + "), so runs that ask for one are turned away.", h("span", { class: "badge " + (k.gpu ? "badge-on" : "badge-outline") }, k.gpu ? "Available" : "Not on this host")),
  );
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
    row("Expiry during a job", "The host checks for active work before deleting a box. A job's own timeout can still end its command.", value("Protected while active")),
    row("Kept after a job", "A throwaway box and its log stay up to this long once the job ends, so an agent can pull what it built. A queued job, or another launch that needs the room, takes it sooner.", value(dur(k.keptAfterJob))),
  ];
}

// Renaming (#291): the name is a label on the page, kept on the host so every device sees it.
// While the field is open, a redraw moves the same input into the new page rather than making
// another, so what is typed, the caret and the focus survive the stream's events.
const MAX_NAME = 40; // dash.MaxMachineName
let ed = null; // { id, saving, err }

function startRename(m) {
  ed = { id: m.id, saving: false, err: "" };
  changed();
  requestAnimationFrame(() => {
    const input = document.querySelector(`[data-key="rename-${CSS.escape(m.id)}"]`);
    input?.focus();
    input?.select();
  });
}

function stopRename() {
  ed = null;
  changed();
  requestAnimationFrame(() => document.querySelector('[data-key="rename-open"]')?.focus());
}

// The form's submit is the one way in (Enter submits it). It reads the host as it is now: another
// device may have renamed it while the field was open.
async function saveRename(id, input) {
  const m = S.machines.get(id);
  if (!ed || ed.saving || !m) return;
  const name = input.value.trim();
  if (name === m.name) return stopRename();
  ed.saving = true;
  ed.err = "";
  changed();
  try {
    S.machines.set(m.id, await post("/api/machine/name", { name }));
    stopRename();
  } catch (e) {
    if (ed) {
      ed.saving = false;
      ed.err = e.message || "Couldn't rename the host.";
    }
    changed();
    // The field was disabled while saving, which took the focus away; give it back.
    requestAnimationFrame(() => input.focus());
  }
}

function nameHead(m, el) {
  if (ed?.id !== m.id) {
    return h("div", { class: "host-name" }, h("h2", null, m.name), h("button", { class: "icon-btn", type: "button", title: "Rename", "aria-label": "Rename " + m.name, "data-key": "rename-open", onclick: () => startRename(m) }, icon("pencil")));
  }
  const key = "rename-" + m.id;
  let input = el.querySelector(`[data-key="${CSS.escape(key)}"]`);
  if (!input) {
    input = h("input", { type: "text", value: m.name, maxlength: MAX_NAME, placeholder: "Name from the host's setup", "aria-label": "Host name", "data-key": key, spellcheck: "false", autocomplete: "off" });
    input.addEventListener("keydown", (e) => e.key === "Escape" && stopRename());
  }
  input.disabled = ed.saving;
  return h(
    "div",
    null,
    h(
      "form",
      { class: "host-rename", onsubmit: (e) => (e.preventDefault(), saveRename(m.id, input)) },
      h("label", { class: "field" }, input),
      h("button", { class: "btn btn-sm btn-primary", type: "submit", disabled: ed.saving }, ed.saving ? "Saving…" : "Save"),
      h("button", { class: "btn btn-sm btn-outline", type: "button", onclick: stopRename }, "Cancel"),
    ),
    h("p", { class: "host-rename-note" }, "Only a label on this page, the same on every device. Agents keep using the host's ID, " + m.id + ". Leave it empty for the name from the host's setup."),
    ed.err ? h("p", { class: "row-error", role: "alert" }, ed.err) : null,
  );
}

const host = {
  mount(el, id) {
    this.update(el, id);
  },
  leave() {
    if (!checking) checkErr = "";
    ed = null;
  },
  update(el, id) {
    const m = S.machines.get(id);
    if (!m) {
      el.replaceChildren(h("div", { class: "page" }, empty("offline", "No such host", "The fleet has no host called " + id + ". It may have been removed.", h("a", { class: "btn btn-sm btn-outline", href: "#overview" }, "Back to Overview"))));
      return;
    }
    const kind = hostKind(m);
    const running = jobsOf((j) => j.machine === m.id && (j.state === "running" || j.state === "paused"));
    const q = queued().filter((j) => j.machine === m.id);
    const bx = [...S.boxes.values()].filter((b) => b.machine === m.id).sort((a, b) => a.name.localeCompare(b.name));
    el.replaceChildren(
      h(
        "div",
        { class: "page" },
        h("div", { class: "page-head host-head" }, h("span", { class: "host-ico" }, icon(kind.icon)), h("div", { class: "host-title" }, nameHead(m, el), ed?.id === m.id ? null : h("p", null, kind.about + (m.reachable ? "" : " It isn't answering now.")))),
        banners(m),
        section("Now", null, machineCard(m)),
        m.host ? section("Whole machine", null, useSection(m)) : null,
        section("Running jobs", running.length ? String(running.length) : null, h("div", { class: "group" }, running.length ? running.map((j) => runningRow(j)) : h("p", { class: "row-empty" }, "Nothing is running on " + m.name + "."))),
        q.length ? queueSection(q) : null,
        section("Boxes", bx.length ? plural(bx.length, "box", "boxes") : null, h("div", { class: "group" }, bx.length ? bx.map((b) => boxRow(b, false)) : h("p", { class: "row-empty" }, "No boxes on " + m.name + "."))),
        section("Builds", null, group(buildRows(m).filter(Boolean))),
        section("Box sizes", null, group(setupRows(m))),
        section("Limits and expiry", null, group(limitRows(m))),
      ),
    );
  },
};
