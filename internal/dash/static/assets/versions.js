// What to say when the builds boxd and the tray run differ (drift), and how a build compares to the
// latest stable release (releaseStatus). A host's page shows both; the sidebar's foot shows a
// notice when either wants attention.

import { dur, clock } from "./dom.js";

export { drift, releaseStatus };

// The builds boxd and the tray run (dash.Version), and a note when they differ: twice on
// 2026-11-25 the PC ran builds hours behind the repo and nothing showed it. Both follow stable
// releases and check for one hourly (docs/releasing.md), so a release on one is on the other
// within the hour, and the note warns only once that is well past.
const BEHIND_WARN_MS = 2 * 3600e3; // the hourly check, with room for a slow download

// What is true when boxd and the tray run different builds: which one updates to which release,
// and when, or why neither will by itself. null when they are the same build. Which is behind is
// decided by release tag, as the updaters decide (internal/update.After): a nightly of v0.3.0 is
// behind v0.3.0 even on the same commit, and commit times can disagree with the tags. trayUp is
// whether the enforcer's heartbeat is fresh: a tray that isn't running checks for nothing.
function drift(b, t, trayUp) {
  if (!b || !t || !b.commit || !t.commit || (b.commit === t.commit && (b.tag || "") === (t.tag || ""))) return null;
  const kb = tagKey(b.tag), kt = tagKey(t.tag);
  if ((b.tag && !kb) || (t.tag && !kt)) {
    return { text: "boxd and tray are different builds", why: "The order of these builds is unknown." };
  }
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

// How one reported build compares to the latest stable release (updates: { latest, checkedAt }).
// Uses the same tag order as internal/update.After: a nightly of vX.Y.Z is behind vX.Y.Z.
// Returns { label, cls, why } for a reported build, or null when no build is reported.
function releaseStatus(v, updates) {
  if (!v || !v.commit) return null;
  if (!v.tag) {
    return {
      label: "dev build",
      cls: "badge-outline",
      why: "Built without a release tag, so it doesn't follow stable releases or update itself.",
    };
  }
  if (!updates || !updates.checkedAt) {
    return {
      label: "not checked",
      cls: "badge-outline",
      why: "Click Check for updates to compare this build with the latest stable release.",
    };
  }
  if (!updates.latest) {
    return {
      label: "no stable release",
      cls: "badge-outline",
      why: "No stable release has been published on GitHub yet.",
    };
  }
  // The release workflow also publishes custom prereleases. Their version
  // numbers suffice to compare them with a stable release.
  const prerelease = /^v(\d+)\.(\d+)\.(\d+)-[0-9A-Za-z.-]+$/.exec(v.tag);
  const kv = prerelease ? [...prerelease.slice(1).map(Number), 0, 0, 0] : tagKey(v.tag);
  const kl = tagKey(updates.latest);
  if (!kv || kl?.[3] !== 1) {
    return {
      label: "unknown",
      cls: "badge-outline",
      why: "This build couldn't be compared with the latest stable release.",
    };
  }
  const c = cmpKey(kv, kl);
  if (c === 0) {
    return {
      label: "up to date",
      cls: "badge-on",
      why: "Matches the latest stable release (" + updates.latest + ").",
    };
  }
  if (c < 0) {
    return {
      label: "update available",
      cls: "badge-warn",
      why: "Behind the latest stable release (" + updates.latest + ").",
    };
  }
  return {
    label: "ahead of stable",
    cls: "badge-info",
    why: "Newer than the latest stable release (" + updates.latest + ").",
  };
}

