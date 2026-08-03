# Installing agentctld

`agentctld` is the host-side deployment control plane that runs next
to `gitbridge`. It exposes a narrow local API over a Unix domain
socket: callers propose deploys, run them, inspect state, roll back
managed apps, and list what is managed. The daemon is the boundary
that decides which app, which commit, and which manifest become a
running container — every filesystem path is derived from trusted
host configuration, every candidate identity is checked against the
fixed naming rules before any side effect.

This document covers everything required to install `agentctld` on a
host: user and group setup, the systemd unit, every
`AGENTCTLD_*` configuration variable, the canonical Caddy root
config, and operational procedures (build, install, start,
health-check, log inspection, restart, upgrade, uninstall). It also
documents the timeout relationship and the trust boundary, and
lists every known limitation.

## What this document assumes

- A Debian or RPM-based Linux host with systemd 245 or newer.
- Go 1.19+ on the build host (for `go build` only; the binary has
  no runtime dependencies beyond glibc and standard kernel
  interfaces).
- Docker installed and managed by `docker.service` on the daemon
  host.
- Caddy installed and managed by `caddy.service` (or equivalent) on
  the daemon host. The Caddyfile the daemon validates against is
  the one Caddy itself is running.
- Outbound HTTPS access from the daemon host to `github.com` (for
  `git fetch origin/main`). If the host uses a proxy, configure
  Git's proxy settings at the system level (`/etc/gitconfig` or
  `http_proxy`/`https_proxy`).

The binary does not embed any production-specific paths, group
names, or secrets. The example paths and groups used throughout this
document are operator choices and can be substituted without
changing the daemon.

## Build

The repository uses Go 1.19+ and has no third-party dependencies.

```
git clone https://github.com/simons-agent-space/agentctl.git
cd agentctl
go build -o bin/agentctld ./cmd/agentctld
```

The resulting binary is self-contained: no CGo, no shared libraries,
no embedded paths.

## User and group setup

`agentctld` runs as a dedicated unprivileged system user. The user
must exist before the unit is started; systemd will not create it.

```
useradd --system --no-create-home --shell /usr/sbin/nologin agentctld
```

This creates:

- A system user (UID below the `SYS_UID_MAX` threshold) with no
  password, no home directory, and `/usr/sbin/nologin` as the
  shell so an attacker who gains code execution cannot obtain an
  interactive login.
- A primary group `agentctld` of the same name.

The user must also be in the `docker` group so the daemon can talk
to `/var/run/docker.sock`:

```
usermod --append --groups docker agentctld
```

The systemd unit declares this as `SupplementaryGroups=docker` so
the relationship is documented next to the unit, but systemd does
not maintain group membership. The `usermod` invocation above is
required either way; the unit directive is for human readers, not
a substitute for the actual `usermod`.

### Socket group

`AGENTCTLD_SOCKET_GROUP` is the group name or numeric GID that the
socket is chowned to after bind. The value must be a group that
the `agentctld` user is in; otherwise the chown(2) call fails with
`EPERM` and the daemon exits non-zero.

Two common choices:

- **agentctld** (the primary group). All callers must be in the
  `agentctld` group. Simplest, narrowest blast radius. Recommended
  for setups where only one client (e.g. an OpenClaw sandbox)
  connects.
- **A dedicated client group**, e.g. `agentctl-clients`. The
  `agentctld` user must be in this group too
  (`usermod --append --groups agentctl-clients agentctld`), and
  every client user must be in this group. Use this when several
  clients connect from different primary groups.

The socket is always created with mode `0660` (configurable via
`AGENTCTLD_SOCKET_MODE`); the group ownership is what gates
access, not the mode.

### Docker access and its security implications

`agentctld` invokes the Docker CLI to build images, start containers,
inspect state, and clean up. The CLI is a thin client: it
constructs an HTTP request to the docker daemon over
`/var/run/docker.sock` and streams the response back. The actual
container creation happens inside `dockerd`, which runs under its
own systemd unit and has its own privileges.

What this means in practice:

- **`agentctld` itself never touches mount namespaces, network
  namespaces, or capabilities.** It is a thin orchestrator. The
  standard hardening directives (`RestrictNamespaces=yes`,
  `CapabilityBoundingSet=` empty, `SystemCallFilter=@system-service
  ~@privileged @resources`) all apply without weakening the
  daemon's ability to drive Docker.
- **Anyone who can talk to the daemon socket can drive Docker
  through `agentctld`.** The daemon's deploy argv is built from
  validated inputs (the app name, a 40-lowercase-hex commit SHA,
  and a manifest decoded with `DisallowUnknownFields`); a
  socket-level attacker cannot escalate by injecting Docker
  arguments. They can, however, cause `docker run` to be called
  with arguments derived from any app name and commit they choose,
  within the bounds the daemon allows.
- **The `docker` group grants full control over the Docker
  daemon.** `agentctld` is in this group because it must be; the
  consequence is that any process running as `agentctld` can also
  talk to Docker directly. A future operator-facing service that
  adds a more sensitive capability should not be added to the
  `docker` group.

If the threat model treats local-to-root escalation as in-scope,
the Docker socket access is the dominant risk in this unit. The
trust boundary in this document applies regardless of which
hardening directives are applied: the socket is the trust
boundary.

## Filesystem layout

The unit uses systemd-managed directories for every persistent
path. The operator does not pre-create any of them.

| Purpose | Path | systemd directive | Ownership |
|---|---|---|---|
| Socket parent dir | `/run/agentctld/` | `RuntimeDirectory=agentctld` | `agentctld:agentctld` 0750 |
| Audit log dir | `/var/log/agentctld/` | `LogsDirectory=agentctld` | `agentctld:agentctld` 0750 |
| Persistent state | `/var/lib/agentctld/` | `StateDirectory=agentctld` | `agentctld:agentctld` 0750 |

Subdirectories of `/var/lib/agentctld/` are created by the daemon
on first use:

- `/var/lib/agentctld/state/` — per-app deployment state
  (`<app>.state.json`).
- `/var/lib/agentctld/data/` — per-app persistent data
  (`<app>/data/`, opt-in via manifest).
- `/var/lib/agentctld/sources/` — git bare mirrors and detached
  checkouts (`<app>.git/`, `<app>-checkouts/<commit>/`).
- `/var/lib/agentctld/caddy/` — managed Caddy fragments
  (`<app>.caddy`).

The Caddy canonical Caddyfile is **not** created by systemd. The
operator must install it (typically at `/etc/caddy/Caddyfile`)
and import the managed fragment directory. See [Caddy root
config](#caddy-root-config).

## Configuration: every AGENTCTLD_* variable

Configuration is supplied via a single `EnvironmentFile`. The
example unit points at `/etc/agentctld/agentctld.env`. The file is
read once at startup; changes require `systemctl restart
agentctld`.

The daemon validates every variable at startup and exits non-zero
with a specific missing-key message if a required variable is
unset. Unknown `AGENTCTLD_*` variables are passed through to the
environment but ignored by the daemon — typo'ing a variable name
silently turns it into a no-op.

### Complete example: `/etc/agentctld/agentctld.env`

Replace every value below with the operator's choice. This file
**must not be committed**: it is host configuration.

```
# ─── Socket ───────────────────────────────────────────────────────
# Absolute path of the Unix domain socket. The parent directory
# (/run/agentctld/) is created by systemd's RuntimeDirectory
# directive; the daemon refuses to start if the parent is missing.
# The daemon binds, chmods (default 0660), and chowns (to
# AGENTCTLD_SOCKET_GROUP) the socket after creation.
AGENTCTLD_SOCKET_PATH=/run/agentctld/socket

# Group name or numeric GID the socket is chowned to after bind.
# The agentctld user must be in this group or the chown call
# fails with EPERM and the daemon exits non-zero. Empty means
# "leave the group as the daemon process's primary group".
AGENTCTLD_SOCKET_GROUP=agentctld

# Socket mode in octal (default 0660). Accepts 660, 0660, 0o660.
# The group ownership is what gates access; mode 0660 keeps the
# socket reachable by the daemon's primary group and the socket
# group while excluding "other".
AGENTCTLD_SOCKET_MODE=0660

# ─── Operation timeouts ───────────────────────────────────────────
# Per-operation wall-clock budget for deploy/rollback. The HTTP
# WriteTimeout is derived from this value plus
# AGENTCTLD_WRITE_TIMEOUT_MARGIN. Default 30m. Must be positive.
#
# systemd's TimeoutStopSec MUST be strictly greater than
# AGENTCTLD_OPERATION_TIMEOUT + AGENTCTLD_WRITE_TIMEOUT_MARGIN.
# See [Timeouts](#timeouts).
AGENTCTLD_OPERATION_TIMEOUT=30m

# Extra headroom added to AGENTCTLD_OPERATION_TIMEOUT to derive
# the HTTP WriteTimeout. Default 5m. Setting this to "0" disables
# the headroom so WriteTimeout equals OperationTimeout exactly.
# The HTTP server's WriteTimeout must be at least as long as the
# longest operation the daemon runs so a 30-minute deploy can
# write its response. Must not be negative.
AGENTCTLD_WRITE_TIMEOUT_MARGIN=5m

# ─── Audit log ────────────────────────────────────────────────────
# Path for the structured JSON audit log. Empty (default) writes
# to stderr; journald captures stderr under the unit's standard
# error stream. The example below writes to a dedicated file so
# logrotate-style tooling can manage it independently. The
# directory's owner must match the agentctld user; the daemon
# opens the file with mode 0640.
AGENTCTLD_AUDIT_LOG=/var/log/agentctld/audit.log

# Max request body in bytes (default 65536 = 64 KiB). Every JSON
# request body is well under this; the cap is defense-in-depth.
AGENTCTLD_MAX_REQUEST_BYTES=65536

# ─── Source layer ─────────────────────────────────────────────────
# Trusted host path under which git bare mirrors and detached
# checkouts live. The daemon creates subdirectories on first use;
# mode is 0755 inside the daemon. The operator should not put any
# other content under this path.
AGENTCTLD_SOURCE_REPOSITORY_ROOT=/var/lib/agentctld/sources

# Trusted remote URL. The daemon validates that the bare mirror's
# `remote.origin.url` matches this on every deploy; a mismatch is
# fatal. Use the form `https://github.com/<org>/<repo>.git`.
AGENTCTLD_SOURCE_ORIGIN_URL=https://github.com/<your-org>/<your-repo>.git

# Expected GitHub organisation. Every deploy's `app` value is
# matched against this in addition to the app-name regex. The
# bare mirror's origin URL must belong to this organisation.
AGENTCTLD_SOURCE_ALLOWED_ORG=<your-org>

# ─── Runtime layer ────────────────────────────────────────────────
# Inclusive localhost port range the runtime may allocate for
# candidate containers. Both values in [1024, 65535];
# AGENTCTLD_RUNTIME_PORT_RANGE_START must not exceed
# AGENTCTLD_RUNTIME_PORT_RANGE_END. Containers publish only to
# 127.0.0.1; no public port is ever opened.
AGENTCTLD_RUNTIME_PORT_RANGE_START=40000
AGENTCTLD_RUNTIME_PORT_RANGE_END=40099

# Total health-check budget for a candidate container. The runtime
# polls http://127.0.0.1:<host-port><health_path> every 100ms
# (with backoff) until it succeeds or the budget expires. Must be
# positive. Choose a value comfortably larger than the slowest
# expected app startup.
AGENTCTLD_RUNTIME_HEALTH_TIMEOUT=30s

# Trusted host path used to validate that the source layer's
# checkout path is under the expected root. In a standard setup
# this is the same path as AGENTCTLD_SOURCE_REPOSITORY_ROOT.
AGENTCTLD_RUNTIME_REPOSITORY_ROOT=/var/lib/agentctld/sources

# ─── Caddy layer ──────────────────────────────────────────────────
# Parent domain. The hostname for a managed app is derived as
# "<app>.<AGENTCTLD_CADDY_BASE_DOMAIN>". The Caddy canonical
# Caddyfile and the daemon's managed fragment directory must
# agree on this value.
AGENTCTLD_CADDY_BASE_DOMAIN=apps.example.com

# Directory where the daemon writes per-app managed Caddy fragments
# (<app>.caddy) and transient staging files (.partial, .bak). The
# canonical Caddyfile must `import` from this directory; see
# [Caddy root config](#caddy-root-config). The daemon creates
# subdirectories on first use with mode 0755.
AGENTCTLD_CADDY_CONFIG_DIR=/var/lib/agentctld/caddy

# Path to the canonical Caddyfile Caddy is running. The daemon
# only reads this file; every `caddy validate` and `caddy reload`
# invocation targets it. Read access by the agentctld user is
# required; see [Caddy root config](#caddy-root-config).
AGENTCTLD_CADDY_ROOT_PATH=/etc/caddy/Caddyfile

# Path to the caddy binary (default "caddy"). Override if the
# caddy binary lives outside PATH, e.g. /usr/local/bin/caddy.
AGENTCTLD_CADDY_BINARY=/usr/bin/caddy

# ─── State layer ──────────────────────────────────────────────────
# Directory where per-app state files (<app>.state.json) are
# persisted. The daemon writes atomically (CreateTemp + fsync +
# rename) and rejects symlinks before every read, write, and
# delete. Subdirectories are not allowed; one file per app.
AGENTCTLD_STATE_DIR=/var/lib/agentctld/state

# Parent domain used to validate the persisted Hostname field of
# every deployment record. Must equal
# AGENTCTLD_CADDY_BASE_DOMAIN; the daemon rejects mismatches.
AGENTCTLD_STATE_BASE_DOMAIN=apps.example.com

# ─── Data layer ───────────────────────────────────────────────────
# Persistent per-app data root. When a manifest opts into the
# data mount (manifest.version >= 2 and manifest.data.mount=true),
# the daemon creates <AGENTCTLD_DATA_ROOT>/<app>/data/ before the
# candidate container starts and bind-mounts it at /data inside
# the container. Survives every normal operation (deploy,
# rollback, container recreation); only an explicit operator
# action deletes it.
AGENTCTLD_DATA_ROOT=/var/lib/agentctl/data
```

### Variable reference

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `AGENTCTLD_SOCKET_PATH` | yes | — | Absolute path of the UDS listener |
| `AGENTCTLD_SOCKET_GROUP` | no | (primary group) | Group name or GID for socket |
| `AGENTCTLD_SOCKET_MODE` | no | `0660` | Socket mode in octal |
| `AGENTCTLD_OPERATION_TIMEOUT` | no | `30m` | Per-operation budget |
| `AGENTCTLD_WRITE_TIMEOUT_MARGIN` | no | `5m` | Headroom for HTTP WriteTimeout |
| `AGENTCTLD_MAX_REQUEST_BYTES` | no | `65536` | Max request body bytes |
| `AGENTCTLD_AUDIT_LOG` | no | stderr | Path for structured audit log |
| `AGENTCTLD_SOURCE_REPOSITORY_ROOT` | yes | — | Source mirror + checkouts |
| `AGENTCTLD_SOURCE_ORIGIN_URL` | yes | — | Trusted origin URL |
| `AGENTCTLD_SOURCE_ALLOWED_ORG` | yes | — | Expected GitHub org |
| `AGENTCTLD_RUNTIME_PORT_RANGE_START` | yes | — | Inclusive port range start |
| `AGENTCTLD_RUNTIME_PORT_RANGE_END` | yes | — | Inclusive port range end |
| `AGENTCTLD_RUNTIME_HEALTH_TIMEOUT` | yes | — | Total health-check budget |
| `AGENTCTLD_RUNTIME_REPOSITORY_ROOT` | yes | — | Trusted host checkout root |
| `AGENTCTLD_CADDY_BASE_DOMAIN` | yes | — | Parent domain for app hostnames |
| `AGENTCTLD_CADDY_CONFIG_DIR` | yes | — | Managed fragment directory |
| `AGENTCTLD_CADDY_ROOT_PATH` | yes | — | Canonical Caddyfile |
| `AGENTCTLD_CADDY_BINARY` | no | `caddy` | Path to caddy binary |
| `AGENTCTLD_STATE_DIR` | yes | — | Per-app state directory |
| `AGENTCTLD_STATE_BASE_DOMAIN` | yes | — | Parent domain for state |
| `AGENTCTLD_DATA_ROOT` | yes | — | Persistent app data root |

## Caddy root config

`agentctld` does not own the canonical Caddyfile. The operator
installs one and ensures Caddy runs it. The canonical Caddyfile
must do exactly two things for the daemon to work:

1. Set the global options the operator wants (admin listener,
   email, logging, etc.).
2. Import the managed fragment directory using the exact glob
   `import <AGENTCTLD_CADDY_CONFIG_DIR>/*.caddy`.

The import glob matches only the final `<app>.caddy` fragments the
daemon writes. The daemon's transient staging files (`<app>.caddy.partial`,
`<app>.caddy.bak`) are **never** part of the import glob, so a stale
or in-flight partial or backup cannot leak into the running
configuration. See `docs/DEPLOYMENT.md#caddy-promotion` for the
exact staging dance and its safety properties.

### Example canonical Caddyfile

```
{
    # admin off  # leave on for the admin API; required by caddy reload
    auto_https off
}

import /var/lib/agentctld/caddy/*.caddy
```

Adjust the admin and TLS settings to the operator's policy. The
two important properties are:

- The `import` glob is exactly `<config-dir>/*.caddy`. Do not use a
  broader pattern (e.g. `*.caddy*`) that would pick up `.partial`
  or `.bak` files.
- The path in the `import` directive must equal
  `AGENTCTLD_CADDY_CONFIG_DIR`.

### Caddy user / agentctld user interaction

`agentctld` invokes `caddy validate --config <Caddyfile>` and
`caddy reload --config <Caddyfile>` as the `agentctld` user. Two
consequences:

- The canonical Caddyfile and every managed fragment must be
  readable by the `agentctld` user. If the Caddyfile is owned by
  `root:caddy` with mode `0640`, the `agentctld` user cannot read
  it. Options:
  - Make the Caddyfile world-readable (`chmod 0644`).
  - Add the `agentctld` user to the `caddy` group and grant group
    read access (`chmod 0640`).
  - Run Caddy under the `agentctld` user instead.
- The managed fragments directory (`AGENTCTLD_CADDY_CONFIG_DIR`)
  is owned by `agentctld:agentctld` with mode `0750`. The
  long-running `caddy` process (started by `caddy.service`, which
  typically runs as `caddy`) must be able to read this directory
  so it can serve the routes. Options:
  - Add the `caddy` user to the `agentctld` group
    (`usermod --append --groups agentctld caddy`). The group can
    then traverse and read fragments.
  - Widen the directory mode to `0755` (world-readable).

Pick one and document it alongside the Caddyfile.

## Hardening

The unit at `systemd/agentctld.service.example` applies the
following hardening directives. Each one has been verified
against the daemon's actual requirements; the daemon does not
need anything that is restricted, so the directives are not
weakening its functionality.

| Directive | Effect | Why it is safe |
|---|---|---|
| `NoNewPrivileges=yes` | Cannot regain privileges via setuid | Daemon never needs them |
| `ProtectSystem=strict` | Whole filesystem read-only except API subtrees | All writes are under systemd-managed dirs (RuntimeDirectory, LogsDirectory, StateDirectory, ConfigurationDirectory) |
| `ProtectHome=yes` | `/home`, `/root`, `/run/user` inaccessible | Daemon reads no home files |
| `PrivateTmp=yes` | Isolated `/tmp` namespace | Daemon writes no temp files |
| `PrivateDevices=yes` | Restrict device nodes | Only `/dev/null`, `/dev/random`, `/dev/urandom`, `/dev/zero`, `/dev/full` needed |
| `ProtectKernelTunables=yes` | `/proc` and `/sys` tunables read-only | Daemon reads `/proc`/`/sys` only |
| `ProtectKernelModules=yes` | Cannot load kernel modules | Daemon loads none |
| `ProtectKernelLogs=yes` | Cannot read kernel log buffer | Daemon reads none |
| `ProtectControlGroups=yes` | Cgroup hierarchy read-only | Daemon writes no cgroups |
| `ProtectClock=yes` | Cannot set system clock | Daemon sets no clock |
| `ProtectHostname=yes` | Cannot change hostname | Daemon changes none |
| `RestrictNamespaces=yes` | No new namespaces | Daemon does not create containers; Docker CLI is a thin client to dockerd |
| `RestrictRealtime=yes` | No realtime scheduling | Daemon schedules no realtime work |
| `RestrictSUIDSGID=yes` | No SUID/SGID bits honoured | Daemon execs no SUID binaries |
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6` | Only these address families | UDS listener, GitHub HTTPS fetch, loopback health probes |
| `LockPersonality=yes` | Lock execution domain | Go runtime needs none |
| `MemoryDenyWriteExecute=yes` | No W^X memory mappings | Daemon and its children need no JIT |
| `SystemCallArchitectures=native` | Only native syscall ABI | No compat-mode processes |
| `SystemCallFilter=@system-service ~@privileged @resources` | Restrict syscalls | Daemon only needs basic process/network/filesystem syscalls |
| `SystemCallErrorNumber=EPERM` | Failed syscall returns EPERM | Daemon code handles EPERM cleanly (probeSocket classifies ECONNREFUSED specifically) |
| `CapabilityBoundingSet=` | No capabilities | Daemon needs none; Docker daemon runs under its own unit with its own caps |
| `AmbientCapabilities=` | No inheritable capabilities | Daemon inherits none |
| `MemoryMax=512M` | Memory ceiling | Defense-in-depth; child processes are bounded by their own commands |

The `SystemCallFilter=@system-service ~@privileged @resources`
directive is safe because `agentctld` never executes a container
or namespace operation itself. The Docker CLI is a thin client
that talks to `/var/run/docker.sock` over HTTP; the actual
container creation, image building, and namespace setup happen
inside `dockerd`, which runs under its own systemd unit with its
own privileges. The same is true for `git` (regular process doing
regular filesystem + network work) and `caddy validate`/`caddy
reload` (regular process doing regular filesystem + admin API
work).

If the Docker daemon itself is hardened such that it rejects
agentctld's requests, that is a Docker-side policy decision, not a
systemd-side one. The Docker daemon's systemd unit is outside the
scope of this document.

## Timeouts

Three timeouts must satisfy a strict relationship:

- `AGENTCTLD_OPERATION_TIMEOUT` (default `30m`) — the wall-clock
  budget the daemon's deploy/rollback handlers run under.
- `AGENTCTLD_WRITE_TIMEOUT_MARGIN` (default `5m`) — added to the
  operation timeout to derive the HTTP server's `WriteTimeout`.
- systemd's `TimeoutStopSec` (set to `40m` in the example unit).

The relationship is:

```
HTTP WriteTimeout   = AGENTCTLD_OPERATION_TIMEOUT + AGENTCTLD_WRITE_TIMEOUT_MARGIN
TimeoutStopSec      >  HTTP WriteTimeout
```

`agentctld`'s deploy and rollback handlers run on a background
context bounded by `AGENTCTLD_OPERATION_TIMEOUT`. The response
write happens inside the HTTP `WriteTimeout`, which is the
operation timeout plus the margin. `TimeoutStopSec` is the hard
cap systemd applies; if it fires before the daemon's in-flight
handlers return, systemd sends `SIGKILL` and the deployment is
abandoned halfway through.

If `AGENTCTLD_OPERATION_TIMEOUT` is raised, raise `TimeoutStopSec`
accordingly. The example `TimeoutStopSec=40m` gives 5 minutes of
headroom over the default `WriteTimeout` of 35 minutes. For a
heavily-customised setup (e.g. `AGENTCTLD_OPERATION_TIMEOUT=2h`),
`TimeoutStopSec=2h15m` or larger is appropriate.

`AGENTCTLD_WRITE_TIMEOUT_MARGIN=0` is supported and sets the
margin to exactly zero, so `WriteTimeout` equals
`OperationTimeout`. This is not recommended in production because
a deploy that runs right up to its budget leaves no time for the
response write.

## Install on the daemon host

1. **Build the binary** as described in [Build](#build).

2. **Install the binary:**

   ```
   install -m 0755 bin/agentctld /usr/local/bin/agentctld
   ```

3. **Create the user and groups** as described in [User and group
   setup](#user-and-group-setup).

4. **Install the systemd unit:**

   ```
   install -m 0644 systemd/agentctld.service.example \
       /etc/systemd/system/agentctld.service
   systemctl daemon-reload
   ```

5. **Create the environment file:**

   ```
   install -d -m 0750 -o root -g agentctld /etc/agentctld
   install -m 0640 -o root -g agentctld /dev/null \
       /etc/agentctld/agentctld.env
   ```

   Then edit `/etc/agentctld/agentctld.env` and fill in every
   variable in [Configuration](#configuration-every-agentctld_-variable).
   Every required variable must be set; the daemon exits
   non-zero with a specific missing-key message otherwise.

6. **Install the canonical Caddyfile.** Write the Caddyfile at
   the path set in `AGENTCTLD_CADDY_ROOT_PATH` (typically
   `/etc/caddy/Caddyfile`). It must include the import directive
   described in [Caddy root config](#caddy-root-config). Reload
   or restart `caddy.service` so it picks up the new Caddyfile
   before agentctld starts touching the import directory.

7. **Set the `caddy` user/group permissions** as described in
   [Caddy user / agentctld user interaction](#caddy-user--agentctld-user-interaction).

8. **Start the daemon:**

   ```
   systemctl enable --now agentctld
   ```

9. **Verify the service:**

   ```
   systemctl status agentctld
   systemctl is-active agentctld
   curl --unix-socket /run/agentctld/socket http://localhost/healthz
   ```

   The `healthz` response body is `{"status":"ok"}` with HTTP 200.
   Any other response indicates a configuration or wiring problem.

## Health check

```
curl --unix-socket /run/agentctld/socket http://localhost/healthz
```

Returns 200 with `{"status":"ok"}`. While the daemon is draining,
mutating endpoints (`deploy`, `rollback`, `inspect`) return 503
with `{"code":"shutting_down"}`; `healthz` continues to return 200
so a load balancer's pre-shutdown check does not flag the drain.

## Example requests

Replace `/run/agentctld/socket` with `AGENTCTLD_SOCKET_PATH` and
`<app>`, `<commit>`, `<manifest>` with real values. Every request
uses the `curl --unix-socket` transport; no TCP listener is
exposed.

### List managed apps

```
curl --unix-socket /run/agentctld/socket http://localhost/v1/apps
```

Response:

```json
{"apps":["price-tracker","agentctl"]}
```

### Inspect a deploy proposal

```
curl --unix-socket /run/agentctld/socket \
    -X POST -H 'Content-Type: application/json' \
    --data @- http://localhost/v1/inspect <<'EOF'
{
  "app": "price-tracker",
  "commit": "0123456789abcdef0123456789abcdef01234567",
  "manifest": {
    "version": 2,
    "app": "price-tracker",
    "container_port": 8080,
    "health_path": "/healthz"
  }
}
EOF
```

`inspect` validates the manifest, resolves the commit against the
trusted mirror, and reports the result without performing any side
effect. The `valid` field is `true` when every check passes;
`errors` lists the validation failures otherwise.

### Deploy

```
curl --unix-socket /run/agentctld/socket \
    -X POST -H 'Content-Type: application/json' \
    --data @- http://localhost/v1/apps/price-tracker/deploy <<'EOF'
{
  "commit": "0123456789abcdef0123456789abcdef01234567",
  "manifest": {
    "version": 2,
    "app": "price-tracker",
    "container_port": 8080,
    "health_path": "/healthz"
  }
}
EOF
```

The response includes the derived image name, container name, host
port, hostname (`<app>.<AGENTCTLD_CADDY_BASE_DOMAIN>`), and
deployment timestamp. `warnings` carries non-fatal post-commit
cleanup issues that did not abort the deployment.

### Status

```
curl --unix-socket /run/agentctld/socket \
    http://localhost/v1/apps/price-tracker/status
```

Returns the persisted state and the live container status
(`"running"`, `"stopped"`, `"absent"`, or `"unknown"`). The
`status_check_error` field is set when the `docker inspect`
failed for any reason other than "no such container".

### State

```
curl --unix-socket /run/agentctld/socket \
    http://localhost/v1/apps/price-tracker/state
```

Returns just the persisted current and previous deployments, with
no live container status. Use this when only the persisted state
matters.

### Rollback

```
curl --unix-socket /run/agentctld/socket \
    -X POST -H 'Content-Type: application/json' \
    -H 'Content-Type: application/json' \
    --data '{"health_path":"/healthz"}' \
    http://localhost/v1/apps/price-tracker/rollback
```

`health_path` is required because the persisted state does not
include it (it is a manifest value, not a deployment identity
value). The path must start with `/` and contain no `?` or `#`.
Returns the swapped Current deployment. `409` with
`{"code":"no_previous"}` indicates a single deployment with no
Previous to roll back to.

## Log inspection

The daemon writes two streams:

- **stderr** — captured by journald under the unit. Search with
  `journalctl -u agentctld` (add `-f` to follow, `--since` /
  `--until` to bound the window).
- **Audit log** — when `AGENTCTLD_AUDIT_LOG` is set, structured
  JSON records at that path. The audit logger redacts any field
  whose name contains a sensitive substring (`token`, `jwt`,
  `key`, `private_key`, `pem`, `secret`, `authorization`,
  `bearer`, `password`, `client_secret`).

Typical events on the audit log: `agentctld-startup`,
`daemon-listening`, `deploy-succeeded`, `deploy-failed`,
`rollback-succeeded`, `rollback-failed`, `agentctld-stopped`.
The `time`, `level`, `msg`, and structured fields give the
lifecycle story for every request the daemon accepted.

For end-to-end deploy diagnostics, both streams should be
available: stderr captures `caddy`/`docker` command output
truncated to a bounded prefix, while the audit log records the
high-level lifecycle and any errors.

## Restart, upgrade, uninstall

### Restart

```
systemctl restart agentctld
```

The daemon handles `SIGTERM` gracefully: it stops accepting new
mutating requests, drains in-flight handlers, and exits. systemd
applies `TimeoutStopSec` as the hard cap.

### Upgrade

```
# build the new binary on a build host or in-place
cd /path/to/agentctl
git pull
go build -o bin/agentctld ./cmd/agentctld
install -m 0755 bin/agentctld /usr/local/bin/agentctld
systemctl restart agentctld
```

For systemd-side changes (unit, env file), also run:

```
install -m 0644 systemd/agentctld.service.example \
    /etc/systemd/system/agentctld.service
systemctl daemon-reload
systemctl restart agentctld
```

The unit is a single file; no additional assets are shipped
besides the binary.

### Uninstall

```
systemctl disable --now agentctld
rm /etc/systemd/system/agentctld.service
systemctl daemon-reload
rm /usr/local/bin/agentctld
rm -rf /etc/agentctld /var/log/agentctld /var/lib/agentctld /run/agentctld
userdel agentctld
```

`rm -rf /var/lib/agentctld` deletes all sources, checkouts, state,
data, and Caddy fragments. **This is destructive.** The data
directories under `/var/lib/agentctld/data/<app>/data/` are not
backed up elsewhere; back up first if the data must be preserved.

After uninstall, the agentctld user has no reason to remain; the
`docker` group membership on the user has no effect once the user
is removed.

## Trust boundary

`agentctld` accepts connections only on its Unix domain socket.
There is no TCP listener, no localhost port, no admin interface.
The socket is the trust boundary.

Any process that can connect to the socket can drive the full
deployment pipeline: build images from arbitrary commits (subject
to the trusted source mirror's `origin/main`), start and stop
candidate containers, promote routes through Caddy, persist state,
and roll back managed apps. There is no authentication beyond
Unix-socket filesystem permissions.

Implications:

- **Group membership in `AGENTCTLD_SOCKET_GROUP` is deployment
  authority.** Treat that group like a privileged role.
- **World-access to the socket directory** (mode 0755 on the
  parent, mode 0666 on the socket) would expose deployment
  control to every local user. The unit's default mode
  (`RuntimeDirectoryMode=0750`, `AGENTCTLD_SOCKET_MODE=0660`)
  prevents this; do not widen without understanding the
  consequences.
- **The `docker` group membership** of the `agentctld` user is a
  parallel trust boundary: any process that can execute as
  `agentctld` can drive the Docker daemon directly, not just via
  the socket. This is required for the daemon to function.
- **A compromised agent sandbox that is in the socket group has
  deployment authority.** That is the intended design — the
  sandbox is the only consumer in a single-tenant setup — but
  the trust model must be explicit.

This is the same trust model `gitbridge` operates under. The two
services are intentionally narrow in attack surface and broad in
trust assumption: anyone with socket access has the service's
authority. The mitigation is filesystem permissions plus the
trust that the broker/daemon host is appropriately isolated from
untrusted workloads.

## Known limitations

- **No authentication beyond Unix-socket filesystem
  permissions.** Anyone who can connect to the socket can drive
  the daemon. There is no token, no mTLS, no per-app
  authorisation. A future version may add per-request
  authentication; today, control is delegated entirely to the
  filesystem.

- **No multi-tenant deployment authority.** Every socket caller
  has the same authority. There is no concept of "can deploy
  app X but not app Y" or "can inspect but not deploy". A future
  version may add per-app authorisation.

- **No notification of graceful shutdown.** The daemon does not
  implement `sd_notify(3)`. systemd's `Type=simple` does not wait
  for the daemon to be ready before considering the unit
  "started"; in practice the daemon binds its socket within
  milliseconds and the `/healthz` endpoint serves from the first
  request, so this is not a problem in steady-state operation.
  For future readiness probes, switch to `Type=notify` and add
  `sd_notify` calls in the daemon.

- **Audit log writes are best-effort.** The `audit` package
  writes structured JSON records to the configured writer. A
  write failure is not propagated to the daemon's main error
  path; the daemon does not exit on audit failure. This is
  consistent with `gitbridge` and is intentional: a failed
  audit write should not abort a deployment.

- **No HTTP listener.** No TCP or localhost endpoint is exposed.
  The daemon is reachable only through its Unix socket. A future
  reverse-proxy need must use the socket.

- **Docker and Caddy run as subprocesses of agentctld.** A
  compromised `agentctld` can issue arbitrary `docker` and
  `caddy` invocations. The daemon's argv builders are built from
  validated inputs (see `docs/DEPLOYMENT.md`), but the trust
  model assumes a compromised `agentctld` is a full host
  compromise via its `docker` group membership.

- **No support for image registries beyond the local Docker
  daemon.** `docker build` uses the local daemon's build cache;
  no pull-through cache, no remote-only registries. The
  persistent image retention is local-only.

- **No database for state.** State lives in
  `/var/lib/agentctld/state/<app>.state.json`, one file per
  app. There is no central index beyond `listManagedApps`'s
  directory scan. A backup strategy must include this directory
  (and the data directory under
  `/var/lib/agentctld/data/<app>/data/`).

- **Single deployment per app.** Each app has exactly one
  Current and one Previous deployment. Multi-replica routing,
  blue/green, and canary strategies are out of scope.

- **The daemon's own network access is unrestricted beyond
  address-family restrictions.** It will talk to any
  `github.com` host (or whatever `AGENTCTLD_SOURCE_ORIGIN_URL`
  points to), any loopback address, and any reachable host on
  the configured DNS resolvers. Operators behind a proxy must
  configure Git's proxy settings at the system level.