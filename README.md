# agentctl

`agentctl` hosts [`gitbridge`](cmd/gitbridge), a small security-focused
GitHub App installation-token broker. The broker is the counterpart to
the agent sandbox: it holds the GitHub App's long-lived private key on
a more-trusted host, so the agent never has direct access to it.

## What gitbridge does

The agent sandbox needs to call the GitHub REST API — to push branches,
open pull requests, fetch repository state — but should never hold the
GitHub App's permanent private key. If it did, a sandbox compromise
would let an attacker mint installation tokens for any repository the
App is installed on, with whatever permissions the App is granted.

`gitbridge` is the boundary between those two worlds:

- It listens on a Unix domain socket (no TCP listener, no network
  surface). The socket is created with mode `0660` by default.
- It accepts a JSON request containing a repository slug and the
  fixed `builder` permission profile.
- It validates that the slug is in the configured organisation and on
  the configured allowlist.
- It mints a fresh short-lived installation token (≤1 hour) scoped to
  that repository and the minimum builder permissions.
- It returns the token and its expiry to the caller over UDS.
- It writes a structured redacted audit line per request. The private
  key, the JWT, and the installation token never appear in any log.

## Trust boundary

```
┌────────────────────┐         ┌────────────────────┐        ┌────────────────────┐
│ Agent sandbox      │   UDS   │ gitbridge host     │  HTTPS │ api.github.com     │
│                    │ ──────► │                    │ ──────► │                    │
│ curl --unix-socket │         │ gitbridge binary   │        │ access_tokens API  │
└────────────────────┘         └────────────────────┘        └────────────────────┘
```

The only path across the trust boundary is the local UDS socket. The
broker never opens a non-TLS TCP listener; it speaks TLS to
`api.github.com` and exits otherwise.

## Build and install

The repository uses Go 1.19+ and has no third-party dependencies.

```
git clone https://github.com/simons-agent-space/agentctl.git
cd agentctl
go build -o bin/gitbridge ./cmd/gitbridge
```

For installation on the broker host (system user, UDS directory,
systemd unit, audit log), see [`docs/INSTALL.md`](docs/INSTALL.md).

## Configuration

The broker is configured by a single JSON file. See
[`docs/INSTALL.md`](docs/INSTALL.md) for the schema and an example.

The current configuration supports only the `builder` permission
profile. Requests for any other profile are rejected with
`PROFILE_NOT_ALLOWED`.

## Detailed documentation

- [`docs/INSTALL.md`](docs/INSTALL.md) — installation, configuration,
  update, uninstall.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — architecture, threat
  model, design choices, and unresolved risks.

## Security

See [`SECURITY.md`](SECURITY.md). This repository contains trusted
deployment-control software: do not commit secrets, private keys, or
production configuration. Security-sensitive changes require manual
review by the repository owner (see [`.github/CODEOWNERS`](.github/CODEOWNERS)).