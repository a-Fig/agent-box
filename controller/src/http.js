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

/**
 * Matches a controller-v1 route path.
 * @param {string} pathname
 * @returns {{ route: "health" | "hosts" | "usage" | "host_state", method: string, rawId?: string } | null}
 */
export function matchRoute(pathname) {
  if (pathname === "/v1/health") return { route: "health", method: "GET" };
  if (pathname === "/v1/hosts") return { route: "hosts", method: "GET" };
  if (pathname === "/v1/usage") return { route: "usage", method: "GET" };
  const m = /^\/v1\/hosts\/([^/]*)\/state$/.exec(pathname);
  if (m) return { route: "host_state", method: "PUT", rawId: m[1] };
  return null;
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
