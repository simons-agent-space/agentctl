# DEPLOYMENT.md

This document describes the `deploy.json` manifest format consumed by
`agentctld` — the deployment daemon that runs on the host alongside
`gitbridge`.

## Version 1 contract

`deploy.json` is a single JSON object with the following required
fields:

| Field | Type | Constraints |
|---|---|---|
| `version` | integer | Must equal `1` or `2`. See [Version 2 contract](#version-2-contract). |
| `app` | string | Must match `^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`. The first character must be a lowercase letter, the last character must be a lowercase letter or digit, and the middle characters may be lowercase letters, digits, or hyphens. Total length is 2–32. Must equal the expected repository short name passed to `Validate`. |
| `container_port` | integer | Must be in `[1024, 65535]`. |
| `health_path` | string | Must start with `/`. Must not contain `?` or `#`. |

### Example (version 1)

```json
{
  "version": 1,
  "app": "agentctl",
  "container_port": 8080,
  "health_path": "/healthz"
}
```

### Parser rules (`internal/deploy.Load`)

- Unknown fields are rejected (`json.Decoder.DisallowUnknownFields()`).
- Trailing data after the manifest object is rejected: a second
  `Decode` call must return `io.EOF`.
- A version-1 manifest containing a `data` field is rejected at parse
  time. The v1 contract explicitly did not support per-app persistent
  data, so silently accepting the field would be a silent contract
  change.

### Validator rules (`internal/deploy.Validate`)

- `version` must equal `1` or `2`.
- `app` must match `^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`.
- `container_port` must be in `[1024, 65535]`.
- `health_path` must start with `/` and must not contain `?` or `#`.
- For version 3 manifests, `repository` is required and must
  match the same regex as `app`. `app` and `repository` are
  independent fields: `app` drives the API path / hostname /
  state key, `repository` drives the source mirror. They are
  allowed to differ.

## Scope of version 1

Version 1 describes a **single HTTP container** and nothing else.

The following are explicitly **not** supported in version 1 and will
require a future version bump:

- Environment variables
- Secrets
- Databases
- Multiple services per manifest
- Per-app persistent data mounts (use version 2; see below)

## Version 2 contract

Version 2 is a strict superset of version 1. It adds one optional
top-level field:

| Field | Type | Constraints |
|---|---|---|
| `data` | object \| absent | Optional. When absent, the deployment is identical to version 1 in behaviour. When present, the deployment opts into the per-app persistent data directory; see [Per-app persistent data](#per-app-persistent-data). |

The `data` object has the following fields:

| Field | Type | Constraints |
|---|---|---|
| `mount` | boolean | Optional, default `false`. When `true`, the deployment mounts the per-app data directory at `/data` inside the container. When `false` (or omitted), the deployment behaves identically to a manifest without a `data` field at all. |
| `read_only` | boolean | Optional, default `false`. When `true`, the in-container mount is read-only. Has no effect when `mount` is `false`; the orchestrator normalizes this combination on save. |

A version-2 manifest without the `data` field is functionally
identical to a version-1 manifest; existing manifests can move from
version `1` to version `2` without changing any other field. Unknown
fields anywhere in the manifest (including inside `data`) are still
rejected.

### Example (version 2 with data mount)

```json
{
  "version": 2,
  "app": "price-tracker",
  "container_port": 8080,
  "health_path": "/healthz",
  "data": {
    "mount": true,
    "read_only": true
  }
}
```

### Example (version 2 without data mount)

```json
{
  "version": 2,
  "app": "agentctl",
  "container_port": 8080,
  "health_path": "/healthz"
}
```

## Source resolution

Deployment sources are resolved by `agentctld` as follows:

- The caller must supply an **exact full commit SHA** (40 lowercase hex
  characters). Short or non-hex SHAs are rejected at the input boundary.
- The named commit must **already be reachable from `origin/main`**.
  Tags, trees, blobs, unmerged branches, and pull-request refs are all
  rejected. The check is equivalent to
  `git merge-base --is-ancestor <commit> refs/remotes/origin/main`.
- The source is fetched into a **host-side bare mirror** at
  `<repository-root>/<repository>.git`. The trusted host configuration
  supplies the origin URL. If the mirror does not exist, it is created
  with `git clone --bare <origin-url>` and then immediately populated
  with the explicit fetch refspec
  `+refs/heads/main:refs/remotes/origin/main` (bare clones do not create
  remote-tracking branches on their own).
- On every deployment, the mirror is re-validated: `git rev-parse --is-bare-repository`
  must return `true`, and `remote.origin.url` must equal the trusted
  origin URL. A non-bare directory at the mirror path is rejected.
- After re-validation, the same explicit fetch refspec is run. A failed
  fetch is fatal — there is no fallback that accepts a stale
  `refs/remotes/origin/main` in lieu of a successful fetch.
- Each deployment uses a **detached temporary checkout** of the exact
  commit, created via `git worktree add --detach`. The checkout does
  not track a branch and the bare mirror's branch state is untouched.
- The caller **cannot choose arbitrary Git URLs or host paths**. The
  organisation is checked against a trusted configuration; the
  repository root and the origin URL are supplied by trusted host
  configuration, not by the caller.
- The checkout directory is removed by `CleanupCheckout`, which
  validates the identity on the result: `result.Repository` must match
  the app-name format and `result.Commit` must be exactly 40 lowercase
  hex characters. The expected checkout path is derived from the
  trusted repository root and the validated identity:

  ```
  <RepositoryRoot>/<Repository>-checkouts/<Commit>
  ```

  Both that derived path and the caller-supplied `result.CheckoutPath`
  are resolved to absolute paths and required to be **exactly equal**
  — `CleanupCheckout` does not accept merely any path beneath the
  trusted root. The trusted mirror path is derived from the validated
  `result.Repository`, never from caller input. On success
  `CleanupCheckout` runs `git worktree remove --force <path>` followed
  by `git worktree prune` from the mirror to clear stale worktree
  metadata, then removes the checkout directory. The same
  worktree-remove + prune + remove-all sequence is used as the
  failure-cleanup path when `CheckoutSource` itself fails after the
  worktree has been created (e.g. if credential stripping fails).

## Candidate container

After source resolution, `agentctld` builds a Docker image from the
validated checkout, starts a candidate container on a localhost-only
port, polls its health endpoint, and cleans it up safely.

### Docker image

- Built from the **exact checkout path** with
  `docker build --pull --tag <image> <checkout-path>`.
- Image name is derived entirely from validated values:

  ```
  agentctl/<app>:<full-commit-sha>
  ```

  `:latest` is never used. If the build fails, the candidate phase
  aborts before any container is started.

### Localhost-only port publishing

- The runtime allocates a host port from a configurable inclusive range
  in `[1024, 65535]`. Allocation binds a temporary TCP listener on
  `127.0.0.1:<candidate-port>` to test availability, then closes the
  listener immediately.
- If Docker reports that the chosen port is already in use (e.g.
  another process grabbed the brief race window), the runtime moves to
  the next available port in the range rather than failing immediately.
- Containers are published **only** to localhost:

  ```
  127.0.0.1:<host-port>:<manifest.container_port>
  ```

  No public hostname is ever contacted during the candidate phase.

### Fixed runtime restrictions

Every `docker run` invocation uses this fixed argv:

```
docker run
  --detach
  --name <container-name>
  --restart unless-stopped
  --memory 256m
  --cpus 0.5
  --pids-limit 128
  --cap-drop ALL
  --security-opt no-new-privileges
  --publish 127.0.0.1:<host-port>:<container-port>
  <image>
```

- Container name is derived from validated values:
  `agentctl-<app>-<first-12-chars-of-sha>`.
- No `--privileged`, no `--network host`, no Docker socket bind mount,
  no bind mounts of any kind, no additional Linux capabilities, no
  caller-supplied Docker arguments.
- `--read-only` is intentionally not yet applied because many
  application images require writable temporary directories.
- If a container with the derived name exists and is running, the
  runtime returns a conflict error rather than replacing it. If it
  exists but is stopped, it is removed first.

### Health check

- The runtime polls `http://127.0.0.1:<host-port><manifest.health_path>`
  with an `http.Client` that has an explicit per-request timeout.
- Any `2xx` response is healthy. `3xx`, `4xx`, and `5xx` are not.
  Redirects are not followed.
- Polling continues until `RuntimeConfig.HealthTimeout` expires.
- The poll respects context cancellation; every response body is
  closed.
- The runtime does not rely on a Dockerfile `HEALTHCHECK`.

### Failed candidates are removed

If the candidate never becomes healthy:

- The final health-check error is collected.
- The last 100 lines of container logs are retrieved (bounded output).
- The candidate container is stopped and removed.
- The error returned to the caller includes both the health failure and
  the bounded logs.

### Images are retained

On candidate success, the **image is not removed**. It is retained on
the host so a later layer can roll back to it. `RemoveCandidate`
removes only the container; images persist.

### Cleanup API

```go
func RemoveCandidate(
    ctx context.Context,
    cfg RuntimeConfig,
    candidate CandidateResult,
) error
```

- The supplied `candidate.App` is validated against the app-name regex.
- The supplied `candidate.Commit` is validated against the SHA regex.
- The expected image and container names are re-derived from the
  validated identity; the supplied values must match exactly.
- Fabricated container or image names are rejected with
  `ErrInvalidCandidate` before any Docker command runs.
- Cleanup is idempotent: removing an absent container is not an error.

### Out of scope

This layer does not yet implement Caddy, public routing, deployment
state, rollback orchestration, sockets, HTTP handlers, systemd units,
or the final deployment orchestrator. Those come in later layers.

## Caddy promotion

Once a candidate is health-checked and healthy, `agentctld` promotes
it through Caddy so external traffic can reach it. Promotion is the
bridge between the localhost-only candidate phase and the public
hostname.

### Inputs

Promotion receives a validated `CandidateResult` and a trusted
`CaddyConfig`. No caller input is accepted for hostnames, paths,
upstreams, or raw Caddy configuration.

```
type CaddyConfig struct {
    BaseDomain     string  // e.g., "apps.simonontheweb.de"
    ConfigDir      string  // trusted directory for managed fragments
    RootConfigPath string  // canonical Caddyfile Caddy is running
    CaddyBinary    string  // defaults to "caddy" when empty
}
```

Caddy is expected to run as a single long-lived process whose
canonical config file is `RootConfigPath`. That root file is
expected to import the managed fragments from `ConfigDir`, e.g.

```
import <ConfigDir>/*.caddy
```

The import glob matches only the final fragments
(`<app>.caddy`). The promotion flow uses `<app>.caddy.partial` and
`<app>.caddy.bak` as transient staging paths during the
install/validate/reload dance, but neither suffix is ever part of
the import glob, so a stale or in-flight partial or backup can
never leak into the running configuration. Every `caddy validate`
and `caddy reload` invocation this package makes targets the
canonical root file; no individual app fragment is ever passed to
Caddy directly.

### Derived names

- **Hostname** is derived as `<app>.<BaseDomain>`. The `app` comes from
  the candidate's identity, the `BaseDomain` from the trusted host
  configuration. The caller cannot influence either.
- **Upstream** is always `127.0.0.1:<candidate.HostPort>`. No
  `0.0.0.0`, no other interfaces, no caller-supplied addresses.
- **Config path** is `<ConfigDir>/<app>.caddy`.

### Managed config fragment

The runtime writes a Caddyfile fragment of the form:

```
<hostname> {
    reverse_proxy 127.0.0.1:<hostPort>
}
```

The fragment is staged through two transient siblings of the final
path:

- `<config>.partial` — holds the new content during the
  install/validate/reload dance. The root config's import glob
  (`*.caddy`) does not match this suffix, so a parked partial is
  invisible to Caddy.
- `<config>.bak` — holds the previous final fragment while the new
  one is being installed. The root config's import glob does not
  match this suffix either, so the backup is invisible to Caddy
  and can never be served to a client.

Neither `<config>.partial` nor `<config>.bak` is ever passed to
`caddy validate` or `caddy reload` directly. Every Caddy
invocation in this flow targets the canonical root config, never
the per-app fragment or any of its siblings.

### Validation and reload

1. The new fragment body is written to `<config>.partial`. The
   root config's import glob does not match `.partial`, so writing
   the temp cannot affect what the running Caddy sees.
2. If a previous final fragment exists, it is renamed to
   `<config>.bak`. The root config's import glob does not match
   `.bak` either, so this rename is also invisible to Caddy:
   from this point until step 4, the root config sees no
   `<app>.caddy` for this app.
3. `<config>.partial` is renamed to the final `<config>` path. On
   rename failure, the temp is removed and the previous fragment
   (if any) is restored from the backup; the function returns
   `ErrAtomicWriteFailed`.
4. `caddy validate --config <RootConfigPath>` runs against the
   canonical root. The root imports only the final `<app>.caddy`
   fragments; the `.partial` and `.bak` siblings on disk are
   invisible to this validation. On failure the function returns
   `ErrCaddyValidateFailed`. The restore path:
   - Parks the new content back at `<config>.partial` (a
     non-imported suffix) so the backup can be moved into place
     without overwriting it.
   - Renames the backup (or, for first-time promotions, removes
     the new final) back to `<config>`.
   - Removes the parked partial.
   - No reload is needed on the validate-failure path because
     validate never changes the running Caddy.
5. `caddy reload --config <RootConfigPath>` applies the new
   configuration to the running Caddy. On failure:
   - The new fragment is parked back at `<config>.partial` so the
     backup can be moved into its place without overwriting.
   - The backup (or absence of one, for first-time promotions) is
     restored at `<config>` and a best-effort reload is issued so
     the running Caddy state matches disk.
   - The parked partial is removed so it cannot leak into a later
     reload.
   - The function returns `ErrCaddyReloadFailed`.
6. On success, the backup is removed (best-effort; a leftover `.bak`
   is harmless because the root config's import glob does not
   match `.bak`, and the next promotion for the same app would
   simply overwrite it as part of its own backup step).

### Identity validation

Before writing or invoking Caddy, the candidate identity is
re-derived and required to match exactly:

- `app` matches the app-name regex.
- `commit` matches `^[0-9a-f]{40}$`.
- `image` equals `agentctl/<app>:<commit>`.
- `container name` equals `agentctl-<app>-<first-12-chars-of-commit>`.
- `host port` is a valid port number.

`HealthURL` is intentionally not part of the promotion identity:
promotion derives its upstream exclusively from `HostPort`, and
`HealthURL` is the runtime layer's concern. Fabricated candidates
are rejected with `ErrInvalidCandidate` before any disk or Caddy
operation runs.

### Result

```
type PromotionResult struct {
    App        string
    Commit     string
    Hostname   string
    Upstream   string
    ConfigPath string
}
```

### Removal

```go
func RemovePromotion(ctx context.Context, cfg CaddyConfig, app string) error
```

- Validates `app` against the app-name regex.
- The derived config path `<ConfigDir>/<app>.caddy` must exist;
  otherwise `ErrPromotionNotFound`.
- Moves the fragment to `<config>.bak`. The root config's import
  statement no longer sees the fragment after this rename, so the
  following validate/reload reflects the post-removal state.
- `caddy validate --config <RootConfigPath>` confirms the canonical
  config is still valid without the fragment. On failure the
  fragment is restored from the backup and `ErrCaddyValidateFailed`
  is returned.
- `caddy reload --config <RootConfigPath>` applies the change to the
  running Caddy. On failure the fragment is restored from the
  backup, a best-effort reload is issued, and
  `ErrCaddyReloadFailed` is returned.
- On success, the backup is removed. Backup deletion is
  best-effort: by this point the route has already been removed
  from the running Caddy via a successful reload, so a leftover
  `.bak` file on disk is harmless (the root config's import glob
  does not match `.bak`) and must not turn a successful removal
  into an error. Any future promotion for the same app would
  overwrite the leftover `.bak` as part of its own backup step.
- Only the exact derived app config is touched; other apps' configs
  are left alone.

### Out of scope

This layer does not yet implement container or Caddy rollback
orchestration, multi-replica routing, rate limiting, authentication
middleware, or the final deployment orchestrator. Those come in
later layers.

## Deployment state

Once a route is promoted, `agentctld` records the deployment so a
future layer can roll back to either the current or the previous
build without re-deriving identity from the runtime or Caddy
layers. State lives in a trusted host-side directory, one JSON
file per app.

### State directory and base domain

State is configured by a trusted `StateConfig`:

```
type StateConfig struct {
    StateDir   string // <StateDir>/<app>.state.json per app
    BaseDomain string // every persisted Hostname must equal "<app>.<BaseDomain>"
}
```

`StateDir` is a trusted host path supplied by configuration. Each
app's state is written to `<StateDir>/<app>.state.json`. The
app-name regex (`^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`) gates the
filename, so path traversal is rejected before disk is touched.
`BaseDomain` is validated by the same rules the Caddy layer uses
(`isValidDomain`), so a state file validated here is guaranteed
to be usable by Caddy without further checks.

### Schema

The state file is a single JSON object:

```
type Deployment struct {
    App           string    // matches app-name regex
    Commit        string    // exactly 40 lowercase hex chars
    Image         string    // "agentctl/<app>:<commit>"
    ContainerName string    // "agentctl-<app>-<first-12-chars>"
    HostPort      int       // in [1, 65535]
    ContainerPort int       // in [1024, 65535]
    Hostname      string    // "<app>.<BaseDomain>"
    Upstream      string    // "127.0.0.1:<HostPort>"
    DeployedAt    time.Time // RFC3339
}

type DeploymentState struct {
    Version  int          // must equal 1
    App      string       // must match the filename's app
    Current  *Deployment
    Previous *Deployment  // omitted when absent
}
```

Only version 1 is written or accepted on read. Older or newer
versions return `ErrUnsupportedStateVersion` so a future bump can
migrate deliberately rather than silently misinterpret old data.

### Identity validation

Before any read, write, or delete, every field of every persisted
`Deployment` (Current and Previous) is re-derived against
`cfg.BaseDomain` and required to match exactly:

- `Image` must equal `deriveImage(App, Commit)`.
- `ContainerName` must equal `deriveContainerName(App, Commit)`.
- `Commit` must match the 40-lowercase-hex regex.
- `HostPort` must be in `[1, 65535]`; `ContainerPort` in `[1024, 65535]`.
- `Hostname` must equal `"<app>.<BaseDomain>"` exactly.
- `Upstream` must equal `"127.0.0.1:<HostPort>"` exactly.
- `DeployedAt` must be non-zero.

Any mismatch returns `ErrInvalidDeploymentState`. This catches
both fabricated writes and tampered reads, including any tampering
that survives the previous-deployment move.

### Atomic write

`SaveDeployment` writes the new state through an unpredictable
temp file: `os.CreateTemp(dir, "state-*.tmp")` opens the temp file
with `O_RDWR|O_CREATE|O_EXCL` semantics, so an attacker who
pre-placed the predictable `<file>.tmp` (or any other) path as a
symlink cannot redirect the write. The flow is: chmod `0644`,
write, `fsync`, close, `rename` into place over the existing file,
then `fsync` the parent directory. A reader that opens the state
file at any instant sees either the full previous state or the
full new state — never a partial write. The temp file is unlinked
on every error path so a failed save leaves no stale temp behind
(the deferred unlink is a no-op on success because the file has
been renamed).

### Symlink and path-traversal rejection

`Lstat` is consulted before every read, write, and delete. If the
state file is a symlink, the operation is rejected with
`ErrSymlinkedStateFile` and the symlink target is left untouched
(deletion must not follow symlinks). Path traversal is rejected at
the app-name regex and again at the resolved-path check inside
`stateFilePath` (defense in depth against a symlinked state
directory).

### API

```go
func SaveDeployment(cfg StateConfig, dep Deployment) error
func LoadDeploymentState(cfg StateConfig, app string) (*DeploymentState, error)
func DeleteDeploymentState(cfg StateConfig, app string) error
```

`SaveDeployment` validates `cfg` (both `StateDir` and `BaseDomain`
are required; `BaseDomain` must be a valid domain), validates the
supplied `Deployment` against `cfg.BaseDomain`, refuses to write
through a symlink, loads any existing state, moves the existing
`Current` to `Previous`, installs the new `Current`, and writes
atomically. A corrupt or fabricated existing state causes the save
to fail rather than be silently overwritten — losing the previous
deployment record is worse than refusing a save.

`LoadDeploymentState` returns `ErrDeploymentStateNotFound` when no
state file exists, `ErrCorruptDeploymentState` when the JSON is
malformed, `ErrUnsupportedStateVersion` when the version field
does not equal 1, `ErrSymlinkedStateFile` when the state file is a
symlink, and `ErrInvalidDeploymentState` for any identity
mismatch (including a fabricated `Previous`).

`DeleteDeploymentState` is "safe" in three ways: `cfg` and the
app name are validated (no path traversal), the file is rejected
if it is a symlink (the symlink target is left alone), and
deleting an already-absent file returns `ErrDeploymentStateNotFound`
rather than silently succeeding.

### Out of scope

Multi-replica routing, rate limiting, authentication middleware,
and the final deployment orchestrator are out of scope for this
layer. The state record is the input to those layers.

## Per-app persistent data

Version-2 deployments may opt into a single per-app persistent
data directory. The feature is deliberately narrow: one host
directory per app, one fixed in-container target, no env-var
interpolation, no caller-supplied paths, no multiple mounts.

### Goals

- The host path is derived from a **trusted root** (host
  configuration) and the **validated app name**; the deployment
  manifest never specifies a host path.
- The host path is `/srv/agentctl/apps/<app>/data` by default,
  where `<DataRoot>` is `/srv/agentctl/apps` (operator-overridable
  via the trusted `DataConfig.DataRoot`).
- The in-container mount target is the **fixed constant `/data`**;
  the manifest cannot change it.
- The directory is **created and managed** by the `agentctl` data
  layer (`internal/deploy/data.go`); the deployment flow never
  touches it with raw `os.MkdirAll` or `rm -rf`.
- The directory is **preserved across every normal operation**:
  initial deploy, replacement deploy, rollback, container
  recreation, `docker rm --force` of an unrouted container.
- The directory is **never deleted implicitly** by deploy,
  rollback, or any cleanup path. Deletion is an explicit
  operator action (`RemoveAppDataDir(cfg, app, force=true)`).
- The in-container mount can be **read-only** for applications
  like Price Tracker that mount a shared store and must not
  mutate it from inside the container.

### Host-side layout

The data layer derives the host path as:

```
<DataRoot>/<app>/data
```

`<DataRoot>` is supplied by trusted host configuration as
`DataConfig.DataRoot`. The default is `/srv/agentctl/apps` but
operators can point it elsewhere (for example, a dedicated
data volume mounted at `/var/lib/agentctl/apps`). The value is
never taken from the manifest, the caller, or the deployment
artifact.

`<app>` is the same `appNameRe`-validated name the rest of the
deployment pipeline uses (`^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`).
The data layer validates the resolved path against `<DataRoot>`
(abs + rel containment check) and rejects symlinks at any
component of the path.

### Container-side mount

Every persistent-data bind mount uses the canonical Docker
syntax:

```
--mount type=bind,source=<DataRoot>/<app>/data,target=/data,readonly=<true|false>
```

The `readonly` flag matches the manifest's `data.read_only`.
A read-write mount (the default) lets the application write
freely; a read-only mount forbids writes from inside the
container. The whole container is **not** marked `--read-only`
— only the `/data` mount is restricted.

### State record

When a deployment opts into the data mount, the persisted
`Deployment` records two booleans:

```
MountData    bool  // whether the data directory is mounted
DataReadOnly bool  // true if and only if MountData && data.read_only
```

The orchestrator normalizes `DataReadOnly=false` whenever
`MountData=false`, so a "no mount but read-only" record never
appears in state. The rollback layer uses these two booleans to
re-apply the same mount when it has to start the previous
container fresh from its image.

### API

```go
type DataConfig struct {
    DataRoot string // trusted host directory
}

type AppData struct {
    App           string  // validated app name
    HostPath      string  // resolved absolute host path
    ContainerPath string  // fixed: "/data"
    ReadOnly      bool
}

// EnsureAppDataDir ensures the per-app data directory exists.
// Idempotent: an existing directory is left untouched (never
// chmod'd, never wiped). Refuses symlinked data paths. Returns
// the derived AppData for use in docker run argv.
func EnsureAppDataDir(cfg DataConfig, app string, readOnly bool) (*AppData, error)

// AppDataDir returns the derived AppData without touching disk.
func AppDataDir(cfg DataConfig, app string, readOnly bool) (*AppData, error)

// RemoveAppDataDir is the EXPLICIT destructive operation. The
// deployment flow, the rollback flow, and every cleanup path
// MUST NOT call this. Refuses non-empty directories unless
// force is true.
func RemoveAppDataDir(cfg DataConfig, app string, force bool) error
```

Sentinel errors returned by the data layer:

- `ErrInvalidDataConfig` — `DataRoot` is empty or the app name
  fails the `appNameRe` check.
- `ErrAppDataNotFound` — `RemoveAppDataDir` was called for a
  directory that does not exist.
- `ErrAppDataNotEmpty` — `RemoveAppDataDir` was called without
  `force=true` for a non-empty directory.
- `ErrSymlinkedAppData` — the data directory (or any component
  of the path) is a symlink; the symlink target is left
  untouched.

### Lifecycle guarantees

The data directory is preserved across every normal agentctl
operation. Specifically:

- **First deployment** with `data.mount=true`: `EnsureAppDataDir`
  creates the directory before `docker run`. If `docker run`
  fails, the empty directory remains (it is harmless and a
  re-deploy will reuse it idempotently).
- **Replacement deployment**: a second deploy for the same app
  leaves the directory and its contents in place, even if the
  new manifest no longer opts into the mount. The old
  container is removed; the data is not.
- **Rollback**: the data directory is never deleted. The
  rollback's fresh `docker run` for the previous container
  re-applies the matching mount; `docker start` for a still-
  present previous container preserves the existing mount.
- **Container recreation** (`docker rm --force` of an
  unrouted container): the data directory on the host is
  unrelated to the container's lifecycle and survives any
  `docker rm` operation.
- **Image retention**: deleting an image does not affect the
  data directory.

### Explicit deletion

The only operation that removes the data directory is
`RemoveAppDataDir(cfg, app, force)`. It is **not** called by
deploy, rollback, state save, candidate cleanup, or any other
flow in `agentctl`. Operators invoke it directly when an app's
data should be discarded — for example, after decommissioning
an app or before reinstalling it with a different schema.

`RemoveAppDataDir` requires an explicit `force` boolean:

- `force=false`: refuses to remove a non-empty directory
  (returns `ErrAppDataNotEmpty`). This is the safe default
  and is what callers should pass when in doubt.
- `force=true`: removes the directory and its contents
  recursively. The caller is asserting they understand the
  data loss.

Refusing non-empty directories without `force` is the explicit
"are you sure?" gate the task requires.

### Out of scope

The data layer does not implement any of the following. Each is
explicitly left to a future milestone:

- Multiple data directories per app.
- Per-app custom host paths or custom in-container targets.
- Volume drivers other than the host bind mount.
- Backup, snapshot, or migration of data directories.
- Environment-variable interpolation into the path.
- Multiple mounts per app, read-only-on-the-host mounts, or
  any other generalisation of the volume model.

## Rollback orchestration

`RollbackDeployment` reverses the most recent promotion: it
restores the `Previous` deployment as the live one and demotes
the current deployment to `Previous`. Rollback is idempotent in
the toggle sense — calling it twice swaps `Current` and `Previous`
twice, ending where the first call started.

### Flow

1. Load state; require `Previous`. `ErrNoPreviousDeployment` is
   returned when there is nothing to roll back to.
2. Ensure the previous container is running and healthy. The
   status is inspected via `docker inspect`; stopped containers
   are started, absent containers are run fresh from the persisted
   image. The image is not rebuilt or pulled — rollback restores
   what promotion already deployed.
3. Promote the previous route through the existing Caddy layer
   via the same `Promote` path promotion uses.
4. Only after a successful promotion: swap `Current` and
   `Previous` in state and remove the formerly-current container.

If any step before a successful promotion fails, nothing changes
on disk or in Caddy and the current deployment keeps serving.
If the state swap fails after promotion, `RollbackDeployment`
best-effort reverts Caddy to the original current route using a
bounded recovery context and reports the combined failure. If the
final remove-current step fails, the rollback is considered
successful (Caddy serves previous, state is swapped); the old
container being still running is a minor issue callers can clean
up later.

### Bounded recovery contexts

All post-failure cleanup (stopping the previous container we
just started, reverting Caddy after a state-swap failure) runs
under `context.Background()` with a 10-second timeout, never the
caller's context. A cancelled or expired caller context cannot
prevent the rollback layer from restoring disk and Caddy to a
consistent state. This mirrors the Caddy layer's recovery-context
pattern.

### API

```go
func RollbackDeployment(ctx context.Context, cfg RollbackConfig) error

type RollbackConfig struct {
    App        string       // app to roll back
    State      StateConfig
    Runtime    RuntimeConfig
    Caddy      CaddyConfig
    HealthPath string       // for health-checking the previous container
}
```

Sentinel errors:

- `ErrNoPreviousDeployment` — state has no `Previous` to roll
  back to.
- `ErrRollbackFailed` — wrapped around the underlying failure
  (load, container start, health check, Caddy promotion, state
  swap, or Caddy revert).

### Constraints

- The image for the previous deployment must already exist
  locally. Rollback does not build or pull.
- Rollback is not coupled to a specific source checkout; it
  uses the persisted `Deployment` record directly.
- Other apps' state files are untouched.

## Deployment orchestrator

`Deploy` is the single entry point that runs the full deployment
pipeline end-to-end on top of every lower layer: source checkout,
candidate build/start/health-check, Caddy promotion, state
persistence, and removal of the formerly-current container.

### Flow

1. `CheckoutSource` produces a detached worktree of the exact
   merged commit.
2. `startCandidate` builds the Docker image, runs the container
   on a localhost-only port, and health-checks it.
3. `promote` writes the new Caddy route and reloads Caddy
   atomically (Caddy layer's own restore handles a failed reload).
4. `SaveDeployment` persists state (Current → Previous, new →
   Current).
5. `removeCandidate` removes the formerly-current container
   (best-effort).

### Failure semantics

The current live deployment is never touched before promotion
and state persistence both succeed. On any failure, cleanup
targets only resources created by THIS deployment (the candidate
container, the new Caddy route). Caddy promotion failure is
contained by the Caddy layer's own atomic restore. State-save
failure triggers best-effort Caddy revert before cleanup. Every
post-failure cleanup path runs under `context.Background()` with
a 10-second timeout, never the caller context, so a cancelled
caller cannot strand the host with inconsistent state.

### API

```go
func Deploy(ctx context.Context, cfg DeployConfig, manifest Manifest, commit string) (*DeployResult, error)

type DeployConfig struct {
    Source  SourceConfig
    Runtime RuntimeConfig
    Caddy   CaddyConfig
    State   StateConfig
}

type DeployResult struct {
    App, Commit, Image, ContainerName string
    HostPort, ContainerPort           int
    Hostname, Upstream                string
    DeployedAt                        time.Time
    StateFile                         string
}
```

Sentinel errors:

- `ErrDeploymentFailed` — wrapped around the underlying failure
  (source checkout, candidate start/health, Caddy promotion, or
  state save). `errors.Is(err, ErrDeploymentFailed)` is true for
  every failed deploy.

### Constraints

- `Deploy` validates untrusted manifest input internally
  (version, app, container port, health path) before any side
  effect, so an autonomous caller does not have to pre-validate
  the manifest to get a clear error.
- The commit is re-validated by the source layer.
- Retrying `Deploy` with the same commit is safe: a fresh deploy
  attempt validates everything from scratch and leaves the host
  in a consistent state on failure.
- **Old-container removal after successful state persistence is
  NON-FATAL**: a failure here surfaces as a warning on
  `DeployResult.Warnings` (not as an error), so an autonomous
  caller does not retry a deployment that actually succeeded.
  Candidate-cleanup failures on *failed* deployment paths
  remain fatal.
- **Combined failures are preserved structurally**: when more
  than one underlying failure occurs (e.g. state-save failure
  AND Caddy-recovery failure, or state-save failure AND
  candidate-cleanup failure), the returned `deployError`
  preserves them as separate entries in `deployError.secondaries`
  (for the Caddy layer, as `caddyCommandError.cause` /
  `caddyCommandError.rollback`). Callers can detect every
  underlying failure with `errors.Is`; `errors.Is(err,
  ErrDeploymentFailed)` is always true. Go 1.19 has no
  `errors.Join`, so the equivalent is each layer's multi-
  sentinel `Is` method that walks the slice of preserved
  errors.
- HTTP handlers, Telegram approval, and systemd integration are
  intentionally out of scope; this layer is the daemon's
  building block.

## Per-app env-backed secrets

Version-3 manifests may opt into env-backed secrets. The feature is
deliberately narrow: a single env entry per secret, one file per
secret in the daemon's secret directory, no caller-supplied paths,
no multi-line values.

### Goals

- The host path to the secret directory is derived from a **trusted
  root** (host configuration) and the **validated app name**; the
  deployment manifest never specifies a host path.
- The host path is `/etc/agentctl/secrets/<app>` (or whatever the
  operator sets via `AGENTCTLD_SECRET_DIR`); the secret directory
  itself is `ValidateSecretDir`-checked at daemon startup (no
  symlinks, mode 0700-or-stricter, owned by root or the daemon's
  uid).
- Each `secret_ref` is a file inside the secret directory. The
  filename is validated against `secretRefRe`
  (`^[a-z0-9_](?:[a-z0-9_.-]{0,62}[a-z0-9_])$`). Files must be
  regular (symlink-rejected), mode 0600-or-stricter, owned by
  root or the daemon's uid.
- Values are **strictly single-line**. After trimming one trailing
  newline, any embedded `\n` or `\r` is rejected with
  `ErrSecretMultiline` (Docker `--env-file` cannot safely represent
  arbitrary multi-line values with the current `KEY=value` writer).
  The error names the secret_ref but never the value. PEM-encoded
  keys, certificates, and any other multi-line secrets are **not**
  supported as env values; pass them through a different injection
  path (a mounted file is the usual pattern).
- Secret VALUES are only ever written to a per-deploy temp env
  file with mode 0600, mounted via `docker run --env-file <path>`,
  and the temp file is `os.Remove`d the moment the container is
  created. They never appear in `--env`, in `ps`, in shell argv, in
  the daemon's audit log, in deployment state, in the persisted
  manifest, in inspect output, or in error messages.
- The runtime directory that briefly contains the temp env file
  (`Config.RuntimeDir`) is treated as security-equivalent to the
  secret directory: `validateRuntimeDir` enforces mode
  0700-or-stricter, no symlinks at any parent component, trusted
  owner (root or the daemon's uid), and safe-creates a missing
  directory with `os.Mkdir` (never `MkdirAll`, so the daemon
  cannot materialise an arbitrary root tree).

### Schema (v3)

```
type EnvEntry struct {
    Name      string  // matches env-var-name regex
    SecretRef string  // matches secretRefRe
    Required  bool    // false by default
}
```

### Inspect projection

`InspectResponse.EnvStatuses []EnvStatus` reports, per entry:
`{name, secret_ref, required, configured}`. Never the value.

### Deployment semantics

- `Required=false` + secret file missing: silently skipped
  (no entry written, no error).
- `Required=true` + secret file missing: `ErrSecretMissing`
  surfaces at inspect and at deploy; Docker is not started.
- Secret file present but fails any loader check (symlink,
  permissions, owner, multi-line value): always rejected with the
  matching sentinel, regardless of `Required`. A present-but-broken
  secret is a configuration bug, not a missing file.

