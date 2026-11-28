// The cloud host's page and its rows in the sidebar's Fleet (#280). Run as host.test.mjs says:
// the devserver with -scenario headless -still; each test puts its own cloud hosts in the state.
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { after, before, test } from "node:test";

const { chromium } = createRequire(import.meta.url)("playwright-core");
const fixture = process.env.DASH_FIXTURE_URL || "http://127.0.0.1:7767";
let browser;

before(async () => {
  browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH });
});
after(() => browser?.close());

const budget = (spentUSD, capUSD, more = {}) => ({ month: "2026-11", spentUSD, runningUSD: 0, capUSD, creditUSD: 0, warnAt: 0.75, ...more });
const host = (name, more = {}) => ({ name, kind: "modal", mode: "function", paused: false, running: 1, maxConcurrent: 4, budget: budget(3, 25), reportedAt: "2026-11-24T17:29:00Z", reportedBy: "laptop", ...more });

async function cloudPage(t, cloud, hash, arrange = () => {}) {
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  t.after(() => page.close());
  const state = await (await page.request.get(fixture + "/api/state")).json();
  state.cloud = cloud;
  state.controller = { day: "2026-11-24", requests: 2412, limit: 100000 };
  arrange(state);
  await page.route("**/api/state", (route) => route.fulfill({ json: state }));
  await page.addInitScript(() => {
    window.EventSource = class extends EventTarget {
      constructor() {
        super();
        window.fixtureEvents = this;
      }
      close() {}
    };
  });
  await page.goto(fixture + "/" + hash);
  return { page, state };
}

function event(page, type, payload) {
  return page.evaluate(async ({ type, payload }) => {
    window.fixtureEvents.dispatchEvent(new MessageEvent(type, { data: JSON.stringify(payload) }));
    await new Promise(requestAnimationFrame);
  }, { type, payload });
}

const sideRow = (page, name) => page.locator(`[data-key="host-cloud-${name}"]`);

test("the sidebar's Fleet shows each cloud host's state and spend", async (t) => {
  const { page } = await cloudPage(t, [host("ok"), host("warm", { budget: budget(19.5, 25) }), host("capped", { budget: budget(25, 25) }), host("held", { paused: true })], "#overview");
  const cases = [
    ["ok", "dot-on", "Ready · $3 of $25"],
    ["warm", "dot-warn", "Near its cap · $19.50 of $25"],
    ["capped", "dot-bad", "Cap reached · $25 of $25"],
    ["held", "dot-off", "Paused on the runner · $3 of $25"],
  ];
  for (const [name, dot, line] of cases) {
    const row = sideRow(page, name);
    await row.waitFor();
    assert.equal(await row.getAttribute("href"), "#fleet/cloud/" + name);
    assert.equal((await row.locator(".side-host-line").textContent()).trim(), line);
    assert.equal(await row.locator(".dot").getAttribute("class"), "dot " + dot);
  }
});

test("the spend meter warns at warnAt and turns red at the cap", async (t) => {
  const { page, state } = await cloudPage(t, [host("modal", { running: 2, budget: budget(3.12, 25, { runningUSD: 0.04, creditUSD: 30 }) })], "#fleet/cloud/modal");
  await page.getByRole("heading", { name: "modal", exact: true }).waitFor();
  const spend = page.getByRole("meter", { name: "Spend this month" });
  assert.equal(await spend.getAttribute("class"), "bar");
  await page.getByText("$3.16 of $25.00", { exact: true }).waitFor();
  await page.getByText("$0.04 of it is jobs still running · $30.00 of free credit covers it so far", { exact: true }).waitFor();
  await page.getByText("2 of 4", { exact: true }).waitFor();
  await page.getByText("2,412 of 100,000", { exact: true }).waitFor();
  assert.equal(await page.title(), "modal · agent-box");

  const c = state.cloud[0];
  c.budget = budget(18.75, 25);
  await event(page, "cloud", { cloud: [c], cloudPaused: false });
  assert.equal(await spend.getAttribute("class"), "bar hot");
  c.budget = budget(25.4, 25);
  await event(page, "cloud", { cloud: [c], cloudPaused: false });
  assert.equal(await spend.getAttribute("class"), "bar full");
  await page.locator(".badge-bad").getByText("Cap reached").waitFor();
  // An event without the controller takes its meter away.
  assert.equal(await page.getByRole("meter", { name: "Controller requests" }).count(), 0);
});

test("Pause and Resume post the page's cloud pause and show it", async (t) => {
  const { page, state } = await cloudPage(t, [host("modal")], "#fleet/cloud/modal");
  const posts = [];
  await page.route("**/api/cloud/pause", async (route) => {
    const body = route.request().postDataJSON();
    posts.push(body.paused);
    assert.equal(route.request().headers()["x-agent-box"], "1");
    await route.fulfill({ json: { cloud: state.cloud, cloudPaused: body.paused, controller: state.controller } });
  });
  await page.getByText("box cloud stop", { exact: true }).first().waitFor();
  await page.getByRole("button", { name: "Pause cloud launches" }).click();
  await page.getByRole("button", { name: "Resume cloud launches" }).waitFor();
  await page.getByText("Cloud launches are paused from this page.").waitFor();
  assert.equal((await sideRow(page, "modal").locator(".side-host-line").textContent()).trim(), "Paused · $3 of $25");
  await page.getByRole("button", { name: "Resume cloud launches" }).click();
  await page.getByRole("button", { name: "Pause cloud launches" }).waitFor();
  assert.deepEqual(posts, [true, false]);
  assert.equal(await page.locator(".banner").count(), 0);
});

test("a failed pause says why and keeps the button", async (t) => {
  const { page } = await cloudPage(t, [host("modal")], "#fleet/cloud/modal");
  await page.route("**/api/cloud/pause", (route) => route.fulfill({ status: 502, json: { error: "boxd is restarting", code: "incus" } }));
  await page.getByRole("button", { name: "Pause cloud launches" }).click();
  await page.getByRole("alert").getByText("boxd is restarting").waitFor();
  await page.getByRole("button", { name: "Pause cloud launches" }).waitFor();
});

test("a cloud host nobody reported has a page that says so", async (t) => {
  const { page } = await cloudPage(t, [], "#fleet/cloud/ghost");
  await page.getByText("No such cloud host", { exact: true }).waitFor();
  assert.equal(await page.locator('[data-key^="host-cloud-"]').count(), 0);
});
