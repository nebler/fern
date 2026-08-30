# Fern Remote-Agent Wedge: Evidence And Bake-Off Appendix

**Research date:** 2026-08-30

**Companion report:**
[`fern-defensible-remote-agent-wedge-independent-audit-2026-08-30.md`](./fern-defensible-remote-agent-wedge-independent-audit-2026-08-30.md)

**Fern baseline:** `ab945b5a00db3a310b3fcc30fe8bc99669598b6f`, plus the
uncommitted working-tree documents present on the research date

This appendix makes the report's recommendation testable. It does not upgrade
documentation, source inspection, or a vendor claim into hands-on evidence.

## Verified During This Research

The following local checks passed on macOS arm64 with Go 1.24.2:

```text
go test ./...
go test -race ./...
```

The repository tests support Fern's implemented state transitions and fault
handling. They do not prove the physical Ubuntu, Tailscale, phone, provider,
Docker-restart, or live GitHub journeys.

The full OpenHands bake-off was **not run**. It requires a disposable remote
Ubuntu host, a phone path, a funded model-provider account, and a disposable
GitHub repository where response loss and cancellation can be injected safely.
The protocol below is the next product experiment, not a record of results.

## Decision To Test

Current decision:

> **Keep Fern as a personal appliance. Do not start Fern 2.0 or the k3s
> migration yet.**

Conditional product hypothesis:

> Run the real OpenCode on an always-on machine, re-enter the exact native
> session from any device, and retain a reconstructable Git result after the
> task environment is deleted.

The experiment must answer three questions:

1. Is OpenHands plus custom OpenCode ACP already good enough?
2. Does re-entering the official OpenCode UI matter during real weekly work?
3. Does independent exact-result retention change a recovery, review, or
   publication decision often enough to justify a product?

If the answer to the first question is yes, or the answers to the second and
third are no, stop expanding Fern.

## Pinned Bake-Off Inputs

Use immutable commits and image digests. The source pins reviewed in the report
were:

| Component | Version | Commit |
| --- | --- | --- |
| OpenHands Agent Canvas | `v1.16.0` | [`64c1269655012698bc66538967989996191beb6c`](https://github.com/OpenHands/OpenHands/commit/64c1269655012698bc66538967989996191beb6c) |
| OpenHands Agent Server/SDK | `v1.44.1` | [`9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a`](https://github.com/OpenHands/software-agent-sdk/commit/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a) |
| OpenHands Automation | `1.9.1` | [`e4535c85ea158068f554255c44c2bfcf616aa566`](https://github.com/OpenHands/automation/commit/e4535c85ea158068f554255c44c2bfcf616aa566) |
| OpenCode stable comparison | `v1.18.25` | [`cb7d8b2f5e44876ef98b661dc10590c915af3a9f`](https://github.com/anomalyco/opencode/commit/cb7d8b2f5e44876ef98b661dc10590c915af3a9f) |
| Fern production profile | `0.0.0-next-17444` | Image digest and OpenAPI hash from Fern configuration |

Before testing, confirm that these remain the intended pins. If a newer build is
selected, record its commit, package version, image digest, reported runtime
version, OpenAPI hash, and the reason for changing it. Do not mix observations
from two OpenCode V2 builds.

Use one small but real repository with:

- a deterministic setup command;
- a deterministic verification command;
- no production credentials;
- a disposable GitHub remote and GitHub App installation;
- two independent tasks that modify different files;
- one task that deliberately requires user input;
- one long tool command suitable for cancellation testing.

Use the same model, provider, prompt text, repository base commit, CPU/memory
limits, network, and pre-pulled images for both products.

## Evidence Directory

Create one directory per run and retain raw observations:

```bash
export RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
export OUT="$HOME/fern-bakeoff/$RUN_ID"
mkdir -p "$OUT"

uname -a > "$OUT/uname.txt"
docker version > "$OUT/docker-version.txt"
docker info > "$OUT/docker-info.txt"
git -C /path/to/test-repo rev-parse HEAD > "$OUT/base-commit.txt"
```

Record these files for every case:

```text
case.json                  immutable test specification
timeline.jsonl             monotonic and wall-clock timestamps
screenshots/               phone and desktop state transitions
http/                      sanitized request and response metadata
events/                    raw persisted and streamed event exports
containers/                inspect, stats, process trees, and logs
repository-before.txt      HEAD, tree, status, refs, remotes
repository-after.txt       HEAD, tree, status, refs, remotes
github-before.json         branch and PR authoritative reads
github-after.json          branch and PR authoritative reads
artifacts/                 archives, bundles, patches, manifests
operator-notes.md          manual recovery and uncertainty
```

`case.json` must contain the product and commit pins, image digests, repository
identity, base commit, prompt SHA-256, model, environment recipe digest,
conversation/session IDs, client device, start time, and fault injected.

Never retain provider keys, GitHub tokens, OpenHands secrets, OpenCode
credentials, prompt contents from private repositories, or raw environment
dumps.

## OpenHands Installation Profile

Use the OSS single-owner profile, not Enterprise, so the comparison matches
Fern's intended owner-operated product. Follow the pinned checkout's setup
instructions and retain the exact install command and generated configuration.

The default Helm profile is useful as a separate persistence test, but it is not
a per-conversation isolation profile. Its pinned README says that one pod and
one PVC commingle agents, and that isolated per-run containers are an Enterprise
capability. Do not describe the Helm result as universally representative of
all OpenHands backends.

Configure OpenCode through Agent Canvas's **Custom ACP server** path. Record the
exact command. The expected shape is:

```text
opencode acp
```

Add required working-directory arguments only if the selected OpenCode build
documents them. Confirm with a trivial prompt that the process speaks ACP over
stdio. OpenCode was not a built-in Canvas preset at the reviewed pins.

## Twenty-Step OpenHands Bake-Off

### 1. Installation And First Task

Start from a clean Ubuntu VM. Measure from the first install command until a
provider-funded task changes a file and the change is visible in Canvas. Record
every required package, service, port, database, secret, manual edit, restart,
and failed attempt.

### 2. OpenCode ACP Compatibility

Run a prompt that reads a file, edits it, executes a check, and explains the
diff. Record which OpenCode settings, plugins, skills, model choices,
permissions, questions, terminal details, tool events, and session metadata are
preserved, normalized, missing, or misleading in Canvas.

### 3. Native UI Availability

Attempt to open the task in the official OpenCode web UI or TUI without starting
a second unrelated session. Record whether a supported deep link exists and
whether Canvas and OpenCode show the same authoritative session. Do not count an
OpenCode subprocess as native UI access if the user cannot attach to it.

### 4. Two Concurrent Tasks

Start two tasks from the same exact base commit. Make them edit different files.
Record checkout paths, Git common directories, HOME/config/data paths, process
trees, ports, session IDs, and environment variables. Verify whether repository,
OpenCode state, credentials, caches, lock files, and terminal processes are
actually isolated.

### 5. Laptop Disconnect

Close the initiating browser and laptop network path for at least ten minutes.
Verify from the server that work continues. Reconnect without manually restarting
the conversation.

### 6. Phone Reconnect

Open Canvas through the documented Tailscale path on iOS or Android. Record
time-to-current-state, stale content, required refreshes, missing controls,
scroll/keyboard problems, and whether a background/foreground cycle loses the
live state.

### 7. Inspectability

From phone and desktop, inspect conversation, tool activity, files, diff,
terminal, process state, and Git state. Mark each surface as complete, partial,
or absent. Confirm important facts with server and Git reads rather than the UI
alone.

### 8. Steering And Input

Send guidance during an active turn, answer one permission or question, and
leave the browser again. Record whether input is admitted once, queued, applied
to the intended turn, and still answerable after reconnect.

### 9. Agent Canvas Restart

Restart only the Canvas/frontend process while execution continues. Confirm
whether persisted history plus replay returns to the current event without
duplicates or missing events.

### 10. Agent Server Restart

Kill Agent Server during a tool operation, then restart it with the same
persistent storage. Record conversation status, unmatched tool calls, surviving
child processes, late filesystem writes, and required user recovery. The pinned
source suggests persisted `RUNNING` execution is recovered as `ERROR`; verify
that behavior rather than assuming it.

### 11. Docker Or Host Restart

Restart Docker, then reboot the VM in a separate run. Record which services
restart automatically, what remains persisted, whether active work is resumed,
marked interrupted, or lost, and how long the UI takes to become truthful.

### 12. Cancellation During Provisioning

Use a deliberately slow image pull or setup command. Cancel before the
conversation runtime is fully recorded. Observe for five minutes. Verify whether
a late runtime starts, whether it can receive a prompt, and whether cleanup is
required manually.

### 13. Cancellation During A Tool Operation

Run a command that attempts a delayed write and observe the process tree before
and after interrupt. HTTP success or a paused conversation is not enough. The
test passes only if process inactivity is proven or the remaining writer is
clearly classified as uncertain.

### 14. Ambiguous Prompt Admission

Send a prompt containing a unique harmless marker through a proxy that forwards
the request but drops the response. Before retrying, query persisted events and
conversation messages. Then use the product's normal retry path. Count exact
user-message events and agent turns. Record whether the caller can reconcile
the first operation or creates a duplicate prompt.

### 15. Completion Classification

Run this matrix with distinct conversations:

| Case | Required ground truth |
| --- | --- |
| Clean committed change | Exact HEAD/tree/status and checks |
| Dirty-tree change | Exact changed and untracked paths |
| Explicit no-change finish | Base equals result and agent statement exists |
| Provider error | Error identity and partial repository state |
| Tool error after mutation | Tool error plus retained changed state |
| Budget/iteration stop | Limit and repository state |
| Input required | Pending request remains actionable or is explicitly stale |
| Interrupt during model stream | No false success |
| Interrupt during terminal command | Writer activity proven or uncertain |
| Server loss after mutation | No false success and useful work identified |

`IDLE`, event silence, socket EOF, missing backend, and container exit must never
be treated independently as successful completion.

### 16. Retain And Reopen

Finish a task, disconnect all clients for 24 hours, then reopen it from phone and
desktop. Record conversation, workspace, processes, terminal state, Git objects,
diff, credentials, and any backend-specific retention policy.

### 17. Delete Runtime And Reconstruct Result

Before deletion, create a commit plus dirty and untracked changes. Export every
artifact the product offers. Delete the conversation runtime/workspace, then use
a clean clone containing only the advertised retained artifacts. Check:

```bash
git fsck --full
git cat-file -e "$RESULT_COMMIT^{commit}"
git cat-file -e "$RESULT_TREE^{tree}"
```

The exact-result test passes only if all promised result objects can be
materialized and the reconstructed tree equals the recorded tree. A transcript,
commit hash, textual diff, or best-effort live tar is not equivalent.

### 18. Branch And Draft PR

Use the product's normal commit, push, and PR controls. Record which principal
holds the credential, whether the operation is a typed mutation or a natural
language instruction to the agent, the exact branch tip, PR head/base, author,
and all retry controls.

### 19. Lost GitHub Responses

Use a disposable proxy to forward one push or PR-create request and drop the
response. Do not repeat a real mutation until authoritative reads establish what
happened. Query:

```bash
git ls-remote origin "refs/heads/$BRANCH"
gh pr list --repo "$OWNER/$REPO" --head "$BRANCH" --base "$BASE_BRANCH" \
  --state all --json number,isDraft,headRefOid,url
```

Then invoke the normal recovery/retry path. Record duplicate PRs, branch drift,
manual repair, and whether the existing exact effect is adopted.

### 20. Backup And Replacement Host

Stop the original VM after taking only the documented backup. Restore onto a
clean VM with the same pinned software. Determine whether conversations,
workspaces, OpenCode state, exact result objects, pending attention, credentials,
and uncertain mutations can be inspected without contacting the old host.

## Fern Comparison Prototype

Only build this if the OpenHands test leaves a material native-OpenCode gap.
Keep the current persistent workspace untouched.

The prototype may add only:

- two deterministic Docker containers;
- two private exact-base checkouts;
- two distinct OpenCode state volumes and server ports;
- task-ID routing to each official OpenCode UI;
- persisted task, attempt, prompt, OpenCode, container, repository, and image
  identities;
- Fern restart reconciliation without prompt replay;
- explicit writer stop before collection;
- one Git bundle and manifest that materialize in a clean clone.

Do not add Kubernetes, Agent Sandbox, remote runners, a generic agent API, a new
conversation UI, a Gateway, previews, schedules, or native mobile applications.

The prototype is successful only if it demonstrates a user-visible result in
two weekends. Backend schema elegance is not an acceptance criterion.

## Performance Benchmark Protocol

No Fern-versus-OpenHands performance result exists yet. The measurements below
are required before using `faster`, `lighter`, `simpler`, or `lower maintenance`
in positioning.

### Test Discipline

- Use the same Ubuntu host, filesystem, Docker version, provider/model, network,
  repository base, and resource limits.
- Run at least five repetitions after one discarded warm-up.
- Report every sample plus median and p95; do not report only the best run.
- Separate image pull, repository checkout, project setup, OpenCode readiness,
  prompt admission, provider first token, and task completion.
- Report cold-cache and warm-cache results separately.
- Sample CPU, RSS, cgroup memory, process count, open ports, and disk every
  second. Include all required services, not only the frontend process.
- Run idle tests for at least 30 minutes and reconnect tests after at least ten
  minutes of client absence.
- Keep failed runs in the denominator and record operator interventions.

### Metric Definitions

| Metric | Start | Stop |
| --- | --- | --- |
| Installation time | Clean supported Ubuntu login | Product health check passes |
| First successful task | First install command | Provider-funded edit and check visible |
| Cold task start | Durable submit accepted with no image/cache | OpenCode/agent accepts exact prompt |
| Warm task start | Durable submit accepted with image/cache ready | OpenCode/agent accepts exact prompt |
| Reconnect | Browser navigation from disconnected client | Authoritative current task state rendered |
| Interactive UI open | User activates task deep link | Exact session accepts input |
| Restart recovery | Process receives termination | Every task has truthful recovered state |
| Backup | Backup request | Durable completion acknowledged |
| Restore | Clean replacement host begins restore | All promised retained state is inspectable |
| Disk growth | Pre-task retained bytes | Post-cleanup retained bytes |
| Operator burden | Fault begins | No manual repair remains |

Also report:

```text
required services
required exposed ports
persistent databases
persistent volumes/directories
idle CPU and memory
per-active-task CPU and memory
process count
upgrade commands and downtime
faults requiring SSH or database repair
```

### Claim Thresholds

These are product gates, not current claims:

| Proposed claim | Minimum evidence |
| --- | --- |
| Fast to install | Clean-host median under 15 minutes and no undocumented repair in five runs |
| Fast task start | Pre-pulled/local-repository warm p95 under 10 seconds to prompt admission |
| Fast reconnect | p95 under 3 seconds to truthful task state on the private network |
| Fast native takeover | p95 under 3 seconds after the task server is ready |
| Lighter than OpenHands | At least 30% lower total idle RSS and 30% fewer always-on processes on the same host |
| Easier to operate | Zero manual repair across restart/cancel/cleanup fault suite and at least half as many required services/datastores |
| Reliable recovery | Every accepted task reaches a truthful state within 30 seconds of Fern restart; no prompt replay |
| Exact result retention | 100% clean materialization after runtime and checkout deletion, including interrupted-export tests |

Failing a speed threshold does not necessarily kill the native-handoff
hypothesis. It does prohibit the corresponding claim.

## Product Acceptance And Kill Sheet

Run at least six real owner tasks over two weeks after the prototype works.

| Signal | Continue | Kill or narrow |
| --- | --- | --- |
| OpenHands ACP quality | Native OpenCode preserves at least two repeatedly used capabilities that Canvas does not | Canvas is equally good for the owner's work |
| Native attachment | Official UI opened for at least 25% of real tasks for meaningful steering/inspection | UI rarely opened or used only as a demo |
| Unattended yield | At least 60% produce a useful result without laptop-side repair | Fewer than 50% produce useful work |
| Personal recurrence | Owner submits at least six tasks and two concurrent pairs in two weeks | Use remains occasional or one-workspace interactive |
| Result retention | Runtime deletion recovery changes at least one real recovery/review decision | A pushed WIP branch is always sufficient |
| Restart safety | No accepted prompt lost or replayed; all tasks become truthful | Ambiguity requires manual DB repair or silent replay |
| Operations | Setup and environment work are a minority of task effort | Setup drift dominates the benefit |
| External proof | One other OpenCode user completes a real repository workflow by twelve weekends | No external installation completes |

Passing the engineering tests without passing recurrence and user-value tests
means **portfolio-quality personal appliance**, not standalone product.

## Key Primary Sources

### OpenHands

- [Agent Canvas architecture](https://github.com/OpenHands/OpenHands/blob/64c1269655012698bc66538967989996191beb6c/docs/architecture.md)
- [Custom ACP agents](https://docs.openhands.dev/openhands/usage/agent-canvas/acp-agents#custom-acp-servers)
- [Phone and tablet access](https://docs.openhands.dev/openhands/usage/agent-canvas/mobile-access)
- [OSS Kubernetes profile and isolation warning](https://github.com/OpenHands/OpenHands/blob/64c1269655012698bc66538967989996191beb6c/helm/agent-canvas/README.md)
- [Conversation status model](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/conversation/state.py#L48-L80)
- [Conversation leases](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/conversation_lease.py#L18-L164)
- [Conversation creation path](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/conversation_service.py#L1403-L1485)
- [Message request without operation id](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/conversation/request.py#L64-L75)
- [Agent-prompted Git publication controls](https://github.com/OpenHands/OpenHands/blob/64c1269655012698bc66538967989996191beb6c/src/utils/utils.ts#L389-L436)
- [Git and archive API](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/file_router.py#L165-L233)

### OpenCode And Protocols

- [OpenCode V2 API](https://opencode.ai/v2/docs/api/)
- [Durable prompt endpoint](https://opencode.ai/v2/docs/api/session/v2-session-prompt)
- [Durable inbox](https://opencode.ai/v2/docs/api/session/v2-session-inbox-list)
- [Experimental session log](https://opencode.ai/v2/docs/api/session/v2-session-log)
- [Documented wait endpoint](https://opencode.ai/v2/docs/api/session/v2-session-wait)
- [Source evidence that unfinished V2 mutations can return 503](https://github.com/anomalyco/opencode/commit/f5d20c580b605c638d417dd00d74110f08dcfbf2)
- [OpenCode ACP implementation at the comparison pin](https://github.com/anomalyco/opencode/blob/cb7d8b2f5e44876ef98b661dc10590c915af3a9f/packages/opencode/src/cli/cmd/acp.ts)
- [Agent Client Protocol](https://agentclientprotocol.com/protocol/overview)

### Competitive Products And Substrates

- [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent)
- [Cursor bring your own machine](https://cursor.com/docs/cloud-agent/bring-your-own-machine)
- [OpenAI Codex cloud](https://developers.openai.com/codex/cloud)
- [Claude Code Remote Control](https://code.claude.com/docs/en/remote-control)
- [Claude self-hosted environments](https://code.claude.com/docs/en/self-hosted-environments)
- [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent)
- [Google Jules documentation](https://jules.google/docs/)
- [Ona automations](https://ona.com/docs/ona/automations/overview)
- [Warp orchestration](https://docs.warp.dev/platform/orchestration/)
- [Devin Outposts](https://docs.devin.ai/cloud/outposts/overview)
- [Coder Agents](https://coder.com/docs/ai-coder/agents)
- [T3 Code `v0.0.35`](https://github.com/pingdotgg/t3code/tree/v0.0.35)
- [Orbit pinned source](https://github.com/jianghailong-xy/orbit/tree/aca97577e5e57cb2d6d39e503a035115e1cedd9c)
- [Warren pinned source](https://github.com/jayminwest/warren/tree/fe1071562ac957aacba39beba850ef00e10d879a)
- [Deputies pinned source](https://github.com/sidpalas/deputies/tree/60c7e186187839a52d56c23d57cf9e22fe9cd5b4)
- [OpenAI Symphony pinned source](https://github.com/openai/symphony/tree/8001b52e3062495a16e520e4ceaf8f9de868c4d0)
- [Daytona documentation](https://www.daytona.io/docs/)
- [E2B documentation](https://e2b.dev/docs)
- [Runloop documentation](https://docs.runloop.ai/)
- [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox)

### Repeated Pain Evidence

- [OpenCode stale mobile state after sleep/resume](https://github.com/anomalyco/opencode/issues/17769)
- [OpenCode sessions stuck after server restart](https://github.com/anomalyco/opencode/issues/19023)
- [OpenCode subagents stuck without timeout/retry](https://github.com/anomalyco/opencode/issues/11865)
- [Claude Remote Control loses iOS connection](https://github.com/anthropics/claude-code/issues/29726)
- [Claude background agents silently disappear](https://github.com/anthropics/claude-code/issues/63023)
- [Codex remote and local clients diverge](https://github.com/openai/codex/issues/23011)
- Additional linked clusters and observed comment/reaction counts are recorded
  in the companion report. Counts show recurrence in the reviewed corpus, not
  population incidence or willingness to pay.

## Remaining Unknowns

- Whether OpenCode custom ACP works reliably in the pinned Canvas profile.
- Which official OpenCode capabilities users materially miss through Canvas.
- Whether Agent Server interrupt stops nested OpenCode/tool processes.
- Whether OpenHands retries can duplicate an ambiguously admitted message.
- Exact runtime and workspace retention behavior for the selected OSS backend.
- Exact behavior after host reboot and clean-host restoration.
- Whether an OpenHands publication retry duplicates or adopts an existing PR.
- Whether users value Git-object retention beyond an early pushed branch.
- Whether the owner opens the native UI often enough for a weekly product.
- Same-host Fern-versus-OpenHands installation, latency, resource, and recovery
  measurements.

These unknowns are the reason for the personal-appliance verdict. They are not
permission to assume a gap and start a broad build.
