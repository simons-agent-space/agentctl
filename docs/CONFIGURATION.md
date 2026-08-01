# gitbridge configuration

gitbridge reads a single JSON file whose path is given by `--config`
(default `/etc/gitbridge/config.json`) or the `GITBRIDGE_CONFIG`
environment variable.

## Schema

```json
{
  "app_id": 123456,
  "installation_id": 789012,
  "private_key_path": "/etc/gitbridge/key.pem",
  "allowed_org": "simons-agent-space",
  "allowed_repositories": ["agentctl", "another-repo"],
  "socket_path": "/run/gitbridge/socket",
  "socket_mode": "0660",
  "socket_group": "gitbridge"
}
```

## Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `app_id` | integer | yes | Numeric GitHub App ID. |
| `installation_id` | integer | yes | Numeric GitHub App installation ID for the org. |
| `private_key_path` | path | yes | Path to the PEM-encoded RSA private key. Read once at startup. |
| `allowed_org` | string | yes | The single GitHub organisation that repositories must belong to. |
| `allowed_repositories` | array of string | yes | Explicit allowlist of repository names (without org prefix). |
| `socket_path` | path | yes | Where the Unix domain socket is created. |
| `socket_mode` | octal string | no | File mode applied to the socket (default: `0660`). |
| `socket_group` | group name | no | Group applied to the socket (default: broker process group). |

## Validation

Unknown fields cause the broker to refuse to start. This is deliberate:
it prevents typos in the config from silently turning into no-ops.

The config loader rejects:

- `app_id <= 0`
- `installation_id <= 0`
- empty `private_key_path`
- empty `allowed_org`
- empty `allowed_repositories`
- `allowed_repositories` entries containing `/`
- empty `socket_path`

## Request format

Clients send a JSON POST to `/token` over the socket:

```json
{ "repo": "simons-agent-space/agentctl", "profile": "builder" }
```

## Response format

Successful response:

```json
{
  "token": "ghs_...",
  "expires_at": "2026-08-01T18:30:00Z",
  "repository": "simons-agent-space/agentctl",
  "profile": "builder"
}
```

Error response:

```json
{
  "error": "repository not in allowlist or wrong organisation",
  "code": "REPO_NOT_ALLOWED"
}
```

## Error codes

| Code | HTTP | Cause |
|---|---|---|
| `BAD_REQUEST` | 400 | Malformed JSON, missing fields, or unknown fields |
| `PROFILE_NOT_ALLOWED` | 403 | Profile is not `builder` |
| `REPO_NOT_ALLOWED` | 403 | Repository is not in `allowed_org`/`allowed_repositories` |
| `METHOD_NOT_ALLOWED` | 405 | Wrong HTTP method |
| `INTERNAL` | 500 | Unexpected error (e.g. GitHub API failure) |