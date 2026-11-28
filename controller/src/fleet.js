import {
  HOST_ID_RE,
  MAX_BODY_BYTES,
  jsonError,
  jsonResponse,
  matchRoute,
  readLimitedBody,
  toRFC3339,
} from "./http.js";

export const DAILY_LIMIT = 100000;
export const STALE_MS = 180 * 1000;

const RFC3339_RE = /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[Zz]|[+-]\d{2}:\d{2})$/;
const utf8Decoder = new TextDecoder("utf-8", { fatal: true });

function isPlainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

export class Fleet {
  #sql;
  #now;
  #initialized = false;

  /**
   * @param {{ storage: { sql: { exec: (query: string, ...bindings: unknown[]) => any } } }} ctx
   * @param {Record<string, unknown>} env
   * @param {{ now?: () => Date | number | string }} [options]
   */
  constructor(ctx, env, options = {}) {
    this.#sql = ctx.storage.sql;
    this.#now = options.now ?? (() => new Date());
    void env;
  }

  #currentDate() {
    const val = this.#now();
    return val instanceof Date ? val : new Date(val);
  }

  #ensureSchema() {
    if (this.#initialized) return;
    this.#sql.exec(
      `CREATE TABLE IF NOT EXISTS hosts (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        seq INTEGER NOT NULL,
        state TEXT NOT NULL,
        seen_at TEXT NOT NULL
      )`
    );
    this.#sql.exec(
      `CREATE TABLE IF NOT EXISTS usage (
        day TEXT PRIMARY KEY,
        requests INTEGER NOT NULL
      )`
    );
    this.#initialized = true;
  }

  #recordUsage(day) {
    this.#sql.exec(
      `INSERT INTO usage (day, requests) VALUES (?, 1)
       ON CONFLICT(day) DO UPDATE SET requests = requests + 1`,
      day
    );
  }

  /**
   * Handles controller-v1 requests routed to the singleton Fleet Durable Object.
   * @param {Request} request
   * @returns {Promise<Response>}
   */
  async fetch(request) {
    const url = new URL(request.url);
    const matched = matchRoute(url.pathname);
    if (!matched) {
      return jsonError(404, "not_found", "Unknown route.");
    }
    if (request.method !== matched.method) {
      return jsonError(405, "method_not_allowed", "Method not allowed.", { Allow: matched.method });
    }

    const expectedRole = matched.route === "host_state" ? "host" : "client";
    if (request.headers.get("x-controller-role") !== expectedRole) {
      return jsonError(403, "forbidden", "Token lacks permission for this endpoint.");
    }

    this.#ensureSchema();
    const now = this.#currentDate();
    const nowIso = toRFC3339(now);
    const day = nowIso.slice(0, 10);
    this.#recordUsage(day);

    if (matched.route === "usage") {
      const row = this.#sql.exec("SELECT requests FROM usage WHERE day = ?", day).one();
      return jsonResponse(200, { day, requests: row.requests, limit: DAILY_LIMIT });
    }

    if (matched.route === "hosts") {
      const nowMs = now.getTime();
      const rows = this.#sql
        .exec("SELECT id, name, state, seen_at FROM hosts ORDER BY seen_at DESC, id ASC")
        .toArray();
      const items = rows.map((row) => {
        const seenMs = Date.parse(row.seen_at);
        const stale = Number.isFinite(seenMs) ? nowMs - seenMs > STALE_MS : true;
        return `{"id":${JSON.stringify(row.id)},"name":${JSON.stringify(row.name)},"seenAt":${JSON.stringify(row.seen_at)},"stale":${stale},"state":${row.state}}`;
      });
      return new Response(`{"now":${JSON.stringify(nowIso)},"hosts":[${items.join(",")}]}`, {
        status: 200,
        headers: { "content-type": "application/json; charset=utf-8" },
      });
    }

    if (matched.route === "host_state") {
      const hostId = matched.rawId;
      if (!HOST_ID_RE.test(hostId)) {
        return jsonError(400, "bad_request", "Invalid host ID.");
      }
      const limited = await readLimitedBody(request, MAX_BODY_BYTES);
      if (limited.tooLarge) {
        return jsonError(413, "too_large", "Request body exceeds the 262144-byte limit.");
      }

      let body;
      try {
        body = JSON.parse(utf8Decoder.decode(limited.bytes));
      } catch {
        return jsonError(400, "bad_request", "Malformed JSON request body.");
      }

      if (
        !isPlainObject(body) ||
        body.id !== hostId ||
        typeof body.name !== "string" ||
        body.name.length === 0 ||
        !Number.isSafeInteger(body.seq) ||
        body.seq < 0 ||
        typeof body.sentAt !== "string" ||
        !RFC3339_RE.test(body.sentAt) ||
        !Number.isFinite(Date.parse(body.sentAt)) ||
        !isPlainObject(body.state)
      ) {
        return jsonError(400, "bad_request", "Missing or invalid state envelope fields.");
      }

      this.#sql.exec(
        `INSERT INTO hosts (id, name, seq, state, seen_at)
         VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET
           name = excluded.name,
           seq = excluded.seq,
           state = excluded.state,
           seen_at = excluded.seen_at`,
        hostId,
        body.name,
        body.seq,
        JSON.stringify(body.state),
        nowIso
      );
      return new Response(null, { status: 204 });
    }

    return jsonError(404, "not_found", "Unknown route.");
  }
}
