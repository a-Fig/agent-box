import test from "node:test";
import assert from "node:assert/strict";
import worker, { Fleet, verifyGitHubSignature } from "../src/index.js";
import { GitHubApp, base64url, importPkcs8Pem } from "../src/github.js";
import { MAX_BODY_BYTES } from "../src/http.js";
import { createMockStorage } from "./sqlshim.mjs";

const encoder = new TextEncoder();
const REPO = "example-org/widgets";
const SHA = "a".repeat(40);
const SHA2 = "b".repeat(40);
const INSTALLATION = 12345;
const WEBHOOK_SECRET = "webhook-test-secret";

// Ephemeral RSA key pair for RS256: generated per test run, never a real key.
const rsa = await crypto.subtle.generateKey(
  { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
  true,
  ["sign", "verify"]
);
const pkcs8 = new Uint8Array(await crypto.subtle.exportKey("pkcs8", rsa.privateKey));
const PEM = `-----BEGIN PRIVATE KEY-----\n${btoa(String.fromCharCode(...pkcs8)).replace(/(.{64})/g, "$1\n")}\n-----END PRIVATE KEY-----\n`;

function b64urlDecode(s) {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - (s.length % 4)) % 4);
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
}

/** Fake api.github.com: records every call and hands out tokens and check run IDs. */
function makeFakeGitHub(clock) {
  const calls = [];
  let tokens = 0;
  let checkRuns = 0;
  const state = { hold: null }; // a promise that, while set, stalls Check Run creation
  async function fakeFetch(url, init = {}) {
    const u = new URL(url);
    const call = {
      method: init.method ?? "GET",
      path: u.pathname,
      headers: new Headers(init.headers),
      body: init.body ? JSON.parse(init.body) : undefined,
    };
    calls.push(call);
    const json = (status, payload) =>
      new Response(JSON.stringify(payload), { status, headers: { "content-type": "application/json" } });
    if (call.method === "POST" && /^\/app\/installations\/\d+\/access_tokens$/.test(call.path)) {
      tokens++;
      return json(201, { token: `ghs_test_${tokens}`, expires_at: new Date(clock.now.getTime() + 3600_000).toISOString() });
    }
    if (call.method === "POST" && /^\/repos\/[^/]+\/[^/]+\/check-runs$/.test(call.path)) {
      if (state.hold) await state.hold;
      return json(201, { id: 1000 + ++checkRuns });
    }
    const m = /^\/repos\/[^/]+\/[^/]+\/check-runs\/(\d+)$/.exec(call.path);
    if (call.method === "PATCH" && m) return json(200, { id: Number(m[1]) });
    return json(404, { message: "Not Found" });
  }
  return {
    fetch: fakeFetch,
    calls,
    state,
    checkRunCalls: () => calls.filter((c) => c.path.includes("/check-runs")),
    tokenCalls: () => calls.filter((c) => c.path.endsWith("/access_tokens")),
  };
}

function makeHarness({ webhookSecret = WEBHOOK_SECRET, githubApp = true } = {}) {
  const clock = { now: new Date("2026-11-27T16:00:00Z") };
  const gh = makeFakeGitHub(clock);
  const storage = createMockStorage();
  const env = {
    HOST_TOKEN: "host-secret-token",
    CLIENT_TOKEN: "client-secret-token",
    GITHUB_WEBHOOK_SECRET: webhookSecret,
    ...(githubApp ? { GITHUB_APP_ID: "4242", GITHUB_APP_PRIVATE_KEY: PEM } : {}),
  };
  const fleet = new Fleet({ storage }, env, { now: () => clock.now, fetch: gh.fetch });
  env.FLEET = { idFromName: (n) => n, get: () => ({ fetch: (req) => fleet.fetch(req) }) };
  return { env, gh, clock, close: () => storage.close() };
}

async function hmacHex(secret, raw) {
  const key = await crypto.subtle.importKey("raw", encoder.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = new Uint8Array(await crypto.subtle.sign("HMAC", key, encoder.encode(raw)));
  return [...sig].map((b) => b.toString(16).padStart(2, "0")).join("");
}

let deliveryCounter = 0;
async function deliver(h, event, payload, { delivery, secret = WEBHOOK_SECRET, signature, raw } = {}) {
  const body = raw ?? JSON.stringify(payload);
  const headers = {
    "content-type": "application/json",
    "x-github-event": event,
    "x-github-delivery": delivery ?? `delivery-${++deliveryCounter}`,
  };
  const sig = signature === undefined ? `sha256=${await hmacHex(secret, body)}` : signature;
  if (sig !== null) headers["x-hub-signature-256"] = sig;
  return worker.fetch(new Request("https://ctl.example/v1/github/webhook", { method: "POST", headers, body }), h.env);
}

function api(h, method, pathname, { token = h.env.HOST_TOKEN, body } = {}) {
  return worker.fetch(
    new Request(`https://ctl.example${pathname}`, {
      method,
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    }),
    h.env
  );
}

async function putConfig(h, overrides = {}) {
  const res = await api(h, "PUT", "/v1/ci/config", {
    body: { repo: REPO, authors: ["alice"], allowPublic: false, cloudOnlyForForks: false, installationId: INSTALLATION, ...overrides },
  });
  assert.equal(res.status, 200);
  return res.json();
}

function prPayload({ action = "opened", sender = "alice", author = "alice", headRepo = REPO, isPrivate = true, sha = SHA, number = 7 } = {}) {
  return {
    action,
    number,
    repository: { full_name: REPO, private: isPrivate },
    sender: { login: sender },
    pull_request: {
      number,
      user: { login: author },
      head: { sha, repo: headRepo === null ? null : { full_name: headRepo } },
    },
  };
}

function pushPayload({ sender = "alice", after = SHA, deleted = false, isPrivate = true, repo = REPO } = {}) {
  return { ref: "refs/heads/main", after, deleted, repository: { full_name: repo, private: isPrivate }, sender: { login: sender } };
}

const next = (h, hostClass, workerId = "pc") => api(h, "GET", `/v1/ci/next?worker=${workerId}&hostClass=${hostClass}`);

test("webhook verifies X-Hub-Signature-256 over raw bytes with WebCrypto and fails closed", async () => {
  const h = makeHarness();
  try {
    await putConfig(h);
    const ok = await deliver(h, "ping", { zen: "hi", repository: { full_name: REPO } });
    assert.equal(ok.status, 200);
    assert.deepEqual(await ok.json(), { ok: true, ignored: true, reason: "event ping not handled" });

    // Wrong secret, tampered body, missing and malformed headers are all 401.
    const wrong = await deliver(h, "push", pushPayload(), { secret: "not-the-secret" });
    assert.equal(wrong.status, 401);
    const raw = JSON.stringify(pushPayload());
    const tampered = await deliver(h, "push", null, {
      raw: raw.replace("alice", "mallory"),
      signature: `sha256=${await hmacHex(WEBHOOK_SECRET, raw)}`,
    });
    assert.equal(tampered.status, 401);
    assert.equal((await deliver(h, "push", pushPayload(), { signature: null })).status, 401);
    assert.equal((await deliver(h, "push", pushPayload(), { signature: "sha1=abcd" })).status, 401);
    assert.equal((await deliver(h, "push", pushPayload(), { signature: `sha256=${"z".repeat(64)}` })).status, 401);
    assert.equal(h.gh.calls.length, 0);

    assert.equal(await verifyGitHubSignature("", encoder.encode(raw), `sha256=${"0".repeat(64)}`), false);
    assert.equal(await verifyGitHubSignature(WEBHOOK_SECRET, encoder.encode(raw), `sha256=${await hmacHex(WEBHOOK_SECRET, raw)}`), true);
  } finally {
    h.close();
  }

  for (const secret of ["", undefined]) {
    const h2 = makeHarness({ webhookSecret: secret });
    try {
      // No secret means no signature can be valid, whatever the caller sends.
      const res = await deliver(h2, "push", pushPayload(), { secret: "any-guess" });
      assert.equal(res.status, 401, `secret ${JSON.stringify(secret)} must fail closed`);
      assert.equal((await res.json()).error, "unauthorized");
    } finally {
      h2.close();
    }
  }
});

test("webhook enforces 256 KiB body limit and deduplicates X-GitHub-Delivery", async () => {
  const h = makeHarness();
  try {
    await putConfig(h);
    const big = await deliver(h, "push", null, { raw: " ".repeat(MAX_BODY_BYTES + 1) });
    assert.equal(big.status, 413);

    const first = await deliver(h, "push", pushPayload(), { delivery: "dup-1" });
    assert.equal(first.status, 200);
    assert.equal((await first.json()).queued, true);
    const replay = await deliver(h, "push", pushPayload(), { delivery: "dup-1" });
    assert.equal(replay.status, 200);
    assert.deepEqual(await replay.json(), { ok: true, duplicate: true });

    const missing = await worker.fetch(
      new Request("https://ctl.example/v1/github/webhook", {
        method: "POST",
        headers: { "x-github-event": "push", "x-hub-signature-256": `sha256=${await hmacHex(WEBHOOK_SECRET, "{}")}` },
        body: "{}",
      }),
      h.env
    );
    assert.equal(missing.status, 400);
  } finally {
    h.close();
  }
});

test("CI config needs the host token, rejects the client token, and validates the shape", async () => {
  const h = makeHarness();
  try {
    const cfg = { repo: REPO, authors: ["alice"], allowPublic: false, cloudOnlyForForks: true, installationId: INSTALLATION };
    assert.equal((await api(h, "PUT", "/v1/ci/config", { token: h.env.CLIENT_TOKEN, body: cfg })).status, 403);
    assert.equal((await api(h, "GET", "/v1/ci/config", { token: h.env.CLIENT_TOKEN })).status, 403);
    assert.equal((await api(h, "GET", "/v1/ci/config", { token: "nope" })).status, 401);
    assert.equal((await api(h, "GET", "/v1/ci/next?worker=pc&hostClass=home", { token: h.env.CLIENT_TOKEN })).status, 403);
    assert.equal((await api(h, "PATCH", `/v1/ci/jobs/${crypto.randomUUID()}`, { token: h.env.CLIENT_TOKEN, body: {} })).status, 403);
    assert.equal((await api(h, "DELETE", "/v1/ci/config")).headers.get("allow"), "GET, PUT");

    for (const bad of [
      { ...cfg, repo: "no-slash" },
      { ...cfg, authors: "alice" },
      { ...cfg, authors: ["bad login!"] },
      { ...cfg, allowPublic: "no" },
      { ...cfg, installationId: 0 },
      { ...cfg, cloudOnlyForForks: undefined },
    ]) {
      assert.equal((await api(h, "PUT", "/v1/ci/config", { body: bad })).status, 400, JSON.stringify(bad));
    }

    const put = await api(h, "PUT", "/v1/ci/config", { body: cfg });
    assert.equal(put.status, 200);
    const got = await api(h, "GET", "/v1/ci/config");
    assert.equal(got.status, 200);
    assert.deepEqual(await got.json(), { repos: [cfg] });
  } finally {
    h.close();
  }
});

test("Guard 1: an unallowlisted repo is ignored without a Check Run or job", async () => {
  const h = makeHarness();
  try {
    await putConfig(h);
    const res = await deliver(h, "push", pushPayload({ repo: "example-org/other" }));
    assert.equal(res.status, 200);
    assert.deepEqual(await res.json(), { ok: true, ignored: true, reason: "repo not allowlisted" });
    assert.equal(h.gh.calls.length, 0);
    assert.equal((await next(h, "cloud")).status, 204);
  } finally {
    h.close();
  }
});

test("Guard 2: fork PRs are refused (neutral Check Run) unless cloudOnlyForForks, and then only cloud workers can claim them", async () => {
  const h = makeHarness();
  try {
    await putConfig(h, { cloudOnlyForForks: false });
    for (const headRepo of ["someone-else/widgets", null]) {
      const res = await deliver(h, "pull_request", prPayload({ headRepo }));
      const body = await res.json();
      assert.equal(body.refused, true);
      assert.match(body.reason, /forks never run on home hardware/);
    }
    const neutral = h.gh.checkRunCalls();
    assert.equal(neutral.length, 2);
    for (const c of neutral) {
      assert.equal(c.method, "POST");
      assert.equal(c.path, `/repos/${REPO}/check-runs`);
      assert.equal(c.body.status, "completed");
      assert.equal(c.body.conclusion, "neutral");
      assert.equal(c.body.head_sha, SHA);
      assert.match(c.body.output.summary, /cloudOnlyForForks is false/);
    }
    assert.equal((await next(h, "cloud")).status, 204);

    // A fork that also has a non-author: guard 2 answers first.
    const ordered = await (await deliver(h, "pull_request", prPayload({ headRepo: "x/y", sender: "mallory", author: "mallory" }))).json();
    assert.match(ordered.reason, /forks/);

    await putConfig(h, { cloudOnlyForForks: true });
    const queued = await (await deliver(h, "pull_request", prPayload({ headRepo: "someone-else/widgets" }))).json();
    assert.equal(queued.queued, true);
    assert.equal(queued.job.hostClass, "cloud");

    // A home worker provably cannot claim the cloud-only fork job.
    for (let i = 0; i < 3; i++) assert.equal((await next(h, "home")).status, 204);
    const claim = await next(h, "cloud", "cloud-1");
    assert.equal(claim.status, 200);
    const { job } = await claim.json();
    assert.equal(job.id, queued.job.id);
    assert.equal(job.hostClass, "cloud");
    assert.equal(job.claimedBy, "cloud-1");
    assert.equal(job.pr, 7);
  } finally {
    h.close();
  }
});

test("Guard 3: sender and PR author must both be on the authors allowlist (case-insensitive)", async () => {
  const h = makeHarness();
  try {
    await putConfig(h, { authors: ["Alice", "bob"] });
    const badSender = await (await deliver(h, "pull_request", prPayload({ sender: "mallory", author: "alice" }))).json();
    assert.equal(badSender.refused, true);
    assert.match(badSender.reason, /mallory is not on/);
    const badAuthor = await (await deliver(h, "pull_request", prPayload({ sender: "bob", author: "mallory" }))).json();
    assert.equal(badAuthor.refused, true);
    assert.match(badAuthor.reason, /Pull request author mallory/);
    const badPusher = await (await deliver(h, "push", pushPayload({ sender: "mallory" }))).json();
    assert.equal(badPusher.refused, true);
    assert.equal(h.gh.checkRunCalls().filter((c) => c.body.conclusion === "neutral").length, 3);
    assert.equal((await next(h, "cloud")).status, 204);

    const ok = await (await deliver(h, "pull_request", prPayload({ sender: "ALICE", author: "Bob" }))).json();
    assert.equal(ok.queued, true);
    assert.equal(ok.job.hostClass, "any");
  } finally {
    h.close();
  }
});

test("Guard 4: public repos are refused unless allowPublic", async () => {
  const h = makeHarness();
  try {
    await putConfig(h, { allowPublic: false });
    const refused = await (await deliver(h, "push", pushPayload({ isPrivate: false }))).json();
    assert.equal(refused.refused, true);
    assert.match(refused.reason, /allowPublic is false/);
    assert.equal(h.gh.checkRunCalls().at(-1).body.conclusion, "neutral");
    assert.equal((await next(h, "cloud")).status, 204);

    await putConfig(h, { allowPublic: true });
    const queued = await (await deliver(h, "push", pushPayload({ isPrivate: false }))).json();
    assert.equal(queued.queued, true);
  } finally {
    h.close();
  }
});

test("ignored events: branch deletions, other PR and check_suite actions", async () => {
  const h = makeHarness();
  try {
    await putConfig(h);
    const cases = [
      ["push", pushPayload({ deleted: true })],
      ["push", pushPayload({ after: "0".repeat(40) })],
      ["pull_request", prPayload({ action: "closed" })],
      ["check_suite", { action: "completed", repository: { full_name: REPO, private: true }, check_suite: { head_sha: SHA } }],
      ["issues", { action: "opened", repository: { full_name: REPO, private: true } }],
    ];
    for (const [event, payload] of cases) {
      const body = await (await deliver(h, event, payload)).json();
      assert.equal(body.ignored, true, `${event} ${JSON.stringify(payload).slice(0, 60)}`);
    }
    assert.equal(h.gh.calls.length, 0);
  } finally {
    h.close();
  }
});

test("RS256 JWT is signed with WebCrypto and installation tokens are cached until 5 minutes before expiry", async () => {
  const clock = { now: new Date("2026-11-27T16:00:00Z") };
  const gh = makeFakeGitHub(clock);
  const app = new GitHubApp({ appId: "4242", privateKey: PEM.replace(/\n/g, "\\n"), fetch: gh.fetch, now: () => clock.now });

  const jwt = await app.createJwt();
  const [h64, p64, s64] = jwt.split(".");
  assert.deepEqual(JSON.parse(new TextDecoder().decode(b64urlDecode(h64))), { alg: "RS256", typ: "JWT" });
  const claims = JSON.parse(new TextDecoder().decode(b64urlDecode(p64)));
  const nowSec = Math.floor(clock.now.getTime() / 1000);
  assert.deepEqual(claims, { iat: nowSec - 60, exp: nowSec + 540, iss: 4242 });
  const valid = await crypto.subtle.verify("RSASSA-PKCS1-v1_5", rsa.publicKey, b64urlDecode(s64), encoder.encode(`${h64}.${p64}`));
  assert.equal(valid, true);
  assert.equal(base64url(new Uint8Array([0xfb, 0xff])), "-_8");

  const t1 = await app.installationToken(INSTALLATION);
  assert.equal(t1, "ghs_test_1");
  const tokenCall = gh.tokenCalls()[0];
  assert.equal(tokenCall.path, `/app/installations/${INSTALLATION}/access_tokens`);
  assert.match(tokenCall.headers.get("authorization"), /^Bearer [\w-]+\.[\w-]+\.[\w-]+$/);
  assert.equal(tokenCall.headers.get("user-agent"), "agent-box-controller");

  clock.now = new Date("2026-11-27T16:54:59Z"); // 5m01s before expiry: cached
  assert.equal(await app.installationToken(INSTALLATION), "ghs_test_1");
  clock.now = new Date("2026-11-27T16:55:01Z"); // inside the 5-minute margin: refreshed
  assert.equal(await app.installationToken(INSTALLATION), "ghs_test_2");
  assert.equal(gh.tokenCalls().length, 2);

  await assert.rejects(importPkcs8Pem("-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----"), /PKCS#1/);
});

test("enqueue -> claim -> complete drives the Check Run queued -> in_progress -> completed", async () => {
  const h = makeHarness();
  try {
    await putConfig(h);
    const queued = await (await deliver(h, "pull_request", prPayload())).json();
    assert.equal(queued.queued, true);
    assert.equal(queued.job.checkRunId, 1001);
    const [create] = h.gh.checkRunCalls();
    assert.equal(create.method, "POST");
    assert.deepEqual(create.body, { name: "agent-box", head_sha: SHA, status: "queued", external_id: queued.job.id });
    assert.equal(create.headers.get("authorization"), "Bearer ghs_test_1");

    // The same commit arriving again (push alongside pull_request.synchronize) is not queued twice.
    const again = await (await deliver(h, "push", pushPayload())).json();
    assert.deepEqual(again, { ok: true, queued: false, existing: queued.job.id });

    assert.equal((await next(h, "elsewhere")).status, 400);
    assert.equal((await api(h, "GET", "/v1/ci/next?worker=BAD_ID&hostClass=home")).status, 400);

    const claim = await next(h, "home", "pc");
    assert.equal(claim.status, 200);
    const { job } = await claim.json();
    assert.equal(job.state, "running");
    assert.equal(job.hostClass, "any");
    assert.equal(job.sha, SHA);
    const inProgress = h.gh.checkRunCalls().at(-1);
    assert.equal(inProgress.method, "PATCH");
    assert.equal(inProgress.path, `/repos/${REPO}/check-runs/1001`);
    assert.equal(inProgress.body.status, "in_progress");
    assert.equal((await next(h, "home", "pc")).status, 204);

    const patch = (body, id = job.id) => api(h, "PATCH", `/v1/ci/jobs/${id}`, { body });
    assert.equal((await patch({ worker: "other-pc", state: "done", exitCode: 0 })).status, 403);
    assert.equal((await patch({ worker: "pc", state: "running" })).status, 400);
    assert.equal((await patch({ worker: "pc", state: "done", url: "http://insecure" })).status, 400);
    assert.equal((await patch({ worker: "pc", state: "done" }, crypto.randomUUID())).status, 404);

    const done = await patch({ worker: "pc", state: "done", exitCode: 0, summary: "12 tests passed", url: "https://box.example/jobs/1" });
    assert.equal(done.status, 200);
    const doneJob = (await done.json()).job;
    assert.equal(doneJob.state, "done");
    assert.deepEqual(doneJob.result, { exitCode: 0, summary: "12 tests passed", url: "https://box.example/jobs/1" });
    const completed = h.gh.checkRunCalls().at(-1);
    assert.equal(completed.method, "PATCH");
    assert.equal(completed.body.status, "completed");
    assert.equal(completed.body.conclusion, "success");
    assert.equal(completed.body.details_url, "https://box.example/jobs/1");
    assert.deepEqual(completed.body.output, { title: "Passed", summary: "12 tests passed" });
    assert.equal((await patch({ worker: "pc", state: "failed" })).status, 409);
    assert.equal(h.gh.tokenCalls().length, 1, "installation token is reused");

    // failed -> failure, cancelled -> cancelled
    for (const [state, conclusion, sha] of [["failed", "failure", SHA2], ["cancelled", "cancelled", "c".repeat(40)]]) {
      await deliver(h, "push", pushPayload({ after: sha }));
      const j = (await (await next(h, "cloud", "cloud-1")).json()).job;
      const res = await patch({ worker: "cloud-1", state, exitCode: 2 }, j.id);
      assert.equal(res.status, 200);
      const last = h.gh.checkRunCalls().at(-1);
      assert.equal(last.body.conclusion, conclusion);
    }
    assert.match(h.gh.checkRunCalls().at(-4).body.output.title, /Failed \(exit 2\)/);
  } finally {
    h.close();
  }
});

test("check_suite rerequested re-queues a commit with its earlier classification", async () => {
  const h = makeHarness();
  try {
    await putConfig(h, { cloudOnlyForForks: true });
    const suite = (sha, sender = "alice") => ({
      action: "rerequested",
      repository: { full_name: REPO, private: true },
      sender: { login: sender },
      check_suite: { head_sha: sha, pull_requests: [] },
    });

    const none = await (await deliver(h, "check_suite", suite(SHA2))).json();
    assert.equal(none.refused, true);
    assert.match(none.reason, /no earlier run/);

    const fork = await (await deliver(h, "pull_request", prPayload({ headRepo: "someone-else/widgets" }))).json();
    const j = (await (await next(h, "cloud", "cloud-1")).json()).job;
    assert.equal(j.id, fork.job.id);
    await api(h, "PATCH", `/v1/ci/jobs/${j.id}`, { body: { worker: "cloud-1", state: "failed", exitCode: 1 } });

    assert.equal((await (await deliver(h, "check_suite", suite(SHA, "mallory"))).json()).refused, true);
    const rerun = await (await deliver(h, "check_suite", suite(SHA))).json();
    assert.equal(rerun.queued, true);
    assert.equal(rerun.job.hostClass, "cloud");
    assert.equal((await next(h, "home")).status, 204);

    // Turning cloud runs for forks off also stops re-runs of fork commits.
    await putConfig(h, { cloudOnlyForForks: false });
    const blocked = await (await deliver(h, "check_suite", suite(SHA))).json();
    assert.equal(blocked.refused, true);
    assert.match(blocked.reason, /forks/);
  } finally {
    h.close();
  }
});

test("without GitHub App credentials jobs still queue and no GitHub calls are made", async () => {
  const h = makeHarness({ githubApp: false });
  try {
    await putConfig(h);
    const queued = await (await deliver(h, "push", pushPayload())).json();
    assert.equal(queued.queued, true);
    assert.equal(queued.job.checkRunId, null);
    const refused = await (await deliver(h, "push", pushPayload({ sender: "mallory", after: SHA2 }))).json();
    assert.equal(refused.refused, true);
    assert.equal(refused.checkRunId, null);
    const { job } = await (await next(h, "home")).json();
    const done = await api(h, "PATCH", `/v1/ci/jobs/${job.id}`, { body: { worker: "pc", state: "done", exitCode: 0 } });
    assert.equal(done.status, 200);
    assert.equal(h.gh.calls.length, 0);
  } finally {
    h.close();
  }
});

test("concurrent push + pull_request.synchronize for one commit queue one job and one Check Run", async () => {
  const h = makeHarness();
  try {
    await putConfig(h);
    let release;
    h.gh.state.hold = new Promise((r) => (release = r));
    const deliveries = [
      deliver(h, "push", pushPayload()),
      deliver(h, "pull_request", prPayload({ action: "synchronize" })),
    ];
    while (!h.gh.calls.some((c) => c.method === "POST" && c.path.endsWith("/check-runs"))) {
      await new Promise((r) => setImmediate(r));
    }

    // While the Check Run is still being created, a worker claims and finishes the job.
    const { job } = await (await next(h, "home", "pc")).json();
    assert.equal(job.checkRunId, null);
    const done = await api(h, "PATCH", `/v1/ci/jobs/${job.id}`, { body: { worker: "pc", state: "done", exitCode: 0 } });
    assert.equal(done.status, 200);

    release();
    const bodies = await Promise.all(deliveries.map(async (p) => (await p).json()));
    assert.equal(bodies.filter((b) => b.queued === true).length, 1);
    assert.equal(bodies.filter((b) => b.queued === false && b.existing === job.id).length, 1);

    const runs = h.gh.checkRunCalls();
    assert.equal(runs.filter((c) => c.method === "POST").length, 1, "exactly one Check Run");
    // The Check Run created late is caught up to the job's final state.
    const last = runs.at(-1);
    assert.equal(last.method, "PATCH");
    assert.equal(last.body.status, "completed");
    assert.equal(last.body.conclusion, "success");
    assert.equal((await next(h, "cloud")).status, 204);
  } finally {
    h.close();
  }
});

test("job lookups ignore repo-name case, like ci_config", async () => {
  const h = makeHarness();
  try {
    await putConfig(h);
    const first = await (await deliver(h, "push", pushPayload())).json();
    assert.equal(first.queued, true);
    const shouted = pushPayload({ repo: REPO.toUpperCase() });
    const again = await (await deliver(h, "push", shouted)).json();
    assert.deepEqual(again, { ok: true, queued: false, existing: first.job.id });
  } finally {
    h.close();
  }
});
