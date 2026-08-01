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
| `app` | string | Must match `^[a-z][a-z0-9-]{1,31}$`. Must equal the expected repository short name passed to `Validate`. |
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
- `app` must match `^[a-z][a-z0-9-]{1,31}$`.
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
