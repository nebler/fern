# Fern phone demo guide

This guide describes the current Fern demonstration path: submit a coding-agent
task from OpenCode, pair a phone to the private Fern host, inspect the run, and
retain its Git result.

It is intentionally a **pre-release operator guide**, not a promise that a
fresh laptop can run Fern. Read the [usage guide](usage.md) for the complete
configuration and security contract.

## What the phone experience is today

Fern does not currently ship a native app or a mobile run dashboard. The phone
flow pairs a browser with Fern over a private Tailscale HTTPS route. That paired
browser can access Fern's authenticated run APIs. The pairing page is suitable
for a short demo; a polished phone UI is a separate future feature.

## The demo you can honestly claim

After completing this guide and one real task, you can demonstrate:

1. A task is submitted against one committed revision of a repository.
2. Fern runs the agent in a disposable Docker container on a separate Linux
   host.
3. The phone pairs with the host through private Tailscale HTTPS.
4. You can inspect the live run and use the normal Fern client to attach and
   steer it.
5. You deliberately **Seal** the run, which stops the exact writer and retains
   its Git result; or **Stop** it, which cancels and cleans up without treating
   unfinished work as retained.

Do not claim that the phone proves hostile-code isolation, test success, PR
creation, or recovery after a host failure. Those are separate properties.

## Before starting

You need two machines.

| Machine | Purpose |
| --- | --- |
| Fern host | Native Linux amd64/arm64, kernel 5.14+, local Docker socket, repository clone, Fern state, and Tailscale. |
| Client and phone | A computer running OpenCode with the local Fern plugin, plus a phone on the same tailnet. The client may be macOS. |

The Fern host must also have all of the following before it can accept a real
run:

- An operator-provisioned XFS runtime filesystem with enforced project **byte
  and inode** quotas, separate from Fern's durable state.
- A deliberate solution for the current quota-query privilege requirement
  (`CAP_SYS_ADMIN`). Do not solve this by casually running Fern as root.
- A qualified Background Run image.
- A GitHub App installed only on the bound repository.
- A usable credential-free model provider for Fern's pinned runtime. A normal
  hosted-model API key cannot be injected into the container in the current
  profile.

Docker Desktop, remote Docker daemons, macOS execution hosts, and an ordinary
unbounded Docker volume are not supported substitutes.

## 1. Prepare Fern on the Linux host

Build Fern and its source-pinned Background Run image from the repository root:

```sh
go build -o ./fern ./cmd/fern
docker build -t fern/opencode-background-source:local images/opencode-background-source
BACKGROUND_IMAGE_ID=$(docker image inspect fern/opencode-background-source:local --format '{{.Id}}')
```

Create `fern.yaml` and `fern.env` with `fern init`, then complete GitHub App
onboarding. Important configuration values for the phone route look like this:

```yaml
tasks:
  runtimeStorageRoot: /var/lib/fern-runtime # provisioned XFS project storage
  backgroundRoute:
    listen: 127.0.0.1:8443
    origin: https://your-host.your-tailnet.ts.net:8443
proxy:
  listen: 127.0.0.1:8080
  operatorListen: 127.0.0.1:8081
  remoteOrigin: https://your-host.your-tailnet.ts.net
```

`remoteOrigin` must exactly match the Fern host's Tailscale HTTPS origin.
Never publish `operatorListen` (`127.0.0.1:8081`) through Tailscale Serve or a
public reverse proxy.

Start Fern:

```sh
./fern up --config fern.yaml --env-file fern.env
```

In another host terminal, check basic readiness:

```sh
./fern doctor --config fern.yaml --env-file fern.env --json
```

Readiness and configuration checks do not prove that a real agent can complete
a task. Before relying on the host, run the opt-in Linux qualification described
in [`integration/background-run-opencode/README.md`](../integration/background-run-opencode/README.md).

## 2. Publish only private Tailscale routes

With Tailscale running on the Fern host, publish the remote and background
listeners. Substitute the configured loopback listeners if you changed them.

```sh
tailscale serve --bg http://127.0.0.1:8080
tailscale serve --bg --https=8443 http://127.0.0.1:8443
tailscale serve status
```

Do not enable Tailscale Funnel. Fern's phone path is intended for a private
tailnet route.

## 3. Verify the phone path and pair the phone

On the Fern host, run:

```sh
./fern doctor --config fern.yaml --env-file fern.env --phone
```

For a stricter preflight, use `--field-demo` instead. It includes the phone
checks but still warns that it did not perform model execution or a GitHub
mutation.

When all checks pass, Fern prints a QR code and a one-time URL. The URL expires
after five minutes.

1. Ensure the phone is logged into the same tailnet and can reach private
   Tailscale devices.
2. Scan the QR code with the phone camera, or open the printed URL in its
   browser.
3. Review the page and choose **Pair this phone**.
4. Give the device a recognizable name.

Pairing creates a browser credential with a 30-day lifetime. Treat the paired
phone as an authenticated Fern client. Revoke it from the operator-controlled
device API if the phone is lost, shared, or no longer needed.

## 4. Submit a small, repeatable task

Use a separate small blog repository for the first demo. It should have a clean,
committed working tree and its exact revision must exist in the repository clone
bound to the Fern host.

On the client computer, build and install the local plugin as documented in
[`plugins/opencode/README.md`](../plugins/opencode/README.md). Open OpenCode in
the blog repository and choose `/fern` → **Run**.

Start with a narrow task, for example:

> Add a posts index page and its relevant test. Commit the work to a new branch,
> run the relevant tests, push the branch, and open a draft PR. Do not merge.

Fern reports successful admission when the task is durably accepted. Admission
does not mean the agent finished or that tests passed.

## 5. Inspect, steer, and finish deliberately

Use `/fern` → **Runs** to see the task. To open the existing remote OpenCode
session from the client computer:

```sh
fern runs --endpoint https://your-host.your-tailnet.ts.net
fern attach --endpoint https://your-host.your-tailnet.ts.net tsk_...
```

`attach` opens the existing agent session; it does not start a second writer.
Use it to inspect progress, answer questions, or steer the task.

When ready, choose one of these actions in `/fern`:

| Action | Meaning |
| --- | --- |
| Seal | Stop and fence the exact writer, retain the Git result, then clean up the disposable workspace. |
| Stop | Cancel and clean up. Do not use this as a way to retain unfinished work. |
| Result | Inspect retained commit, tree, change manifest, integrity facts, and cleanup status after sealing. |

Sealing does not itself push a branch, open a PR, or certify test success. Review
the resulting branch/PR and CI in the normal way.

## Demo checklist

Before recording or presenting the demo, collect evidence for each item:

- [ ] `fern doctor --phone` passes and produces a one-time pairing URL.
- [ ] The phone has successfully paired through private Tailscale HTTPS.
- [ ] The blog repository was clean and committed before submission.
- [ ] The task was admitted and a disposable run container was created.
- [ ] You inspected or attached to the live agent session.
- [ ] The agent's changes and test output were reviewed.
- [ ] The run was sealed and `Result` reports the retained Git artifact.
- [ ] Any pushed PR was compared with the sealed result.

If you want to claim interruption recovery, run and record a separate scenario.
State precisely whether Fern resumed, retried safely, or reported an uncertain
outcome; those are different capabilities.
