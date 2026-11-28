import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import worker, { Fleet, constantTimeEqual } from "../src/index.js";
import { MAX_BODY_BYTES } from "../src/http.js";
import { createMockStorage } from "./sqlshim.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const FIXTURES = path.join(ROOT, "docs", "protocol", "controller-v1");

function readFixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURES, name), "utf8"));
}

function makeEnv({ hostToken = "host-secret-token", clientToken = "client-secret-token", now = "2026-11-27T15:52:00Z" } = {}) {
  const storage = createMockStorage();
  const fleet = new Fleet({ storage }, {}, { now: () => new Date(now) });
  const receivedBodies = [];
  const env = {
    HOST_TOKEN: hostToken,
    CLIENT_TOKEN: clientToken,
    FLEET: {
      idFromName(name) {
        return name;
      },
      get() {
        return {
          async fetch(req) {
            const clone = req.clone();
            if (req.method === "PUT") {
              receivedBodies.push({
                role: req.headers.get("x-controller-role"),
                body: await clone.text(),
              });
            }
            return fleet.fetch(req);
          },
        };
      },
    },
  };
  return { env, receivedBodies, close: () => storage.close() };
}

test("constantTimeEqual handles UTF-8 lengths and rejects empty/missing secrets fail-closed", () => {
  assert.equal(constantTimeEqual("abc", "abc"), true);
  assert.equal(constantTimeEqual("abc", "abd"), false);
  assert.equal(constantTimeEqual("abc", "ab"), false);
  assert.equal(constantTimeEqual("tök€n", "tök€n"), true);
  assert.equal(constantTimeEqual("", ""), false);
  assert.equal(constantTimeEqual("abc", ""), false);
  assert.equal(constantTimeEqual("abc", undefined), false);
});

test("GET /v1/health requires no auth and matches health.json fixture", async () => {
  const { env, close } = makeEnv();
  try {
    const res = await worker.fetch(new Request("https://ctl.example/v1/health"), env);
    assert.equal(res.status, 200);
    assert.deepEqual(await res.json(), readFixture("health.json"));
  } finally {
    close();
  }
});

test("Worker enforces bearer auth (401/403) and fails closed on missing secrets", async () => {
  const { env, close } = makeEnv();
  const unauthorizedFixture = readFixture("error-unauthorized.json");
  try {
    // Missing auth
    const noAuth = await worker.fetch(new Request("https://ctl.example/v1/hosts"), env);
    assert.equal(noAuth.status, 401);
    assert.deepEqual(await noAuth.json(), unauthorizedFixture);

    // Wrong token
    const badAuth = await worker.fetch(
      new Request("https://ctl.example/v1/usage", { headers: { authorization: "Bearer nope" } }),
      env
    );
    assert.equal(badAuth.status, 401);
    assert.deepEqual(await badAuth.json(), unauthorizedFixture);

    // Cross-role 403: client token on host write
    const clientOnPut = await worker.fetch(
      new Request("https://ctl.example/v1/hosts/pc/state", {
        method: "PUT",
        headers: { authorization: `Bearer ${env.CLIENT_TOKEN}` },
        body: "{}",
      }),
      env
    );
    assert.equal(clientOnPut.status, 403);
    assert.equal((await clientOnPut.json()).error, "forbidden");

    // Cross-role 403: host token on client reads
    for (const p of ["/v1/hosts", "/v1/usage"]) {
      const hostOnGet = await worker.fetch(
        new Request(`https://ctl.example${p}`, {
          headers: { authorization: `Bearer ${env.HOST_TOKEN}` },
        }),
        env
      );
      assert.equal(hostOnGet.status, 403);
      assert.equal((await hostOnGet.json()).error, "forbidden");
    }

    // Missing/empty env secrets fail closed
    const emptyEnv = makeEnv({ hostToken: "", clientToken: undefined });
    try {
      const emptySecretRes = await worker.fetch(
        new Request("https://ctl.example/v1/hosts", { headers: { authorization: "Bearer " } }),
        emptyEnv.env
      );
      assert.equal(emptySecretRes.status, 401);
    } finally {
      emptyEnv.close();
    }
  } finally {
    close();
  }
});

test("Worker validates routes (404), methods (405), host IDs (400), and 256 KiB body limit (413)", async () => {
  const { env, close } = makeEnv();
  const tooLargeFixture = readFixture("error-too-large.json");
  try {
    const notFound = await worker.fetch(new Request("https://ctl.example/v1/nope"), env);
    assert.equal(notFound.status, 404);
    assert.equal((await notFound.json()).error, "not_found");

    const badMethodHealth = await worker.fetch(
      new Request("https://ctl.example/v1/health", { method: "POST" }),
      env
    );
    assert.equal(badMethodHealth.status, 405);
    assert.equal(badMethodHealth.headers.get("allow"), "GET");

    const badMethodHosts = await worker.fetch(
      new Request("https://ctl.example/v1/hosts", {
        method: "POST",
        headers: { authorization: `Bearer ${env.CLIENT_TOKEN}` },
      }),
      env
    );
    assert.equal(badMethodHosts.status, 405);

    const badId = await worker.fetch(
      new Request(`https://ctl.example/v1/hosts/${"a".repeat(41)}/state`, {
        method: "PUT",
        headers: { authorization: `Bearer ${env.HOST_TOKEN}` },
        body: "{}",
      }),
      env
    );
    assert.equal(badId.status, 400);
    assert.equal((await badId.json()).error, "bad_request");

    // 413 via Content-Length header
    const clTooLarge = await worker.fetch(
      new Request("https://ctl.example/v1/hosts/pc/state", {
        method: "PUT",
        headers: {
          authorization: `Bearer ${env.HOST_TOKEN}`,
          "content-length": String(MAX_BODY_BYTES + 1),
        },
        body: "{}",
      }),
      env
    );
    assert.equal(clTooLarge.status, 413);
    assert.deepEqual(await clTooLarge.json(), tooLargeFixture);

    // 413 via chunked stream exceeding 262144 bytes without Content-Length
    const bigStream = new ReadableStream({
      start(controller) {
        controller.enqueue(new Uint8Array(131072).fill(0x20));
        controller.enqueue(new Uint8Array(131073).fill(0x20));
        controller.close();
      },
    });
    const streamTooLarge = await worker.fetch(
      new Request("https://ctl.example/v1/hosts/pc/state", {
        method: "PUT",
        headers: { authorization: `Bearer ${env.HOST_TOKEN}` },
        body: bigStream,
        duplex: "half",
      }),
      env
    );
    assert.equal(streamTooLarge.status, 413);
    assert.deepEqual(await streamTooLarge.json(), tooLargeFixture);
  } finally {
    close();
  }
});

test("Worker passes raw body verbatim to DO and serves PUT -> GET /v1/hosts -> GET /v1/usage", async () => {
  const { env, receivedBodies, close } = makeEnv({ now: "2026-11-27T15:52:00Z" });
  try {
    const putRaw = fs.readFileSync(path.join(FIXTURES, "put-state.json"), "utf8");

    const putRes = await worker.fetch(
      new Request("https://ctl.example/v1/hosts/pc/state", {
        method: "PUT",
        headers: {
          authorization: `Bearer ${env.HOST_TOKEN}`,
          "content-type": "application/json",
          "x-controller-role": "client", // untrusted client header must be overwritten
        },
        body: putRaw,
      }),
      env
    );
    assert.equal(putRes.status, 204);
    assert.equal(receivedBodies.length, 1);
    assert.equal(receivedBodies[0].role, "host");
    assert.equal(receivedBodies[0].body, putRaw);

    const hostsRes = await worker.fetch(
      new Request("https://ctl.example/v1/hosts", {
        headers: { authorization: `Bearer ${env.CLIENT_TOKEN}` },
      }),
      env
    );
    assert.equal(hostsRes.status, 200);
    const hosts = await hostsRes.json();
    assert.equal(hosts.now, "2026-11-27T15:52:00Z");
    assert.equal(hosts.hosts.length, 1);
    assert.equal(hosts.hosts[0].id, "pc");
    assert.equal(hosts.hosts[0].stale, false);
    assert.deepEqual(hosts.hosts[0].state, JSON.parse(putRaw).state);

    const usageRes = await worker.fetch(
      new Request("https://ctl.example/v1/usage", {
        headers: { authorization: `Bearer ${env.CLIENT_TOKEN}` },
      }),
      env
    );
    assert.equal(usageRes.status, 200);
    assert.deepEqual(await usageRes.json(), {
      day: "2026-11-27",
      requests: 3,
      limit: 100000,
    });
  } finally {
    close();
  }
});
