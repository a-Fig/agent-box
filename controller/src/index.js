import { Fleet } from "./fleet.js";
import { HOST_ID_RE, MAX_BODY_BYTES, jsonError, jsonResponse, matchRoute, readLimitedBody } from "./http.js";

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
    if (request.method !== matched.method) {
      return jsonError(405, "method_not_allowed", "Method not allowed.", { Allow: matched.method });
    }
    if (matched.route === "health") {
      return jsonResponse(200, { ok: true, protocol: 1 });
    }

    const requiredRole = matched.route === "host_state" ? "host" : "client";
    const auth = authorize(request, env, requiredRole);
    if (!auth.ok) {
      return auth.response;
    }

    if (matched.route === "host_state" && !HOST_ID_RE.test(matched.rawId)) {
      return jsonError(400, "bad_request", "Invalid host ID.");
    }

    const headers = new Headers();
    const contentType = request.headers.get("content-type");
    if (contentType) headers.set("content-type", contentType);
    headers.set("x-controller-role", auth.role);

    let body;
    if (matched.route === "host_state") {
      const limited = await readLimitedBody(request, MAX_BODY_BYTES);
      if (limited.tooLarge) {
        return jsonError(413, "too_large", "Request body exceeds the 262144-byte limit.");
      }
      body = limited.bytes;
    }

    const stub = env.FLEET.get(env.FLEET.idFromName("fleet"));
    return stub.fetch(new Request(url.toString(), {
      method: request.method,
      headers,
      body,
    }));
  },
};
