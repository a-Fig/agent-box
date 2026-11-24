# The queue

Planned 2026-11-24. Tyler asked for it; the design below is Claude's, built first and reviewed by
Tyler afterwards.

## Why

Before the queue, a run with no room (the caps were full) or a paused PC (My PC or a game) got exit 75,
and the agent ran the command on the laptop, often the starving laptop that offloading exists to
spare. Once results arrive as a pushed turn (see the push design in DESIGN.md), an agent doesn't need
to sit and wait. So it can hand the run to a queue and keep working.

## Behaviour

- **A run that can't start now is queued, not refused.** `box_run` returns
  `state: queued`, with the job ID, the position ("2nd in line"), why it waits (no room, My PC,
  yielding to a game), and `push` as for a running job. The result comes by the same push when the
  job has run.
- **What is never queued:** these still get exit 75 at once, as before:
  - `queue: false` on `box_run`, for "I need the answer now or I'll run it locally";
  - a GPU run while the PC yields to a game or is in My PC: games can last hours, and GPU work is
    rarely urgent enough to wait out a game;
  - a session with no push (`push: false`): it couldn't hear when the job starts.
- **The code is snapshotted when the job is queued.** The agent keeps editing after it hands the run
  off, so `box` sends the worktree as it is now. That covers uncommitted and untracked files, as a
  sync does, packed as a tar of the tracked and untracked non-ignored files. It goes to `boxd`, which
  keeps it in `/var/lib/agent-box/queue/<job>.tar` until the job starts. The job runs on that
  snapshot. The tar is deleted when the job starts or is cancelled, and it never outlives 24 hours,
  so the PC stays compute, not storage. The cap is 2GiB in all; a run that would exceed it isn't
  queued and gets exit 75.
- **Order: first in, first out, with one rule for fairness.** When a job can start, the next one is
  the oldest job from the session with the fewest jobs running. One agent's burst of ten can't hold
  back everyone else. Size matters only in that a job starts when its size fits: a small job can
  start ahead of a waiting large one if the large one doesn't fit yet.
- **The nudge.** A job that has waited 20 minutes gets one push: "still queued (why); run it locally
  and box_cancel <job> if you can't wait". The agent decides. Jobs queued over 24 hours are dropped,
  with a push saying so.
- **Cancel:** `box_cancel` removes a queued job and deletes its snapshot.
- **On the dashboard:**
  - Overview shows a Queue section: each job's reason, agent, size, how long it has waited and why
    it waits.
  - Controls: cancel, and move to front.
  - History counts queued jobs and their wait times, next to refusals.

## Where it lives

`boxd` owns the queue. It already sees every box, the mode and the caps, and it is the one place every
laptop and agent reaches.
- The queue is a JSON file per job in `/var/lib/agent-box/queue/`, plus its tar. It is
  crash-safe: on start, `boxd` reloads it.
- `boxd` starts a queued job by doing what `box run` does: it launches or reuses the box, unpacks the
  snapshot as the checkout, and starts the job with the same `.meta`. The job then looks like any
  other.
- `boxd` has no T3 access, so it doesn't push. The laptop's `box watch <job>` process (from the push
  design) follows the job while it is queued too, and sends the turn when it ends, or the nudge at 20
  minutes.

## API

- **`POST /api/queue`** (multipart): the job's `.meta` fields, `size`, `gpu`, `new`, `timeout`,
  `env`, plus the tar. It returns `{job, position, why}`.
- **`GET /api/queue`:** the queued jobs, in order.
- **`POST /api/queue/{job}/cancel`** and **`/front`:** controls.
- Queued jobs appear in `/api/state` as `Job.state: queued` with `queuedAt`, `position` and `why`.

## Order of work

It is built after the push design and `boxd`'s read side, both of which it depends on.
1. `boxd`: the queue store, the scheduler, the starter and the API.
2. `box`: queue on no room or pause, the snapshot upload, `box watch` following queued jobs, the
   nudge, and `queue: false`.
3. The page: the Queue section and its controls.
