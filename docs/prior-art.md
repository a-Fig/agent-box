# Prior art for the fleet

Researched 2026-11-25 for the fleet (#159, #161-#171). Tyler's rule: don't reinvent the wheel;
steal ideas and credit where they came from. A fleet issue or PR that takes an idea names the
system here. Every fact below was read from the linked source on 2026-11-25; a claim that could
not be checked there is left out. [ideas.md](ideas.md) has single-host ideas (BOINC's suspend
settings, Slurm's fair share) and is not repeated here.

What sets agent-box apart from most of these: its hosts are personal machines, each with an
owner who comes first, there are a handful of them, and nothing is always on. Systems built for
a datacenter assume a central master and machines that exist to serve it, so the parts worth
taking are mostly the vocabulary and the small rules, not the architecture.

## Cycle scavenging: the owner comes first

**HTCondor** (University of Wisconsin) runs batch jobs on dedicated clusters and
on idle desktops. Its founders' rule: "we must be careful to provide tools for the owner of a
resource to set policies and even instantly retract a resource for private use"
([Thain, Tannenbaum, Livny 2005](https://www.chtc.cs.wisc.edu/doc/condor-practice.pdf)).

- *Owner policy as expressions on the host* (#166). Each machine's own config decides, through
  `START` (may a job start here now), `SUSPEND`/`CONTINUE`, `PREEMPT` (the job must go), then
  `WANT_VACATE` (graceful: wait up to `MachineMaxVacateTime`) or straight to `KILL`
  ([policy configuration](https://htcondor.readthedocs.io/en/23.0/admin-manual/policy-configuration.html)).
  A machine sits in the `Owner` state while `IS_OWNER` holds. Our My PC, freeze and stop are the
  same ladder; the names are worth borrowing, and a vacate grace period is the step we lack.
- *Matchmaking is two-sided* (#163, #171). A job ad and a machine ad each carry `Requirements` and
  a `Rank`, so a host can say which jobs it will take and which it prefers, not only the job
  ([Raman, Livny, Solomon 1998](https://www.chtc.cs.wisc.edu/doc/hpdc98.pdf);
  [ClassAds](https://htcondor.readthedocs.io/en/latest/classads/classad-mechanism.html)). The
  host's side is where #171's priority belongs: reported with its state, so every client agrees.
- *Match, then claim.* "A match is an introduction between two compatible entities, whereas a
  claim is the establishment of a working relationship" (1998 paper). The host re-checks when the
  job arrives, so a stale view only costs a retry. That is our pick from `box status` followed by
  boxd accepting or queueing.
- *Flocking* (#165). An agent its own pool can't serve advertises to a second pool and runs there
  (2005 paper, fig. 6): the closest precedent for moving a waiting job to a host that frees up.
- *Checkpointing.* A job that exits with its `checkpoint_exit_code` has its checkpoint saved and is
  restarted ([self-checkpointing](https://htcondor.readthedocs.io/en/latest/users-manual/self-checkpointing-applications.html)).
- Don't copy: the central negotiator and the ClassAd language. With a few hosts a struct of
  reported fields does the job, and there is no always-on machine to run a negotiator. Its
  `KeyboardIdle` start policy is automatic switching; ours is manual.

**BOINC** (Berkeley, volunteer computing) runs science jobs on volunteers' machines.

- *Pull, sized to what is idle* (#169). The client asks a project for work when its buffer runs
  low, and requests "enough jobs to fill the number of idle instances"
  ([client scheduling](https://github.com/BOINC/boinc/wiki/ClientSched)). If the shared queue
  ever comes, hosts ask for work this way; the server never pushes.
- *Owner limits as plain percentages* (#166): use at most N% of CPUs, N% of memory, N GB of disk,
  suspend while the owner is active ([computing preferences](https://github.com/BOINC/boinc/wiki/Computing-preferences)).
- Don't copy: `exclusive_app`, which suspends work while a named program such as a game runs
  ([client configuration](https://github.com/BOINC/boinc/wiki/Client-configuration)). Tyler ruled
  out game detection (2026-11-25); modes are chosen by hand.

## Spreading builds over a few machines

**distcc** sends C compiles from a client to a static list of volunteer daemons
([distcc(1)](https://raw.githubusercontent.com/distcc/distcc/master/man/distcc.1)).

- *The host list is the client's, ordered* (#161, #171): "distcc prefers hosts towards the start of
  the list", with `/LIMIT` capping jobs sent to each host (default four, "may be further restricted
  by the server"). This is the simplest form of #161 plus #171 and it has worked for decades.
- *Fall back only on accidents.* distcc retries locally after an "accidental" failure (network,
  unreachable host) and never after a genuine compile error. For us: an unreachable or paused
  host means try the next host; the command failing is the answer.
- Don't copy: `localhost` first in the list and run directly, not through a daemon. #167 says the
  agents' own machine is a host like any other.

**icecream** (icecc, SUSE) adds a central scheduler to distcc's idea
([README](https://github.com/icecc/icecream);
[scheduler](https://raw.githubusercontent.com/icecc/icecream/master/doc/icecc-scheduler.adoc)).

- *Per-host caps the owner sets* (#166): `--max-processes`, `--nice` (default 5) and `--no-remote`,
  which stops a node taking others' jobs ([iceccd](https://raw.githubusercontent.com/icecc/icecream/master/doc/iceccd.adoc)).
  `--no-remote` is the "client but not host" switch.
- *Ship the environment with the job.* `icecc-create-env` packs the compiler and libraries into a
  tarball so any node can run any client's compile. Our git bundle plus a common image is the
  same move.
- Don't copy: preferring the requester's own machine when it is "not less than 70% of the fastest"
  (#167 again), and the always-on scheduler.

**Nix remote builders** let `nix build` run derivations on other machines listed in the
`builders` setting ([nix.conf](https://nix.dev/manual/nix/latest/command-ref/conf-file)).

- *A host entry is one line* (#161): URI, systems, `maxJobs`, `speedFactor`, `supportedFeatures`,
  `mandatoryFeatures`. "If there are multiple machines of the right type, Nix prefers the fastest,
  taking load into account." `speedFactor` is #171's priority number.
- *Features both ways* (#163). A build needing `kvm` goes only to a machine that supports it; a
  machine with a mandatory feature takes only builds that ask for it. "GPU" fits the first, and
  "only GPU jobs on this host" the second.

## Remote build and test execution

**Google Forge / Blaze.** Google has run distributed builds since 2008; "Google's remote execution
system is called Forge", with a scheduler that answers from a cache of action results or queues the
action for a worker pool ([Bazel: distributed builds](https://bazel.build/basics/distributed-builds)).
Nothing public describes how it picks workers. Its open successor is:

**The Bazel Remote Execution API** (REAPI), a gRPC protocol implemented by Buildbarn, BuildBuddy,
EngFlow and others ([remote_execution.proto](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto)).

- *Platform properties, interpreted by the server* (#163). A job states needs ("OS", RAM); the
  server may run it "on any worker satisfying the requirements". Our size and `gpu` are platform
  properties; keep the vocabulary small and let each host decide what satisfies it.
- *Upload only what is missing.* `FindMissingBlobs` before uploading. Moving a large worktree to a
  new host (#159's "later") is cheap if only what the host lacks is sent; for us, a bundle that
  leaves out commits the host already has.
- *A job is a long-running operation you can reattach to.* `Execute` returns an operation name and
  `WaitExecution` reattaches to it, which is #164's job ID that carries its host.
- *Cache results by input.* The action cache answers repeated actions without running them. That
  needs hermetic actions; ours are arbitrary shell commands, so at most the "same tree, same
  command" replay in ideas.md.
- *Bazel's fallback modes.* `--remote_local_fallback` runs locally if remote fails, and dynamic
  execution races local against remote and keeps the first to finish
  ([dynamic execution](https://bazel.build/remote/dynamic)). Don't copy either into `box`: both
  treat the client as special (#167), and a race doubles the load on machines that have owners.

**Buildbarn** ([bb-remote-execution](https://github.com/buildbarn/bb-remote-execution)) is an
open REAPI server.

- *Workers pull* (#169). A worker calls `Synchronize()` with its state and platform; the scheduler
  answers with work or holds the call open "until more work is available", and a worker that
  misses its next sync time is treated as gone
  ([remoteworker.proto](https://github.com/buildbarn/bb-remote-execution/blob/master/pkg/proto/remoteworker/remoteworker.proto)).
  The model for a shared queue: hosts make only outbound calls and take work when their limits
  allow.
- *Learn the size* (later). Its feedback-driven size-class analyser runs an action on the smallest
  size that has worked, and on the largest at once if it failed recently
  ([scheduler.proto](https://github.com/buildbarn/bb-remote-execution/blob/master/pkg/proto/configuration/scheduler/scheduler.proto)).
  Our agents pick a size by hand; history in boxd could suggest one.

**BuildBuddy** ([RBE platform properties](https://www.buildbuddy.io/docs/rbe-platforms)).

- *Warm runners, with a bounded wait* (#163). `recycle-runner` keeps a runner for the next action
  and `runner-recycling-max-wait` sets "how long to wait for a recycled runner before also trying
  to schedule on another machine". That is our "prefer the host with warm caches" with a timeout,
  the number #163 needs.
- *Late binding* (#165). Enqueue a task on several workers and "the first worker to get to the task
  takes it" ([blog](https://www.buildbuddy.io/blog/distributed-scheduling-for-faster-builds/)). A
  cheaper alternative to moving jobs: queue on two hosts, cancel the other when one starts. It needs
  a start that can't happen twice, which boxd's cancel already gives.

**EngFlow** groups identical workers into pools that an action selects with a `Pool` property
([glossary](https://docs.engflow.com/glossary.html)). Nothing public on its queue policy.

**Gradle Develocity test distribution** splits test tasks over agents
([docs](https://docs.develocity.ai/test-distribution/)). Two rules worth taking: tasks state
requirements "matched against the capabilities of the agent pool", and if an agent disconnects the
work is rescheduled on another executor (#165 for a host that crashes mid-job). Don't copy: "local
executors are assumed to provide all required capabilities" (#167).

**GitHub Actions self-hosted runners** run workflow jobs on machines their owners register
([using runners](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/using-self-hosted-runners-in-a-workflow)).

- *Labels, all of them* (#163). "A self-hosted runner must have all four labels to be eligible";
  groups and labels combine. A good, small form for host capabilities beyond size and GPU.
- *Outbound only* (#168). "Self-hosted runners connect to GitHub to receive job assignments", and
  the host needs only outbound HTTPS ([communicating with runners](https://docs.github.com/en/actions/concepts/runners/communicating-with-self-hosted-runners)).
  A friend's machine behind NAT can host without opening a port; with Tailscale we have this
  already, but a pull queue (#169) would keep it without a tailnet.
- *Ephemeral* (`--ephemeral`): one job per runner, then it deregisters
  ([self-hosted runners](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)).
  Our throwaway boxes are the same idea.

**Jane Street.** Its public blog names Jenga (its earlier build system) and Dune, and mentions
"distributed builds" without describing them
([blog](https://blog.janestreet.com/how-we-accidentally-built-a-better-build-system-for-ocaml-index/)).
No public source found describes a Jane Street compute or job-routing system, so none is cited.

## Cluster schedulers

**Google Borg** runs Google's jobs on cells of tens of thousands of machines
([Verma et al. 2015](https://research.google.com/pubs/archive/43438.pdf)).

- *Feasibility, then scoring* (#163). "Feasibility checking, to find machines on which the task
  could run, and scoring, which picks one of the feasible machines." Our pick is the same two
  steps: can run now (room, not paused, GPU), then prefer.
- *Package locality is a score* (#163). Scoring includes "picking machines that already have a copy
  of the task's packages". Borg's precedent for preferring the host that holds the repo.
- *Spread or pack.* E-PVM "ends up spreading load across all the machines, leaving headroom" at the
  cost of fragmentation; best fit packs tightly and "penalizes any mis-estimations". With owners on
  every host, spreading (lightest-loaded feasible host) disturbs each owner least.
- *The master polls.* "The Borgmaster polls each Borglet every few seconds", which controls the rate
  of traffic and "prevents recovery storms". Our client polling each host's boxd (#162) is the same
  choice.
- Don't copy: priority bands and preemption between jobs, quota, and the scaling tricks (score
  caching, equivalence classes). They pay at thousands of machines and many teams.

**Omega** replaced Borg's single scheduler with several that share cluster state and commit
placements optimistically, retrying on conflict
([Schwarzkopf et al. 2013](https://cs.brown.edu/~malte/pub/papers/2013-eurosys-omega.pdf)). Several
`box` clients picking hosts on their own is this design without the shared store: each host's boxd
is the arbiter, and a client that loses the race is queued or picks again.

**Kubernetes** took Borg's lessons into open source
([Burns et al. 2016](https://sites.cs.ucsb.edu/~rich/class/cs293-cloud/papers/k8.pdf)).

- *Filter, score, random tie-break* ([kube-scheduler](https://kubernetes.io/docs/concepts/scheduling-eviction/kube-scheduler/)):
  "If there is more than one node with equal scores, kube-scheduler selects one of these at random."
  Equal-priority hosts (#171's default) should be broken at random, not by list order.
- *ImageLocality* "favors nodes that already have the container images"
  ([plugins](https://kubernetes.io/docs/reference/scheduling/config/)), as Borg's package locality.
- *Cordon is not drain* (#166). `cordon` marks a node unschedulable and leaves running pods; `drain`
  also evicts them ([drain](https://kubernetes.io/docs/tasks/administer-cluster/safely-drain-node/)).
  A host's "not now" needs both: take no new jobs, and optionally stop what runs.
- *Taints repel* (#171). A taint keeps pods off a node unless they tolerate it
  ([taints](https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/)): the
  shape of "use only when no other host can run the job" if a priority number ever isn't enough.

**Apache Mesos** offers resources to frameworks, which accept or reject them
([Hindman et al. 2011](https://www.usenix.org/legacy/event/nsdi11/tech/full_papers/Hindman_new.pdf)).
It cites *delay scheduling* (Zaharia et al. 2010): waiting "a limited time to acquire nodes storing
their data" gives "nearly optimal data locality with a wait time of 1-5s". The evidence for #163's
warm-cache preference having a short timeout. Offers themselves need a central master; don't copy.

**Slurm** schedules HPC clusters ([slurm.conf](https://slurm.schedmd.com/slurm.conf.html)).

- *Node `Weight`* (#171): "All things being equal, jobs will be allocated the nodes with the lowest
  weight which satisfies their requirements." One number per node, set in config, applied only
  among feasible nodes: what #171 asks for.
- *`Features` and `--constraint`* (#163): a node either has a feature string or not.
- *Drain states* (#166, [sinfo](https://slurm.schedmd.com/sinfo.html)): DRAINING ("will not be
  allocated additional jobs") then DRAINED, kept apart from DOWN (failure). Our status should say
  "paused by its owner" and "unreachable" as different things.
- *Power saving* (#167): nodes idle for `SuspendTime` are suspended by `SuspendProgram` and woken by
  `ResumeProgram` when work comes ([power save](https://slurm.schedmd.com/power_save.html)). The
  pattern for shutting WSL down when a host has no boxes.
- Don't copy: partitions, accounts, backfill. They solve contention among many users on shared
  iron.

**HashiCorp Nomad** ([scheduling](https://developer.hashicorp.com/nomad/docs/concepts/scheduling/scheduling)):
feasibility then ranking, hard `constraint` and soft `affinity`
([placement](https://developer.hashicorp.com/nomad/docs/concepts/scheduling/placement)), and node
*eligibility* apart from *drain*: an ineligible node takes no new work and keeps what it runs
([eligibility](https://developer.hashicorp.com/nomad/docs/commands/node/eligibility)). Clients
report their own attributes and `meta`, so the host describes itself (#162). Same lessons as
Kubernetes, with a single binary.

**Ray** schedules tasks across a cluster for Python workloads
([scheduling](https://docs.ray.io/en/latest/ray-core/scheduling/index.html)). Its default prefers
nodes "that have large task arguments local to avoid transferring data", but "if the locality
preferred node is not available, Ray may run the task somewhere else": locality is a preference,
never a reason to wait long. Custom resources (a named count on a node) are another form of
labels.

## Sandboxes for agents

**Cloudflare Sandboxes** (Sandbox SDK, on Cloudflare Containers) give an agent a Linux container
that belongs to one task, driven from a TypeScript API for "executing commands, managing files, and
exposing services". Each instance is "a microVM with its own kernel and network"
([overview](https://developers.cloudflare.com/sandbox/)). Generally available since 2026-04-13, on
the Workers Paid plan ([changelog](https://developers.cloudflare.com/changelog/post/2026-04-13-containers-sandbox-ga/)).
Pricing figures are not on those pages.

- *Preview URLs* (#190). A web server running in a sandbox opens at a URL, "so agents can run
  long-lived services and verify in-flight changes". That is the feature #190 takes; its limits
  (private, own origin, short-lived) are ours to add, since the pages read here don't describe
  them.
- *The API as a checklist* (#191). What the SDK offers is what its authors think agents expect:
  run commands and stream output, move files in and out, keep processes running, persistent code
  interpreters, interactive terminals, filesystem watching, and backup and restore of a workspace
  ([changelog](https://developers.cloudflare.com/changelog/post/2026-04-13-containers-sandbox-ga/)).
  Compare it with `box_run`, `box_job` and `box_pull`, and file a gap only where an agent hit it.
- *Credentials stay outside* (#192). For self-run agents (Claude Code, Codex, OpenCode, Pi) the
  Worker "adds the API token to each request, so the sandbox never receives it"
  ([coding agents](https://developers.cloudflare.com/sandbox/coding-agents/)). The rule for a
  cloud host: its keys live with the client, not in a box.
- *A paid sandbox as a host* (#192). When the owner's machines are full or paused, a rented
  sandbox is one more host with an explicit priority (#171) and a spending limit. It takes a host
  adapter for a non-Incus backend; the router treats it like any other host.
- Don't copy: the Worker-fronted design, where HTTP reaches an instance only through a Worker the
  customer writes. Our boxes are driven by `box` and the dashboard, and need no per-task code.

**Cloudflare Workers** run code in V8 isolates: "Instead of creating a virtual machine for each
function, an isolate is created within an existing environment"
([how Workers works](https://developers.cloudflare.com/workers/reference/how-workers-works/)).
Cloudflare's own 2018 post (updated 2026-07-15, numbers as first written) claims isolates start in
5 ms and use about 3 MB against 35 MB for a basic Node Lambda
([Cloud Computing without Containers](https://blog.cloudflare.com/cloud-computing-without-containers/)).

- Nothing to take for placement or limits: isolates fit thousands of short request handlers per
  machine, a job shape agent-box doesn't have.
- Don't copy: the isolate model. The same post says "An Isolate-based system can't run arbitrary
  compiled code"; code must be JavaScript, TypeScript or something that compiles to WebAssembly.
  Test suites, `go build` and `-race` need a shell, a toolchain and a full Linux, so a box stays a
  container. Cloudflare's own Sandboxes for coding agents are containers, not isolates.

## Lessons for #159

1. **Pick in two steps** (Borg, Kubernetes, Nomad): filter hosts that can run the job now, then
   score. Break ties at random (Kubernetes).
2. **Locality is a soft preference with a short wait** (Borg packages, Kubernetes ImageLocality,
   Mesos delay scheduling at 1-5 s, BuildBuddy `runner-recycling-max-wait`, Ray). Never queue on a
   warm host when a cold one can run now for long.
3. **Priority is one number per host, used only among feasible hosts** (Slurm `Weight`, Nix
   `speedFactor`, distcc's list order) (#171). Report it with the host's state, so every client
   agrees (HTCondor's machine-side `Rank`).
4. **The host owns its policy** (HTCondor `START`/`SUSPEND`/`PREEMPT`/`VACATE`, BOINC limits,
   icecream `--max-processes`/`--no-remote`) (#166). The client only reads it.
5. **Two kinds of "not now"** (Kubernetes cordon/drain, Nomad eligibility/drain, Slurm
   DRAINING/DOWN): no new jobs vs stop what runs; paused by the owner vs unreachable.
6. **The host re-checks on arrival** (HTCondor match then claim, Omega's optimistic commits): the
   client's view may be stale; a refused start goes to the next host or the queue.
7. **Capabilities as labels, all must match** (GitHub Actions, Nix features, Slurm features, REAPI
   platform), with Nix's mandatory features for "this host only takes GPU jobs".
8. **Fall back only on accidents** (distcc): unreachable or paused means try another host; a failed
   command is the result.
9. **If a shared queue comes, hosts pull over outbound connections** (BOINC, Buildbarn
   `Synchronize`, GitHub runners) (#169). Late binding (BuildBuddy) is a cheaper step first.
10. **Don't copy the datacenter**: no central master, no preemption between jobs, no quota, and no
    special case for the machine the agents work on (distcc localhost, icecream's 70% rule, Bazel's
    local fallback, Develocity's local executors) (#167).
