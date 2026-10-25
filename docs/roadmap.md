# Roadmap: the fastest way to v1

**Status:** plan, 2026-10-18. This is the source of truth for the order of work.
- [architecture.md](architecture.md): what we build
- [engine-local.md](engine-local.md): the experiments (E0–E16)
- [design.md §21](design.md#21-roadmap-about-8-weeks-kubernetes-first): the demo script and the agent-sandbox contribution plan

**Local first, then remote.**
- **Phase 1, local (weeks 0–3):** everything is built and tested on the Mac, inside Docker: k3s runs in a privileged container with a loop-mounted btrfs image. Nothing is installed on the Mac itself. It is the **same code path** as remote (Kubernetes only); there is no local driver.
- **Phase 2, remote (from week 4):** the same Helm chart on a Linux VPS, for what a laptop can't show: gVisor (if it doesn't work locally), real timings, the Mac and phone as thin clients, days of sleep and wake. After `v0.1`, the same checks also run on EKS (week 4b, added 2026-10-21).
- **Rule after that:** develop locally, verify remotely.

**Why this is realistic:** earlier spikes already ran btrfs snapshots, rewinds and Postgres recovery in privileged Docker containers on this Mac (kernel 6.10, btrfs-progs 6.6), and k3s in Docker (spike 05).

**Two finish lines:**
- **MVP (`v0.1`, about 3.5 weeks):** in T3, "Revert files too" also brings back the database, and the box sleeps when idle. It works locally first (week 3), then on the remote machine (week 4). One person, one node, Claude only, one sample repo.
- **v1 (about 8 weeks):** golden starts, agent forks, CRDs, security, two nodes, crash tests at scale, the 2.5-minute demo for a Kubernetes lead, and first agent-sandbox PRs.

---

## 1. What the MVP is (and is not)

**The one promise:** *undo works for the whole environment, not just the code.*

**The 60-second MVP demo:**
1. Open a T3 thread (on the local lab in week 3, on the remote machine from week 4). The first message wakes a gVisor box with Postgres.
2. Claude writes a migration that drops `users.email`. The turn ends → a snapshot in milliseconds.
3. "Edit from here → Revert files too" → code **and** Postgres are back; `email` has its data again.
4. Leave it idle → the box is gone (0 MB RAM). Send a message → it wakes and keeps going.

**Not in the MVP:** golden snapshots, forks, previews through the front door, CRDs, a second node, Codex, Helm, crash tests at scale, admission policy.

---

## 2. Shortcuts that make it fast

These keep T3 changes to one small hook and skip whole components.

| Shortcut | How | Saves |
|---|---|---|
| **Agent inside the box without patching T3's exec** | T3 already has a **Claude binary path** setting (`ClaudeAdapterV2.ts:744-763`: a non-script path is spawned directly). Point it at a small `fern-claude` wrapper that runs `fern exec --stdio --env <from cwd> -- claude "$@"`. | No dependency on PR #15350 (`SandboxService`) and no exec patch |
| **Worktrees already on btrfs** | T3's **worktrees directory** setting (`worktreesDirectory.ts:10`) points into the pool (`/var/lib/fern/worktrees`). On the first baseline checkpoint, the hook calls `adopt`: Fern turns the folder into a subvolume (reflink copy + swap, milliseconds for a fresh worktree). | No T3 change to worktree creation |
| **Same paths everywhere** | The T3 pod mounts the pool at `/var/lib/fern`; each box mounts its worktree at the same path, plus T3's `~/.claude` (outside the worktree, so logins and transcripts never enter snapshots). | Claude history, resume and git all just work |
| **No golden yet** | The first wake runs `.agents/setup` cold (minutes, once per thread). | The setup runner and golden keys move to v1 |
| **No CRDs yet** | The daemon creates `Sandbox` objects directly. | The operator moves to v1 |
| **One node, one person** | Single-node k3s; you log in to Claude on the VM yourself. | Placement, cross-node routing, multi-user auth |
| **Wake = exec** | Every turn starts with `fern exec`, so the wake path is just "exec on a sleeping environment". | The front door's HTTP holding and previews move to v1 |
| **The CLI before T3** | Weeks 1–2 are driven by `fern …` inside the local lab (`docker exec fern-lab fern …`), no T3 needed. | You can test the engine before touching T3 |

**The only T3 patch in the MVP:** the checkpoint hook (~100 lines): `captureCheckpoint` → snapshot (and `adopt` on the first one), `restoreCheckpoint` → restore, `deleteCheckpointRefs` → delete. Run it as a patched build; propose it upstream later.

---

## 3. MVP, week by week

### Phase 1: local (Docker on the Mac)

**The local lab** (details in [engine-local.md §1](engine-local.md#1-the-dev-machines)):
- Docker Desktop (you start it), one privileged `rancher/k3s` container, a btrfs image file loop-mounted inside it
- Go builds and unit tests in a `golang` container
- the agent-sandbox controller and Fern's chart installed into that k3s

**Agent briefs:** weeks 0–1 are [briefs/iteration-1.md](briefs/iteration-1.md), the first thing to review.

### Week 0 (2 days, local): prove the riskiest piece
- **Build:** the local lab, the agent-sandbox controller, a minimal CSI node plugin.
- **Done when:** a `Sandbox` (runc) uses a btrfs subvolume through our CSI inline volume; Postgres runs in it; a host snapshot and restore work; `Suspended` → `Running` is timed. (E0, local)
- **Bonus:** try gVisor (`runsc`) in the lab. If it doesn't work in Docker Desktop, gVisor waits for week 4.
- **If it fails:** if agent-sandbox is the blocker, the driver creates plain pods (only `internal/driver` changes). If CSI bind mounts fail, stop and debug before anything else: this is the core.

### Week 1 (local): the engine, driven from the CLI
- **Build:**
  - `store`: create, snapshot, restore by swap, delete, `adopt`
  - daemon API on `/run/fern.sock`
  - `driver`: one `Sandbox` per environment
  - `csi`: mount by environment name
  - `fern-init`: exec with stdio and PTY, services from `services.yaml`
  - CLI: `fern create | exec | snapshot | restore | ls | rm`
- **Done when:** on the sample repo, `fern snapshot` → drop a table → `fern restore` → the data is back. `fern exec -it <env> -- claude` already works by hand (you log in to Claude inside the lab once). (E1, E4)

### Week 2 (local): safe and sleepy
- **Build:**
  - `actor`: one mailbox per environment; only mutations queue; generation number
  - `oplog`: restore, delete and adopt are logged steps, replayed on start; restore keeps the old workspace as a `pre-restore` snapshot instead of deleting it (architecture §7.3)
  - idle timer → `Suspended`; exec on a sleeping environment → `Running`, then run
  - basic `/metrics`, including a wake-duration histogram (it decides whether pre-wake is needed in week 7)
- **Done when:**
  - Postgres recovers from every snapshot taken under load (E2)
  - 50 goroutines can't break an environment, under `-race` (E5)
  - killing the daemon mid-restore always ends "before" or "after" and never loses the old workspace, over ~100 runs with the fake driver (E6-lite)
  - the box sleeps and wakes (E8, sleep part)

### Week 3 (local): T3 on top
- **Build:**
  - T3 in a k3s pod (1-replica StatefulSet), using the same base image as boxes and the pool mounted
  - the checkpoint hook in a patched T3 build: a failed snapshot doesn't fail the turn, and a restore without a snapshot is refused with the reason (architecture §7.2–7.3)
  - the hook's tests replay real `fern` output: Go tests write each command's output and exit code to `testdata/`, and the hook's fake `fern` answers from them and fails the test on a broken rule, such as a snapshot before `create` (copied from Delta's Claude Code plugin)
  - the `fern-claude` wrapper
  - the two T3 settings (Claude binary path, worktrees directory)
  - the T3 app on the Mac connects to the lab through a forwarded port
- **Done when:** the 60-second demo (§1) runs locally with a real Claude turn. (E10)

### Phase 2: remote

### Week 4 (first 2–3 days): go remote → tag `v0.1`
- **Build:** an 8 GB VPS; the same bootstrap script and Helm chart; gVisor RuntimeClass; pair the T3 app on the Mac (and phone) over SSH or Tailscale.
- **Done when:** the whole test suite and the 60-second demo pass on the VPS, with gVisor; real timings replace the Docker Desktop numbers.
- **Ship:** tag `v0.1`, record the clip, write a short README "how to try it".

### Week 4b (about 1 week, after `v0.1`): EKS as a second remote target
The VPS stays the cheap daily target. EKS proves Fern runs on a managed cluster where nodes come and go. The Go code only talks to Kubernetes, so this is node setup and packaging, not a rewrite.

- **Build:**
  - Terraform modules: `vpc`, `eks`, `fern-nodegroup` (launch template), `ecr`, `iam` (Pod Identity). Optional: ECR in a shared-services account, the cluster in a sandbox account.
  - A dedicated node group, labelled and tainted for Fern; the daemon and boxes tolerate the taint. Prefer instance-store NVMe (m6id, c6id) for the pool; a gp3 EBS volume also works.
  - Node user data: `mkfs.btrfs` on the pool disk and mount it at `/var/lib/fern`; install a pinned `runsc` (checksum-checked) and add the runtime to containerd through `nodeadm`'s NodeConfig instead of k3s's `config-v3.toml.d` (`lab/gvisor.sh`). The RuntimeClass stays the same.
  - Images in ECR with pinned tags, replacing `fern:dev` and `imagePullPolicy: Never` in `deploy/fern.yaml`.
  - Claude credentials from Secrets Manager or a Kubernetes Secret instead of a manual login.
- **Done when:** `terraform apply` + install → the `lab/blockers.sh` checks pass on EKS (gVisor box, Postgres snapshot and restore, exec stream), a table compares EKS timings with the local lab, then `terraform destroy`.
- **Line budget:** about 600 lines of Terraform and YAML; Go changes limited to config flags.
- **Stop conditions:** no moving environments between nodes (design note only), no Karpenter, no Helm if plain YAML works, no iteration-2 actor refactor on the same branch.
- **Write down what EKS adds that the lab hides:**
  - **Nodes are replaced** (AMI updates, spot reclaims, scale-in) and take the local pool with them. Design: snapshot on drain, `btrfs send` to S3, `btrfs receive` on another node.
  - **Pod density:** the VPC CNI gives every box a VPC IP, so ENI limits cap boxes per node; turn on prefix delegation.
  - **Egress:** box NetworkPolicy with an allowlist (VPC CNI network policy or Cilium).
  - **CSI mount authorization:** `NodePublishVolume` has none yet (iteration-1 report); on a shared cluster any pod could mount any environment. Check the pod's service account against the environment's owner.

### MVP risks to check early
| Risk | Check in | Fallback |
|---|---|---|
| CSI bind mount under gVisor | week 0 (local try), week 4 (remote) | runc until it works |
| `Sandbox` wake is too slow | week 0 | plain pods in the driver |
| Docker Desktop timings mislead (virtual disk: 1 in 10 snapshots took 200 ms–1 s in spike 01) | week 4 | trust only VPS numbers; locally, test correctness, not speed |
| The wrapper breaks Claude's stdio stream or T3's MCP tools (T3's MCP server may only listen on loopback) | week 1, by hand | `fern-init` forwards one port back to T3; or the MVP runs Claude in the T3 pod and only services in the box |
| `adopt` races with the agent's first writes | week 3 | adopt on worktree creation instead (a small extra T3 hook) |
| T3 holds files open in the worktree during restore | week 3 | restore only between turns (T3's flow already does this) |
| T3's terminal panel runs in the T3 pod, not the box | known | fine for the MVP; route it through `fern exec` in v1 |
| The EKS AL2023 AMI has no btrfs kernel module | week 4b, first step (`modprobe btrfs` on a node) | Canonical's Ubuntu EKS AMI (check btrfs there too) |
| `mount --make-rshared /` was a k3s-in-Docker workaround; EKS nodes may behave differently | week 4b | keep `Bidirectional` on `/var/lib/kubelet/pods`; add the workaround to user data only if mounts don't reach the box |

---

## 4. From MVP to v1 (weeks 4–8)

Develop each item in the local lab first, then verify it on the VPS. Two local k3s nodes (two containers) are enough to develop the second-node work before week 6.

| Week | Build | Done when |
|---|---|---|
| **4** (rest of the week) | golden snapshots (setup runner, golden key); the setup contract reads Amp's and Delta's `.agents/` files (`setup`, `prepare`, `resume`, `linked`, `.fern/` overrides); agent fork with the identity hook and `env_fork`; the front door for previews (hold, 503, one wake per burst, a URL for every port); **agent-sandbox stress test → first upstream docs PR** | a new thread starts from golden in well under 1 s plus pod start; a repo set up only for Delta starts unchanged; three forks each run Postgres on :5432; 20 concurrent requests → one wake (E3, E7, E9, E16) |
| **5** | thin operator: `Environment` / `EnvironmentClass` CRDs, status, finalizers; security: CSI mount authorization, admission policy, user namespaces, credential scan; Helm chart and `fern up` | `kubectl get environments` matches reality; every E14 attack is refused (E13, E14) |
| **6** | crash harness at scale (thousands of failpoints in CI); `fern-init` reattach; second node with `btrfs send` forks; Grafana dashboard | thousands of crashes, zero bad states; benchmark forks run on node 2 (E6, E8, E15) |
| **7** | measurements, failure analysis (daemon, box, node, disk full), record the 2.5-minute demo (design §21.1); bug-fix PRs to agent-sandbox; **pre-wake on thread open**, only if wake is slower than about 2 s: a small T3 hook sends "thread opened", Fern wakes in the background after about 2 s on the thread, at most 2 pre-wakes at a time, back to sleep after about 2 min without a message, never resetting the idle timer; lists, diffs and status never wake | every number in the demo comes from a script; pre-wake: a table of wake time hidden vs. wasted pre-wakes |
| **8** | buffer, blog post, upstream proposals (T3 checkpoint hook; agent-sandbox auto-suspend and snapshot provider, with Fern as working code) | tag `v1.0` |

---

## 5. Rules for staying fast
- **One vertical slice first.** Nothing gets generalized until the MVP demo works end to end.
- **One sample repo, Claude only.** Codex, devcontainer and `t3.json` support come after v1.
- **Every week ends with something you can run and show.** If a week slips, cut scope, not the demo.
- **Tests as you go:** unit tests on the fake driver run on every change; integration tests on the VM run before each tag.
- **Don't build what T3 or agent-sandbox already does.** Patch or contribute instead.
- **Cut list (after v1):** ZFS, Alchemy-style `.fern/env.ts`, Code Mode, portless, warm pools, memory snapshots, Kata / the `docker` class, webhooks, multi-person cells, the `SandboxService` backend.

---

## 6. Cost
- **Weeks 0–3:** €0 (local).
- **Weeks 4–5:** one 8 GB VPS, about €10/month.
- **Weeks 6–8:** plus a 4 GB second node, about €15–20/month in total.
- **Week 4b (EKS):** the control plane is about $0.10/hour (about $73/month) plus one node. Apply for a demo, destroy right after; never leave it running.
