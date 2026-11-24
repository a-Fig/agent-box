// Overview, Boxes and Jobs, and the controls on them. Each view has mount, which builds it when it
// opens, and update, which redraws it when something changes; a view with an input or a log
// redraws only around them, so typing and scrolling survive the stream's events.

import { h, icon, clock, ago, until, dur, bytes, pct, shortRepo, plural, stamp } from "./dom.js";
import { S, api, post, changed, toast, jobsOf, boxOf, trayAlive } from "./store.js";

export { overview, boxes, jobs, jobDetail, confirmDialog, empty, section, modeLabel };

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

function jobBadge(j) {
  switch (j.state) {
    case "running":
      return h("span", { class: "badge badge-info" }, h("span", { class: "dot dot-on dot-live" }), "running");
    case "paused":
      return h("span", { class: "badge badge-warn" }, "paused");
    case "lost":
      return h("span", { class: "badge badge-warn" }, "lost");
  }
  const r = result(j);
  const cls = { pass: "badge-on", fail: "badge-bad", cancelled: "" }[r] ?? "";
  return h("span", { class: "badge " + cls }, r === "fail" ? "exit " + j.exit : r);
}

function boxBadge(b) {
  const cls = { running: "badge-on", frozen: "badge-warn", stopped: "" }[b.state] ?? "";
  return h("span", { class: "badge " + cls }, b.state);
}

// The reason is the job's headline; a job from before box run asked has none.
function reasonEl(j, cls = "row-title") {
  return j.reason ? h("p", { class: cls }, j.reason) : h("p", { class: cls + " no-reason" }, "no reason given");
}

function elapsed(j) {
  const end = j.endedAt ? Date.parse(j.endedAt) : clock.now();
  return dur((end - Date.parse(j.startedAt)) / 1000);
}

function jobMeta(j) {
  return h(
    "div",
    { class: "meta" },
    h("span", null, icon("user"), j.agent?.session || "unknown agent", j.agent?.client ? h("span", { class: "muted" }, "· " + j.agent.client) : null),
    h("span", null, icon("branch"), shortRepo(j.repo), j.branch ? "@" + j.branch : ""),
    h("span", null, icon("box"), j.box, " · ", j.size + (j.gpu ? " GPU" : "")),
    h("span", { title: stamp(j.startedAt) }, icon("clock"), j.state === "running" || j.state === "paused" ? elapsed(j) : ago(j.endedAt || j.startedAt) + " · took " + elapsed(j)),
  );
}

function lastLines(text, n) {
  const lines = (text || "").replace(/\n$/, "").split("\n");
  return lines.slice(-n).join("\n");
}

function jobLink(j) {
  return "#jobs/" + encodeURIComponent(j.key);
}

// Controls. Each goes through act, which keeps the button busy and says what failed.

async function act(btn, fn, done) {
  if (btn) btn.disabled = true;
  try {
    const out = await fn();
    done?.(out);
  } catch (e) {
    toast(e.message || "That didn't work.", true);
  } finally {
    if (btn) btn.disabled = false;
    changed();
  }
}

function confirmDialog({ title, body, action, danger = true, run }) {
  const prev = document.activeElement;
  const close = () => {
    scrim.remove();
    document.removeEventListener("keydown", onKey);
    prev?.focus?.();
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
  document.addEventListener("keydown", onKey);
  document.body.append(scrim);
  go.focus();
}

function setMode(m, mode, btn) {
  act(
    btn,
    () => post("/api/mode", { mode }),
    (machine) => {
      if (machine?.id) S.machines.set(machine.id, machine);
    },
  );
}

function cancelJob(j, btn) {
  confirmDialog({
    title: "Cancel this job?",
    body: "Its command on " + j.box + " is killed and the job ends with exit 130. The agent that started it sees it cancelled.",
    action: "Cancel job",
    run: () => act(btn, () => post("/api/jobs/" + encodeURIComponent(j.key) + "/cancel"), (job) => job?.key && S.jobs.set(job.key, job)),
  });
}

function deleteBox(b, btn) {
  const running = jobsOf((j) => j.box === b.name && j.machine === b.machine && (j.state === "running" || j.state === "paused"));
  confirmDialog({
    title: "Delete " + b.name + "?",
    body: (running.length ? plural(running.length, "running job") + " on it will be lost. " : "") + "Boxes are compute, not storage: anything on it that wasn't pushed is gone.",
    action: "Delete box",
    run: () => act(btn, () => post("/api/boxes/" + encodeURIComponent(b.name) + "/delete"), () => S.boxes.delete(b.machine + "/" + b.name)),
  });
}

function extendBox(b, hours, btn) {
  act(btn, () => post("/api/boxes/" + encodeURIComponent(b.name) + "/extend", { hours }), (box) => box?.name && S.boxes.set(box.machine + "/" + box.name, box));
}

// Overview: what is happening on the PC right now.

function machineCard(m) {
  const now = clock.now();
  const alive = trayAlive(m);
  const req = m.modeRequested;
  const switcher = h(
    "div",
    { class: "segmented segmented-lg", role: "group", "aria-label": "Mode" },
    MODES.map(([id, label]) => {
      const pending = req?.mode === id;
      const btn = h("button", { "aria-pressed": !m.yielding && !req && m.mode === id ? "true" : "false", class: pending ? "pending" : null, title: pending ? "Asked " + ago(req.at) + " from " + req.by : null }, label, pending ? h("span", { class: "muted" }, "· pending") : null);
      btn.addEventListener("click", () => setMode(m, id, btn));
      return btn;
    }),
  );
  let state;
  if (m.yielding) state = h("span", { class: "badge badge-warn" }, icon("game"), "Yielding: " + (m.modeReason || "gaming").replace(/^gaming:\s*/, ""));
  else state = h("span", { class: "badge " + (m.mode === "mypc" ? "" : "badge-on") }, modeLabel(m.mode));
  let note = null;
  if (req) {
    const what = modeLabel(req.mode);
    if (m.yielding) note = h("p", { class: "mode-note pending" }, what + " when the game ends. Asked " + ago(req.at) + " from " + req.by + ".");
    else if (!alive) note = h("p", { class: "mode-note pending" }, what + " is waiting for the tray, which hasn't been seen " + (m.trayLastSeen ? "since " + ago(m.trayLastSeen) : "yet") + ".");
    else note = h("p", { class: "mode-note pending" }, "Switching to " + what + ": the tray applies it within a few seconds.");
  } else if (m.yielding) note = h("p", { class: "mode-note" }, "A game is running, so every box is frozen until it ends. Games always win.");
  else if (m.mode === "mypc") note = h("p", { class: "mode-note" }, "Boxes are frozen. Pick Shared or Full to let agents run.");
  else note = h("p", { class: "mode-note" }, m.mode === "full" ? "Boxes get their size's Full CPUs." : "Boxes get their size's Shared CPUs, so the PC stays smooth.");

  const meter = (label, used, cap, fmt) => {
    const p = pct(used, cap);
    return h("div", null, h("div", { class: "meter-label" }, h("span", null, label), h("strong", { class: "num" }, fmt(used) + " / " + fmt(cap))), h("div", { class: "bar" + (p >= 100 ? " full" : p >= 80 ? " hot" : ""), role: "meter", "aria-label": label, "aria-valuenow": Math.round(p), "aria-valuemin": 0, "aria-valuemax": 100 }, h("i", { "data-w": p })));
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
      h("div", { class: "machine-head" }, h("span", { class: "machine-name" }, h("span", { class: "dot " + (m.reachable ? "dot-on" : "dot-off") }), m.name), state, switcher),
      note,
    ),
    h("div", { class: "row" }, h("div", { class: "meters" }, meter("CPUs", m.allocated.cpu, m.caps.cpu, n), meter("Memory", m.allocated.memory, m.caps.memory, bytes), meter("Boxes", m.allocated.instances, m.caps.instances, n), meter("Disk", m.allocated.disk, m.caps.disk, bytes))),
    h(
      "div",
      { class: "row room" },
      h("span", null, "Room now:"),
      fits.map(([size, k]) => h("span", { class: "badge " + (k > 0 ? "badge-on" : "") }, k + " " + size)),
      h("span", { class: "badge " + (r.gpu > 0 ? "badge-on" : "") }, r.gpu > 0 ? "GPU free" : gpuBox ? "GPU in use" : "no room for GPU"),
      h("span", { class: "section-aside room-aside" }, h("span", { class: "conn" }, h("span", { class: "dot " + (alive ? "dot-on" : "dot-bad") }), alive ? "tray seen " + ago(m.trayLastSeen) : "tray down" + (m.trayLastSeen ? ", last seen " + ago(m.trayLastSeen) : "")), h("span", null, "WSL holds " + bytes(m.wslMemory))),
    ),
  );
}

function banners(m) {
  const out = [];
  if (!trayAlive(m))
    out.push(h("div", { class: "banner banner-bad", role: "status" }, icon("alert"), h("div", null, h("strong", null, "The tray isn't running on " + m.name + ". "), "Modes and game yields aren't being enforced, and mode requests wait until it is back. Start agent-box's tray on the PC.")));
  return out;
}

function runningRow(j) {
  return h(
    "div",
    null,
    h("a", { class: "row row-link", href: jobLink(j) }, h("div", { class: "row-grid" }, h("div", null, reasonEl(j), h("code", { class: "cmd" }, j.command), jobMeta(j)), h("div", { class: "row-control" }, jobBadge(j)))),
    j.tail ? h("pre", { class: "log log-short", "data-stick": "1" }, lastLines(j.tail, 12)) : null,
  );
}

function boxRow(b, full) {
  const job = jobsOf((j) => j.box === b.name && j.machine === b.machine && (j.state === "running" || j.state === "paused"))[0];
  const controls = full
    ? h(
        "div",
        { class: "row-control" },
        h(
          "div",
          { class: "menu", role: "group", "aria-label": "Extend " + b.name },
          [1, 4, 24].map((hours) => {
            const btn = h("button", { class: "btn btn-xs btn-outline", title: "Add " + hours + "h to its expiry" }, "+" + hours + "h");
            btn.addEventListener("click", () => extendBox(b, hours, btn));
            return btn;
          }),
        ),
        (() => {
          const btn = h("button", { class: "btn btn-xs btn-danger-ghost" }, "Delete");
          btn.addEventListener("click", () => deleteBox(b, btn));
          return btn;
        })(),
      )
    : h("div", { class: "row-control" }, boxBadge(b));
  return h(
    "div",
    { class: "row row-grid" },
    h(
      "div",
      null,
      h("p", { class: "row-title" }, b.name, full ? boxBadge(b) : null, h("span", { class: "badge badge-outline" }, b.size + (b.gpu ? " · GPU" : "")), b.throwaway ? h("span", { class: "badge" }, "throwaway") : null),
      h(
        "div",
        { class: "meta" },
        h("span", null, icon("branch"), shortRepo(b.repo)),
        h("span", { class: "num" }, "mem " + bytes(b.memUsed) + " / " + bytes(b.memLimit)),
        h("span", { class: "num" }, b.state === "running" ? "CPU " + Math.round(b.cpuPct) + "% of " + plural(b.cpus, "CPU") : plural(b.cpus, "CPU")),
        h("span", { title: b.expiresAt ? stamp(b.expiresAt) : null }, b.expiresAt ? "expires " + until(b.expiresAt) : "no expiry"),
        full ? h("span", { title: stamp(b.createdAt) }, "made " + ago(b.createdAt)) : null,
      ),
      job ? h("a", { class: "meta", href: jobLink(job) }, h("span", null, "running: ", job.reason || job.command)) : null,
      full ? h("div", { class: "bar" }, h("i", { "data-w": pct(b.memUsed, b.memLimit) })) : null,
    ),
    controls,
  );
}

const overview = {
  mount(el) {
    this.update(el);
  },
  update(el) {
    const ms = [...S.machines.values()];
    const running = jobsOf((j) => j.state === "running" || j.state === "paused");
    const done = jobsOf((j) => j.state === "done" || j.state === "lost").slice(0, 5);
    const bx = [...S.boxes.values()].sort((a, b) => a.name.localeCompare(b.name));
    el.replaceChildren(
      h(
        "div",
        { class: "page" },
        ms.flatMap(banners),
        ms.length ? section(ms.length > 1 ? "Machines" : "Machine", null, ms.map(machineCard)) : null,
        section("Running jobs", running.length ? String(running.length) : null, h("div", { class: "group" }, running.length ? running.map(runningRow) : h("p", { class: "row-empty" }, "Nothing is running. Agents start jobs with box run; they show here as they start."))),
        section("Boxes", bx.length ? plural(bx.length, "box", "boxes") : null, h("div", { class: "group" }, bx.length ? bx.map((b) => boxRow(b, false)) : h("p", { class: "row-empty" }, "No boxes. One is made when an agent first runs something here."))),
        done.length ? section("Recently finished", h("a", { href: "#jobs" }, "All jobs →"), h("div", { class: "group" }, done.map(finishedRow))) : null,
      ),
    );
  },
};

function finishedRow(j) {
  return h("a", { class: "row row-link", href: jobLink(j) }, h("div", { class: "row-grid" }, h("div", null, reasonEl(j), h("code", { class: "cmd" }, j.command), jobMeta(j)), h("div", { class: "row-control" }, jobBadge(j))));
}

// Boxes: every box with its full state, and the controls on it.

const boxes = {
  mount(el) {
    this.update(el);
  },
  update(el) {
    const bx = [...S.boxes.values()].sort((a, b) => a.name.localeCompare(b.name));
    el.replaceChildren(
      h(
        "div",
        { class: "page" },
        h("div", { class: "page-head" }, h("h2", null, "Boxes"), h("p", null, "Throwaway Linux boxes on the PC. A box goes when it expires; extend one an agent still needs.")),
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

function matches(j) {
  const q = F.q.trim().toLowerCase();
  if (q && ![j.reason, j.command, j.branch, j.agent?.session].some((v) => (v || "").toLowerCase().includes(q))) return false;
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
  if (more && list.jobs.length) p.set("before", list.jobs[list.jobs.length - 1].startedAt);
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
    f.sel.replaceChildren(h("option", { value: "" }, label), values.map((v) => h("option", { value: v }, v)));
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
      options("Repo", "repo", uniq((j) => shortRepo(j.repo))),
      options("Agent", "agent", uniq((j) => j.agent?.session)),
      options("Kind", "kind", KINDS),
      S.machines.size > 1 ? options("Machine", "machine", [...S.machines.keys()]) : null,
      options("Result", "result", RESULTS),
    ].filter(Boolean);
    // Only when they change, so a held-open select or the focused search stays put.
    if (fs.some((f, i) => this.filters.children[i] !== f) || fs.length !== this.filters.children.length) this.filters.replaceChildren(...fs);
    const shown = known.filter(matches).sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt));
    const live = shown.filter((j) => j.state === "running" || j.state === "paused");
    const done = shown.filter((j) => j.state === "done" || j.state === "lost");
    const filtered = Object.values(F).some(Boolean);
    const kids = [];
    if (list.error) kids.push(h("div", { class: "banner banner-bad" }, icon("alert"), h("div", null, "Couldn't load finished jobs: " + list.error + " ", h("button", { class: "btn btn-xs btn-outline", onclick: () => fetchJobs() }, "Try again"))));
    if (live.length) kids.push(section("Running", String(live.length), h("div", { class: "group" }, live.map(finishedRow))));
    if (done.length) {
      const moreBtn = list.more ? h("button", { class: "btn btn-sm btn-outline", disabled: list.loading, onclick: () => fetchJobs(true) }, list.loading ? "Loading…" : "Show older") : null;
      kids.push(section("Finished", filtered ? plural(done.length, "match", "matches") : null, h("div", { class: "group" }, done.map(finishedRow)), moreBtn));
    }
    if (!live.length && !done.length) {
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

const tailer = { key: "", pre: null, offset: 0, timer: 0, status: "", gone: false, seq: 0 };

function stopTail() {
  clearTimeout(tailer.timer);
  tailer.seq++;
  tailer.key = "";
}

async function pull(j, seq) {
  if (seq !== tailer.seq) return;
  const cur = S.jobs.get(j.key) || j;
  let next = 0;
  try {
    const c = await api("/api/jobs/" + encodeURIComponent(j.key) + "/log?offset=" + tailer.offset);
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
    const j = S.jobs.get(this.key) || list.jobs.find((x) => x.key === this.key);
    if (!j) {
      el.replaceChildren(h("div", { class: "page" }, empty("search", "Job not found", "It isn't among the running jobs or the last finished ones. It may be older than this page has loaded.", h("a", { class: "btn btn-sm btn-outline", href: "#jobs" }, "Back to Jobs"))));
      return;
    }
    if (!this.body.isConnected) el.replaceChildren(this.body);
    if (tailer.key !== j.key) {
      stopTail();
      tailer.key = j.key;
      tailer.offset = 0;
      tailer.gone = !boxOf(j.machine, j.box);
      tailer.status = "";
      tailer.pre.textContent = tailer.gone ? j.tail || "" : "";
      if (!tailer.gone) pull(j, tailer.seq);
    }
    if (tailer.gone && tailer.pre.textContent !== (j.tail || "")) tailer.pre.textContent = j.tail || "";
    const live = j.state === "running" || j.state === "paused";
    let cancel = null;
    if (live) {
      cancel = h("button", { class: "btn btn-sm btn-danger-ghost" }, "Cancel job");
      cancel.addEventListener("click", () => cancelJob(j, cancel));
    }
    const kv = (k, v) => h("dl", { class: "kv" }, h("dt", null, k), h("dd", null, v || "—"));
    this.head.replaceChildren(h("div", { class: "group" }, h("div", { class: "row row-grid" }, h("div", null, reasonEl(j, "headline"), h("code", { class: "cmd" }, j.command), jobMeta(j)), h("div", { class: "row-control" }, jobBadge(j), cancel))));
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
          kv("Kind", j.kind),
          kv("Box", j.box + " · " + j.size + (j.gpu ? " GPU" : "")),
          kv("Started", stamp(j.startedAt)),
          kv(live ? "Running for" : "Took", elapsed(j)),
          kv("Exit", j.exit == null ? (j.state === "lost" ? "never read" : "—") : String(j.exit)),
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
