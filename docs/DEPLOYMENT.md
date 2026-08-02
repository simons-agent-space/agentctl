# DEPLOYMENT.md

This document describes the `deploy.json` manifest format consumed by
`agentctld` — the deployment daemon that runs on the host alongside
`gitbridge`.

## Version 1 contract

`deploy.json` is a single JSON object with the following required
fields:

| Field | Type | Constraints |
|---|---|---|
| `version` | integer | Must equal `1`. |
| `app` | string | Must match `^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`. The first character must be a lowercase letter, the last character must be a lowercase letter or digit, and the middle characters may be lowercase letters, digits, or hyphens. Total length is 2–32. Must equal the expected repository short name passed to `Validate`. |
| `container_port` | integer | Must be in `[1024, 65535]`. |
| `health_path` | string | Must start with `/`. Must not contain `?` or `#`. |

### Example

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

### Validator rules (`internal/deploy.Validate`)

- `version` must equal `1`.
- `app` must match `^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`.
- `container_port` must be in `[1024, 65535]`.
- `health_path` must start with `/` and must not contain `?` or `#`.
- The `expectedRepo` argument passed to `Validate` must match the same
  regex and must equal `m.App`.

## Scope of version 1

Version 1 describes a **single HTTP container** and nothing else.

The following are explicitly **not** supported in version 1 and will
require a future version bump:

- Environment variables
- Secrets
- Databases
- Volumes
- Cron jobs
- Custom domains
- Multiple services per manifest

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

This layer does not yet implement container or Caddy rollback
orchestration. The state is the input to that layer; the layer
itself comes next.
