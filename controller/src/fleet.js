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

const utf8Decoder = new TextDecoder("utf-8", { fatal: true });

function skipWs(src, i) {
  while (i < src.length) {
    const c = src.charCodeAt(i);
    if (c === 0x20 || c === 0x09 || c === 0x0a || c === 0x0d) {
      i++;
      continue;
    }
    break;
  }
  return i;
}

function scanStringEnd(src, start) {
  let i = start + 1;
  while (i < src.length) {
    const c = src.charCodeAt(i);
    if (c === 0x22) return i + 1;
    if (c === 0x5c) {
      if (i + 1 >= src.length) throw new SyntaxError("Unterminated escape");
      i += 2;
      continue;
    }
    i++;
  }
  throw new SyntaxError("Unterminated string");
}

function scanCompositeEnd(src, start) {
  const stack = [src.charCodeAt(start) === 0x7b ? 0x7d : 0x5d];
  let i = start + 1;
  while (i < src.length) {
    const c = src.charCodeAt(i);
    if (c === 0x22) {
      i = scanStringEnd(src, i);
      continue;
    }
    if (c === 0x7b) {
      stack.push(0x7d);
    } else if (c === 0x5b) {
      stack.push(0x5d);
    } else if (c === 0x7d || c === 0x5d) {
      if (stack.pop() !== c) throw new SyntaxError("Mismatched bracket");
      if (stack.length === 0) return i + 1;
    }
    i++;
  }
  throw new SyntaxError("Unterminated JSON container");
}

function scanValueEnd(src, start) {
  if (start >= src.length) throw new SyntaxError("Unexpected end of JSON");
  const c = src.charCodeAt(start);
  if (c === 0x22) return scanStringEnd(src, start);
  if (c === 0x7b || c === 0x5b) return scanCompositeEnd(src, start);
  let i = start;
  while (i < src.length) {
    const ch = src.charCodeAt(i);
    if (ch === 0x2c || ch === 0x7d || ch === 0x5d || ch === 0x20 || ch === 0x09 || ch === 0x0a || ch === 0x0d) {
      break;
    }
    i++;
  }
  if (i === start) throw new SyntaxError("Missing JSON value");
  return i;
}

/**
 * Parses top-level PUT /v1/hosts/{id}/state envelope without parsing the `state` object.
 * @param {string} raw
 * @returns {{ id?: unknown, name?: unknown, seq?: unknown, sentAt?: unknown, stateRaw?: string }}
 */
export function parseStateEnvelope(raw) {
  let pos = skipWs(raw, 0);
  if (raw.charCodeAt(pos) !== 0x7b) throw new SyntaxError("Expected JSON object");
  pos = skipWs(raw, pos + 1);
  const out = {};
  if (raw.charCodeAt(pos) === 0x7d) {
    pos = skipWs(raw, pos + 1);
    if (pos !== raw.length) throw new SyntaxError("Trailing JSON content");
    return out;
  }
  while (true) {
    if (raw.charCodeAt(pos) !== 0x22) throw new SyntaxError("Expected string key");
    const keyEnd = scanStringEnd(raw, pos);
    const key = JSON.parse(raw.slice(pos, keyEnd));
    pos = skipWs(raw, keyEnd);
    if (raw.charCodeAt(pos) !== 0x3a) throw new SyntaxError("Expected ':'");
    const valStart = skipWs(raw, pos + 1);
    const valEnd = scanValueEnd(raw, valStart);
    if (key === "state") {
      if (raw.charCodeAt(valStart) !== 0x7b) {
        throw new SyntaxError("Expected state object");
      }
      out.stateRaw = raw.slice(valStart, valEnd);
    } else if (key === "id" || key === "name" || key === "seq" || key === "sentAt") {
      out[key] = JSON.parse(raw.slice(valStart, valEnd));
    }
    pos = skipWs(raw, valEnd);
    const sep = raw.charCodeAt(pos);
    if (sep === 0x2c) {
      pos = skipWs(raw, pos + 1);
      continue;
    }
    if (sep === 0x7d) {
      pos = skipWs(raw, pos + 1);
      break;
    }
    throw new SyntaxError("Expected ',' or '}'");
  }
  if (pos !== raw.length) throw new SyntaxError("Trailing JSON content");
  return out;
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
    const callerRole = request.headers.get("x-controller-role");
    if (callerRole !== null && callerRole !== expectedRole) {
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

      let raw;
      let envBody;
      try {
        raw = utf8Decoder.decode(limited.bytes);
        envBody = parseStateEnvelope(raw);
      } catch {
        return jsonError(400, "bad_request", "Malformed JSON request body.");
      }

      if (
        envBody.id !== hostId ||
        typeof envBody.name !== "string" ||
        envBody.name.length === 0 ||
        typeof envBody.seq !== "number" ||
        !Number.isInteger(envBody.seq) ||
        envBody.seq < 0 ||
        typeof envBody.sentAt !== "string" ||
        Number.isNaN(Date.parse(envBody.sentAt)) ||
        typeof envBody.stateRaw !== "string"
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
        envBody.name,
        envBody.seq,
        envBody.stateRaw,
        nowIso
      );
      return new Response(null, { status: 204 });
    }

    return jsonError(404, "not_found", "Unknown route.");
  }
}
