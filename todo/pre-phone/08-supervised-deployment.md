# Task 08: Document A Supervised Private Deployment

## Commit

```text
docs: add supervised systemd and Tailscale deployment
```

## Purpose

Define one reproducible always-on host deployment so the phone experiment does not depend on an interactive SSH terminal.

## Dependencies

Author from task `00`. Final reboot verification depends on the integrated versioned binary, explicit origin, and authentication behavior.

## Owned Files

Create only:

```text
deploy/systemd/fern.service
deploy/systemd/fern.env.example
deploy/systemd/fern.yaml.example
docs/DEPLOYMENT.md
```

Do not edit README, root example config, production Go, or CI.

## Runbook Contract

Document:

- tested Linux host context;
- Docker installation and permissions;
- exact binary/image installation;
- dedicated config, environment, state and working-directory paths;
- restrictive secret permissions;
- loopback-only Fern listener;
- private Tailscale Serve/direct tailnet publication without Funnel;
- service enable/start/stop/restart/status and journal commands;
- graceful SIGTERM;
- retained repository, volume and pause-intent state;
- backup before destructive tests;
- uninstall versus explicit data deletion;
- local-Docker and trusted-repository assumptions.

The unit must use explicit paths, a separate environment file, a non-spinning restart policy, suitable Docker/network ordering, enough stop time for Fern shutdown, and no embedded secrets.

## Security Requirements

- Keep Fern on loopback when Tailscale Serve publishes it.
- Store the OpenCode password in a restricted environment file.
- Explain that Fern does not provide TLS.
- Explain the trusted-user/trusted-repository model.
- Exclude public Funnel from the first deployment.

## Acceptance

On the target host, install binary/image/config/unit, enable the service, verify local and tailnet access, reboot, verify retained state, stop cleanly, and verify documented uninstall retains data.

Task `10` records executed results. Do not claim reboot success from an unexecuted runbook.
