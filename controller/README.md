# Fleet Controller (`controller/`)

Cloudflare Worker + SQLite Durable Object implementing the [`controller-v1`](../docs/controller-protocol.md) fleet state cache.

## Layout

- `src/index.js`: Worker entrypoint (`GET /v1/health`, bearer auth, route/ID/body-size validation, raw streaming to `FLEET` Durable Object).
- `src/fleet.js`: `Fleet` SQLite Durable Object (`hosts` and `usage` tables, zero-parse `state` storage and embedding).
- `wrangler.jsonc`: Worker and `FLEET` SQLite Durable Object migration config (no D1, KV, or R2).

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
npx wrangler deploy
```

Check bundle validity without deploying:

```sh
npx wrangler deploy --dry-run
```
