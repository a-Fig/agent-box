// The builds boxd and the tray run, and what to say when they differ. The sidebar shows the short
// line (versions); Settings shows each build and the whole note (drift).

import { h, dur, stamp, clock } from "./dom.js";
import { trayAlive } from "./store.js";
import { hint } from "./tip.js";

export { versions, drift };

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

