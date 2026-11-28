// GitHub App client for Check Runs: RS256 app JWT -> installation token -> REST calls.
// Pure WebCrypto and an injectable fetch, so tests run offline.

const API = "https://api.github.com";
const REFRESH_MARGIN_MS = 5 * 60 * 1000;
const encoder = new TextEncoder();

/**
 * Encodes bytes as unpadded base64url.
 * @param {Uint8Array} bytes
 * @returns {string}
 */
export function base64url(bytes) {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/**
 * Imports a PKCS#8 PEM RSA private key for RS256 signing.
 * GitHub hands out PKCS#1 ("BEGIN RSA PRIVATE KEY"), which WebCrypto can't import.
 * @param {string} pem
 * @returns {Promise<CryptoKey>}
 */
export async function importPkcs8Pem(pem) {
  const text = pem.replace(/\\n/g, "\n");
  if (text.includes("BEGIN RSA PRIVATE KEY")) {
    throw new Error("GITHUB_APP_PRIVATE_KEY is PKCS#1; convert it with `openssl pkcs8 -topk8 -nocrypt`.");
  }
  const m = /-----BEGIN PRIVATE KEY-----([\s\S]+?)-----END PRIVATE KEY-----/.exec(text);
  if (!m) throw new Error("GITHUB_APP_PRIVATE_KEY is not a PKCS#8 PEM key.");
  const der = Uint8Array.from(atob(m[1].replace(/\s+/g, "")), (c) => c.charCodeAt(0));
  return crypto.subtle.importKey("pkcs8", der, { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" }, false, ["sign"]);
}

export class GitHubApp {
  #appId;
  #pem;
  #key = null;
  #fetch;
  #now;
  /** @type {Map<number, { token: string, expiresAt: number }>} */
  #tokens = new Map();

  /**
   * @param {{ appId: string, privateKey: string, fetch?: typeof fetch, now?: () => Date }} options
   */
  constructor({ appId, privateKey, fetch: fetchImpl, now }) {
    this.#appId = String(appId);
    this.#pem = privateKey;
    this.#fetch = fetchImpl ?? ((...args) => fetch(...args));
    this.#now = now ?? (() => new Date());
  }

  #nowMs() {
    return this.#now().getTime();
  }

  /**
   * Signs a GitHub App JWT valid for 9 minutes (iat backdated 60 s for clock skew).
   * @returns {Promise<string>}
   */
  async createJwt() {
    this.#key ??= await importPkcs8Pem(this.#pem);
    const iat = Math.floor(this.#nowMs() / 1000) - 60;
    // iss is the numeric app ID, or the app's client ID string.
    const iss = /^\d+$/.test(this.#appId) ? Number(this.#appId) : this.#appId;
    const header = base64url(encoder.encode(JSON.stringify({ alg: "RS256", typ: "JWT" })));
    const payload = base64url(encoder.encode(JSON.stringify({ iat, exp: iat + 600, iss })));
    const data = `${header}.${payload}`;
    const sig = await crypto.subtle.sign("RSASSA-PKCS1-v1_5", this.#key, encoder.encode(data));
    return `${data}.${base64url(new Uint8Array(sig))}`;
  }

  /**
   * Returns an installation access token, reusing a cached one until 5 minutes before it expires.
   * @param {number} installationId
   * @returns {Promise<string>}
   */
  async installationToken(installationId) {
    const cached = this.#tokens.get(installationId);
    if (cached && cached.expiresAt - this.#nowMs() > REFRESH_MARGIN_MS) return cached.token;
    const jwt = await this.createJwt();
    const body = await this.#call("POST", `/app/installations/${installationId}/access_tokens`, jwt);
    const expiresAt = Date.parse(body.expires_at);
    if (typeof body.token !== "string" || !Number.isFinite(expiresAt)) {
      throw new Error("GitHub returned a malformed installation token.");
    }
    this.#tokens.set(installationId, { token: body.token, expiresAt });
    return body.token;
  }

  async #call(method, path, token, payload) {
    const res = await this.#fetch(API + path, {
      method,
      headers: {
        accept: "application/vnd.github+json",
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
        "user-agent": "agent-box-controller",
        "x-github-api-version": "2022-11-28",
      },
      body: payload === undefined ? undefined : JSON.stringify(payload),
    });
    const text = await res.text();
    if (!res.ok) {
      throw new Error(`GitHub ${method} ${path}: ${res.status} ${text.slice(0, 200)}`);
    }
    return text ? JSON.parse(text) : {};
  }

  /**
   * @param {number} installationId
   * @param {string} repo "owner/name"
   * @param {Record<string, unknown>} payload
   * @returns {Promise<{ id: number }>}
   */
  async createCheckRun(installationId, repo, payload) {
    const token = await this.installationToken(installationId);
    return this.#call("POST", `/repos/${repo}/check-runs`, token, payload);
  }

  /**
   * @param {number} installationId
   * @param {string} repo "owner/name"
   * @param {number} checkRunId
   * @param {Record<string, unknown>} payload
   */
  async updateCheckRun(installationId, repo, checkRunId, payload) {
    const token = await this.installationToken(installationId);
    return this.#call("PATCH", `/repos/${repo}/check-runs/${checkRunId}`, token, payload);
  }
}

/**
 * Builds a GitHubApp from GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY, or null when either is unset
 * (CI jobs still queue; no Check Runs are posted).
 * @param {Record<string, any>} env
 * @param {{ fetch?: typeof fetch, now?: () => Date }} [options]
 * @returns {GitHubApp | null}
 */
export function githubAppFromEnv(env, options = {}) {
  if (!env?.GITHUB_APP_ID || !env?.GITHUB_APP_PRIVATE_KEY) return null;
  return new GitHubApp({ appId: env.GITHUB_APP_ID, privateKey: env.GITHUB_APP_PRIVATE_KEY, ...options });
}
