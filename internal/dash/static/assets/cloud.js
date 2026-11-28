// A cloud host's page (#fleet/cloud/<name>, #280): a cloud runner (Modal today) as box last
// reported it to this boxd (dash/cloud.go): what it has spent this month against its cap, what
// runs on it, and the page's pause on cloud launches. Cloud hosts have no boxd, mode or boxes, so
// they get a page of their own rather than the PC's.

import { h, icon, timeEl } from "./dom.js";
import { S, post, applyCloud, changed } from "./store.js";
import { hint } from "./tip.js";
import { section, empty } from "./views.js";
import { hostKind } from "./host.js";

export { cloudPage, cloudStatus, cloudLink, cloudLine, cloudLeave };

const CLOUD_KEY = "cloud/";

function cloudLink(c) {
  return "#fleet/" + CLOUD_KEY + encodeURIComponent(c.name);
}

const usd = (n) => "$" + (n || 0).toFixed(2);
// The sidebar's narrower money: whole dollars without the cents, so the cap fits on the line.
const usdShort = (n) => usd(n).replace(/\.00$/, "");
const count = (n) => (n || 0).toLocaleString("en-US");

// What this month costs so far: settled spend and what running jobs have run up, as box's own cap
// check counts it (cloudhost.go).
function spend(c) {
  const b = c.budget;
  return b ? (b.spentUSD || 0) + (b.runningUSD || 0) : 0;
}

// One state for a cloud host, for its dot, badge and sidebar line. The page's pause comes first:
// it is the one a person here can change.
function cloudStatus(c) {
  const b = c.budget;
  const cap = b?.capUSD || 0;
  const spent = spend(c);
  if (S.cloudPaused) return { key: "paused", label: "Paused", dot: "dot-off", badge: "badge-outline", why: "Cloud launches are paused from this page, so box sends no new job to a cloud host." };
  if (c.paused) return { key: "paused", label: "Paused on the runner", dot: "dot-off", badge: "badge-outline", why: "The runner itself is paused (box cloud pause); box cloud resume starts it again." };
  if (cap > 0 && spent >= cap) return { key: "capped", label: "Cap reached", dot: "dot-bad", badge: "badge-bad", why: "This month's spend reached the runner's cap, so it takes no new job until next month or a higher cap." };
  if (cap > 0 && b.warnAt > 0 && spent / cap >= b.warnAt) return { key: "warn", label: "Near its cap", dot: "dot-warn", badge: "badge-warn", why: "This month's spend passed " + Math.round(b.warnAt * 100) + "% of the runner's cap." };
  if (c.maxConcurrent > 0 && c.running >= c.maxConcurrent) return { key: "busy", label: "Busy", dot: "dot-shared", badge: "badge-shared", why: "Every job slot on the runner is taken; the next cloud run waits for one." };
  return { key: "ready", label: "Ready", dot: "dot-on", badge: "badge-on", why: "Takes cloud jobs when the PCs have no room." };
}

// The sidebar's one line for a cloud host: its state, then its spend against the cap.
function cloudLine(c) {
  const st = cloudStatus(c);
  const cap = c.budget?.capUSD || 0;
  return st.label + (c.budget ? " · " + usdShort(spend(c)) + (cap > 0 ? " of " + usdShort(cap) : "") : "");
}

const MODES = {
  function: "Each job runs as a function call, so nothing is left to pull once it ends.",
  sandbox: "Each job runs in a sandbox the runner keeps a while, so box pull can copy what it built.",
};

let pausing = false;
let pauseErr = "";

async function togglePause() {
  if (pausing) return;
  pausing = true;
  pauseErr = "";
  changed();
  try {
    applyCloud(await post("/api/cloud/pause", { paused: !S.cloudPaused }));
  } catch (e) {
    pauseErr = e.message || "Couldn't change the pause.";
  } finally {
    pausing = false;
    changed();
  }
}

function cloudLeave() {
  if (!pausing) pauseErr = "";
}

// A meter as the PC's card draws them, with a line under it. cls is "", "hot" or "full".
function meter(label, value, p, cls, tip, under, key) {
  return h(
    "div",
    { class: key === "spend" ? "cloud-spend" : null },
    h("div", { class: "meter-label" }, h("span", hint(tip, {}, "cloud-" + key), label), h("strong", { class: "num" }, value)),
    p == null ? null : h("div", { class: "bar" + (cls ? " " + cls : ""), role: "meter", "aria-label": label, "aria-valuenow": Math.round(p), "aria-valuemin": 0, "aria-valuemax": 100 }, h("i", { "data-w": p })),
    under ? h("p", { class: "meter-under" }, under) : null,
  );
}

function spendMeter(c) {
  const b = c.budget;
  if (!b) return meter("Spend this month", "not reported", null, "", "The runner didn't report a budget.", null, "spend");
  const cap = b.capUSD || 0;
  const spent = spend(c);
  const under = [];
  if (b.runningUSD > 0) under.push(usd(b.runningUSD) + " of it is jobs still running");
  if (b.creditUSD > 0) under.push(usd(b.creditUSD) + " of free credit" + (spent <= b.creditUSD ? " covers it so far" : ", used up"));
  if (!cap) {
    under.push("no monthly cap is set");
    return meter("Spend this month", usd(spent), null, "", "What the runner has spent this month (" + b.month + ").", under.join(" · "), "spend");
  }
  const p = Math.min(100, (spent / cap) * 100);
  const cls = spent >= cap ? "full" : b.warnAt > 0 && spent / cap >= b.warnAt ? "hot" : "";
  const tip = "What the runner has spent this month (" + b.month + "), of its monthly cap. It warns past " + Math.round((b.warnAt || 0) * 100) + "% and takes no new job at the cap.";
  return meter("Spend this month", usd(spent) + " of " + usd(cap), p, cls, tip, under.join(" · ") || null, "spend");
}

function runningMeter(c) {
  const max = c.maxConcurrent || 0;
  const p = max ? Math.min(100, (c.running / max) * 100) : null;
  return meter("Running", c.running + " of " + max, p, max && c.running >= max ? "full" : "", "Jobs running on the runner now, of the most it runs at once.", null, "running");
}

function controllerMeter(u) {
  const p = u.limit ? Math.min(100, (u.requests / u.limit) * 100) : null;
  return meter("Controller requests", count(u.requests) + " of " + count(u.limit), p, p >= 100 ? "full" : p >= 80 ? "hot" : "", "Requests the fleet controller (a free Cloudflare Worker) has answered today, of its free daily limit.", "requests today" + (u.day ? " (" + u.day + ", UTC)" : ""), "controller");
}

function cloudPage(el, key) {
  const name = key.slice(CLOUD_KEY.length);
  const c = S.cloud.get(name);
  if (!c) {
    el.replaceChildren(h("div", { class: "page" }, empty("offline", "No such cloud host", "No box has reported a cloud host called " + name + " to this PC since boxd started. One shows up here once box probes it.", h("a", { class: "btn btn-sm btn-outline", href: "#overview" }, "Back to Overview"))));
    return;
  }
  const kind = hostKind(c);
  const st = cloudStatus(c);
  const paused = S.cloudPaused;
  const pauseBtn = h(
    "button",
    hint(paused ? "Let box launch cloud jobs again." : "Stop box from launching new cloud jobs, on every cloud host.", { class: "btn btn-sm " + (paused ? "btn-primary" : "btn-outline") + " cloud-pause", type: "button", "data-key": "cloud-pause", "aria-disabled": pausing ? "true" : null, "aria-pressed": paused ? "true" : "false", onclick: togglePause }),
    icon(paused ? "play" : "pause"),
    pausing ? (paused ? "Resuming…" : "Pausing…") : paused ? "Resume cloud launches" : "Pause cloud launches",
  );
  el.replaceChildren(
    h(
      "div",
      { class: "page" },
      h("div", { class: "page-head host-head" }, h("span", { class: "host-ico" }, icon(kind.icon)), h("div", { class: "host-title" }, h("div", { class: "host-name" }, h("h2", null, c.name)), h("p", null, kind.about))),
      paused ? h("div", { class: "banner", role: "status" }, icon("pause"), h("div", null, h("strong", null, "Cloud launches are paused from this page. "), "box sends no new job to a cloud host until you resume. Jobs already running go on; ", h("code", null, "box cloud stop"), " stops them.")) : null,
      section(
        "Now",
        null,
        h(
          "div",
          { class: "group" },
          h(
            "div",
            { class: "row" },
            h("div", { class: "machine-head" }, h("span", hint(st.why, { class: "badge " + st.badge }, "cloud-state"), h("span", { class: "dot " + st.dot }), st.label), h("span", hint(MODES[c.mode] || "", { class: "badge badge-outline" }, "cloud-mode"), c.mode || "function"), pauseBtn),
            h("p", { class: "mode-note" }, "Pausing stops new cloud launches from this page, for every agent. Jobs already running keep going: ", h("code", null, "box cloud stop"), " stops them."),
            pauseErr ? h("p", { class: "row-error", role: "alert" }, pauseErr) : null,
          ),
          h("div", { class: "row" }, h("div", { class: "meters cloud-meters" }, spendMeter(c), runningMeter(c), S.controller ? controllerMeter(S.controller) : null)),
          h("div", { class: "row room" }, h("span", { class: "muted" }, timeEl("ago", c.reportedAt, "Reported " + (c.reportedBy ? "from " + c.reportedBy + " " : "")), ". box reports a cloud host each time it probes one or a job on it ends.")),
        ),
      ),
    ),
  );
}
