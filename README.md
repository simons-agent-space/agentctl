# agentctl

`agentctl` hosts two security-focused services that run alongside the
agent sandbox on a more-trusted host. Each service exposes a narrow
local API over a Unix domain socket; together they let the sandbox
interact with GitHub and drive application deployments without
holding long-lived secrets.

The two services are:

- **[`gitbridge`](cmd/gitbridge)** — a GitHub App installation-token
  broker. The agent sandbox asks `gitbridge` for a short-lived,
  repository-scoped token instead of holding the GitHub App's
  long-lived private key.
- **[`agentctld`](cmd/agentctld)** — a host-side deployment control
  plane. The agent sandbox drives deploys, inspects state, and rolls
  back managed applications through `agentctld` over a Unix domain
  socket.

Both services are intentionally narrow in attack surface and broad
in trust assumption: anyone who can connect to the socket has the
service's authority. The mitigation is filesystem permissions plus
the trust that the broker/daemon host is appropriately isolated
from untrusted workloads.

## What gitbridge does

The agent sandbox needs to call the GitHub REST API — to push
branches, open pull requests, fetch repository state — but should
never hold the GitHub App's permanent private key. If it did, a
sandbox compromise would let an attacker mint installation tokens
for any repository the App is installed on, with whatever
permissions the App is granted.

`gitbridge` is the boundary between those two worlds:

- It listens on a Unix domain socket (no TCP listener, no network
  surface). The socket is created with mode `0660` by default.
- It accepts a JSON request containing a repository slug and the
  fixed `builder` permission profile.
- It validates that the slug is in the configured organisation and
  on the configured allowlist.
- It mints a fresh short-lived installation token (≤1 hour) scoped
  to that repository and the minimum builder permissions.
- It returns the token and its expiry to the caller over UDS.
- It writes a structured redacted audit line per request. The
  private key, the JWT, and the installation token never appear in
  any log.

### gitbridge trust boundary

```
┌────────────────────┐         ┌────────────────────┐        ┌────────────────────┐
│ Agent sandbox      │   UDS   │ gitbridge host     │  HTTPS │ api.github.com     │
│                    │ ──────► │                    │ ──────► │                    │
│ curl --unix-socket │         │ gitbridge binary   │        │ access_tokens API  │
└────────────────────┘         └────────────────────┘        └────────────────────┘
```

The only path across the trust boundary is the local UDS socket.
The broker never opens a non-TLS TCP listener; it speaks TLS to
`api.github.com`. The broker's code scopes outbound traffic to
GitHub as the only intended outbound destination.

## What agentctld does

`agentctld` is the host-side control plane for application
deployments. It owns the full deployment pipeline: source checkout,
Docker image build, candidate container run with health check,
Caddy route promotion, deployment state persistence, and rollback
orchestration. The daemon runs every privileged operation through
the docker daemon and Caddy, but accepts no arbitrary host paths,
Docker arguments, shell commands, Caddy fragments, or container
names from the caller.

The daemon exposes a narrow local API over a Unix domain socket
(no TCP listener, no network surface):

- `GET /healthz` — liveness probe.
- `GET /v1/apps` — list managed apps.
- `GET /v1/apps/<app>/state` — persisted current and previous
  deployments.
- `GET /v1/apps/<app>/status` — persisted state plus live
  container status.
- `POST /v1/apps/<app>/deploy` — run a deploy for an approved
  commit and manifest.
- `POST /v1/apps/<app>/rollback` — roll back to the previous
  deployment.
- `POST /v1/inspect` — validate a manifest and resolve a commit
  without performing any side effect.

Every deploy argv is built from validated inputs: the app name
(matches a strict regex), a 40-lowercase-hex commit SHA, and a
manifest decoded with `DisallowUnknownFields`. A socket-level
attacker cannot escalate by injecting Docker arguments.

### agentctld trust boundary

```
┌────────────────────┐         ┌────────────────────┐  UDS   ┌────────────────────┐
│ Agent sandbox      │   UDS   │ agentctld host     │ ──────► │ dockerd            │
│                    │ ──────► │ (agentctld)        │         │ caddy              │
└────────────────────┘         │                    │         └────────────────────┘
                               │ git (HTTPS fetch)  │ ────► github.com
                               └────────────────────┘
```

The only path across the trust boundary is the local UDS socket.
There is no TCP listener on the daemon host. The daemon's
filesystem reads are limited to the canonical Caddyfile and the
managed fragment directory; its writes are limited to the socket,
the audit log, the source mirror, the state directory, the
persistent data directory, and the managed Caddy fragments.

Anyone who can connect to the socket can drive the full
deployment pipeline. There is no authentication beyond Unix-socket
filesystem permissions. See
[`docs/AGENTCTLD_INSTALL.md`](docs/AGENTCTLD_INSTALL.md#trust-boundary)
for the full discussion.

## Build and install

The repository uses Go 1.19+ and has no third-party dependencies.

```
git clone https://github.com/simons-agent-space/agentctl.git
cd agentctl
go build -o bin/gitbridge   ./cmd/gitbridge
go build -o bin/agentctld   ./cmd/agentctld
```

Both resulting binaries depend only on the Go standard library; the
repository has no third-party Go dependencies. Neither binary
embeds any host-specific paths, group names, or secrets.

- For installation of `gitbridge` on the broker host, see
  [`docs/INSTALL.md`](docs/INSTALL.md).
- For installation of `agentctld` on the daemon host (system user,
  UDS directory, systemd unit, audit log, Docker and Caddy wiring),
  see [`docs/AGENTCTLD_INSTALL.md`](docs/AGENTCTLD_INSTALL.md).

The two services are independent and may be installed on the same
host or on different hosts. They do not share state and do not
communicate with each other; the agent sandbox is the only client
of either, in the standard setup.

## Configuration

- `gitbridge` is configured by a single JSON file. See
  [`docs/INSTALL.md`](docs/INSTALL.md#configuration) for the
  schema and an example. The current configuration supports only
  the `builder` permission profile. Requests for any other profile
  are rejected with `PROFILE_NOT_ALLOWED`.

- `agentctld` is configured by `AGENTCTLD_*` environment
  variables, typically sourced from a systemd `EnvironmentFile`.
  See
  [`docs/AGENTCTLD_INSTALL.md`](docs/AGENTCTLD_INSTALL.md#configuration-every-agentctld_-variable)
  for the complete variable reference and a full example env file.

## Detailed documentation

- [`docs/INSTALL.md`](docs/INSTALL.md) — `gitbridge` installation,
  configuration, update, uninstall.
- [`docs/AGENTCTLD_INSTALL.md`](docs/AGENTCTLD_INSTALL.md) —
  `agentctld` installation, configuration, systemd unit, timeouts,
  Docker and Caddy wiring, trust boundary, known limitations.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — `gitbridge`
  architecture, threat model, design choices, and unresolved risks.
- [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) — `agentctld` deploy
  manifest format (`deploy.json`), source resolution, candidate
  container, Caddy promotion, deployment state, per-app persistent
  data, rollback orchestration, and the deployment orchestrator.
- [`docs/AGENT_DEPLOYMENT_WORKFLOW.md`](docs/AGENT_DEPLOYMENT_WORKFLOW.md) —
  mandatory `inspect → approve → deploy` workflow for any coding
  agent that prepares or performs an application deployment through
  `agentctld`.

## Security

See [`SECURITY.md`](SECURITY.md). This repository contains trusted
deployment-control software: do not commit secrets, private keys,
access tokens, credentials, or production configuration.
Security-sensitive changes require manual review by the repository
owner (see [`.github/CODEOWNERS`](.github/CODEOWNERS)).