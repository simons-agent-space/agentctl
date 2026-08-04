# Agent Deployment Workflow

## Purpose

This document is the canonical operating procedure for any coding agent
that prepares or performs an application deployment through `agentctld`.
It applies to every agent — sandboxed, local, or otherwise — that
interacts with the deployment pipeline.

The contract is **`inspect → approve → deploy`**: the agent inspects
the proposal, a human approves the same approved deployment proposal,
and the agent submits that same approved deployment proposal — the
application name, the commit SHA, and the manifest. (Inspect and
deploy use different HTTP request shapes; what must be identical is
the deployment proposal itself.) Skipping or reordering any step is
a violation of this procedure.

## The mandatory workflow

Every deployment goes through these twelve steps in order.

1. **Build or modify the application.** Make the code changes that the
   deployment is intended to ship.

2. **Run all relevant tests.** Unit tests, integration tests, lint,
   type checks — whatever the project defines as "relevant". Record
   results; any failure must be reported.

3. **Build and locally smoke-test the container when possible.** If the
   container can be built and run on the agent's host, do so and
   exercise the health path. Record results.

4. **Commit and push the code.** All changes must be pushed to the
   remote before a deploy is finalised.

5. **Resolve the full 40-character commit SHA.** Get the SHA from the
   remote after the push, not from the local working tree. A short
   SHA is never acceptable.

6. **Prepare the final immutable deployment proposal.** Compose the
   **deployment proposal** — the triple `(app, commit, manifest)`.
   `commit` is the full 40-character SHA; `manifest` is the full
   `deploy.json`. Once composed, this proposal is frozen — its
   values are what will be inspected, shown to the human, and
   submitted after approval.

7. **Call `POST /v1/inspect` with the proposal.** The inspect
   request body carries `app`, `commit`, and `manifest`. The daemon
   validates the manifest, parses the commit, and resolves the
   commit against the trusted source mirror. It performs no side
   effects.

8. **Present a human-readable deployment report** containing:

   - application name
   - public domain (the derived `<app>.<AGENTCTLD_CADDY_BASE_DOMAIN>`)
   - repository
   - exact commit SHA (full 40 lowercase hex characters)
   - summary of changes
   - tests and checks run, including failures
   - container port and health-check path
   - persistent or read-only mounts
   - the result of `/v1/inspect` (`valid`, `source_reachable`,
     `current`, `previous`, `errors`)
   - whether this is an initial deploy, update, or rollback

9. **Stop and wait for explicit human approval.** Do not proceed.

10. **After approval, submit the same deployment proposal to the
    deploy endpoint.** The deploy endpoint is
    `/v1/apps/{app}/deploy` — the application name moves into the
    URL path. The request body carries `commit` and `manifest`. The
    values submitted after approval must match what was inspected
    exactly: the same application name in the URL path, the same
    commit SHA, and the same manifest bytes.

11. **Verify:**
    - agentctld deployment status (`GET /v1/apps/{app}/status`)
    - application health endpoint
      (`curl https://<app>.<base-domain><health_path>`)
    - public HTTPS URL (the same URL)

12. **Report the final result**, including any failure and useful
    diagnostic information (daemon status, container status, HTTP
    response codes, relevant log excerpts).

## Explicit rules

These rules are binding. Violations are bugs in the agent, not
discretionary choices.

- **The agent MUST NOT deploy without explicit human approval.**
  "Approved" means a human reviewed the deployment report from step 8
  and said so in the same interaction or a clearly traceable channel.
  Silence is not approval.

- **The agent MUST NOT treat permission to build, test, commit, or
  push as permission to deploy.** Those are preconditions. Deploy is
  a separate, later step that requires its own approval.

- **The agent MUST run `/v1/inspect` before requesting approval.**
  The inspection is the agent's check that the request is valid, the
  commit is reachable, and the deployment will not collide with the
  existing state. Approval is requested on the basis of the inspect
  result.

- **The agent MUST deploy an exact full commit SHA, never a mutable
  branch or tag such as `main`.** Branches and tags move. The deploy
  must be reproducible from the same SHA tomorrow.

- **The deployment proposal shown to the human, inspected by
  agentctld, and submitted after approval MUST be identical —
  meaning the same application name, the same commit SHA, and the
  same manifest bytes.** If any of those three values changes after
  approval, the previously given approval is invalid.

- **If anything changes after approval, including the application
  name, commit, manifest, mounts, port, domain, or health check,
  approval is invalid and must be requested again.** A new
  `inspect → report → approve` cycle is required.

- **The agent MUST report failed tests, warnings, or failed health
  checks honestly.** Failures are not omitted, downplayed, or hidden
  behind a green deployment report.

- **The agent MUST NOT bypass agentctld by directly using SSH, sudo,
  Docker, Caddy, or host filesystem changes for deployment operations
  supported by agentctld.** The agent is a client of agentctld, not
  a peer with the same authority.

- **The agent MUST NOT modify unrelated applications or
  infrastructure.** A deploy of `<app>` touches only `<app>`'s
  resources. The agent does not touch other apps' state, Caddy
  fragments, source mirrors, or persistent data.

- **On deployment failure, the agent must stop, gather diagnostics
  available through its permitted interfaces, and report them. It
  must not improvise destructive host changes.** No `docker rm`,
  no manual Caddy reload, no `rm -rf` on the data directory, no
  rollback by hand. The agent reads `status`, reads `state`, and
  reports.

- **Rollback also requires explicit human approval unless the human
  explicitly approved an automatic rollback as part of that exact
  deployment proposal.** Rollback is a deployment. It is subject to
  the same approval rules.

## Trust boundary

The agent does not have direct access to the deployment host. All
deployment operations flow through `agentctld` over a Unix domain
socket. The agent can:

- read the application repository
- call `agentctld` over its socket
- read the public HTTPS URL of the deployed application

The agent cannot:

- SSH to the deployment host
- invoke `docker`, `caddy`, `git`, or any host binary directly
- read or write the deployment host's filesystem
- reach `agentctld` over the network (TCP is not exposed)

The agent's authority is exactly the authority of a socket client
that is a member of `agentctl-clients`. It can drive the full
deployment pipeline, and it must do so through `agentctld` only.

The `inspect → approve → deploy` contract is the mechanism that keeps
the agent in this lane: every deploy is approved by a human who can
see the full request before it lands.

## Example deployment report

The agent's report to the human must include at least the following
fields. Real names, SHAs, and values are placeholders here.

```text
Deployment proposal — <app-name>

  Application:      <app-name>
  Public domain:    <app-name>.<base-domain>
  Repository:       <repo-owner>/<repo-name>
  Commit:           <commit-sha>          # full 40 lowercase hex
  Operation:        initial deploy | update | rollback

  Summary of changes:
    <one-paragraph summary from the commit message and / or diff>

  Tests and checks run:
    Unit tests:        passed (N tests) | failed (N tests, see ...)
    Integration tests: passed | failed (...)
    Container build:   succeeded | failed (...)
    Local smoke test:  passed | failed (...)
    POST /v1/inspect:  valid=true, source_reachable=true, errors=[]
      current:  <nil | commit-sha, container_name, deployed_at>
      previous: <nil | commit-sha, container_name, deployed_at>

  Manifest:
    container_port:    <port>
    health_path:       <path>
    persistent mounts: <none | manifest.data.mount=true>
    read-only mounts:  <none | manifest.data.read_only=true>

  Inspect result (verbatim):
    <JSON body returned by POST /v1/inspect>

Waiting for explicit approval before calling
POST /v1/apps/<app-name>/deploy.
```

## Example commands

All requests go over the Unix domain socket. No TCP listener is
exposed. Replace `<socket-path>` with the agentctld socket path
(default `/run/agentctld/socket`) and the placeholders with the
specific values for the deployment.

### Inspect

```sh
curl --unix-socket <socket-path> \
    -X POST -H 'Content-Type: application/json' \
    --data @- http://localhost/v1/inspect <<'EOF'
{
  "app": "<app-name>",
  "commit": "<commit-sha>",
  "manifest": {
    "version": 2,
    "app": "<app-name>",
    "container_port": <port>,
    "health_path": "<path>"
  }
}
EOF
```

### Deploy

The deploy call uses the same application name in the URL path
(`<app-name>`), the same commit SHA, and the same manifest as the
inspect call. The request body shape differs from `/v1/inspect`
because the application name moves into the URL path on the deploy
endpoint; only `commit` and `manifest` are in the body.

```sh
curl --unix-socket <socket-path> \
    -X POST -H 'Content-Type: application/json' \
    --data @- http://localhost/v1/apps/<app-name>/deploy <<'EOF'
{
  "commit": "<commit-sha>",
  "manifest": {
    "version": 2,
    "app": "<app-name>",
    "container_port": <port>,
    "health_path": "<path>"
  }
}
EOF
```

### Status

```sh
curl --unix-socket <socket-path> \
    http://localhost/v1/apps/<app-name>/status
```

### Application health (public HTTPS)

```sh
curl --fail --silent --show-error \
    https://<app-name>.<base-domain><health-path>
```

### Daemon health

```sh
curl --unix-socket <socket-path> \
    http://localhost/healthz
```

## Future: Telegram approval

The manual approval step (step 9) is currently the agent pausing
and waiting for a human to approve the deployment report in the
same chat. A future change will replace that interaction with a
Telegram approval flow:

- The agent sends the deployment report to a Telegram bot.
- The bot presents the report with inline approval / rejection
  controls.
- The human's selection is signalled back to the agent.
- The agent proceeds to step 10 exactly as it does now.

The contract — `inspect → approve → deploy` — does not change. The
approval transport moves from chat-text to a button. The agent's
behaviour around the approval boundary is unchanged:

- no deploy without an explicit human selection
- the inspected deployment proposal is the same proposal that is
  submitted after approval
- any change after approval invalidates the approval and forces a
  new inspection

This means the workflow documented here is the workflow the
Telegram approval UI will enforce, not just the workflow the agent
currently follows. The rules in this document are the rules that
both the agent and the future approval UI must respect.

## What the API does not yet support

The workflow described above is fully supported by the current
agentctld API. Every endpoint and field referenced in this document
exists in `internal/daemon/types.go` and `internal/daemon/server.go`:

- `POST /v1/inspect` — request `app`, `commit`, `manifest`; response
  `app`, `commit`, `manifest_version`, `valid`, `source_reachable`,
  `current`, `previous`, `errors`.
- `POST /v1/apps/{app}/deploy` — request `commit`, `manifest`;
  response `app`, `commit`, `image`, `container_name`, `host_port`,
  `container_port`, `hostname`, `upstream`, `deployed_at`, `warnings`.
- `GET /v1/apps/{app}/status` — response `app`, `current`, `previous`,
  `container_status`, `status_check_error`.
- `GET /v1/apps/{app}/state` — response `app`, `current`, `previous`.
- `POST /v1/apps/{app}/rollback` — request `health_path`; response
  mirrors the swapped deployment.
- `GET /healthz` — response `{"status":"ok"}`.
- `GET /v1/apps` — response `{"apps":[...]}`.

The manifest schema is `version`, `app`, `container_port`,
`health_path`, and optional `data.mount` / `data.read_only`. The
`DeploymentSummary` returned by read endpoints exposes `mount_data`
and `data_read_only` so the agent can report mounts without
re-parsing the manifest.

The only forward-looking item in this document is the Telegram
approval transport. The current daemon has no Telegram-specific
endpoint and needs none; the Telegram approval flow lives outside
`agentctld` and signals the agent, which then calls the same
endpoints documented above. The `inspect → approve → deploy`
contract is invariant.
