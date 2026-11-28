// The dashboard's page. Plain DOM, no framework and no build step, as Murmur's page: boxd embeds
// these files as they are. Everything an agent wrote (reasons, commands, branches, logs) is
// untrusted text and reaches the page only as text nodes, under a Content-Security-Policy of
// default-src 'self'. Widths and colors are set through the CSSOM, never a style attribute.

import { h, icon, tickTimes } from "./dom.js";
import { S, start, retryNow, setChanged, trayAlive, jobsOf, RETRY_MS } from "./store.js";
import { overview, boxes, jobs, jobDetail, modeLabel, checkDialog, MODE_HINTS } from "./views.js";
import { history } from "./history.js";
import { settings, themeSwitch } from "./settings.js";
import { drift } from "./versions.js";
import { host, hostKind, hostLink } from "./host.js";
import { syncTip, hint } from "./tip.js";

const root = document.getElementById("root");
const NARROW = window.matchMedia("(max-width: 767px)");

const VIEWS = {
  overview: { title: "Overview", icon: "grid", view: overview },
  boxes: { title: "Boxes", icon: "box", view: boxes },
  jobs: { title: "Jobs", icon: "list", view: jobs },
  history: { title: "History", icon: "chart", view: history },
};
// Reached from the sidebar's foot (#287) and from a host in Fleet (#288), not from the nav list.
const PAGES = { ...VIEWS, settings: { title: "Settings", view: settings } };

// Routes: #overview, #boxes, #jobs, #jobs/<key>, #history, #settings, #fleet/<host id>.
function route() {
  const [name, ...rest] = location.hash.slice(1).split("/");
  // A hand-typed hash can hold a bad escape, which decodeURIComponent throws on: it opens Overview.
  const key = () => {
    try {
      return decodeURIComponent(rest.join("/"));
    } catch {
      return null;
    }
  };
  if ((name === "jobs" || name === "fleet") && rest.length && key() != null) return { name, key: key() };
  return { name: PAGES[name] ? name : "overview" };
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
      S.machines.size ? h("p", { class: "group-label" }, "Fleet") : null,
      S.machines.size ? h("ul", { class: "sidebar-menu" }, [...S.machines.values()].map((m) => h("li", null, fleetRow(m, r)))) : null,
    ),
    foot(r),
  );
}

// A host in the sidebar's Fleet (#288): what it is, its name, and one line of state; a link to
// its page. A row says what kind of host it is, so cloud hosts can sit beside the PCs later.
function fleetRow(m, r) {
  const alive = trayAlive(m);
  const req = m.modeRequested;
  const state = !m.reachable ? "Not answering" : !alive ? "Nothing enforcing" : modeLabel(m.mode) + (req ? " → " + modeLabel(req.mode) : "") + (m.enforcer === "headless" ? " · headless" : "");
  // The same dots as Overview's card: hollow when the host doesn't answer, red when it answers but
  // nothing applies its mode.
  const dot = !m.reachable ? "dot-off" : !alive ? "dot-bad" : m.mode === "mypc" ? "dot-off" : "dot-on";
  // The hint says what the line says.
  const about = !m.reachable
    ? "The host isn't answering: it may be asleep or off."
    : !alive
      ? "Nothing applies the mode on this host, so mode changes wait."
      : req
        ? "Switching to " + modeLabel(req.mode) + ", asked from " + req.by + "."
        : (MODE_HINTS[m.mode] || "") + (m.enforcer === "headless" ? " Nobody is logged on, so the headless enforcer applies it." : "");
  return h(
    "a",
    hint(hostKind(m).label + ": " + about, { class: "menu-button side-host", href: hostLink(m), "data-key": "host-" + m.id, "aria-current": r.name === "fleet" && r.key === m.id ? "page" : null, onclick: () => setDrawer(false) }, "inner"),
    icon(hostKind(m).icon),
    h("span", { class: "label side-host-text" }, h("span", { class: "side-host-name truncate" }, m.name), h("span", { class: "side-host-line" }, h("span", { class: "dot " + dot }), state)),
  );
}

// The sidebar's foot, as Murmur's (#287): a notice when a host's builds have been out of step past
// the time both update by themselves (drift's warning), then the connection and the Settings
// button. A release that is merely out isn't a notice: both install it within the hour. The builds
// themselves are on each host's page.
function foot(r) {
  const notes = [...S.machines.values()]
    .map((m) => {
      const d = drift(m.boxd, m.tray, trayAlive(m));
      return d?.warn && h("a", hint(d.why, { class: "side-notice", href: hostLink(m), "data-key": "notice-" + m.id, onclick: () => setDrawer(false) }, "inner"), icon("alert"), h("span", { class: "side-notice-text" }, h("span", null, "Builds out of step"), S.machines.size > 1 ? h("span", { class: "side-notice-host truncate" }, m.name) : null));
    })
    .filter(Boolean);
  const live = S.conn === "live";
  return h(
    "div",
    { class: "sidebar-foot" },
    notes,
    h(
      "div",
      { class: "foot-row" },
      h("span", hint(live ? "Connected: the page updates by itself as things change on the PC." : "The page lost its connection to the PC and is trying again; what it shows may be out of date.", { class: "conn" }, "conn"), h("span", { class: "dot " + (live ? "dot-on" : "dot-bad") }), live ? "Live" : "Reconnecting…"),
      h("a", { class: "icon-btn foot-settings", href: "#settings", title: "Settings", "aria-label": "Settings", "aria-current": r.name === "settings" ? "page" : null, "data-key": "nav-settings", onclick: () => setDrawer(false) }, icon("gear")),
    ),
  );
}

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
  // A host's page has its name as its heading (with the rename), so the top bar says where it is:
  // Fleet. Every other page's name is said once, in the top bar (#293).
  const page = r.name === "fleet" ? { title: "Fleet", doc: S.machines.get(r.key)?.name || "Host", view: host } : r.key ? { title: "Job", view: jobDetail } : PAGES[r.name];
  const v = page.view;
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
  const name = r.name === "fleet" ? "fleet" : r.key ? "job" : r.name;
  shell.title.textContent = page.title;
  document.title = (page.doc || page.title) + " · agent-box";
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
