// Overview, Boxes and Jobs, and the controls on them. Each view has mount, which builds it when it
// opens, and update, which redraws it when something changes; a view with an input or a log
// redraws only around them, so typing and scrolling survive the stream's events.

import { h, icon, clock, dur, bytes, pct, shortRepo, plural, stamp, hhmm, timeEl } from "./dom.js";
import { S, api, post, changed, toast, jobsOf, boxOf, trayAlive } from "./store.js";
import { hint, hideTip } from "./tip.js";

export { overview, boxes, jobs, jobDetail, confirmDialog, checkDialog, empty, section, modeLabel, jobBox, fold, MODE_HINTS, RESULT_HINTS };

const MODES = [
  ["full", "Full"],
  ["shared", "Shared"],
  ["mypc", "My PC"],
];
const KINDS = ["go-test", "go-build", "js-test", "py-test", "gradle", "build", "gpu", "other"];
const RESULTS = ["pass", "fail", "cancelled", "lost"];

function modeLabel(m) {
  return (MODES.find(([id]) => id === m) || [m, m])[1];
}

// Hints (tip.js) for agent-box's words and controls, written for someone who hasn't read the docs.
// Each says what the code does: internal/mode for the modes, internal/boxd for the controls.
const MODE_HINTS = {
  full: "Boxes run at full speed: each gets its size's Full CPUs, more than in Shared, at a higher priority. For when nobody is using the PC.",
  shared: "Boxes run beside you: each gets its size's Shared CPUs at normal priority, so the PC stays smooth to use.",
  mypc: "Every box is paused, so the PC is all yours; new runs wait in the queue or are turned away. A job left paused more than 15 minutes may be lost.",
};
const RESULT_HINTS = {
  pass: "The command exited 0.",
  fail: "The command exited with a code other than 0 or 130: usually tests failed or a build broke.",
  cancelled: "Exit 130: the job was cancelled, from this page or by its agent, or the command was interrupted.",
  lost: "No exit code was read: its box was deleted, stopped or went away while it ran, or, still queued, it was dropped without starting.",
};

// What an exit code means here. 124 is box's when --timeout fired (cmd/box exitTimeout), but a
// command can exit 124 itself, so the hint says both. box
// also exits 75 when it couldn't run a job now, but such a run is never a job here (History counts
// it as refused), and a job paused or lost shows as that, so a finished job's 75 is the command's.
function exitHint(code) {
  if (code === 0) return RESULT_HINTS.pass;
  if (code === 130) return RESULT_HINTS.cancelled;
  if (code === 124) return "Exit 124: the run hit its time limit (--timeout) and box killed it, or the command itself exited 124.";
  if (code === 75) return "The command itself exited 75.";
  return "The command exited " + code + ": usually tests failed or a build broke.";
}

// Shared pieces.

function section(title, aside, ...body) {
  return h("section", { class: "section" }, h("div", { class: "section-head" }, h("h2", { class: "section-title" }, title), aside && h("span", { class: "section-aside" }, aside)), ...body);
}

function empty(ico, title, line, ...extra) {
  return h("div", { class: "empty" }, h("span", { class: "empty-ico" }, icon(ico)), h("p", { class: "empty-title" }, title), line && h("p", { class: "empty-line" }, line), ...extra);
}

function result(j) {
  if (j.state === "lost") return "lost";
  if (j.state !== "done" || j.exit == null) return "";
  return j.exit === 0 ? "pass" : j.exit === 130 ? "cancelled" : "fail";
}

// A job's badge sits inside the row's link on Overview and Jobs, where a tab stop of its own would
// be a second one for the same row: there it is the link's inner hint (tip.js), shown when the
// keyboard focuses the link.
function jobBadge(j, term = false) {
  const tip = (text, props) => hint(text, props, term || "inner");
  switch (j.state) {
    case "running":
      return h("span", tip("The command is running on its box now.", { class: "badge badge-info" }), h("span", { class: "dot dot-on dot-live" }), "running");
    case "paused":
      return h("span", tip("Its box is paused because the PC is in My PC. It carries on if the mode goes back to Shared or Full within 15 minutes (longer once its memory is moved to swap); after that it is lost. It can't be cancelled while its box is paused.", { class: "badge badge-warn" }), "paused");
    case "lost":
      return h("span", tip(RESULT_HINTS.lost, { class: "badge badge-warn" }), "lost");
  }
  const r = result(j);
  const cls = { pass: "badge-on", fail: "badge-bad", cancelled: "" }[r] ?? "";
  return h("span", r ? tip(exitHint(j.exit), { class: "badge " + cls }) : { class: "badge " + cls }, r === "fail" ? "exit " + j.exit : r);
}

const BOX_STATE_HINTS = {
  frozen: "Paused because the PC is in My PC. It resumes when the mode goes back to Shared or Full.",
  stopped: "Stopped by My PC: a GPU box at once, others after 15 minutes paused (longer once their memory is moved to swap). A job that was running on it is lost.",
};

function boxBadge(b) {
  const cls = { running: "badge-on", frozen: "badge-warn", stopped: "" }[b.state] ?? "";
  const tip = BOX_STATE_HINTS[b.state];
  return h("span", tip ? hint(tip, { class: "badge " + cls }, "box-" + b.name + "-state") : { class: "badge " + cls }, b.state);
}

// The reason is the job's headline; a job from before box run asked has none.
function reasonEl(j, cls = "row-title") {
  return j.reason ? h("p", { class: cls }, j.reason) : h("p", { class: cls + " no-reason" }, "no reason given");
}

function elapsed(j) {
  if (!j.endedAt) return timeEl("elapsed", j.startedAt);
  return dur((Date.parse(j.endedAt) - Date.parse(j.startedAt)) / 1000);
}

// A job's box is the box of that name made no later than 10 minutes after the job started
// (dash.JobBoxGrace: box run --new starts the job's clock before it launches the box). Names get
// reused, and a later box of that name holds none of this job's log.
const JOB_BOX_GRACE_MS = 10 * 60 * 1000;
function jobBox(j) {
  const b = boxOf(j.machine, j.box);
  return b && ranOn(j, b) ? b : null;
}
function ranOn(j, b) {
  return j.box === b.name && j.machine === b.machine && Date.parse(b.createdAt) <= Date.parse(j.startedAt) + JOB_BOX_GRACE_MS;
}

function sizeLabel(size, gpu, sep = " ") {
  return size + (gpu && size !== "gpu" ? sep + "GPU" : "");
}

function jobMeta(j) {
  return h(
    "div",
    { class: "meta" },
    h("span", null, icon("user"), j.agent?.session || "unknown agent", j.agent?.client ? h("span", { class: "muted" }, "· " + j.agent.client) : null),
    h("span", null, icon("branch"), shortRepo(j.repo), j.branch ? "@" + j.branch : ""),
    h("span", null, icon("box"), j.box, " · ", sizeLabel(j.size, j.gpu)),
    h("span", { title: stamp(j.startedAt) }, icon("clock"), j.state === "running" || j.state === "paused" ? elapsed(j) : [timeEl("ago", j.endedAt || j.startedAt), " · took " + elapsed(j)]),
  );
}

function lastLines(text, n) {
  const lines = (text || "").replace(/\n$/, "").split("\n");
  return lines.slice(-n).join("\n");
}

// Actions in flight, by what they act on, so a button drawn again while its request is out stays
// disabled and the action isn't sent twice.
const busy = new Set();

function jobLink(j) {
  return "#jobs/" + encodeURIComponent(j.key);
}

// Controls. Each goes through act, which keeps the button busy and says what failed.

async function act(key, fn, done) {
  if (busy.has(key)) return;
  busy.add(key);
  changed();
  try {
    const out = await fn();
    done?.(out);
  } catch (e) {
    toast(e.message || "That didn't work.", true);
  } finally {
    busy.delete(key);
    changed();
  }
}

// A control: its data-key lets the page put the focus back on it after a redraw.
function control(key, props, ...kids) {
  return h("button", { ...props, "data-key": key, disabled: props.disabled || busy.has(key) }, ...kids);
}

// The open confirm, if any. valid says whether what it acts on is still the thing it was opened
// for; after each redraw checkDialog closes it if not, so a click can't land on a replacement.
let dialog = null;

function checkDialog() {
  if (dialog && !dialog.valid()) {
    const { close, goneText } = dialog;
    close();
    toast(goneText);
  }
}

function confirmDialog({ title, body, action, danger = true, run, valid = () => true, goneText = "That changed while the dialog was open." }) {
  // The opener is found again by its data-key on close: a redraw may have replaced it.
  const opener = document.activeElement?.closest?.("[data-key]")?.dataset.key;
  const close = () => {
    scrim.remove();
    document.removeEventListener("keydown", onKey);
    dialog = null;
    const back = opener && document.querySelector(`[data-key="${CSS.escape(opener)}"]`);
    (back || document.querySelector(".content"))?.focus?.({ preventScroll: true });
  };
  const onKey = (e) => {
    if (e.key === "Escape") close();
  };
  const go = h("button", { class: "btn btn-sm " + (danger ? "btn-danger" : "btn-primary"), onclick: () => (close(), run()) }, action);
  const scrim = h(
    "div",
    { class: "scrim", onclick: (e) => e.target === scrim && close() },
    h("div", { class: "dialog", role: "alertdialog", "aria-modal": "true", "aria-labelledby": "dlg-t" }, h("h2", { id: "dlg-t" }, title), h("p", null, body), h("div", { class: "dialog-actions" }, h("button", { class: "btn btn-sm btn-ghost", onclick: close }, "Keep it"), go)),
  );
  dialog?.close();
  hideTip(); // a tap on the button that opened it showed its hint, which would sit on the dialog
  document.addEventListener("keydown", onKey);
  document.body.append(scrim);
  dialog = { close, valid, goneText };
  go.focus();
}

// A box is one generation of a name: the box of that name made at createdAt. Controls send it,
// and boxd refuses with 409 "gone" when the box of that name now is another.
function sameBox(b) {
  const now = boxOf(b.machine, b.name);
  return !!now && now.createdAt === b.createdAt;
}

function setMode(m, mode) {
  act(
    "mode-" + m.id,
    () => post("/api/mode", { mode }),
    (machine) => {
      if (machine?.id) S.machines.set(machine.id, machine);
    },
  );
}

function cancelJob(j) {
  confirmDialog({
    valid: () => ["running", "paused"].includes(S.jobs.get(j.key)?.state),
    goneText: "The job ended before it was cancelled.",
    title: "Cancel this job?",
    body: "Its command on " + j.box + " is killed and the job ends with exit 130. The agent that started it sees it cancelled.",
    action: "Cancel job",
    run: () => act("cancel-" + j.key, () => post("/api/jobs/" + encodeURIComponent(j.key) + "/cancel"), (job) => job?.key && S.jobs.set(job.key, job)),
  });
}

function deleteBox(b) {
  const running = jobsOf((j) => j.box === b.name && j.machine === b.machine && (j.state === "running" || j.state === "paused"));
  confirmDialog({
    valid: () => sameBox(b),
    goneText: "Box " + b.name + " went away or was replaced, so nothing was deleted.",
    title: "Delete " + b.name + "?",
    body: (running.length ? plural(running.length, "running job") + " on it will be lost. " : "") + "Boxes are compute, not storage: anything on it that wasn't pushed is gone.",
    action: "Delete box",
    run: () => act("del-" + b.name, () => post("/api/boxes/" + encodeURIComponent(b.name) + "/delete", { createdAt: b.createdAt }), () => S.boxes.delete(b.machine + "/" + b.name)),
  });
}

function extendBox(b, hours) {
  act("ext-" + b.name, () => post("/api/boxes/" + encodeURIComponent(b.name) + "/extend", { hours, createdAt: b.createdAt }), (box) => box?.name && S.boxes.set(box.machine + "/" + box.name, box));
}

// The queue (docs/queue.md): runs that couldn't start yet wait in boxd's line and start when their
// size fits. A waiting job's key is box/id/queuedAt; when it starts, job-gone takes that key and the
// running job comes under its own key with the same box and ID. Controls name the job as box does,
// "<box>.<id>".

const WHY = {
  full: (j) => "no room for a " + j.size + " box",
  "gpu-busy": () => "the GPU is in use",
  mypc: () => "the PC is in My PC",
};
// After dash.QueueNudge in line the agent is told once that the job still waits.
const NUDGE_MS = 20 * 60 * 1000;

function jobName(j) {
  return j.box + "." + j.id;
}

// The nudged badge, shown by the 1s ticker once its time comes (dom.js tickTimes).
function nudgedBadge(j) {
  const at = new Date(Date.parse(j.queuedAt) + NUDGE_MS).toISOString();
  return h("span", hint("It has waited 20+ minutes in the queue. The agent may have been told it still waits, and box_cancel takes it off.", { class: "badge badge-warn", "data-show-after": at, hidden: clock.now() < Date.parse(at) }, "q-" + j.key + "-nudged"), "nudged");
}

function whyText(j) {
  return (WHY[j.why] || (() => j.why || "waiting"))(j);
}

function queued() {
  return [...S.jobs.values()].filter((j) => j.state === "queued").sort((a, b) => a.position - b.position);
}

function stillQueued(j) {
  return S.jobs.get(j.key)?.state === "queued";
}

function dequeueJob(j) {
  confirmDialog({
    valid: () => stillQueued(j),
    goneText: "The job left the queue.",
    title: "Take this job out of the queue?",
    body: "It won't run, and its snapshot of the worktree is deleted. The agent that queued it is told it was cancelled.",
    action: "Cancel job",
    run: () => act("dequeue-" + j.key, () => post("/api/queue/" + encodeURIComponent(jobName(j)) + "/cancel"), () => S.jobs.delete(j.key)),
  });
}

function frontJob(j) {
  act("front-" + j.key, () => post("/api/queue/" + encodeURIComponent(jobName(j)) + "/front"), (job) => job?.key && S.jobs.set(job.key, job));
}

function queueRow(j) {
  const why = whyText(j);
  return h(
    "div",
    { class: "row row-grid" },
    h(
      "div",
      { class: "queue-item" },
      h("span", hint("Its place in the queue. Among jobs that fit, a free box goes to the session with the fewest jobs running, then to the job nearest the front.", { class: "queue-pos num" }, "q-" + j.key + "-pos"), String(j.position)),
      h(
        "div",
        null,
        h("a", { class: "row-title-link", href: jobLink(j), "data-key": "qlink-" + j.key }, reasonEl(j)),
        h("code", { class: "cmd" }, j.command),
        h(
          "div",
          { class: "meta" },
          h("span", null, icon("user"), j.agent?.session || "unknown agent", j.agent?.client ? h("span", { class: "muted" }, "· " + j.agent.client) : null),
          h("span", null, icon("branch"), shortRepo(j.repo), j.branch ? "@" + j.branch : ""),
          h("span", null, icon("box"), j.box + " · " + sizeLabel(j.size, j.gpu)),
          h("span", hint("Queued " + stamp(j.queuedAt) + ". It waits because " + why + ", and starts by itself once that clears.", {}, "q-" + j.key + "-wait"), icon("hourglass"), untitled(timeEl("elapsed", j.queuedAt, "waiting ")), h("span", { class: "muted" }, " · " + why)),
        ),
      ),
    ),
    h(
      "div",
      { class: "row-control" },
      nudgedBadge(j),
      j.position > 1 ? control("front-" + j.key, hint("Puts this job first in line. A free box goes to the session with the fewest jobs running, so a job of another session can still start before it.", { class: "btn btn-xs btn-outline", onclick: () => frontJob(j) }), icon("front"), "Move to front") : h("span", hint("First in line. When there is room it starts, unless a job of a session with fewer jobs running fits too: that one goes first.", { class: "badge" }, "q-" + j.key + "-next"), "next"),
      control("dequeue-" + j.key, hint("Takes the job out of the queue, after you confirm. It never runs, and its agent is told.", { class: "btn btn-xs btn-danger-ghost", onclick: () => dequeueJob(j) }), "Cancel"),
    ),
  );
}

function queueSection(q) {
  return section("Queue", plural(q.length, "job") + " waiting", h("div", { class: "group" }, q.map(queueRow)));
}

// Overview: what is happening on the PC right now.

// Who applies the mode (dash.Machine.enforcer): the tray while someone is logged on to the PC, or
// the headless enforcer while nobody is. "" is a boxd from before the key, when the tray was the
// only one.
function enforcerName(m) {
  return m.enforcer === "headless" ? "headless enforcer" : "tray";
}

function machineCard(m) {
  const now = clock.now();
  const alive = trayAlive(m);
  const headless = alive && m.enforcer === "headless";
  const req = m.modeRequested;
  const switcher = h(
    "div",
    { class: "segmented segmented-lg", role: "group", "aria-label": "Mode" },
    MODES.map(([id, label]) => {
      const pending = req?.mode === id;
      const tip = MODE_HINTS[id] + (pending ? " Asked from " + req.by + "; it applies once the " + enforcerName(m) + " picks it up." : "");
      return control("mode-" + m.id + "-" + id, hint(tip, { "aria-pressed": !req && m.mode === id ? "true" : "false", class: pending ? "pending" : null, disabled: busy.has("mode-" + m.id), onclick: () => setMode(m, id) }), label, pending ? h("span", { class: "muted" }, "· pending") : null);
    }),
  );
  const state = h("span", { class: "badge " + (m.mode === "mypc" ? "" : "badge-on") }, modeLabel(m.mode));
  const enforcerBadge = headless ? h("span", hint("Nobody is logged on to the PC, so the headless enforcer, a background task, applies the mode. The tray takes over at logon.", { class: "badge badge-outline" }, true), "headless") : null;
  let note = null;
  if (req) {
    const what = modeLabel(req.mode);
    if (!alive) note = h("p", { class: "mode-note pending" }, what + " is waiting: nothing is applying requests. The " + enforcerName(m) + " was last seen ", m.trayLastSeen ? timeEl("ago", m.trayLastSeen) : "never", ".");
    else note = h("p", { class: "mode-note pending" }, "Switching to " + what + ": the " + enforcerName(m) + " applies it within a few seconds.");
  } else if (headless) note = h("p", { class: "mode-note" }, "Nobody is logged on to the PC, so the headless enforcer applies the mode. The tray takes over at logon.");
  else if (m.mode === "mypc") note = h("p", { class: "mode-note" }, "Boxes are frozen. Pick Shared or Full to let agents run.");
  else note = h("p", { class: "mode-note" }, m.mode === "full" ? "Boxes get their size's Full CPUs." : "Boxes get their size's Shared CPUs, so the PC stays smooth.");

  const meter = (label, used, cap, fmt, tip) => {
    const p = pct(used, cap);
    return h("div", null, h("div", { class: "meter-label" }, h("span", hint(tip, {}, true), label), h("strong", { class: "num" }, fmt(used) + " / " + fmt(cap))), h("div", { class: "bar" + (p >= 100 ? " full" : p >= 80 ? " hot" : ""), role: "meter", "aria-label": label, "aria-valuenow": Math.round(p), "aria-valuemin": 0, "aria-valuemax": 100 }, h("i", { "data-w": p })));
  };
  const n = (x) => String(x);
  const r = m.room || {};
  const fits = [
    ["small", r.small],
    ["medium", r.medium],
    ["large", r.large],
  ];
  const gpuBox = [...S.boxes.values()].find((b) => b.machine === m.id && b.gpu);
  return h(
    "div",
    { class: "group" },
    h(
      "div",
      { class: "row" },
      h("div", { class: "machine-head" }, h("span", { class: "machine-name" }, h("span", { class: "dot " + (m.reachable ? "dot-on" : "dot-off") }), m.name), state, enforcerBadge, switcher),
      note,
    ),
    h("div", { class: "row" }, h("div", { class: "meters" }, meter("CPUs", m.allocated.cpu, m.caps.cpu, n, "CPUs the boxes hold, each counted at its size's Full share, of the most all boxes together may have."), meter("Memory", m.allocated.memory, m.caps.memory, bytes, "Memory the boxes may use, by their sizes, of the most all boxes together may have. A new box starts only if its size fits."), meter("Boxes", m.allocated.instances, m.caps.instances, n, "Boxes that exist, of the most allowed at once."), meter("Disk", m.allocated.disk, m.caps.disk, bytes, "Disk the boxes and the image they start from use, of the most allowed for all of them."))),
    h(
      "div",
      { class: "row room" },
      h("span", hint("How many more boxes of each size could start now, with what is left under the limits above. A run that doesn't fit waits in the queue.", {}, true), "Room now:"),
      fits.map(([size, k]) => h("span", { class: "badge " + (k > 0 ? "badge-on" : "") }, k + " " + size)),
      h("span", { class: "badge " + (r.gpu > 0 ? "badge-on" : "") }, r.gpu > 0 ? "GPU free" : gpuBox ? "GPU in use" : "no room for GPU"),
      h(
        "span",
        { class: "section-aside room-aside" },
        h("span", hint(alive ? "The " + enforcerName(m) + " on the PC applies the mode and checks in every 30 seconds." : "Nothing has checked in for 90 seconds, so the mode isn't being applied and mode requests wait.", { class: "conn" }, "enforcer-" + m.name), h("span", { class: "dot " + (alive ? "dot-on" : "dot-bad") }), alive ? untitled(timeEl("ago", m.trayLastSeen, enforcerName(m) + " seen ")) : m.trayLastSeen ? untitled(timeEl("ago", m.trayLastSeen, "nothing enforcing, " + enforcerName(m) + " last seen ")) : "nothing enforcing"),
        h("span", hint("Memory the Linux that runs the boxes (WSL) holds on the PC now, boxes included.", {}, true), "WSL holds " + bytes(m.wslMemory)),
      ),
    ),
  );
}

function banners(m) {
  const out = [];
  if (!trayAlive(m))
    out.push(
      h(
        "div",
        { class: "banner banner-bad", role: "status" },
        icon("alert"),
        h("div", null, h("strong", null, "Nothing is enforcing modes on " + m.name + ". "), "Neither the tray (while someone is logged on) nor the headless enforcer (while nobody is) has checked in for 90s, so modes aren't applied, and mode requests wait. Start agent-box's tray on the PC."),
      ),
    );
  return out;
}

function runningRow(j) {
  return h(
    "div",
    null,
    h("a", { class: "row row-link", href: jobLink(j), "data-key": "run-" + j.key }, h("div", { class: "row-grid" }, h("div", null, reasonEl(j), h("code", { class: "cmd" }, j.command), jobMeta(j)), h("div", { class: "row-control" }, jobBadge(j)))),
    j.tail ? h("pre", { class: "log log-short", "data-stick": "1", "data-key": "tail-" + j.key }, lastLines(j.tail, 12)) : null,
  );
}

// A box's size sets its memory, and its CPUs in each mode (internal/mode/size.go). The figures are
// the box's own, so the hint holds for any host's sizes.
function sizeHint(b) {
  return "Its size: up to " + bytes(b.memLimit) + " of memory and " + plural(b.cpus, "CPU") + "; the mode sets the CPUs." + (b.gpu ? " It has the PC's GPU." : "");
}

// A time whose hint gives the time: without its title, which would show a second tooltip.
function untitled(el) {
  el.removeAttribute("title");
  return el;
}

// When the reaper deletes the box (user.expires). A job's start sets it 24h ahead, and once the
// job ends boxd brings it down to 2h, or 30 minutes for a throwaway, unless it is later than the
// job's own 24h, as an extend makes it (sched.go, endedExpiry).
function expiry(b) {
  if (!b.expiresAt) return h("span", hint("Nothing deletes this box on a timer; delete it when it's no longer needed.", {}, "box-" + b.name + "-expiry"), "no expiry");
  const tip = "Deleted " + stamp(b.expiresAt) + ", with anything on it. A job starting sets this a day ahead; when a job ends it is usually reset to 2 hours from then (30 minutes for a throwaway), and that can undo an extend made while the job ran.";
  return h("span", hint(tip, {}, "box-" + b.name + "-expiry"), untitled(timeEl("until", b.expiresAt, "expires ")));
}

function boxRow(b, full) {
  const job = jobsOf((j) => j.box === b.name && j.machine === b.machine && (j.state === "running" || j.state === "paused"))[0];
  const open = full && opened.has(boxKey(b));
  const controls = full
    ? h(
        "div",
        { class: "row-control" },
        h(
          "div",
          { class: "menu", role: "group", "aria-label": "Extend " + b.name },
          [1, 4, 24].map((hours) => {
            const tip = b.expiresAt
              ? "Pushes this box's expiry back " + plural(hours, "hour") + ". A job that ends afterwards can reset it (to 2 hours, 30 minutes for a throwaway), so to be sure, extend an idle box, after its job, not during one."
              : "This box has no expiry: this gives it one, " + plural(hours, "hour") + " from now, when it is deleted.";
            return control("ext-" + b.name + "-" + hours, hint(tip, { class: "btn btn-xs btn-outline", disabled: busy.has("ext-" + b.name), onclick: () => extendBox(b, hours) }), "+" + hours + "h");
          }),
        ),
        control("del-" + b.name, hint("Deletes the box now, after you confirm. A job running on it is lost (it stays listed in Jobs), and anything on the box that wasn't pushed is gone.", { class: "btn btn-xs btn-danger-ghost", onclick: () => deleteBox(b) }), "Delete"),
      )
    : h("div", { class: "row-control" }, boxBadge(b));
  const row = h(
    "div",
    { class: "row row-grid" + (full ? " box-row" : ""), onclick: full ? (e) => rowClick(e, b) : null },
    h(
      "div",
      null,
      h("p", { class: "row-title" }, full ? boxToggle(b, open) : b.name, full ? boxBadge(b) : null, h("span", hint(sizeHint(b), { class: "badge badge-outline" }, "box-" + b.name + "-size"), sizeLabel(b.size, b.gpu, " · ")), b.throwaway ? h("span", hint("Made for one run. It is usually deleted 30 minutes after that run ends, or sooner if another run needs the room. An extend after the run keeps it longer.", { class: "badge" }, "box-" + b.name + "-throwaway"), "throwaway") : null),
      h(
        "div",
        { class: "meta" },
        h("span", null, icon("branch"), shortRepo(b.repo)),
        h("span", hint("Memory the box uses now, of the most its size allows.", { class: "num" }, "box-" + b.name + "-mem"), "mem " + bytes(b.memUsed) + " / " + bytes(b.memLimit)),
        h("span", hint(b.state === "running" ? "How busy the box's CPUs are: 100% is all " + plural(b.cpus, "CPU") + " it may use now. The mode sets how many that is." : "The CPUs the box may use; the mode sets how many.", { class: "num" }, "box-" + b.name + "-cpu"), b.state === "running" ? "CPU " + Math.round(b.cpuPct) + "% of " + plural(b.cpus, "CPU") : plural(b.cpus, "CPU")),
        expiry(b),
        full ? timeEl("ago", b.createdAt, "made ") : null,
      ),
      job && !open ? h("a", { class: "meta", href: jobLink(job) }, h("span", null, "running: ", job.reason || job.command)) : null,
      full ? h("div", hint("Memory in use, as a share of what the box's size allows.", { class: "bar" }, "box-" + b.name + "-membar"), h("i", { "data-w": pct(b.memUsed, b.memLimit) })) : null,
    ),
    controls,
  );
  return open ? [row, boxDetail(b, job)] : row;
}

// Boxes: a row opens in place (#184) to show what its box is doing, from the jobs the page already
// has and the box's finished jobs from history (/api/jobs?box=&since=). A box is one generation of
// its name, so what is open is keyed by its createdAt too.
const opened = new Set();
const boxJobs = new Map(); // by boxKey: { state: loading|ok|error, jobs, more, error }
const BOX_JOBS = 20;

function boxKey(b) {
  return b.machine + "/" + b.name + "/" + b.createdAt;
}

function toggleBox(b) {
  const k = boxKey(b);
  if (opened.delete(k)) return changed();
  opened.add(k);
  if (boxJobs.get(k)?.state !== "ok") loadBoxJobs(b);
  changed();
}

// A click anywhere on the row opens it, except on its own controls, links and hints, and except
// the end of a drag that selected text.
function rowClick(e, b) {
  const own = e.target.closest("a, button, [tabindex]");
  if ((own && e.currentTarget.contains(own)) || String(window.getSelection?.() || "")) return;
  toggleBox(b);
}

function loadBoxJobs(b) {
  const k = boxKey(b);
  const mine = { state: "loading", jobs: [], more: false };
  boxJobs.set(k, mine);
  const since = new Date(Date.parse(b.createdAt) - JOB_BOX_GRACE_MS).toISOString();
  const p = new URLSearchParams({ box: b.name, machine: b.machine, since, limit: String(BOX_JOBS) });
  api("/api/jobs?" + p)
    .then((page) => {
      if (boxJobs.get(k) !== mine) return;
      Object.assign(mine, { state: "ok", jobs: page.jobs || [], more: !!page.more });
    })
    .catch((e) => {
      if (boxJobs.get(k) !== mine) return;
      Object.assign(mine, { state: "error", error: e.message });
    })
    .finally(changed);
}

function boxToggle(b, open) {
  return h(
    "button",
    hint(open ? "Hides this box's jobs." : "Shows what this box is running now, or since when it has been idle, and the jobs it ran before.", {
      class: "box-toggle",
      "aria-expanded": open ? "true" : "false",
      "aria-controls": open ? "box-detail-" + b.name : null,
      "data-key": "open-" + b.name,
      onclick: () => toggleBox(b),
    }),
    icon("chevron"),
    b.name,
  );
}

function exitText(j) {
  if (j.state === "lost") return "was lost";
  return j.exit == null ? "ended" : "exited " + j.exit;
}

// The box's finished jobs: history's, with the stream's newer copies and the ones that ended since.
function boxDone(b) {
  const by = new Map();
  for (const j of boxJobs.get(boxKey(b))?.jobs || []) by.set(j.key, j);
  for (const j of S.jobs.values()) if ((j.state === "done" || j.state === "lost") && ranOn(j, b)) by.set(j.key, j);
  return [...by.values()].sort((a, c) => Date.parse(c.startedAt) - Date.parse(a.startedAt)).slice(0, BOX_JOBS);
}

function boxJobRow(j) {
  return h(
    "a",
    { class: "box-job", href: jobLink(j), "data-key": "bjob-" + j.key },
    jobBadge(j),
    h("span", { class: "box-job-title" }, j.reason || j.command),
    h("span", { class: "box-job-when" }, untitled(timeEl("ago", j.endedAt || j.startedAt)), " · " + elapsed(j)),
  );
}

function boxDetail(b, job) {
  const load = boxJobs.get(boxKey(b));
  const done = boxDone(b);
  const last = done[0];
  let now;
  if (job) now = runningRow(job);
  else {
    const since = last ? last.endedAt || last.startedAt : b.createdAt;
    const tail = last
      ? [", last job ", h("a", { href: jobLink(last), "data-key": "blast-" + last.key }, last.id), " " + exitText(last) + "."]
      : load?.state === "loading"
        ? "."
        : ", when it was made. It hasn't run a job yet.";
    now = h("p", { class: "box-idle" }, h("span", { class: "dot dot-off" }), h("span", null, "Idle since ", h("span", { title: stamp(since) }, hhmm(since)), tail));
  }
  let before = null;
  if (done.length) before = [done.map(boxJobRow), load?.more ? h("p", { class: "row-empty" }, "Its last " + BOX_JOBS + " jobs; Jobs has the rest.") : null];
  else if (load?.state === "loading") before = h("p", { class: "row-empty" }, "Loading its jobs…");
  else if (load?.state !== "error") before = h("p", { class: "row-empty" }, "No finished jobs on this box yet.");
  const err =
    load?.state === "error"
      ? h("p", { class: "row-empty row-error" }, "Couldn't load its earlier jobs: " + load.error + " ", control("bjobs-retry-" + b.name, { class: "btn btn-xs btn-outline", onclick: () => (loadBoxJobs(b), changed()) }, "Try again"))
      : null;
  return h(
    "div",
    { class: "box-detail", id: "box-detail-" + b.name, role: "region", "aria-label": b.name + ": its jobs" },
    h("p", { class: "box-detail-label" }, job ? "Running now" : "Now"),
    now,
    h("p", { class: "box-detail-label" }, "Ran before"),
    before,
    err,
  );
}

const overview = {
  mount(el) {
    this.update(el);
  },
  update(el) {
    const ms = [...S.machines.values()];
    const running = jobsOf((j) => j.state === "running" || j.state === "paused");
    const q = queued();
    const done = jobsOf((j) => j.state === "done" || j.state === "lost").slice(0, 5);
    const bx = [...S.boxes.values()].sort((a, b) => a.name.localeCompare(b.name));
    el.replaceChildren(
      h(
        "div",
        { class: "page" },
        ms.flatMap(banners),
        ms.length ? section(ms.length > 1 ? "Machines" : "Machine", null, ms.map(machineCard)) : null,
        section("Running jobs", running.length ? String(running.length) : null, h("div", { class: "group" }, running.length ? running.map(runningRow) : h("p", { class: "row-empty" }, "Nothing is running. Agents start jobs with box run; they show here as they start."))),
        q.length ? queueSection(q) : null,
        section("Boxes", bx.length ? plural(bx.length, "box", "boxes") : null, h("div", { class: "group" }, bx.length ? bx.map((b) => boxRow(b, false)) : h("p", { class: "row-empty" }, "No boxes. One is made when an agent first runs something here."))),
        done.length ? section("Recently finished", h("a", { href: "#jobs" }, "All jobs →"), h("div", { class: "group" }, done.map(finishedRow))) : null,
      ),
    );
  },
};

function finishedRow(j) {
  return h("a", { class: "row row-link", href: jobLink(j), "data-key": "job-" + j.key }, h("div", { class: "row-grid" }, h("div", null, reasonEl(j), h("code", { class: "cmd" }, j.command), jobMeta(j)), h("div", { class: "row-control" }, jobBadge(j))));
}

// Boxes: every box with its full state, and the controls on it.

const boxes = {
  mount(el) {
    this.update(el);
  },
  update(el) {
    const bx = [...S.boxes.values()].sort((a, b) => a.name.localeCompare(b.name));
    // A box that went closes, and its jobs are let go.
    const live = new Set(bx.map(boxKey));
    for (const k of opened) if (!live.has(k)) opened.delete(k);
    for (const k of boxJobs.keys()) if (!live.has(k)) boxJobs.delete(k);
    el.replaceChildren(
      h(
        "div",
        { class: "page" },
        h("div", { class: "page-head" }, h("h2", null, "Boxes"), h("p", null, "Linux boxes on the PC that agents run their tests and builds in. A box is deleted when it expires; extend one an agent still needs. Open one to see its jobs.")),
        bx.length ? h("div", { class: "group" }, bx.map((b) => boxRow(b, true))) : empty("box", "No boxes", "Agents make them with box run or box new. Nothing is using the PC now."),
      ),
    );
  },
};

// Jobs: running and finished, with filters. Finished jobs come from /api/jobs with the filters;
// the state's own jobs (running and the last 50) are merged in, so a job that just ended shows
// before the next fetch.

const F = { q: "", repo: "", agent: "", kind: "", machine: "", result: "" };
const list = { jobs: [], more: false, loading: false, error: "", seq: 0 };

// Search folds case as the server does (Go's strings.ToLower): each code point by Unicode's
// simple lower-case mapping. Lower-casing one code point at a time gives that, except for
// U+0130 (İ), whose full mapping is two code points; its simple mapping is i.
function fold(s) {
  let out = "";
  for (const ch of s || "") out += ch === "\u0130" ? "i" : ch.toLowerCase();
  return out;
}

function matches(j) {
  const q = fold(F.q.trim());
  if (q && ![j.reason, j.command, j.branch].some((v) => fold(v).includes(q))) return false;
  if (F.repo && shortRepo(j.repo) !== F.repo) return false;
  if (F.agent && j.agent?.session !== F.agent) return false;
  if (F.kind && j.kind !== F.kind) return false;
  if (F.machine && j.machine !== F.machine) return false;
  if (F.result && result(j) !== F.result) return false;
  return true;
}

async function fetchJobs(more = false) {
  const seq = ++list.seq;
  list.loading = true;
  list.error = "";
  changed();
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(F)) if (v) p.set(k, v.trim());
  if (more && list.jobs.length) p.set("before", list.jobs[list.jobs.length - 1].key);
  p.set("limit", "50");
  try {
    const page = await api("/api/jobs?" + p);
    if (seq !== list.seq) return;
    list.jobs = more ? list.jobs.concat(page.jobs || []) : page.jobs || [];
    list.more = !!page.more;
  } catch (e) {
    if (seq !== list.seq) return;
    list.error = e.message;
  }
  list.loading = false;
  changed();
}

function allJobs() {
  const by = new Map();
  for (const j of list.jobs) by.set(j.key, j);
  for (const j of S.jobs.values()) by.set(j.key, j); // the stream's copy is the newer one
  return [...by.values()];
}

// A filter's select is built once and only its options change, so one held open survives the
// stream's events.
const selects = {};
function options(label, key, values) {
  let f = selects[key];
  if (!f) {
    const sel = h("select", { "aria-label": label });
    sel.addEventListener("change", () => {
      F[key] = sel.value;
      fetchJobs();
    });
    f = selects[key] = { el: h("label", { class: "field" }, sel), sel, seen: null };
  }
  if (F[key] && !values.includes(F[key])) values = [...values, F[key]];
  const sig = values.join("|");
  if (sig !== f.seen) {
    f.seen = sig;
    f.sel.replaceChildren(h("option", { value: "" }, label), ...values.map((v) => h("option", { value: v }, v)));
  }
  f.sel.value = F[key];
  return f.el;
}

const jobs = {
  mount(el) {
    let t = 0;
    const search = h("input", { type: "search", placeholder: "Search reasons, commands, branches", "aria-label": "Search jobs", value: F.q });
    search.addEventListener("input", () => {
      F.q = search.value;
      clearTimeout(t);
      t = setTimeout(() => fetchJobs(), 250);
      this.update(el);
    });
    this.filters = h("div", { class: "filters" });
    this.search = h("label", { class: "field search" }, icon("search"), search);
    this.listEl = h("div", { class: "section" });
    el.replaceChildren(h("div", { class: "page" }, h("div", { class: "page-head" }, h("h2", null, "Jobs"), h("p", null, "What agents ran on boxes, and why.")), h("div", { class: "section" }, this.filters, this.listEl)));
    fetchJobs();
    this.update(el);
  },
  update() {
    const known = allJobs();
    const uniq = (f) => [...new Set(known.map(f).filter(Boolean))].sort();
    const fs = [
      this.search,
      options("Repo", "repo", uniq((j) => (j.repo ? shortRepo(j.repo) : ""))),
      options("Agent", "agent", uniq((j) => j.agent?.session)),
      options("Kind", "kind", KINDS),
      S.machines.size > 1 ? options("Machine", "machine", [...S.machines.keys()]) : null,
      options("Result", "result", RESULTS),
    ].filter(Boolean);
    // Only when they change, so a held-open select or the focused search stays put.
    if (fs.some((f, i) => this.filters.children[i] !== f) || fs.length !== this.filters.children.length) this.filters.replaceChildren(...fs);
    const shown = known.filter(matches).sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt));
    const live = shown.filter((j) => j.state === "running" || j.state === "paused");
    const waiting = shown.filter((j) => j.state === "queued").sort((a, b) => a.position - b.position);
    const done = shown.filter((j) => j.state === "done" || j.state === "lost");
    const filtered = Object.values(F).some(Boolean);
    const kids = [];
    if (list.error) kids.push(h("div", { class: "banner banner-bad" }, icon("alert"), h("div", null, "Couldn't load finished jobs: " + list.error + " ", h("button", { class: "btn btn-xs btn-outline", onclick: () => fetchJobs() }, "Try again"))));
    if (live.length) kids.push(section("Running", String(live.length), h("div", { class: "group" }, live.map(finishedRow))));
    if (waiting.length) kids.push(queueSection(waiting));
    if (done.length) {
      const moreBtn = list.more ? h("button", { class: "btn btn-sm btn-outline", disabled: list.loading, onclick: () => fetchJobs(true) }, list.loading ? "Loading…" : "Show older") : null;
      kids.push(section("Finished", filtered ? plural(done.length, "match", "matches") : null, h("div", { class: "group" }, done.map(finishedRow)), moreBtn));
    }
    if (!live.length && !waiting.length && !done.length) {
      if (list.loading) kids.push(empty("retry", "Loading jobs…", null));
      else if (filtered) kids.push(empty("search", "No jobs match", "Try another search, or clear the filters.", h("button", { class: "btn btn-sm btn-outline", onclick: clearFilters }, "Clear filters")));
      else kids.push(empty("list", "No jobs yet", "When an agent runs something with box run, it shows here with its reason."));
    }
    this.listEl.replaceChildren(...kids);
  },
};

function clearFilters() {
  for (const k of Object.keys(F)) F[k] = "";
  location.hash = "#jobs";
  jobs.mount(document.querySelector(".content"));
}

// A job: its reason, what ran, who asked, and its log. The log tails with offset while the box
// exists; after, the kept tail is all there is.

const tailer = { key: "", pre: null, offset: 0, timer: 0, status: "", gone: false, seq: 0, abort: null };

function stopTail() {
  clearTimeout(tailer.timer);
  tailer.abort?.abort();
  tailer.abort = null;
  tailer.seq++;
  tailer.key = "";
}

async function pull(j, seq) {
  if (seq !== tailer.seq) return;
  const cur = S.jobs.get(j.key) || j;
  let next = 0;
  try {
    tailer.abort = new AbortController();
    const c = await api("/api/jobs/" + encodeURIComponent(j.key) + "/log?offset=" + tailer.offset, { signal: tailer.abort.signal });
    if (seq !== tailer.seq) return;
    const pre = tailer.pre;
    const stick = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 24;
    if (tailer.offset === 0) pre.textContent = "";
    if (c.data) pre.append(c.data);
    tailer.offset = c.offset;
    if (stick) pre.scrollTop = pre.scrollHeight;
    if (c.paused) {
      tailer.status = "Paused: the box is frozen, so its log can't be read until it resumes.";
      next = 5000;
    } else if (!c.eof) {
      tailer.status = "Loading…";
      next = 10;
    } else if (cur.state === "running" || cur.state === "paused") {
      tailer.status = "Following the log";
      next = 2000;
    } else tailer.status = "The whole log";
  } catch (e) {
    if (seq !== tailer.seq) return;
    if (e.status === 404) {
      tailer.gone = true;
      tailer.status = "";
      tailer.pre.textContent = cur.tail || "";
    } else {
      tailer.status = "Couldn't read the log: " + e.message + " Trying again.";
      next = 5000;
    }
  }
  changed();
  if (next) tailer.timer = setTimeout(() => pull(j, seq), next);
}

// Jobs the detail page has seen, by key, and its lookups of jobs it hadn't.
const seen = new Map();
const lookups = new Map();

// A waiting job's page: where it is in line and why, or, once it has left the line, where it went.
// The running job keeps the box and ID, so it is found by them.
function waitingPage(j) {
  const now = S.jobs.get(j.key);
  const ran = now ? null : [...S.jobs.values()].find((x) => x.box === j.box && x.id === j.id && x.state !== "queued");
  const back = h("a", { class: "btn btn-sm btn-ghost back", href: "#jobs" }, icon("back"), "Jobs");
  if (!now) {
    const body = ran
      ? empty("list", "It has started", "This job left the queue and runs as " + jobName(ran) + ".", h("a", { class: "btn btn-sm btn-outline", href: jobLink(ran), "data-key": "ran-" + ran.key }, "Open the running job"))
      : empty("list", "The job left the queue", "It was cancelled, or it started and has since left the page's list. Look for " + jobName(j) + " in Jobs.", h("a", { class: "btn btn-sm btn-outline", href: "#jobs" }, "Back to Jobs"));
    return h("div", { class: "page" }, back, body);
  }
  const kv = (k, v) => h("dl", { class: "kv" }, h("dt", null, k), h("dd", null, v || "—"));
  return h(
    "div",
    { class: "page" },
    back,
    h("div", { class: "section" }, h("div", { class: "group" }, h("div", { class: "row row-grid" }, h("div", null, reasonEl(now, "headline"), h("code", { class: "cmd" }, now.command)), h("div", { class: "row-control" }, h("span", hint("Waiting in the PC's queue: it starts by itself once a box of its size fits.", { class: "badge" }, true), "queued"), nudgedBadge(now))))),
    section(
      "In the queue",
      null,
      h(
        "div",
        { class: "group detail-grid" },
        kv("Place", now.position === 1 ? "next" : String(now.position) + " in line"),
        kv("Waiting", timeEl("elapsed", now.queuedAt)),
        kv("Why", whyText(now)),
        kv("Queued", stamp(now.queuedAt)),
        kv("Agent", (now.agent?.session || "unknown") + (now.agent?.client ? " · " + now.agent.client : "")),
        kv("Repo", now.repo),
        kv("Branch", now.branch),
        kv("Box", now.box + " · " + sizeLabel(now.size, now.gpu)),
      ),
      h("div", { class: "row-control queue-actions" }, now.position > 1 ? control("front-" + now.key, { class: "btn btn-sm btn-outline", onclick: () => frontJob(now) }, icon("front"), "Move to front") : null, control("dequeue-" + now.key, { class: "btn btn-sm btn-danger-ghost", onclick: () => dequeueJob(now) }, "Cancel")),
    ),
  );
}

const jobDetail = {
  mount(el, key) {
    stopTail();
    this.key = key;
    this.head = h("div", { class: "section" });
    this.logHead = h("div", { class: "log-head" });
    tailer.pre = h("pre", { class: "log log-tall", tabindex: "0", "aria-label": "Log" });
    this.logBox = h("div", { class: "group" }, this.logHead, tailer.pre);
    this.details = h("div", { class: "section" });
    this.body = h("div", { class: "page" }, h("a", { class: "btn btn-sm btn-ghost back", href: "#jobs" }, icon("back"), "Jobs"), this.head, this.logBox, this.details);
    el.replaceChildren(this.body);
    this.update(el);
  },
  update(el) {
    // The stream's copy is the newest; a job the stream let go (job-gone, or older than the state's
    // last 50) keeps the last copy seen, by key, and one never seen is asked for.
    const key = this.key;
    const j = S.jobs.get(key) || list.jobs.find((x) => x.key === key) || seen.get(key);
    if (j) seen.set(key, j);
    if (!j) {
      stopTail();
      const look = lookups.get(key);
      if (!look || look.state === "retry") {
        const mine = { state: "loading" };
        lookups.set(key, mine);
        api("/api/jobs/" + encodeURIComponent(key))
          .then((job) => {
            // Only this request's answer counts, and it is filed under its own key, so a late
            // answer for another job can't overwrite the one on screen.
            if (lookups.get(key) !== mine) return;
            seen.set(key, job);
            lookups.delete(key);
          })
          .catch((e) => {
            if (lookups.get(key) !== mine) return;
            mine.state = e.status === 404 ? "missing" : "error";
            mine.error = e.message;
          })
          .finally(changed);
      }
      const now = lookups.get(key);
      let body = empty("retry", "Loading the job…", null);
      if (now?.state === "missing") body = empty("search", "Job not found", "boxd has no job with this key. Its history keeps jobs for 180 days.", h("a", { class: "btn btn-sm btn-outline", href: "#jobs" }, "Back to Jobs"));
      if (now?.state === "error")
        body = empty("alert", "Couldn't load the job", now.error, h("button", { class: "btn btn-sm btn-outline", onclick: () => ((now.state = "retry"), changed()) }, "Try again"));
      el.replaceChildren(h("div", { class: "page" }, body));
      return;
    }
    if (j.state === "queued") {
      stopTail();
      el.replaceChildren(waitingPage(j));
      return;
    }
    if (!this.body.isConnected) el.replaceChildren(this.body);
    if (tailer.key !== j.key) {
      stopTail();
      tailer.key = j.key;
      tailer.offset = 0;
      tailer.gone = !jobBox(j);
      tailer.status = "";
      tailer.pre.textContent = tailer.gone ? j.tail || "" : "";
      if (!tailer.gone) pull(j, tailer.seq);
    }
    // The box can go, or its name be taken by a new box, while the page is open: from then on the
    // kept tail is all there is.
    if (!tailer.gone && !jobBox(j)) {
      stopTail();
      tailer.key = j.key;
      tailer.gone = true;
      tailer.status = "";
    }
    if (tailer.gone && tailer.pre.textContent !== (j.tail || "")) tailer.pre.textContent = j.tail || "";
    const live = j.state === "running" || j.state === "paused";
    // A paused job's box is frozen, so boxd refuses the kill (409): the button shows only while
    // it runs, and the paused badge says why it's gone.
    const cancel = j.state === "running" ? control("cancel-" + j.key, hint("Kills the command, after you confirm. The job ends with exit 130, and its agent hears it was cancelled.", { class: "btn btn-sm btn-danger-ghost", onclick: () => cancelJob(j) }), "Cancel job") : null;
    const kv = (k, v, tip) => h("dl", { class: "kv" }, h("dt", tip ? hint(tip, {}, "kv-" + k) : null, k), h("dd", null, v || "—"));
    this.head.replaceChildren(h("div", { class: "group" }, h("div", { class: "row row-grid" }, h("div", null, reasonEl(j, "headline"), h("code", { class: "cmd" }, j.command), jobMeta(j)), h("div", { class: "row-control" }, jobBadge(j, "job-state"), cancel))));
    this.details.replaceChildren(
      section(
        "Details",
        null,
        h(
          "div",
          { class: "group detail-grid" },
          kv("Agent", (j.agent?.session || "unknown") + (j.agent?.client ? " · " + j.agent.client : "")),
          kv("Repo", j.repo),
          kv("Branch", j.branch),
          kv("Commit", (j.commit || "").slice(0, 10)),
          kv("Kind", j.kind, "What sort of run it is, read from the command: go-test, js-test, build and so on, or other."),
          kv("Box", j.box + " · " + sizeLabel(j.size, j.gpu)),
          kv("Started", stamp(j.startedAt)),
          kv(live ? "Running for" : "Took", elapsed(j)),
          kv("Exit", j.exit == null ? (j.state === "lost" ? "never read" : "—") : String(j.exit), j.exit == null ? (j.state === "lost" ? RESULT_HINTS.lost : null) : exitHint(j.exit)),
          kv("Host", j.agent?.host),
          kv("Worktree", j.agent?.worktree),
        ),
      ),
    );
    const note = tailer.gone ? "The box is gone, so this is the kept tail: the log's last 2 KiB." : tailer.status;
    this.logHead.replaceChildren(h("span", null, live && !tailer.gone ? h("span", { class: "dot dot-on dot-live" }) : null), h("span", null, note || "Log"));
    if (j.state === "lost" && !j.tail) tailer.pre.textContent = "No log was kept: the box went before boxd could read it.";
  },
  leave() {
    stopTail();
  },
};
