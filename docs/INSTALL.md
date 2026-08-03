# Installing gitbridge

gitbridge is a small Go binary that brokers GitHub App installation tokens
over a Unix domain socket. It runs on a host that is more trusted than the
agent sandbox; the sandbox talks to it via the socket, never over the network.

For the host-side deployment control plane (the companion service that
drives application deploys, inspects state, and rolls back managed apps),
see [`AGENTCTLD_INSTALL.md`](AGENTCTLD_INSTALL.md).

## Build

The repository uses Go 1.19+ and has no third-party dependencies.

```
git clone https://github.com/simons-agent-space/agentctl.git
cd agentctl
go build -o bin/gitbridge ./cmd/gitbridge
```

The resulting binary is self-contained: no CGo, no shared libraries.

## Install on the broker host

1. Create a dedicated system user:

   ```
   useradd --system --no-create-home --shell /usr/sbin/nologin gitbridge
   ```

2. Create the configuration directory:

   ```
   install -d -m 0750 -o gitbridge -g gitbridge /etc/gitbridge
   ```

3. Drop the GitHub App private key into `/etc/gitbridge/key.pem` with
   mode `0640`, owner `gitbridge:gitbridge`.

4. Drop the configuration file at `/etc/gitbridge/config.json`. See
   [Configuration](#configuration) below for the schema.

5. Copy the example systemd unit:

   ```
   install -m 0644 systemd/gitbridge.service.example /etc/systemd/system/gitbridge.service
   systemctl daemon-reload
   systemctl enable --now gitbridge
   ```

   The example unit is at `systemd/gitbridge.service.example`. It uses
   the hardened directives described in [`ARCHITECTURE.md`](ARCHITECTURE.md).

6. Verify the service:

   ```
   systemctl status gitbridge
   curl --unix-socket /run/gitbridge/socket \
        -X POST -H 'Content-Type: application/json' \
        -d '{"repo":"simons-agent-space/agentctl","profile":"builder"}' \
        http://localhost/token
   ```

## Update

```
git pull
go build -o bin/gitbridge ./cmd/gitbridge
install -m 0755 bin/gitbridge /usr/local/bin/gitbridge
systemctl restart gitbridge
```

## Uninstall

```
systemctl disable --now gitbridge
rm /etc/systemd/system/gitbridge.service
rm -rf /etc/gitbridge /var/log/gitbridge
userdel gitbridge
```

## Configuration

gitbridge reads a single JSON file whose path is given by `--config`
(default `/etc/gitbridge/config.json`) or the `GITBRIDGE_CONFIG`
environment variable.

### Schema

```json
{
  "app_id": 123456,
  "installation_id": 789012,
  "private_key_path": "/etc/gitbridge/key.pem",
  "allowed_org": "simons-agent-space",
  "allowed_repositories": ["agentctl", "another-repo"],
  "socket_path": "/run/gitbridge/socket"
}
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `app_id` | integer | yes | Numeric GitHub App ID. |
| `installation_id` | integer | yes | Numeric GitHub App installation ID for the org. |
| `private_key_path` | path | yes | Path to the PEM-encoded RSA private key. Read once at startup. |
| `allowed_org` | string | yes | The single GitHub organisation that repositories must belong to. |
| `allowed_repositories` | array of string | yes | Explicit allowlist of repository names (without org prefix). |
| `socket_path` | path | yes | Where the Unix domain socket is created. The socket is always created with mode `0660`. |

### Validation

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

### Request format

Clients send a JSON POST to `/token` over the socket:

```json
{ "repo": "simons-agent-space/agentctl", "profile": "builder" }
```

### Response format

Successful response:

```json
{
  "token": "***",
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

### Error codes

| Code | HTTP | Cause |
|---|---|---|
| `BAD_REQUEST` | 400 | Malformed JSON, missing fields, unknown fields, or trailing data |
| `PROFILE_NOT_ALLOWED` | 403 | Profile is not `builder` |
| `REPO_NOT_ALLOWED` | 403 | Repository is not in `allowed_org`/`allowed_repositories` |
| `METHOD_NOT_ALLOWED` | 405 | Wrong HTTP method |
| `INTERNAL` | 500 | Unexpected error. The audit log on the broker side records an `error_class` (e.g. `upstream`, `transport`, `internal`) and, for upstream errors, the GitHub `http_status`. The free-form upstream message is never logged or returned. |
