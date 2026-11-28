# Fleet Controller (`controller/`)

Cloudflare Worker + SQLite Durable Object implementing the [`controller-v1`](../docs/controller-protocol.md) fleet state cache.

## Layout

- `src/index.js`: Worker entrypoint (`GET /v1/health`, bearer auth, route/ID/body-size validation, raw streaming to `FLEET` Durable Object).
- `src/fleet.js`: `Fleet` SQLite Durable Object (`hosts` and `usage` tables, validates the 256 KiB-capped payload once via `JSON.parse`, stores serialized `state`, and embeds it directly on reads).
- `src/ci.js`: GitHub webhook ingest, the four CI guards, and the CI job queue (`ci_deliveries`, `ci_config`, `ci_jobs` tables in the same Durable Object).
- `src/github.js`: GitHub App client (WebCrypto RS256 JWT, cached installation tokens, Check Runs).
- `wrangler.jsonc`: Worker and `FLEET` SQLite Durable Object migration config (no D1, KV, or R2).

## GitHub CI (#279)

`POST /v1/github/webhook` takes GitHub `push`, `pull_request` (`opened`, `synchronize`, `reopened`) and `check_suite` (`rerequested`) events. It answers `401` unless `X-Hub-Signature-256` matches `GITHUB_WEBHOOK_SECRET` (an unset secret refuses everything), caps bodies at 256 KiB and ignores replayed `X-GitHub-Delivery` IDs. Guards, in order:

1. The repo must have a config, or the event is ignored with no Check Run.
2. Fork PRs never run on home hardware: refused, or with `cloudOnlyForForks: true` queued as `hostClass: "cloud"`.
3. `sender` and the PR author must be in `authors` (case-insensitive).
4. Public repos need `allowPublic: true`.

A refusal by guards 2–4 posts a `neutral` Check Run saying why and queues nothing. A `check_suite` re-run inherits the commit's earlier job (and its fork classification).

Host-token endpoints (the client token gets `403`):

- `GET /v1/ci/config`, `PUT /v1/ci/config` with `{"repo":"owner/name","authors":["owner"],"allowPublic":false,"cloudOnlyForForks":false,"installationId":12345}`.
- `GET /v1/ci/next?worker=<id>&hostClass=home|cloud` claims the oldest queued job (`200 {"job":…}`, or `204`). `home` workers only get `any` jobs; `cloud` workers get `cloud` and `any`. The Check Run moves to `in_progress`.
- `PATCH /v1/ci/jobs/{id}` with `{"worker":"<id>","state":"done|failed|cancelled","exitCode":0,"summary":"…","url":"https://…"}` from the claiming worker completes the Check Run (`success`, `failure`, `cancelled`).

Check Runs need `GITHUB_APP_ID` and `GITHUB_APP_PRIVATE_KEY` (PKCS#8; convert GitHub's key with `openssl pkcs8 -topk8 -nocrypt -in app.pem`). Without them jobs still queue, with no Check Runs.

## Local Tests

Requires Node 22+ (uses built-in `node:test` and `node:sqlite`, zero `npm ci` dependencies):

```sh
node --test controller/test/*.test.mjs
```

## Deploy

From `controller/`:

```sh
npm ci
npx wrangler secret put HOST_TOKEN
npx wrangler secret put CLIENT_TOKEN
npx wrangler secret put GITHUB_WEBHOOK_SECRET   # for /v1/github/webhook
npx wrangler secret put GITHUB_APP_ID           # optional: Check Runs
npx wrangler secret put GITHUB_APP_PRIVATE_KEY  # optional: PKCS#8 PEM
npx wrangler deploy
```

Check bundle validity without deploying:

```sh
npx wrangler deploy --dry-run
```
