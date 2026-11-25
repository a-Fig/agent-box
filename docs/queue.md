# The queue

Planned 2026-11-24. Tyler asked for it; the design below is Claude's, built first and reviewed by
Tyler afterwards.

## Why

Before the queue, a run with no room (the caps were full) or a paused PC (My PC) got exit 75,
and the agent ran the command on the laptop, often the starving laptop that offloading exists to
spare. Once results arrive as a pushed turn (see the push design in DESIGN.md), an agent doesn't need
to sit and wait. So it can hand the run to a queue and keep working.

## Behaviour

- **A run that can't start now is queued, not refused.** `box_run` returns
  `state: queued`, with the job ID, the position ("2nd in line"), why it waits (no room, My PC),
  and `push` as for a running job. The result comes by the same push when the
  job has run.
- **What is never queued:** these still get exit 75 at once, as before:
  - `queue: false` on `box_run`, for "I need the answer now or I'll run it locally";
  - a GPU run while the PC is in My PC: My PC can last hours, and GPU work is rarely urgent
    enough to wait it out;
  - a `box_run` with no push (`push: false`): it couldn't hear when the job starts;
  - `box run --no-queue` on the CLI.
- **`box run` on the CLI queues too** (#93, 2026-11-25). It prints `queued job X, 2nd in line
  (waiting for room)` and follows the job through the queue and then in its box, since that
  process is there to hear when the job starts. Ctrl-C leaves the job queued: `box job X` follows
  it again, and `box cancel X` takes it off. No watcher or push is started for it.
- **The code is snapshotted when the job is queued.** The agent keeps editing after it hands the run
  off, so `box` sends the worktree as it is now. That covers uncommitted and untracked files, as a
  sync does: the snapshot is the git bundle `box run` already makes and pushes, with the commit at
  `refs/agent-box/<ref>`. It goes to `boxd`, which keeps it in
  `/var/lib/agent-box/queue/<job>.bundle` until the job starts, and the job checks it out the way a
  run does. A tar was the first plan. The bundle was picked when it was built (2026-11-24), because
  it reuses `box run`'s snapshot and checkout unchanged. The bundle is deleted when the job starts or
  is cancelled, and it outlives 24 hours only for the restart case below, so the PC stays compute, not storage. The cap is
  2GiB in all; a run that would exceed it isn't queued (507 `queue-full`) and gets exit 75.
- **Order: first in, first out, with one rule for fairness.** When a job can start, the next one is
  the oldest job from the session with the fewest jobs running. One agent's burst of ten can't hold
  back everyone else. Size matters only in that a job starts when its size fits: a small job can
  start ahead of a waiting large one if the large one doesn't fit yet.
- **The nudge.** A job that has waited 20 minutes gets one push: "still queued (why); run it locally
  and box_cancel <job> if you can't wait". The agent decides. Jobs queued over 24 hours are dropped,
  with a push saying so, except one whose start a restart cut short while its box is there: it may
  be running, so it stays until a start looks in the box or the box is reaped.
- **Cancel:** `box_cancel` removes a queued job and deletes its snapshot.
- **On the dashboard:**
  - Overview shows a Queue section: each job's reason, agent, size, how long it has waited and why
    it waits.
  - Controls: cancel, and move to front.
  - History counts queued jobs and their wait times, next to refusals.

## Where it lives

`boxd` owns the queue. It already sees every box, the mode and the caps, and it is the one place every
laptop and agent reaches.
- The queue is a JSON file per job in `/var/lib/agent-box/queue/`, plus its bundle. It is
  crash-safe: on start, `boxd` reloads it, and drops a job without its bundle or a bundle without its
  job. A job's file records when a start began, before anything is launched or run. A start cut
  short by a restart is then retried in the same box, unless the job's files are already in it, in
  which case it has started and is not run twice. Until that retry looks, such a job with its box
  there (or before `boxd` has listed the boxes) can't be cancelled from the queue.
- **Scheduling.** Each poll (3s), `boxd` works out what each job waits for: My PC, the GPU,
  or room for its size. A job on an existing running box needs no room. `boxd` starts at most one
  job at a time; the caps are read again on the next poll.
- **Starting.** `boxd` starts a queued job by doing what `box run` does. It launches the box if
  needed (a throwaway, or the repo's box with box new's 2h expiry) or reuses it, then pushes the
  bundle and the `.meta`, sets the 24h job expiry, and runs the same start script.
  - The start script, payload and launch flags live in `internal/jobrun`, which both `box` and
    `boxd` use, so a queued job looks like any other.
  - A launch the caps refuse leaves the job waiting. Any other failure drops it as `lost`, and a
    throwaway launched for it is deleted.
- **The job's name and key.** The name, `<box>.<id>`, is fixed when the job is queued; a
  throwaway's name gets its random suffix then. Its key changes when it starts:
  - while it waits, the key's time is `queuedAt`;
  - once it runs, the key's time is its real start, which `boxd` writes into the `.meta` as
    `startedAt`, next to `queuedAt`;
  - so the page gets `job-gone` for the waiting key, then `job` for the running one.
- **Throwaways.** A queued job's end may go unread (a push, or a CLI follower that was
  interrupted), so once that job ends and nothing else runs
  there, `boxd` gives a throwaway it launched 30 minutes to live, as `box run` does since #23, so
  `box pull` and the full log still work; the reaper deletes it then. A `box run` throwaway whose
  end nobody read (#53) gets the same 30 minutes, and its room can be taken like any kept one: its
  exit code and tail are in boxd's history, which `box job` gives once the box is gone (#90). A queued job that needs the room of kept throwaways (its own, or `box run`'s) gets
  it: boxd deletes them first, soonest to expire first, as `box`'s claimGPU deletes an idle GPU
  throwaway. Otherwise back-to-back queued jobs would wait 30 minutes each.
- `boxd` has no T3 access, so it doesn't push. The laptop's `box watch <job>` process (from the push
  design) follows the job while it is queued too, and sends the turn when it ends, or the nudge at 20
  minutes.

## API

The types are in `internal/dash` (`QueueRequest`, `QueueReply`, `QueuePage`). Every POST needs
`X-Agent-Box: 1`.
- **`POST /api/queue`** (multipart/form-data): a `request` part, then a `bundle` part.
  - `request` is `{box, new, size, gpu, checkout, ref, command, env, timeout, meta}`. `meta` is the
    `.meta` box would write.
  - It returns 201 with `{job, key, position, why}`.
  - A GPU run while the PC is in My PC gets 409 (`frozen`).
- **`GET /api/queue`:** `{jobs}`, the waiting jobs in the order they start.
- **`GET /api/queue/{job}`:** one job by its name, whether it is waiting, running or finished. This
  is so `box watch` can follow it through its start.
- **`POST /api/queue/{job}/cancel`** (204) and **`/front`** (200, the `Job`): controls. Both get 409
  `not-running` once the job has started, and cancel also while a job whose start a restart cut
  short may be running in its box.
- Queued jobs appear in `/api/state` as `Job.state: queued` with `queuedAt`, `position` and `why`
  (one of the refusal causes).
- `/api/history` buckets carry `queued: {jobs, started, medianWaitSeconds}`: the jobs queued in the
  bucket (cancelled and dropped ones too), and the queued jobs that started in it with their median
  wait.

## Order of work

It is built after the push design and `boxd`'s read side, both of which it depends on.
1. `boxd`: the queue store, the scheduler, the starter and the API.
2. `box`: queue on no room or pause, the snapshot upload, `box watch` following queued jobs, the
   nudge, and `queue: false`. Built in `cmd/box/queue.go`. At first only `box_run` queued, since
   only it could push; the CLI's `box run` queues since #93, following its own job.
3. The page: the Queue section and its controls. Overview and Jobs show the line in order, with
   each job's place, reason, agent, size, wait and why; a job past `QueueNudge` is marked "nudged".
   **Move to front** and **Cancel** (which asks first) are on each one, and a waiting job's own page
   says where it is in line. The `full` and `mypc` fixtures hold queued jobs, and the devserver
   plays the queue. History counts queued jobs beside the runs turned away, with their median wait.
