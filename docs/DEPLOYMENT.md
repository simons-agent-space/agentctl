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
