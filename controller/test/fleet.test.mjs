import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Fleet, DAILY_LIMIT } from "../src/fleet.js";
import { MAX_BODY_BYTES } from "../src/http.js";
import { createMockStorage } from "./sqlshim.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const FIXTURES = path.join(ROOT, "docs", "protocol", "controller-v1");

function readFixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURES, name), "utf8"));
}

function makeFleet(initialIso = "2026-11-27T15:50:00Z") {
  let now = new Date(initialIso);
  const storage = createMockStorage();
  const fleet = new Fleet({ storage }, {}, { now: () => now });
  return {
    fleet,
    setNow(iso) {
      now = new Date(iso);
    },
    close() {
      storage.close();
    },
  };
}

function putReq(id, body, headers = {}) {
  return new Request(`https://controller.test/v1/hosts/${id}/state`, {
    method: "PUT",
    headers: { "content-type": "application/json", "x-controller-role": "host", ...headers },
    body,
  });
}

function clientReq(pathname, headers = {}) {
  return new Request(`https://controller.test${pathname}`, {
    headers: { "x-controller-role": "client", ...headers },
  });
}

test("PUT /v1/hosts/{id}/state and GET /v1/hosts store valid state and track staleness (>180s)", async () => {
  const { fleet, setNow, close } = makeFleet("2026-11-27T15:50:00Z");
  try {
    const stateObj = { machines: [{ id: "pc", arr: [1, 2, 3] }], boxes: [], jobs: [], now: "2026-11-27T15:50:00Z" };
    const rawBody = JSON.stringify({
      id: "pc",
      name: "Gaming PC",
      seq: 42,
      sentAt: "2026-11-27T15:50:00Z",
      state: stateObj,
    });

    const putRes = await fleet.fetch(putReq("pc", rawBody));
    assert.equal(putRes.status, 204);
    assert.equal(await putRes.text(), "");

    // Exactly 180s later: not stale (strict > 180s)
    setNow("2026-11-27T15:53:00Z");
    const res180 = await fleet.fetch(clientReq("/v1/hosts"));
    assert.equal(res180.status, 200);
    const body180 = await res180.json();
    assert.equal(body180.now, "2026-11-27T15:53:00Z");
    assert.equal(body180.hosts.length, 1);
    assert.equal(body180.hosts[0].id, "pc");
    assert.equal(body180.hosts[0].name, "Gaming PC");
    assert.equal(body180.hosts[0].seenAt, "2026-11-27T15:50:00Z");
    assert.equal(body180.hosts[0].stale, false);
    assert.deepEqual(body180.hosts[0].state, stateObj);

    // 181s later: stale is true
    setNow("2026-11-27T15:53:01Z");
    const res181 = await fleet.fetch(clientReq("/v1/hosts"));
    const body181 = await res181.json();
    assert.equal(body181.hosts[0].stale, true);

    // Process restart with lower seq updates cleanly
    const restartBody = JSON.stringify({
      id: "pc",
      name: "Gaming PC",
      seq: 1,
      sentAt: "2026-11-27T15:53:01Z",
      state: { machines: [], boxes: [], jobs: [], now: "2026-11-27T15:53:01Z" },
    });
    const restartRes = await fleet.fetch(putReq("pc", restartBody));
    assert.equal(restartRes.status, 204);
    const afterRestart = await (await fleet.fetch(clientReq("/v1/hosts"))).json();
    assert.equal(afterRestart.hosts[0].stale, false);
    assert.equal(afterRestart.hosts[0].seenAt, "2026-11-27T15:53:01Z");
  } finally {
    close();
  }
});

test("Fleet matches golden controller-v1 fixtures (put-state.json, hosts.json, usage.json)", async () => {
  const { fleet, setNow, close } = makeFleet("2026-11-27T15:50:00Z");
  try {
    const putFixtureRaw = fs.readFileSync(path.join(FIXTURES, "put-state.json"), "utf8");
    const hostsFixture = readFixture("hosts.json");
    const usageFixture = readFixture("usage.json");

    const resPc = await fleet.fetch(putReq("pc", putFixtureRaw));
    assert.equal(resPc.status, 204);

    setNow("2026-11-27T15:45:00Z");
    const labHost = hostsFixture.hosts.find((h) => h.id === "lab-linux");
    const resLab = await fleet.fetch(
      putReq("lab-linux", JSON.stringify({
        id: "lab-linux",
        name: labHost.name,
        seq: 1,
        sentAt: "2026-11-27T15:45:00Z",
        state: labHost.state,
      }))
    );
    assert.equal(resLab.status, 204);

    setNow(hostsFixture.now);
    const hostsRes = await fleet.fetch(clientReq("/v1/hosts"));
    assert.equal(hostsRes.status, 200);
    const hostsJson = await hostsRes.json();
    assert.equal(hostsJson.now, hostsFixture.now);
    assert.equal(hostsJson.hosts.length, 2);

    const gotPc = hostsJson.hosts.find((h) => h.id === "pc");
    const gotLab = hostsJson.hosts.find((h) => h.id === "lab-linux");
    assert.equal(gotPc.stale, false);
    assert.equal(gotPc.seenAt, "2026-11-27T15:50:00Z");
    assert.deepEqual(gotPc.state, JSON.parse(putFixtureRaw).state);
    assert.equal(gotLab.stale, true);
    assert.deepEqual(gotLab.state, labHost.state);

    const usageRes = await fleet.fetch(clientReq("/v1/usage"));
    assert.equal(usageRes.status, 200);
    const usageJson = await usageRes.json();
    assert.deepEqual(Object.keys(usageJson).sort(), Object.keys(usageFixture).sort());
    assert.equal(usageJson.day, "2026-11-27");
    assert.equal(usageJson.requests, 4);
    assert.equal(usageJson.limit, DAILY_LIMIT);
  } finally {
    close();
  }
});

test("Fleet enforces 256 KiB limit (413), invalid id / envelope (400), and UTC day rollover", async () => {
  const { fleet, setNow, close } = makeFleet("2026-11-27T23:59:50Z");
  try {
    const tooLargeFixture = readFixture("error-too-large.json");

    // Bad host id
    const badIdRes = await fleet.fetch(putReq("Bad_ID", "{}"));
    assert.equal(badIdRes.status, 400);
    assert.equal((await badIdRes.json()).error, "bad_request");

    // Envelope id mismatch
    const mismatchRes = await fleet.fetch(
      putReq("pc", JSON.stringify({
        id: "other",
        name: "Other",
        seq: 1,
        sentAt: "2026-11-27T23:59:50Z",
        state: {},
      }))
    );
    assert.equal(mismatchRes.status, 400);

    // Malformed JSON / truncated state
    const badJsonRes = await fleet.fetch(putReq("pc", '{"id":"pc","name":"PC","seq":1,"sentAt":"2026-11-27T23:59:50Z","state":{"a":'));
    assert.equal(badJsonRes.status, 400);

    // Exactly 262144 bytes succeeds
    const prefix = '{"id":"pc","name":"PC","seq":1,"sentAt":"2026-11-27T23:59:50Z","state":{"pad":"';
    const suffix = '"}}';
    const exactPad = "a".repeat(MAX_BODY_BYTES - prefix.length - suffix.length);
    const exactRes = await fleet.fetch(putReq("pc", prefix + exactPad + suffix));
    assert.equal(exactRes.status, 204);

    // 262145 bytes via streamed body (no Content-Length) returns 413
    const overStream = new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(prefix + exactPad + "a" + suffix));
        controller.close();
      },
    });
    const overRes = await fleet.fetch(
      new Request("https://controller.test/v1/hosts/pc/state", {
        method: "PUT",
        headers: { "x-controller-role": "host" },
        body: overStream,
        duplex: "half",
      })
    );
    assert.equal(overRes.status, 413);
    assert.deepEqual(await overRes.json(), tooLargeFixture);

    // Rollover to next UTC day starts a fresh usage counter
    setNow("2026-11-28T00:00:05Z");
    const nextDayUsage = await (await fleet.fetch(clientReq("/v1/usage"))).json();
    assert.deepEqual(nextDayUsage, { day: "2026-11-28", requests: 1, limit: DAILY_LIMIT });
  } finally {
    close();
  }
});

test("Fleet rejects malformed state, non-object state, unsafe seq, non-RFC3339 sentAt, and missing role without poisoning GET /v1/hosts", async () => {
  const { fleet, close } = makeFleet("2026-11-27T15:50:00Z");
  try {
    // Seed a valid host first
    const validState = { machines: [], boxes: [], jobs: [], now: "2026-11-27T15:50:00Z" };
    const seedRes = await fleet.fetch(
      putReq("pc", JSON.stringify({
        id: "pc",
        name: "Gaming PC",
        seq: 1,
        sentAt: "2026-11-27T15:50:00Z",
        state: validState,
      }))
    );
    assert.equal(seedRes.status, 204);

    // Missing or mismatched x-controller-role fails closed with 403
    for (const req of [
      new Request("https://controller.test/v1/hosts/pc/state", { method: "PUT", body: "{}" }),
      new Request("https://controller.test/v1/hosts/pc/state", {
        method: "PUT",
        headers: { "x-controller-role": "client" },
        body: "{}",
      }),
      new Request("https://controller.test/v1/hosts"),
      new Request("https://controller.test/v1/hosts", { headers: { "x-controller-role": "host" } }),
      new Request("https://controller.test/v1/usage"),
      new Request("https://controller.test/v1/usage", { headers: { "x-controller-role": "host" } }),
    ]) {
      const res = await fleet.fetch(req);
      assert.equal(res.status, 403);
      assert.equal((await res.json()).error, "forbidden");
    }

    // Invalid JSON inside state must be rejected with 400
    const badPayloads = [
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27T15:50:00Z","state":{x}}',
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27T15:50:00Z","state":{"a":1 "b":2}}',
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27T15:50:00Z","state":{"a":NaN}}',
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27T15:50:00Z","state":{"a":"\\u"}}',
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27T15:50:00Z","state":{"a":"raw\ncontrol"}}',
      // Non-object state
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27T15:50:00Z","state":null}',
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27T15:50:00Z","state":[]}',
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27T15:50:00Z","state":123}',
      // Unsafe or invalid seq
      '{"id":"pc","name":"PC","seq":1e300,"sentAt":"2026-11-27T15:50:00Z","state":{}}',
      '{"id":"pc","name":"PC","seq":1.5,"sentAt":"2026-11-27T15:50:00Z","state":{}}',
      '{"id":"pc","name":"PC","seq":-1,"sentAt":"2026-11-27T15:50:00Z","state":{}}',
      // Non-RFC3339 sentAt
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-11-27","state":{}}',
      '{"id":"pc","name":"PC","seq":2,"sentAt":"Nov 27, 2026","state":{}}',
      '{"id":"pc","name":"PC","seq":2,"sentAt":"2026-99-99T00:00:00Z","state":{}}',
    ];

    for (const payload of badPayloads) {
      const res = await fleet.fetch(putReq("pc", payload));
      assert.equal(res.status, 400, `expected 400 for payload: ${payload}`);
      assert.equal((await res.json()).error, "bad_request");
    }

    // GET /v1/hosts still returns valid JSON with the original seeded snapshot
    const hostsRes = await fleet.fetch(clientReq("/v1/hosts"));
    assert.equal(hostsRes.status, 200);
    const hostsJson = await hostsRes.json();
    assert.equal(hostsJson.hosts.length, 1);
    assert.deepEqual(hostsJson.hosts[0].state, validState);
  } finally {
    close();
  }
});
