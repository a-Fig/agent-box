// The dashboard's page. Plain DOM, no framework and no build step, as Murmur's page: boxd embeds
// these files as they are. Everything an agent wrote (reasons, commands, branches, logs) is
// untrusted text and reaches the page only as text nodes, under a Content-Security-Policy of
// default-src 'self'. Widths and colors are set through the CSSOM, never a style attribute.

import { h, icon, tickTimes, dur, stamp, clock } from "./dom.js";
import { S, start, retryNow, setChanged, trayAlive, jobsOf, RETRY_MS } from "./store.js";
import { overview, boxes, jobs, jobDetail, modeLabel, checkDialog, MODE_HINTS } from "./views.js";
import { history } from "./history.js";
import { syncTip, hint } from "./tip.js";

const root = document.getElementById("root");
const NARROW = window.matchMedia("(max-width: 767px)");

const VIEWS = {
  overview: { title: "Overview", icon: "grid", view: overview },
  boxes: { title: "Boxes", icon: "box", view: boxes },
  jobs: { title: "Jobs", icon: "list", view: jobs },
  history: { title: "History", icon: "chart", view: history },
};

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
  render();
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
            root.querySelector(`[data-key="theme-${next}"]`)?.focus();
          },
        },
        label,
      ),
    ),
  );
}

// Routes: #overview, #boxes, #jobs, #jobs/<key>, #history.
function route() {
  const [name, ...rest] = location.hash.slice(1).split("/");
  if (name === "jobs" && rest.length) return { name: "jobs", key: decodeURIComponent(rest.join("/")) };
  return { name: VIEWS[name] ? name : "overview" };
}

let shell = null; // { el, side, content, title, mounted }
let drawer = false;

function sidebar(r) {
  const live = jobsOf((j) => j.state === "running" || j.state === "paused").length;
  const counts = { boxes: S.boxes.size || null, jobs: live || null };
  return h(
    "aside",
    { class: "sidebar", "data-app-sidebar": true, "aria-label": "Navigation" },
    h("div", { class: "sidebar-header" }, h("img", { class: "brand-mark", src: "/assets/favicon.svg", alt: "" }), h("span", { class: "brand" }, "agent-box")),
    h(
      "nav",
      { class: "sidebar-body" },
      h(
        "ul",
        { class: "sidebar-menu" },
        Object.entries(VIEWS).map(([id, v]) =>
          h("li", null, h("a", { class: "menu-button", href: "#" + id, "data-key": "nav-" + id, "aria-current": r.name === id ? "page" : null, onclick: () => setDrawer(false) }, icon(v.icon), h("span", { class: "label" }, v.title), counts[id] ? h("span", { class: "menu-count" }, String(counts[id])) : null)),
        ),
      ),
      S.machines.size ? h("p", { class: "group-label" }, "Machines") : null,
      [...S.machines.values()].map((m) =>
        h(
          "div",
          hint(modeLabel(m.mode) + ": " + MODE_HINTS[m.mode] + " Change the mode on Overview.", { class: "side-machine" }, "machine-" + m.name),
          h("span", { class: "side-machine-name" }, h("span", { class: "dot " + (m.mode === "mypc" ? "dot-off" : "dot-on") }), m.name),
          h("span", { class: "side-machine-line" }, modeLabel(m.mode), m.modeRequested ? " → " + modeLabel(m.modeRequested.mode) + " pending" : ""),
          trayAlive(m) ? (m.enforcer === "headless" ? h("span", { class: "side-machine-line" }, "Headless: nobody logged on") : null) : h("span", { class: "side-machine-line" }, "Nothing enforcing"),
        ),
      ),
    ),
    h("div", { class: "sidebar-foot" }, themeSwitch(), h("span", hint(S.conn === "live" ? "Connected: the page updates by itself as things change on the PC." : "The page lost its connection to the PC and is trying again; what it shows may be out of date.", { class: "conn" }, "conn"), h("span", { class: "dot " + (S.conn === "live" ? "dot-on" : "dot-bad") }), S.conn === "live" ? "Live" : "Reconnecting…"), [...S.machines.values()].map((m) => versions(m, S.machines.size > 1))),
  );
}

// The builds boxd and the tray run (dash.Version), and a note when they differ: twice on
// 2026-11-25 the PC ran builds hours behind the repo and nothing showed it. Both follow stable
// releases and check for one hourly (docs/releasing.md), so a release on one is on the other
// within the hour, and the note warns only once that is well past.
const BEHIND_WARN_MS = 2 * 3600e3; // the hourly check, with room for a slow download

function versions(m, named) {
  const b = m.boxd, t = m.tray;
  if (!b && !t) return null;
  const short = (v) => (v && v.commit ? v.commit.slice(0, 7) + (v.modified ? "+" : "") : "unknown");
  const name = (v) => (v?.tag ? v.tag : short(v));
  const about = (who, v) => who + ": " + (v && v.commit ? (v.tag ? v.tag + ", " : "") + v.commit.slice(0, 10) + (v.time ? ", committed " + stamp(v.time) : "") + (v.modified ? ", with uncommitted changes" : "") : "unknown");
  const lines = ["The builds of boxd (the server behind this page) and of the tray, which applies the mode on the PC.", about("boxd", b), about("tray", t)];
  const note = drift(b, t, trayAlive(m));
  if (note) lines.push(note.why);
  return h(
    "div",
    hint(lines.join("\n"), { class: "versions" }, "versions-" + m.name),
    h("span", null, (named ? m.name + ": " : "") + "boxd " + name(b) + " · tray " + name(t)),
    note ? h("span", { class: note.warn ? "versions-drift" : "versions-note" }, note.text) : null,
  );
}

// What is true when boxd and the tray run different builds: which one updates to which release,
// and when, or why neither will by itself. null when they are the same build. Which is behind is
// decided by release tag, as the updaters decide (internal/update.After): a nightly of v0.3.0 is
// behind v0.3.0 even on the same commit, and commit times can disagree with the tags. trayUp is
// whether the enforcer's heartbeat is fresh: a tray that isn't running checks for nothing.
function drift(b, t, trayUp) {
  if (!b || !t || !b.commit || !t.commit || (b.commit === t.commit && (b.tag || "") === (t.tag || ""))) return null;
  const kb = tagKey(b.tag), kt = tagKey(t.tag);
  // A dev build (no release tag) never updates itself; the release beside it can't be ordered
  // against it.
  if (!kb && !kt) return { text: "boxd and tray are different builds", why: "Neither is a release, so neither updates itself: build both from the same commit, or install a release." };
  if (!kb || !kt) {
    const [dev, rel] = kb ? ["tray", "boxd"] : ["boxd", "tray"];
    return { text: dev + " is a dev build, " + rel + " a release", why: "A dev build doesn't update itself: install a release, or build both from the same commit." };
  }
  const c = cmpKey(kb, kt);
  if (!c) return { text: "boxd and tray are different builds", why: "Both say " + b.tag + " but were built from different commits." };
  const [newer, older, behind, ahead] = c > 0 ? [b, t, "tray", "boxd"] : [t, b, "boxd", "tray"];
  if (behind === "tray" && !trayUp) return { text: "tray is behind boxd", why: "The tray isn't running, so it can't update; it checks for the latest stable release hourly while it runs." };
  // A prerelease (a nightly or test build) was installed by hand: state both, predict nothing.
  if (newer.tag.includes("-")) return { text: ahead + " runs " + newer.tag + ", " + behind + " " + older.tag, why: "boxd runs " + b.tag + " and the tray " + t.tag + ". Each updates itself from stable releases only." };
  // How long they have differed: since the later of boxd's start and the tray's last new build.
  // A boxd restart starts the count again, so a tray stuck for days reads as pending for 2 hours
  // after one; the warning comes back after that.
  const since = Math.max(b.since ? Date.parse(b.since) : 0, t.since ? Date.parse(t.since) : 0);
  const waited = since ? clock.now() - since : 0;
  if (waited > BEHIND_WARN_MS) return { warn: true, text: behind + " still on " + older.tag + "; " + ahead + " has had " + newer.tag + " for " + dur(waited / 1000).split(" ")[0], why: behind + " checks for a stable release hourly, so it should have updated by now. Its log on the PC says why it hasn't." };
  return { text: behind + " updates at its hourly check", why: behind + " checks for the latest stable release hourly and installs it by itself." };
}

// A release tag's order, as internal/update's key: X, Y, Z, then a stable release above its
// nightlies (by date and number) and its test prereleases. null for anything else.
function tagKey(tag) {
  const m = /^v(\d+)\.(\d+)\.(\d+)(?:-nightly\.(\d{8})\.(\d+)|-test\.(\d+))?$/.exec(tag || "");
  if (!m) return null;
  const n = (s) => Number(s || 0);
  if (m[4]) return [n(m[1]), n(m[2]), n(m[3]), 0, n(m[4]), n(m[5])];
  if (m[6]) return [n(m[1]), n(m[2]), n(m[3]), 0, 0, n(m[6])];
  return [n(m[1]), n(m[2]), n(m[3]), 1, 0, 0];
}
const cmpKey = (a, b) => a.reduce((c, v, i) => c || Math.sign(v - b[i]), 0);

function setDrawer(open) {
  drawer = open && NARROW.matches;
  render();
}

function unreachable() {
  const wait = Math.max(0, Math.round((S.nextTry - Date.now()) / 1000));
  return h(
    "div",
    { class: "full-screen" },
    h(
      "div",
      { class: "empty", role: "alert" },
      h("span", { class: "empty-ico" }, icon("offline")),
      h("p", { class: "empty-title" }, "PC unreachable"),
      h("p", { class: "empty-line" }, "The dashboard runs on the PC, and it isn't answering. It is probably asleep or off, or its memory was released from the tray, which stops the agents' Linux."),
      h("p", { class: "empty-line", "data-retry": true }, "Trying again " + (wait ? "in " + wait + "s" : "now") + " · every " + RETRY_MS / 1000 + "s"),
      h("button", { class: "btn btn-sm btn-outline", onclick: retryNow }, icon("retry"), "Try now"),
      h("div", { class: "sidebar-foot" }, themeSwitch()),
    ),
  );
}

function loading() {
  return h("div", { class: "full-screen" }, h("div", { class: "empty", role: "status" }, h("span", { class: "empty-ico" }, icon("retry", "spin")), h("p", { class: "empty-title" }, "Connecting to the PC…")));
}

// Widths, fills and scroll positions the views asked for with data- attributes, set through the
// CSSOM, which the CSP allows where a style attribute isn't.
function finish(el) {
  for (const n of el.querySelectorAll("[data-w]")) n.style.width = Math.max(0, Math.min(100, Number(n.dataset.w))) + "%";
  for (const n of el.querySelectorAll("[data-bg]")) n.style.background = n.dataset.bg;
  for (const n of el.querySelectorAll("[data-stick]")) n.scrollTop = n.scrollHeight;
}

let current = null; // { name, key, view }
let queued = false;

function render() {
  if (queued) return;
  queued = true;
  requestAnimationFrame(draw);
}

// A redraw replaces the elements under the pointer, so one between a press and its release would
// drop the click: while a pointer is down, the redraw waits for it to come up.
// A release can go missing (the pointer leaves the window, a dialog takes it), so the hold also
// clears when the window loses focus or a pointer event says no button is down.
let pressed = false;
let owed = false;
function release() {
  if (!pressed) return;
  pressed = false;
  // After the click this release makes has run.
  if (owed) setTimeout(render, 0);
  owed = false;
}
document.addEventListener("pointerdown", () => (pressed = true), true);
for (const ev of ["pointerup", "pointercancel"]) document.addEventListener(ev, release, true);
for (const ev of ["pointermove", "pointerover", "pointerenter"]) document.addEventListener(ev, (e) => e.buttons === 0 && release(), true);
window.addEventListener("blur", release);

// What a redraw would lose, by data-key: the focus, and where each scrolled element was (a log
// tail scrolled up stays there; one at the bottom follows new lines).
function keep() {
  const focus = document.activeElement?.closest?.("[data-key]")?.dataset.key;
  const scroll = new Map();
  for (const el of root.querySelectorAll("pre[data-key]")) scroll.set(el.dataset.key, el.scrollTop + el.clientHeight >= el.scrollHeight - 8 ? -1 : el.scrollTop);
  return { focus, scroll };
}

function restore({ focus, scroll }) {
  for (const el of root.querySelectorAll("pre[data-key]")) {
    const at = scroll.get(el.dataset.key);
    if (at != null && at >= 0) el.scrollTop = at;
  }
  if (focus && document.activeElement?.closest?.("[data-key]")?.dataset.key !== focus) root.querySelector(`[data-key="${CSS.escape(focus)}"]`)?.focus({ preventScroll: true });
}

function draw() {
  queued = false;
  if (pressed) {
    owed = true;
    return;
  }
  const kept = keep();
  if (S.conn === "down" || (S.conn === "loading" && !S.machines.size)) {
    current?.view.leave?.();
    current = null;
    shell = null;
    root.replaceChildren(S.conn === "down" ? unreachable() : loading());
    syncTip();
    toastEl();
    return;
  }
  const r = route();
  const v = r.key ? jobDetail : VIEWS[r.name].view;
  if (!shell) {
    const content = h("main", { class: "content", id: "content", tabindex: "-1" });
    const title = h("h1", null);
    const top = h("header", { class: "topbar" }, h("button", { class: "icon-btn menu-toggle", "aria-label": "Open navigation", onclick: () => setDrawer(true) }, icon("menu")), title);
    const el = h("div", { class: "app" }, h("span"), h("div", { class: "main" }, top, content));
    shell = { el, content, title };
    root.replaceChildren(el);
    current = null;
  }
  shell.el.classList.toggle("drawer-open", drawer);
  shell.el.firstChild.replaceWith(sidebar(r));
  const scrim = shell.el.querySelector(".drawer-scrim");
  if (drawer && !scrim) shell.el.append(h("div", { class: "drawer-scrim", onclick: () => setDrawer(false) }));
  if (!drawer) scrim?.remove();
  const name = r.key ? "job" : r.name;
  shell.title.textContent = r.key ? "Job" : VIEWS[r.name].title;
  document.title = (r.key ? "Job" : VIEWS[r.name].title) + " · agent-box";
  if (!current || current.name !== name || current.key !== r.key) {
    current?.view.leave?.();
    current = { name, key: r.key, view: v };
    shell.content.scrollTop = 0;
    v.mount(shell.content, r.key);
  } else v.update(shell.content, r.key);
  finish(shell.content);
  finish(shell.el.firstChild);
  restore(kept);
  syncTip();
  checkDialog();
  toastEl();
}

function toastEl() {
  let t = document.querySelector(".toast");
  if (!S.toast) return t?.remove();
  if (!t) document.body.append((t = h("div", { class: "toast", role: "status" })));
  t.className = "toast" + (S.toast.bad ? " bad" : "");
  t.textContent = S.toast.text;
}

setChanged(render);
window.addEventListener("hashchange", () => {
  drawer = false;
  render();
});
NARROW.addEventListener("change", () => setDrawer(false));
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && drawer) setDrawer(false);
});
// Times on the page ("6m 12s", "expires in 2h") move on without an event: each second the ticker
// rewrites only those spans, and the countdown on the unreachable screen. Every 30s it redraws,
// for what turns on time alone, such as the tray's heartbeat going stale.
let ticks = 0;
setInterval(() => {
  if (S.conn === "down") {
    const n = root.querySelector("[data-retry]");
    if (n) {
      const wait = Math.max(0, Math.round((S.nextTry - Date.now()) / 1000));
      n.textContent = "Trying again " + (wait ? "in " + wait + "s" : "now") + " · every " + RETRY_MS / 1000 + "s";
    }
  } else if (++ticks % 30 === 0) render();
  else tickTimes(root);
}, 1000);

start();
