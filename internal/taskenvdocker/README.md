# taskenvdocker

See the [Go package map](../../docs/go-packages.md) and [maintenance review](../../docs/go-review.md).

Provider for one serial disposable Docker Background Run. It owns exact resource
attestation, clone/volume/container lifecycle effects, runtime transport, and scoped
GitHub credential delivery. It neither schedules work nor mutates taskstore state.

## Place in the architecture

```mermaid
flowchart LR
  cli["cmd/fern task services"] -->|"New / Close"| provider["Provider"]
  coord["backgroundruncoord"] -->|"lifecycle and export calls"| provider
  provider -->|"Docker API"| docker["local Docker daemon"]
  provider -->|"clone and attest"| git["local Git executable"]
  provider -->|"InstallationToken"| issuer["githubapp repository-scoped issuer"]
  provider -->|"New authenticated client"| oc["backgroundopencode"]
  provider -->|"NewTarget / RoundTripper"| route["backgroundroute"]
  provider -.->|"imports: durable run data, not store writes"| store["taskstore: schema 3"]
  provider -.->|"imports: runtime identity / spec 10"| run["internal/run"]
  docker -->|"private volume token files"| agent["OpenCode agent Git and gh tools"]
  agent -->|"agent-owned publication"| github["configured GitHub repository"]
```

Solid edges represent selected runtime calls/data delivery; dotted edges are imports.
Diagrams are representative, not exhaustive static callgraph analysis.
Current resource spec is 10. There is no host publisher, publication journal, or
host-side PR creation path in this provider. Credentials enable the agent's tools;
they are not Fern durable publication authority or a promise that publication occurs.

## Entrypoints and lifetime

| Entry family | Responsibility |
| --- | --- |
| `New` / `Close` | Validate trusted policy, private root/host key, qualified image, and owned clients. |
| `EnsureClone`, `EnsureVolume`, `EnsureContainer` | Create or positively attest the exact intended resource. |
| `StartContainer` | Start the expected container and return observed runtime identity. |
| `CommittedRuntime` | Validate the immutable container/start-time tuple in durable state. |
| `Health` | Bounded readiness probe with runtime attestation. |
| `OpenCodeClient`, `BackgroundRouteTarget` | Construct runtime-fenced authenticated HTTP access. |
| `RefreshGitHubCredentials` | Refresh/deliver an exact-repository token to the running process. |
| `ObserveUsage` | Attest and observe clone usage against configured bounds. |
| `StopContainer`, `ProveWriterInactive` | Prove the intended writer stopped/absent/never started. |
| `RemoveContainer`, `RemoveVolume`, `RemoveClone` | Remove only under explicit writer authority. |
| `AcquireExportSource` | Acquire exclusive filesystem clone lease after fresh inactivity proof. |
| `EnvironmentSHA256` | Deterministic digest of supplied environment values. |

Provider construction clones mutable configuration and can own a Docker client if
one is not injected. Close marks lifecycle closed and closes owned client resources;
it is not a substitute for ordered run cleanup and does not delete all Docker state.
Stop coordinator/route users before provider shutdown. Do not copy provider state
or live `ExportSource` leases; lease `Close` is mandatory and idempotent.

## Representative internal callgraph

```mermaid
flowchart TD
  new["New"] --> root["prepareRoot / loadExistingRoot"]
  new --> config["validateConfig / qualifyImage"]
  clone["EnsureClone"] --> lock["acquireCloneAuthority / acquireCloneLock"]
  clone --> attestclone["attestClone / attestRepository"]
  attestclone --> git["git"]
  ensure["EnsureContainer / StartContainer"] --> attest["attestContainer / requireRuntime"]
  health["Health"] --> once["healthOnce / requestHealth"]
  transport["routeTransport.RoundTrip"] --> fence["routeTransport.attest"]
  fence --> attest
  refresh["RefreshGitHubCredentials"] --> validate["validateRun / committedRuntimeFromRun"]
  refresh --> attest
  refresh --> token["InstallationToken / CopyToContainer / ContainerExec calls"]
  export["AcquireExportSource"] --> lock
  export --> fs["attestExportRoot / readExportClone"]
  export --> inactive["requireExportWriterInactive"]
  stop["ProveWriterInactive"] --> stopping["StopContainer"]
  remove["RemoveContainer / RemoveVolume / RemoveClone"] --> authority["validateCleanupAuthority"]
  remove --> absence["requireContainerAbsent / requireVolumeAbsent / deletion attestation"]
```

## Identity, clone, and resource policy

Private root state includes a host key and exact clone markers tied to run/spec
digest and filesystem device/inode identity. Initialization and recovery reject
unsafe links/modes or ambiguous authority instead of adopting a similarly named
resource. Git clone/config checks reject executable configuration and unsafe
administrative symlinks. Clone locking covers provider operations and export leases.

The source admission/disk-free and observed-clone limits are checks, not a kernel
filesystem quota. `ObserveUsage` walks files; bytes can grow between observations.
Git operations have configured time/output limits and controlled environment.
Clone recovery/deletion validates exact marker/path identity, including interrupted
rename/deletion cases; ambiguous trees are quarantined, not recursively guessed away.

Containers use the qualified image ID and expected labels/config, UID/GID 1001,
workspace bind mount, and private OpenCode named volume. The server port is exposed
only on loopback with an observed host port. Basic authentication derives from
host key and immutable run identity; credentials do not enter observation evidence.

Policy checks include bridge networking, private IPC/cgroup namespace, memory/swap,
CPU/PID limits, init, restart disabled, all capabilities dropped, and
`no-new-privileges`. The root filesystem is writable; bridge egress is unrestricted.
This is not an egress allowlist, read-only-root sandbox, or defense against a hostile
Docker administrator. New nonempty host environment maps are rejected specifically
because the worker has unrestricted egress; legacy digests remain useful for cleanup.

Health and route transports re-attest runtime identity, not merely container name
or port. A restarted process changes start-time/token/epoch identity and must not
inherit previous authority. Individual Docker/Git/health operations are bounded by
configured timeouts; overall workflow deadlines and retry scheduling belong upstream.

## GitHub credential refresh

The injected `githubapp.InstallationTokenSource` must mint for the configured
installation/repository ID and exact `https://github.com/owner/repo` remote.
Contents and pull-request permissions must both be `write`. Tokens must have more
than five minutes left, be at most 4096 bytes, and contain no NUL/CR/LF.
Nil token sources are permitted for hermetic setup/tests, not production credentials.

Refresh is serialized by `githubMu`. A successful in-memory lease records only spec
digest, runtime token, repository identity, and expiry—not the secret itself.
Even a lease hit first inspects/attests the exact live container. Refresh is due
five minutes ahead of expiry; failed refresh invalidates the lease and is not cached.
Provider restart has no durable credential cache and therefore mints afresh.

Token/repository staging files are tar-copied as mode 0600, UID/GID 1001, into the
private OpenCode volume at `/home/user/.local/share/opencode`. Docker exec renames
repository then token staging files into place. Each rename is per-file atomic,
not a two-file transaction. Runtime is re-attested around mutations, writes address
the immutable container ID, and exec completion/exit/container identity are checked.
Token bytes are not passed in exec arguments, labels, durable evidence, or error text.
Transient host memory/tar buffers necessarily contain token bytes; explicit zeroing
or protection against privileged Docker/host readers is not guaranteed.

The qualified image's `gh` and Git credential helpers read these private files.
A refresh failure prevents new prompt admission through the coordinator, but a
running agent may use its prior token until GitHub expiry. Stopping/deleting a local
container is not token revocation. Credentials have repository scope, not branch,
particular command, or individual PR scope; unrestricted egress permits exfiltration
by code entrusted with that token. No global rate-limit or publication guarantee exists.

## Writer fencing and export

`WriterFence` distinguishes never-created, created-never-started, and stopped exact
runtime authority. Constructors encode the variant; provider operations still
validate/prove current Docker state rather than trusting a name or constructor alone.
Cleanup may accept old execution resource policy where needed to safely remove
the exact old object; current execution policy is not silently adopted for running it.

`AcquireExportSource` validates root/marker/inode, holds the exclusive clone lock,
proves inactivity including exact-labeled replacement-container checks, and rechecks
filesystem authority immediately before returning. It performs no Git reads and
grants neither cleanup nor provider-root authority. Snapshot Git validation belongs
to `taskartifact`; the lock coordinates provider operations, not arbitrary host writers.
`Close` releases the lease and does not remove the clone.

`IdentityError` unwraps to both `ErrIdentityMismatch` and `ErrQuarantined`.
Failure to prove identity means operator/recovery attention, not permission to
adopt/delete a similarly named resource. `ErrProviderClosed` rejects relevant new
work after closure; cleanup sequencing remains the composition layer's responsibility.

## Imports and callers

Internal imports: `backgroundopencode`, `backgroundroute`, `githubapp`, `run`,
`task`, and `taskstore`. External imports: Docker API/client/errdefs, Docker NAT
types, OCI image spec, and `golang.org/x/sys/unix` for platform filesystem primitives.
Standard-library HTTP/exec/filesystem/crypto/tar facilities implement local effects.
Imports of taskstore record types do not mean runtime store method calls.

Direct production callers are `cmd/fern` and `backgroundruncoord`.
`integration/background-run-docker` and `integration/background-run-opencode`
exercise provider lifecycle, credential delivery, and composed runtime behavior.

## Naming review

- Keep `Ensure*` versus `attest*` versus `Remove*`: create-or-prove, pure observation,
  and destructive effects have meaningfully different authority requirements.
- `RefreshGitHubCredentials` accurately covers mint/delivery/lease reuse; it must not
  be called publication or revocation. `githubCredentialLease` deliberately holds metadata.
- `AcquireExportSource`/`ExportSource.Close` communicate ownership better than a raw
  path getter. `ProveWriterInactive` may stop a runtime; “prove” is not read-only here.
- `RuntimeCleanupAuthority` constructs a fence variant but is not itself a fresh
  inactivity proof. Keep callers using provider verification before destructive work.
- `password`, `git`, and `specDigest` are locally scoped implementation vocabulary;
  longer names would not improve security or remove external IO cost.

## Performance: local measured vs unmeasured

Provider performance is unmeasured. Credential tests use hermetic fakes;
fake-call timings would not measure Docker/GitHub latency.
Potential costs include clone walks, repeated Docker attestation, Git subprocesses,
and 10 ms exec-completion polling. A lease hit saves mint/copy/exec, not all IO.
These are source observations, not measured bottlenecks or optimization approval.

Run `go test ./internal/taskenvdocker` for local fake-Docker/filesystem tests.
Real daemon/image/credential integration requires the dedicated integration harness;
unit success alone does not establish runtime throughput or network isolation.
