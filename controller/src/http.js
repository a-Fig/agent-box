export const MAX_BODY_BYTES = 262144; // 256 KiB
export const HOST_ID_RE = /^[a-z0-9-]{1,40}$/;

const JSON_HEADERS = { "content-type": "application/json; charset=utf-8" };

/**
 * Formats a Date as RFC 3339 UTC without fractional milliseconds.
 * @param {Date} date
 * @returns {string}
 */
export function toRFC3339(date) {
  return date.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/**
 * Returns a JSON response with status and optional extra headers.
 * @param {number} status
 * @param {unknown} payload
 * @param {Record<string, string>} [extraHeaders]
 * @returns {Response}
 */
export function jsonResponse(status, payload, extraHeaders = {}) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { ...JSON_HEADERS, ...extraHeaders },
  });
}

/**
 * Returns a controller-v1 error envelope response.
 * @param {number} status
 * @param {string} error
 * @param {string} message
 * @param {Record<string, string>} [extraHeaders]
 * @returns {Response}
 */
export function jsonError(status, error, message, extraHeaders = {}) {
  return jsonResponse(status, { error, message }, extraHeaders);
}

/** The `x-controller-role` the Worker must vouch for before the Fleet DO serves each route. */
export const ROUTE_ROLES = {
  hosts: "client",
  usage: "client",
  host_state: "host",
  ci_config: "host",
  ci_next: "host",
  ci_job: "host",
  github_webhook: "github",
};

/**
 * Matches a controller-v1 route path.
 * @param {string} pathname
 * @returns {{ route: string, methods: string[], rawId?: string } | null}
 */
export function matchRoute(pathname) {
  if (pathname === "/v1/health") return { route: "health", methods: ["GET"] };
  if (pathname === "/v1/hosts") return { route: "hosts", methods: ["GET"] };
  if (pathname === "/v1/usage") return { route: "usage", methods: ["GET"] };
  if (pathname === "/v1/github/webhook") return { route: "github_webhook", methods: ["POST"] };
  if (pathname === "/v1/ci/config") return { route: "ci_config", methods: ["GET", "PUT"] };
  if (pathname === "/v1/ci/next") return { route: "ci_next", methods: ["GET"] };
  let m = /^\/v1\/hosts\/([^/]*)\/state$/.exec(pathname);
  if (m) return { route: "host_state", methods: ["PUT"], rawId: m[1] };
  m = /^\/v1\/ci\/jobs\/([^/]*)$/.exec(pathname);
  if (m) return { route: "ci_job", methods: ["PATCH"], rawId: m[1] };
  return null;
}

/**
 * Returns the 405 response for a matched route called with the wrong method.
 * @param {{ methods: string[] }} matched
 * @returns {Response}
 */
export function methodNotAllowed(matched) {
  return jsonError(405, "method_not_allowed", "Method not allowed.", { Allow: matched.methods.join(", ") });
}

/**
 * Reads a request body up to maxBytes, rejecting oversized Content-Length or streamed bytes.
 * @param {Request} request
 * @param {number} [maxBytes]
 * @returns {Promise<{ tooLarge: true } | { tooLarge: false, bytes: Uint8Array }>}
 */
export async function readLimitedBody(request, maxBytes = MAX_BODY_BYTES) {
  const clHeader = request.headers.get("content-length");
  if (clHeader !== null) {
    const declared = Number(clHeader);
    if (Number.isFinite(declared) && declared > maxBytes) {
      return { tooLarge: true };
    }
  }
  if (!request.body) {
    return { tooLarge: false, bytes: new Uint8Array(0) };
  }
  const reader = request.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > maxBytes) {
        await reader.cancel();
        return { tooLarge: true };
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  if (chunks.length === 0) {
    return { tooLarge: false, bytes: new Uint8Array(0) };
  }
  if (chunks.length === 1) {
    return { tooLarge: false, bytes: chunks[0] };
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return { tooLarge: false, bytes: out };
}
