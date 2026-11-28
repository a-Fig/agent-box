import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const CONTROLLER = path.join(ROOT, "controller");

function stripJsoncComments(raw) {
  return raw.replace(/^\s*\/\/.*$/gm, "");
}

test("controller/wrangler.jsonc configures Worker + SQLite Fleet DO with no D1, KV, or R2", () => {
  const wrangler = JSON.parse(
    stripJsoncComments(fs.readFileSync(path.join(CONTROLLER, "wrangler.jsonc"), "utf8"))
  );
  const siteWrangler = JSON.parse(
    stripJsoncComments(fs.readFileSync(path.join(ROOT, "site", "wrangler.jsonc"), "utf8"))
  );

  assert.equal(wrangler.main, "src/index.js");
  assert.equal(wrangler.workers_dev, true);
  assert.equal(wrangler.compatibility_date, siteWrangler.compatibility_date);
  assert.equal(wrangler.account_id, undefined, "must not hardcode account_id");

  assert.equal(wrangler.d1_databases, undefined, "must not bind D1 in v1");
  assert.equal(wrangler.kv_namespaces, undefined, "must not bind KV in v1");
  assert.equal(wrangler.r2_buckets, undefined, "must not bind R2 in v1");

  assert.deepEqual(wrangler.durable_objects?.bindings, [
    { name: "FLEET", class_name: "Fleet" },
  ]);
  assert.deepEqual(wrangler.migrations, [
    { tag: "v1", new_sqlite_classes: ["Fleet"] },
  ]);
});

test("controller/package.json matches ESM and wrangler version conventions", () => {
  const pkg = JSON.parse(fs.readFileSync(path.join(CONTROLLER, "package.json"), "utf8"));
  const sitePkg = JSON.parse(fs.readFileSync(path.join(ROOT, "site", "package.json"), "utf8"));

  assert.equal(pkg.type, "module");
  assert.equal(pkg.devDependencies?.wrangler, sitePkg.devDependencies?.wrangler);
  assert.equal(pkg.dependencies, undefined, "must have zero runtime dependencies");
});
