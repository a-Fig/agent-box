import { Fleet } from "./fleet.js";
import {
  HOST_ID_RE,
  MAX_BODY_BYTES,
  ROUTE_ROLES,
  jsonError,
  jsonResponse,
  matchRoute,
  methodNotAllowed,
  readLimitedBody,
} from "./http.js";

export { Fleet };

const encoder = new TextEncoder();

/**
 * Constant-time UTF-8 string comparison; rejects missing or empty strings fail-closed.
 * @param {unknown} a
 * @param {unknown} b
 * @returns {boolean}
 */
export function constantTimeEqual(a, b) {
  if (typeof a !== "string" || typeof b !== "string" || a.length === 0 || b.length === 0) {
    return false;
  }
  const ab = encoder.encode(a);
  const bb = encoder.encode(b);
  const maxLen = Math.max(ab.length, bb.length);
  let diff = ab.length ^ bb.length;
  for (let i = 0; i < maxLen; i++) {
    diff |= (ab[i] ?? 0) ^ (bb[i] ?? 0);
  }
  return diff === 0;
}

/**
 * Verifies GitHub's `X-Hub-Signature-256: sha256=<hex>` HMAC over the raw body bytes.
 * Uses WebCrypto verify, which compares in constant time. Fails closed on an empty secret.
 * @param {string} secret
 * @param {Uint8Array} body
 * @param {string | null} header
 * @returns {Promise<boolean>}
 */
export async function verifyGitHubSignature(secret, body, header) {
  if (typeof secret !== "string" || secret === "") return false;
  const m = /^sha256=([0-9a-f]{64})$/i.exec(header ?? "");
  if (!m) return false;
  const sig = new Uint8Array(32);
  for (let i = 0; i < 32; i++) sig[i] = parseInt(m[1].slice(i * 2, i * 2 + 2), 16);
  const key = await crypto.subtle.importKey(
    "raw",
    encoder.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["verify"]
  );
  return crypto.subtle.verify("HMAC", key, sig, body);
}

function tooLarge() {
  return jsonError(413, "too_large", "Request body exceeds the 262144-byte limit.");
}

function extractBearer(request) {
  const header = request.headers.get("authorization");
  if (!header) return "";
  const m = /^Bearer\s+(.+)$/i.exec(header);
  return m ? m[1] : "";
}

function authorize(request, env, requiredRole) {
  const token = extractBearer(request);
  const isHost = constantTimeEqual(token, env?.HOST_TOKEN);
  const isClient = constantTimeEqual(token, env?.CLIENT_TOKEN);
  if (requiredRole === "host") {
    if (isHost) return { ok: true, role: "host" };
    if (isClient) return { ok: false, response: jsonError(403, "forbidden", "Token lacks permission for this endpoint.") };
  } else {
    if (isClient) return { ok: true, role: "client" };
    if (isHost) return { ok: false, response: jsonError(403, "forbidden", "Token lacks permission for this endpoint.") };
  }
  return { ok: false, response: jsonError(401, "unauthorized", "Missing or invalid bearer token.") };
}

export default {
  /**
   * Cloudflare Worker entrypoint for controller-v1.
   * @param {Request} request
   * @param {Record<string, any>} env
   * @returns {Promise<Response>}
   */
  async fetch(request, env) {
    const url = new URL(request.url);
    const matched = matchRoute(url.pathname);
    if (!matched) {
      return jsonError(404, "not_found", "Unknown route.");
    }
    if (!matched.methods.includes(request.method)) {
      return methodNotAllowed(matched);
    }
    if (matched.route === "health") {
      return jsonResponse(200, { ok: true, protocol: 1 });
    }

    const headers = new Headers();
    const contentType = request.headers.get("content-type");
    if (contentType) headers.set("content-type", contentType);
    let body;

    if (matched.route === "github_webhook") {
      // GitHub authenticates with the shared webhook secret, not a bearer token.
      const secret = env?.GITHUB_WEBHOOK_SECRET;
      if (typeof secret !== "string" || secret === "") {
        return jsonError(401, "unauthorized", "Webhook secret is not configured.");
      }
      const limited = await readLimitedBody(request, MAX_BODY_BYTES);
      if (limited.tooLarge) return tooLarge();
      if (!(await verifyGitHubSignature(secret, limited.bytes, request.headers.get("x-hub-signature-256")))) {
        return jsonError(401, "unauthorized", "Missing or invalid webhook signature.");
      }
      body = limited.bytes;
      headers.set("x-controller-role", "github");
      for (const name of ["x-github-event", "x-github-delivery"]) {
        const value = request.headers.get(name);
        if (value) headers.set(name, value);
      }
    } else {
      const auth = authorize(request, env, ROUTE_ROLES[matched.route]);
      if (!auth.ok) {
        return auth.response;
      }
      if (matched.route === "host_state" && !HOST_ID_RE.test(matched.rawId)) {
        return jsonError(400, "bad_request", "Invalid host ID.");
      }
      headers.set("x-controller-role", auth.role);
      if (request.method !== "GET") {
        const limited = await readLimitedBody(request, MAX_BODY_BYTES);
        if (limited.tooLarge) return tooLarge();
        body = limited.bytes;
      }
    }

    const stub = env.FLEET.get(env.FLEET.idFromName("fleet"));
    return stub.fetch(new Request(url.toString(), {
      method: request.method,
      headers,
      body,
    }));
  },
};
