# gitbridge architecture and threat model

## Purpose

gitbridge brokers GitHub App installation tokens. It exists so that the
agent sandbox never holds the GitHub App's long-lived private key. The key
lives on a more-trusted broker host, and the sandbox asks the broker for
short-lived, repository-scoped tokens over a Unix domain socket.

## Components

```
┌────────────────────┐         ┌────────────────────┐        ┌────────────────────┐
│ Agent sandbox      │   UDS   │ gitbridge host     │  HTTPS │ api.github.com     │
│                    │ ──────► │                    │ ──────► │                    │
│ curl --unix-socket │         │ gitbridge binary   │        │ access_tokens API  │
└────────────────────┘         └────────────────────┘        └────────────────────┘
```

### Broker

The broker is a single Go binary (`cmd/gitbridge`) with two internal
packages:

- `internal/gitbridge` — config, RS256 JWT minting, validation, UDS server, GitHub REST client
- `internal/audit` — structured JSON logger with secret redaction

### Audit log

Each request lifecycle event is written to the configured logger as a
single JSON line:

```
{"time":"2026-08-01T16:30:00Z","level":"INFO","msg":"audit","op":"validated","repo":"simons-agent-space/agentctl","profile":"builder"}
{"time":"2026-08-01T16:30:00Z","level":"INFO","msg":"audit","op":"minted","repo":"simons-agent-space/agentctl","repo_name":"agentctl","profile":"builder","expires_at":"2026-08-01T17:30:00Z"}
{"time":"2026-08-01T16:30:01Z","level":"WARN","msg":"audit","op":"rejected","repo":"other-org/repo","profile":"builder","reason":"REPO_NOT_ALLOWED: ..."}
```

The audit logger never receives the JWT, the installation token, or the
private key. Every attribute key whose name contains a sensitive
substring (`token`, `jwt`, `key`, `private_key`, `pem`, `secret`,
`authorization`, `bearer`, `password`, `client_secret`) is replaced with
`[REDACTED]` by the logger's redaction hook.

## Trust boundaries

| Component | Trust | Holds |
|---|---|---|
| Agent sandbox | untrusted (depends on threat model) | nothing secret |
| Broker host | trusted | GitHub App private key |
| api.github.com | trusted (third party) | installation tokens |

The only path across the trust boundary is the UDS socket from the
sandbox to the broker. There is no network listener on the broker; it
opens HTTPS to `api.github.com`. The broker's code scopes outbound
traffic to GitHub as the only intended outbound destination; the systemd
unit restricts the broker to `AF_UNIX AF_INET AF_INET6`.

## Threat model

### Threats considered

**T1. Sandbox compromise.** An attacker who controls the agent process
can ask the broker for tokens. *Mitigation:* the broker strictly
enforces the configured organisation and allowlist, accepts only the
fixed `builder` profile, and never grants tokens for repositories
outside the allowlist.

**T2. Network eavesdropping on the broker ↔ GitHub link.** *Mitigation:*
TLS to `api.github.com`. The broker's HTTP client validates
certificates via the Go standard library default pool.

**T3. Token theft from the sandbox.** *Mitigation:* tokens are
short-lived (≤1 hour per GitHub) and scoped to a single repository with
minimum builder permissions. They are not stored on disk by the broker
or returned anywhere except the UDS response.

**T4. Private-key leakage via logs or error paths.** *Mitigation:* the
private key is read into memory once at startup and never returned from
the package that loads it. The audit logger redacts any attribute whose
name contains a sensitive substring. Error messages returned over UDS
are always generic; only `error_class` and an optional `http_status`
are recorded on the broker side.

**T5. Replay of an old installation token.** *Mitigation:* the broker
mints a fresh token for every request. Clients receive the expiry in the
response and are expected to discard the token after use.

**T6. Privilege escalation via profile confusion.** *Mitigation:* the
broker does not accept arbitrary permission profiles from the caller.
Only the fixed `builder` profile is recognised; any other name is
rejected with `PROFILE_NOT_ALLOWED`.

**T7. DoS against the broker host.** *Mitigation:* the broker is a
single binary with a small attack surface. The systemd unit restricts
the process via `ProtectSystem=strict`, `PrivateTmp=yes`,
`RestrictNamespaces=yes`, `MemoryDenyWriteExecute=yes`, and friends.
The `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6` directive
allows the UDS socket and HTTPS to `api.github.com`; it does not
OS-enforce `api.github.com` as the only outbound destination — the
broker's code is what scopes outbound traffic to GitHub. See
`systemd/gitbridge.service.example`.

**T8. Local privilege escalation via the socket.** *Mitigation:* the
socket is created with mode `0660` and owned by a dedicated
`gitbridge` group. Whether the agent sandbox can connect depends on
the deployment joining the sandbox user to that group; see the
unresolved integration tasks below.

### Threats out of scope

- Compromise of the broker host itself (an attacker with root on the
  broker host can read the private key from disk).
- Compromise of `api.github.com`.
- Compromise of the GitHub App's registration or installation.

## Failure modes

| Failure | Behaviour |
|---|---|
| Config file missing or invalid | Broker exits non-zero with a clear error. |
| Private key missing or unreadable | Broker exits non-zero. |
| GitHub API 4xx | Generic `INTERNAL` to the caller; only `error_class` and (when applicable) `http_status` are recorded on the broker side. |
| GitHub API 5xx | Same as 4xx. |
| Audit log write failure | Logging is currently best-effort: write failures are not propagated and the broker does not exit. |

## Design choices

1. **UDS only.** No TCP listener, no localhost port. The broker is
   invisible from the network. The socket lives in
   `/run/gitbridge/socket` with mode `0660`.

2. **Stdlib only.** No third-party dependencies. The JWT is RS256 and is
   signed in-process using `crypto/rsa`, `crypto/sha256`, and
   `encoding/base64`. A third-party JWT library would add an audit
   burden and a supply-chain surface for a security-critical component.

3. **One fixed profile.** The broker never accepts arbitrary
   permissions from the caller. It only ever mints tokens with the
   `builder` profile, which is the minimum permissions required to
   push code and open pull requests.

4. **Repo slug validated locally; repo name sent to GitHub.** The
   broker validates the full `org/name` slug against the configured
   organisation and allowlist before doing anything else. When it
   calls GitHub, it sends only the short repository name — that's
   what the access-tokens endpoint expects.

5. **Single installation per broker process.** The installation ID is
   read from the config file. Multi-installation scenarios would
   require configuration to map repositories to installations; this is
   left for future work.

6. **Repo and profile are both required.** Either missing returns
   `BAD_REQUEST`. This is deliberate: a profile with no repo is a
   misuse, and a repo with no profile is a privilege-escalation risk.

7. **Trailing JSON rejected.** The request decoder rejects any bytes
   after the first JSON object, so an attacker cannot smuggle extra
   data past the broker under the assumption that "the decoder only
   consumes the first object anyway".

8. **Errors returned over UDS are generic.** GitHub API failures,
   filesystem errors, and cryptographic errors are logged in full on
   the broker side; the caller only ever sees the code `INTERNAL` and
   a generic message. The raw error is never serialised into a UDS
   response.

9. **Audit log is the only side effect of a request.** The broker
   never caches tokens and never persists anything else. Each request
   mints a fresh token from GitHub.

10. **Disallow unknown config fields.** Typos in the config file
    should fail loudly, not silently. The loader uses
    `json.Decoder.DisallowUnknownFields()`.

## Open questions / unresolved risks

- **Refresh on key rotation.** The broker reads the private key once at
  startup. Rotating the key requires a restart. This is acceptable for
  now but should be revisited if zero-downtime rotation becomes a
  requirement.

- **Multiple installations.** A single broker process currently
  hard-codes one installation ID. Supporting per-repo installations
  would require either a per-request installation lookup or a more
  elaborate config schema.

- **No replay window enforcement at the broker.** GitHub installation
  tokens are short-lived but not single-use; the broker does not
  track tokens it has already issued. This is consistent with the
  threat model (the sandbox is the only consumer) but worth revisiting
  if the broker ever serves multiple clients.
