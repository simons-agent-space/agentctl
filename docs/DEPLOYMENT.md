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
  `<repository-root>/<repository>.git`. If the mirror does not exist,
  it is initialised with `git clone --bare https://github.com/<organisation>/<repository>.git`.
- Each deployment uses a **detached temporary checkout** of the exact
  commit, created via `git worktree add --detach`. The checkout does
  not track a branch and the bare mirror's branch state is untouched.
- The caller **cannot choose arbitrary Git URLs or host paths**. The
  organisation is checked against a trusted configuration; the
  repository root is supplied by trusted host configuration, not by the
  caller. The expected origin URL is computed from
  `https://github.com/<organisation>/<repository>.git` and must match
  the mirror's configured `remote.origin.url` — anything else is
  rejected.
- The checkout directory is removed by an explicit `CleanupCheckout`
  function, which refuses to operate on paths outside the trusted
  repository root.
