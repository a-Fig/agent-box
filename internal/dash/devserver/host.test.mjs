// Start the devserver with -scenario headless -still, make playwright-core
// available on NODE_PATH, then run node --test internal/dash/devserver/host.test.mjs.
// DASH_FIXTURE_URL and CHROMIUM_PATH override the fixture URL and Chromium path.
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { after, before, test } from "node:test";

const { chromium } = createRequire(import.meta.url)("playwright-core");
const fixture = process.env.DASH_FIXTURE_URL || "http://127.0.0.1:7767";
const tick = "2026-11-26T12:00:00";
let browser;

before(async () => {
  browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH });
});
after(() => browser?.close());

async function hostPage(t, arrange = () => {}) {
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  t.after(() => page.close());
  const state = await (await page.request.get(fixture + "/api/state")).json();
  arrange(state);
  await page.route("**/api/state", (route) => route.fulfill({ json: state }));
  await page.addInitScript(() => {
    // Deliver the fixture's events in the test's order.
    window.EventSource = class extends EventTarget {
      constructor() {
        super();
        window.fixtureEvents = this;
      }
      close() {}
    };
  });
  await page.goto(fixture + "/#fleet/" + state.machines[0].id);
  await page.getByRole("heading", { name: "Builds", exact: true }).waitFor();
  return { page, state };
}

function event(page, type, payload) {
  return page.evaluate(async ({ type, payload }) => {
    window.fixtureEvents.dispatchEvent(new MessageEvent(type, { data: JSON.stringify(payload) }));
    await new Promise(requestAnimationFrame);
  }, { type, payload });
}

function row(page, title) {
  return page.locator(".row").filter({ has: page.locator(".row-title").filter({ hasText: new RegExp("^" + title) }) });
}

test("custom prereleases compare against stable and unsupported tags show unknown", async (t) => {
  const { page, state } = await hostPage(t, (state) => {
    state.updates = { latest: "v0.3.0", checkedAt: tick + "Z" };
    state.machines[0].boxd.tag = "v0.3.1-rc.1";
    state.machines[0].tray.tag = "custom";
  });
  const boxd = row(page, "boxd");
  const tray = row(page, "Tray");
  await boxd.getByText("ahead of stable", { exact: true }).waitFor();
  await tray.getByText("unknown", { exact: true }).waitFor();
  assert.equal(await page.getByText(/is a dev build/).count(), 0);

  state.machines[0].boxd.tag = "v0.3.0-rc.1";
  state.machines[0].tray.tag = "";
  await event(page, "machine", state.machines[0]);
  await boxd.getByText("update available", { exact: true }).waitFor();
  await tray.getByText("dev build", { exact: true }).waitFor();
});

test("the update badge describes the comparison without promising an install", async (t) => {
  const { page } = await hostPage(t, (state) => {
    state.updates = { latest: "v0.3.1", checkedAt: tick + "Z" };
  });
  const badge = row(page, "Tray").getByText("update available", { exact: true });
  assert.equal(await badge.getAttribute("aria-description"), "Behind the latest stable release (v0.3.1).");
});

test("delayed checks cannot replace a newer result from either path", async (t) => {
  const { page } = await hostPage(t);
  const older = { latest: "v0.3.0", checkedAt: tick + "Z" };
  const first = { latest: "v0.3.1", checkedAt: tick + ".000000001Z" };
  const newer = { latest: "v0.3.2", checkedAt: tick + ".000000002Z" };
  const latest = row(page, "Latest stable release");
  const check = page.getByRole("button", { name: "Check for updates" });
  let releaseDelayed;
  const delayReply = new Promise((resolve) => { releaseDelayed = resolve; });
  let checks = 0;
  await page.route("**/api/updates/check", async (route) => {
    if (++checks === 1) return route.fulfill({ json: first });
    await delayReply;
    return route.fulfill({ json: older });
  });
  await check.click();
  await latest.getByText(first.latest, { exact: true }).waitFor();
  await event(page, "updates", older);
  assert.equal(await latest.locator("[data-t]").getAttribute("data-t"), first.checkedAt);
  await event(page, "updates", newer);
  await latest.getByText(newer.latest, { exact: true }).waitFor();
  await check.click();
  try {
    await page.getByRole("button", { name: "Checking…" }).waitFor();
  } finally {
    releaseDelayed();
  }
  await check.waitFor();
  assert.equal(await latest.locator("[data-t]").getAttribute("data-t"), newer.checkedAt);
});

test("the initial stream snapshot cannot replace a newer check response", async (t) => {
  const { page, state } = await hostPage(t, (state) => {
    state.updates = { latest: "v0.3.0", checkedAt: tick + "Z" };
  });
  const newer = { latest: "v0.3.1", checkedAt: tick + ".000000001Z" };
  const latest = row(page, "Latest stable release");
  await page.route("**/api/updates/check", (route) => route.fulfill({ json: newer }));
  await page.getByRole("button", { name: "Check for updates" }).click();
  await latest.getByText(newer.latest, { exact: true }).waitFor();
  await event(page, "state", state);
  assert.equal(await latest.locator("[data-t]").getAttribute("data-t"), newer.checkedAt);
  assert.equal(await latest.getByText(newer.latest, { exact: true }).count(), 1);
});

test("matching release badges keep keyboard focus on the same component after a redraw", async (t) => {
  const { page, state } = await hostPage(t, (state) => {
    state.updates = { latest: "" };
  });
  const tray = row(page, "Tray").getByText("not checked", { exact: true });
  await tray.focus();
  await event(page, "machine", state.machines[0]);
  assert.equal(await tray.evaluate((el) => el === document.activeElement), true);
});

test("Settings holds only what can be changed, and is reached from the sidebar's foot", async (t) => {
  const { page } = await hostPage(t);
  await page.locator(".sidebar-foot").getByRole("link", { name: "Settings" }).click();
  await page.getByRole("heading", { name: "Appearance", exact: true }).waitFor();
  assert.equal(await page.getByRole("heading", { name: "Builds", exact: true }).count(), 0);
  assert.equal(await page.locator(".sidebar-menu").getByRole("link", { name: "Settings" }).count(), 0);
});

test("a host in the sidebar's Fleet opens its page", async (t) => {
  const { page, state } = await hostPage(t);
  await page.goto(fixture + "/#overview");
  const m = state.machines[0];
  await page.locator(".side-host").filter({ hasText: m.name }).click();
  await page.getByRole("heading", { name: m.name, exact: true, level: 2 }).waitFor();
  assert.equal(await page.locator('.side-host[aria-current="page"]').count(), 1);
});

test("the foot's notice opens the host only once its builds are out of step past the hourly updates", async (t) => {
  const long = new Date(Date.now() - 3 * 3600e3).toISOString();
  const { page, state } = await hostPage(t, (state) => {
    state.updates = { latest: "v0.3.2", checkedAt: tick + "Z" };
  });
  const notice = page.locator(".sidebar-foot .side-notice");
  // A release that is out, and builds 20 minutes apart: both update within the hour, so no notice.
  assert.equal(await notice.count(), 0);
  state.machines[0].boxd.since = long;
  state.machines[0].tray.since = long;
  await event(page, "machine", state.machines[0]);
  await notice.waitFor();
  await page.goto(fixture + "/#settings");
  await notice.click();
  await page.getByRole("heading", { name: "Builds", exact: true }).waitFor();
});
