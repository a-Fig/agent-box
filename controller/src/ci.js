// GitHub webhook CI ingest and the CI job queue, stored in the Fleet Durable Object's SQLite.
// The Worker has already verified the webhook signature (role "github") or the host token
// (role "host") before a request reaches here.

import { HOST_ID_RE, MAX_BODY_BYTES, jsonError, jsonResponse, readLimitedBody, toRFC3339 } from "./http.js";

export const CI_SCHEMA = [
  `CREATE TABLE IF NOT EXISTS ci_deliveries (
    delivery_id TEXT PRIMARY KEY,
    seen_at TEXT NOT NULL
  )`,
  `CREATE TABLE IF NOT EXISTS ci_config (
    repo TEXT PRIMARY KEY,
    config TEXT NOT NULL
  )`,
  `CREATE TABLE IF NOT EXISTS ci_jobs (
    id TEXT PRIMARY KEY,
    repo TEXT NOT NULL,
    sha TEXT NOT NULL,
    pr INTEGER,
    check_run_id INTEGER,
    installation_id INTEGER,
    host_class TEXT NOT NULL,
    state TEXT NOT NULL,
    claimed_by TEXT,
    result TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
  )`,
];

export const CHECK_NAME = "agent-box";
const REPO_RE = /^[A-Za-z0-9_.-]{1,100}\/[A-Za-z0-9_.-]{1,100}$/;
const LOGIN_RE = /^[A-Za-z0-9-]{1,39}(\[bot\])?$/;
const SHA_RE = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const ZERO_SHA_RE = /^0+$/;
const DELIVERY_RE = /^[A-Za-z0-9-]{1,100}$/;
const JOB_ID_RE = /^[0-9a-f-]{36}$/;
const DELIVERY_TTL_MS = 7 * 24 * 60 * 60 * 1000;
const SUMMARY_MAX = 60000;
const PR_ACTIONS = new Set(["opened", "synchronize", "reopened"]);
/** Final job states a worker may report, and the Check Run conclusion each maps to. */
const CONCLUSIONS = { done: "success", failed: "failure", cancelled: "cancelled" };
const utf8Decoder = new TextDecoder("utf-8", { fatal: true });

function isPlainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

async function readJson(request) {
  const limited = await readLimitedBody(request, MAX_BODY_BYTES);
  if (limited.tooLarge) {
    return { response: jsonError(413, "too_large", "Request body exceeds the 262144-byte limit.") };
  }
  try {
    return { body: JSON.parse(utf8Decoder.decode(limited.bytes)) };
  } catch {
    return { response: jsonError(400, "bad_request", "Malformed JSON request body.") };
  }
}

/**
 * Validates a per-repo CI config; returns the normalized config or null.
 * @param {unknown} c
 */
export function parseRepoConfig(c) {
  if (
    !isPlainObject(c) ||
    typeof c.repo !== "string" ||
    !REPO_RE.test(c.repo) ||
    !Array.isArray(c.authors) ||
    !c.authors.every((a) => typeof a === "string" && LOGIN_RE.test(a)) ||
    typeof c.allowPublic !== "boolean" ||
    typeof c.cloudOnlyForForks !== "boolean" ||
    !Number.isSafeInteger(c.installationId) ||
    c.installationId <= 0
  ) {
    return null;
  }
  return {
    repo: c.repo,
    authors: [...c.authors],
    allowPublic: c.allowPublic,
    cloudOnlyForForks: c.cloudOnlyForForks,
    installationId: c.installationId,
  };
}

/**
 * Reduces a webhook to what CI needs, or `{ ignored }` for events that never run CI.
 * @param {string} event
 * @param {any} p
 */
export function webhookTrigger(event, p) {
  if (!isPlainObject(p) || !isPlainObject(p.repository) || typeof p.repository.full_name !== "string") {
    return { ignored: "no repository in payload" };
  }
  const base = { repo: p.repository.full_name, sender: p.sender?.login, isPrivate: p.repository.private === true };
  if (event === "push") {
    if (p.deleted === true || typeof p.after !== "string" || ZERO_SHA_RE.test(p.after)) {
      return { ignored: "branch deleted" };
    }
    return { ...base, sha: p.after, pr: null, fork: false };
  }
  if (event === "pull_request") {
    if (!PR_ACTIONS.has(p.action)) return { ignored: `pull_request action ${p.action} not handled` };
    const pr = p.pull_request ?? {};
    const headRepo = pr.head?.repo;
    const fork =
      !isPlainObject(headRepo) ||
      typeof headRepo.full_name !== "string" ||
      headRepo.full_name.toLowerCase() !== base.repo.toLowerCase();
    return { ...base, sha: pr.head?.sha, pr: pr.number ?? p.number ?? null, prAuthor: pr.user?.login ?? "", fork };
  }
  if (event === "check_suite") {
    if (p.action !== "rerequested") return { ignored: `check_suite action ${p.action} not handled` };
    return { ...base, sha: p.check_suite?.head_sha, rerequest: true };
  }
  return { ignored: `event ${event || "(none)"} not handled` };
}

/** The Check Run PATCH body for a claimed job's state. */
function checkRunUpdate(row) {
  if (row.state === "running") return { status: "in_progress", started_at: row.updated_at };
  const r = row.result ? JSON.parse(row.result) : {};
  const title =
    row.state === "done"
      ? "Passed"
      : row.state === "cancelled"
        ? "Cancelled"
        : `Failed${r.exitCode != null ? ` (exit ${r.exitCode})` : ""}`;
  const summary = (r.summary || `agent-box job ${row.id} ${row.state} on ${row.claimed_by}.`).slice(0, SUMMARY_MAX);
  return {
    status: "completed",
    conclusion: CONCLUSIONS[row.state],
    completed_at: row.updated_at,
    ...(r.url ? { details_url: r.url } : {}),
    output: { title, summary },
  };
}

function jobView(row) {
  return {
    id: row.id,
    repo: row.repo,
    sha: row.sha,
    pr: row.pr,
    hostClass: row.host_class,
    state: row.state,
    claimedBy: row.claimed_by,
    checkRunId: row.check_run_id,
    result: row.result ? JSON.parse(row.result) : null,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

export class CI {
  #sql;
  #now;
  #github;

  /**
   * @param {{ exec: (query: string, ...bindings: unknown[]) => any }} sql
   * @param {{ now: () => Date, github: import("./github.js").GitHubApp | null }} options
   */
  constructor(sql, { now, github }) {
    this.#sql = sql;
    this.#now = now;
    this.#github = github;
  }

  #nowIso() {
    return toRFC3339(this.#now());
  }

  /**
   * @param {{ route: string, rawId?: string }} matched
   * @param {Request} request
   * @returns {Promise<Response>}
   */
  async handle(matched, request) {
    switch (matched.route) {
      case "github_webhook":
        return this.#webhook(request);
      case "ci_config":
        return request.method === "GET" ? this.#getConfig() : this.#putConfig(request);
      case "ci_next":
        return this.#next(new URL(request.url));
      case "ci_job":
        return this.#patchJob(matched.rawId ?? "", request);
      default:
        return jsonError(404, "not_found", "Unknown route.");
    }
  }

  #getConfig() {
    const rows = this.#sql.exec("SELECT config FROM ci_config ORDER BY repo").toArray();
    return jsonResponse(200, { repos: rows.map((r) => JSON.parse(r.config)) });
  }

  async #putConfig(request) {
    const parsed = await readJson(request);
    if (parsed.response) return parsed.response;
    const config = parseRepoConfig(parsed.body);
    if (!config) {
      return jsonError(400, "bad_request", "Invalid CI repo config.");
    }
    // GitHub owner/repo names are case-insensitive, so the key is lowercased.
    this.#sql.exec(
      `INSERT INTO ci_config (repo, config) VALUES (?, ?)
       ON CONFLICT(repo) DO UPDATE SET config = excluded.config`,
      config.repo.toLowerCase(),
      JSON.stringify(config)
    );
    return jsonResponse(200, { ok: true, config });
  }

  #repoConfig(repo) {
    const row = this.#sql.exec("SELECT config FROM ci_config WHERE repo = ?", repo.toLowerCase()).toArray()[0];
    return row ? JSON.parse(row.config) : null;
  }

  async #webhook(request) {
    const delivery = request.headers.get("x-github-delivery") ?? "";
    const event = request.headers.get("x-github-event") ?? "";
    if (!DELIVERY_RE.test(delivery)) {
      return jsonError(400, "bad_request", "Missing or invalid X-GitHub-Delivery header.");
    }
    const parsed = await readJson(request);
    if (parsed.response) return parsed.response;

    // Check-and-insert runs with no await between, so it is atomic inside the Durable Object.
    const nowIso = this.#nowIso();
    if (this.#sql.exec("SELECT 1 AS seen FROM ci_deliveries WHERE delivery_id = ?", delivery).toArray().length) {
      return jsonResponse(200, { ok: true, duplicate: true });
    }
    const cutoff = toRFC3339(new Date(this.#now().getTime() - DELIVERY_TTL_MS));
    this.#sql.exec("DELETE FROM ci_deliveries WHERE seen_at < ?", cutoff);
    this.#sql.exec("INSERT INTO ci_deliveries (delivery_id, seen_at) VALUES (?, ?)", delivery, nowIso);
    try {
      return await this.#ingest(event, parsed.body);
    } catch (err) {
      // Let a manual redelivery retry what failed.
      this.#sql.exec("DELETE FROM ci_deliveries WHERE delivery_id = ?", delivery);
      throw err;
    }
  }

  async #ingest(event, payload) {
    const t = webhookTrigger(event, payload);
    if (t.ignored) return jsonResponse(200, { ok: true, ignored: true, reason: t.ignored });

    // Guard 1: unknown repos are ignored outright, with no Check Run.
    const config = this.#repoConfig(t.repo);
    if (!config) return jsonResponse(200, { ok: true, ignored: true, reason: "repo not allowlisted" });

    if (typeof t.sha !== "string" || !SHA_RE.test(t.sha)) {
      return jsonError(400, "bad_request", "Webhook payload has no valid head commit SHA.");
    }

    let { fork, pr } = t;
    if (t.rerequest) {
      // A re-run inherits the classification of the commit's earlier job; a fork's job is the
      // only kind that is ever cloud-only.
      const prev = this.#sql
        .exec(
          "SELECT pr, host_class FROM ci_jobs WHERE LOWER(repo) = ? AND sha = ? ORDER BY created_at DESC, rowid DESC LIMIT 1",
          t.repo.toLowerCase(),
          t.sha
        )
        .toArray()[0];
      if (!prev) return this.#refuse(config, t, "agent-box has no earlier run of this commit to re-run.");
      fork = prev.host_class === "cloud";
      pr = prev.pr;
    }

    // Guard 2: fork PRs never run on home hardware; at most on cloud hosts, and only if allowed.
    if (fork && !config.cloudOnlyForForks) {
      return this.#refuse(
        config,
        t,
        "Pull requests from forks never run on home hardware, and this repo does not allow cloud-only runs for forks (cloudOnlyForForks is false)."
      );
    }
    const hostClass = fork ? "cloud" : "any";

    // Guard 3: the sender and, for PRs, the PR author must be allowlisted authors.
    const authors = new Set(config.authors.map((a) => a.toLowerCase()));
    const allowed = (login) => typeof login === "string" && authors.has(login.toLowerCase());
    if (!allowed(t.sender)) {
      return this.#refuse(config, t, `${t.sender || "The sender"} is not on this repo's agent-box authors allowlist.`);
    }
    if (t.prAuthor !== undefined && !allowed(t.prAuthor)) {
      return this.#refuse(
        config,
        t,
        `Pull request author ${t.prAuthor || "(unknown)"} is not on this repo's agent-box authors allowlist.`
      );
    }

    // Guard 4: public repos run only when the config opts in (a missing `private` counts as public).
    if (!t.isPrivate && !config.allowPublic) {
      return this.#refuse(config, t, "This repo is public and its agent-box config does not allow public repos (allowPublic is false).");
    }

    // A push to a PR branch also fires pull_request.synchronize: one run per commit is enough.
    // The check and the insert run with no await between, so concurrent deliveries can't both
    // pass the check; the Check Run is created after the row exists.
    const existing = this.#sql
      .exec(
        "SELECT id FROM ci_jobs WHERE LOWER(repo) = ? AND sha = ? AND state IN ('queued', 'running') LIMIT 1",
        t.repo.toLowerCase(),
        t.sha
      )
      .toArray()[0];
    if (existing) return jsonResponse(200, { ok: true, queued: false, existing: existing.id });

    const nowIso = this.#nowIso();
    const id = crypto.randomUUID();
    this.#sql.exec(
      `INSERT INTO ci_jobs (id, repo, sha, pr, check_run_id, installation_id, host_class, state, claimed_by, result, created_at, updated_at)
       VALUES (?, ?, ?, ?, NULL, ?, ?, 'queued', NULL, NULL, ?, ?)`,
      id,
      t.repo,
      t.sha,
      pr ?? null,
      config.installationId,
      hostClass,
      nowIso,
      nowIso
    );
    let checkRunId = null;
    if (this.#github) {
      try {
        const run = await this.#github.createCheckRun(config.installationId, t.repo, {
          name: CHECK_NAME,
          head_sha: t.sha,
          status: "queued",
          external_id: id,
        });
        checkRunId = run.id ?? null;
      } catch (err) {
        console.error(`ci: creating check run for ${t.repo}@${t.sha} failed:`, err);
      }
    }
    if (checkRunId !== null) {
      this.#sql.exec("UPDATE ci_jobs SET check_run_id = ? WHERE id = ?", checkRunId, id);
      // A worker may have claimed (or even finished) the job while the Check Run was being
      // created; those updates were skipped for want of an ID, so catch the Check Run up.
      const row = this.#sql.exec("SELECT * FROM ci_jobs WHERE id = ?", id).toArray()[0];
      if (row && row.state !== "queued") await this.#updateCheckRun(row);
    }
    return jsonResponse(200, { ok: true, queued: true, job: { id, hostClass, checkRunId } });
  }

  /** Refuses an allowlisted repo's event: a neutral Check Run says why, and no job is queued. */
  async #refuse(config, t, reason) {
    let checkRunId = null;
    if (this.#github && typeof t.sha === "string" && SHA_RE.test(t.sha)) {
      try {
        const run = await this.#github.createCheckRun(config.installationId, t.repo, {
          name: CHECK_NAME,
          head_sha: t.sha,
          status: "completed",
          conclusion: "neutral",
          completed_at: this.#nowIso(),
          output: { title: "agent-box did not run this commit", summary: reason },
        });
        checkRunId = run.id ?? null;
      } catch (err) {
        console.error(`ci: posting refusal check run for ${t.repo}@${t.sha} failed:`, err);
      }
    }
    return jsonResponse(200, { ok: true, refused: true, reason, checkRunId });
  }

  /** Brings a job's Check Run to its current state: in_progress while running, completed after. */
  async #updateCheckRun(row) {
    if (!this.#github || row.check_run_id == null) return;
    try {
      await this.#github.updateCheckRun(row.installation_id, row.repo, row.check_run_id, checkRunUpdate(row));
    } catch (err) {
      console.error(`ci: updating check run ${row.check_run_id} for job ${row.id} failed:`, err);
    }
  }

  async #next(url) {
    const worker = url.searchParams.get("worker") ?? "";
    const hostClass = url.searchParams.get("hostClass") ?? "";
    if (!HOST_ID_RE.test(worker)) {
      return jsonError(400, "bad_request", "Invalid worker ID.");
    }
    // Home hardware only ever sees 'any' jobs; 'cloud' jobs (fork PRs) are for cloud workers.
    let where;
    if (hostClass === "home") where = "host_class = 'any'";
    else if (hostClass === "cloud") where = "host_class IN ('cloud', 'any')";
    else return jsonError(400, "bad_request", "hostClass must be home or cloud.");

    // Select and claim run with no await between, so the claim is atomic inside the Durable Object.
    const row = this.#sql
      .exec(`SELECT * FROM ci_jobs WHERE state = 'queued' AND ${where} ORDER BY created_at, rowid LIMIT 1`)
      .toArray()[0];
    if (!row) return new Response(null, { status: 204 });
    const nowIso = this.#nowIso();
    this.#sql.exec(
      "UPDATE ci_jobs SET state = 'running', claimed_by = ?, updated_at = ? WHERE id = ? AND state = 'queued'",
      worker,
      nowIso,
      row.id
    );
    const claimed = { ...row, state: "running", claimed_by: worker, updated_at: nowIso };
    await this.#updateCheckRun(claimed);
    return jsonResponse(200, { job: jobView(claimed) });
  }

  async #patchJob(id, request) {
    if (!JOB_ID_RE.test(id)) {
      return jsonError(400, "bad_request", "Invalid job ID.");
    }
    const parsed = await readJson(request);
    if (parsed.response) return parsed.response;
    const b = parsed.body;
    if (
      !isPlainObject(b) ||
      typeof b.worker !== "string" ||
      !HOST_ID_RE.test(b.worker) ||
      !Object.hasOwn(CONCLUSIONS, b.state) ||
      (b.exitCode !== undefined && !Number.isSafeInteger(b.exitCode)) ||
      (b.summary !== undefined && typeof b.summary !== "string") ||
      (b.url !== undefined && (typeof b.url !== "string" || !/^https:\/\/\S+$/.test(b.url)))
    ) {
      return jsonError(400, "bad_request", "Missing or invalid job update fields.");
    }

    const row = this.#sql.exec("SELECT * FROM ci_jobs WHERE id = ?", id).toArray()[0];
    if (!row) return jsonError(404, "not_found", "Unknown job.");
    if (row.claimed_by !== b.worker) {
      return jsonError(403, "forbidden", "Job is not claimed by this worker.");
    }
    if (row.state !== "running") {
      return jsonError(409, "conflict", `Job is already ${row.state}.`);
    }

    const nowIso = this.#nowIso();
    const result = { exitCode: b.exitCode ?? null, summary: b.summary ?? null, url: b.url ?? null };
    this.#sql.exec(
      "UPDATE ci_jobs SET state = ?, result = ?, updated_at = ? WHERE id = ? AND state = 'running'",
      b.state,
      JSON.stringify(result),
      nowIso,
      id
    );
    const updated = { ...row, state: b.state, result: JSON.stringify(result), updated_at: nowIso };
    await this.#updateCheckRun(updated);
    return jsonResponse(200, { job: jobView(updated) });
  }
}
