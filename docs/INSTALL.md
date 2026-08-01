# Installing gitbridge

gitbridge is a small Go binary that brokers GitHub App installation tokens
over a Unix domain socket. It runs on a host that is more trusted than the
agent sandbox; the sandbox talks to it via the socket, never over the network.

## Build

The repository uses Go 1.19+ and has no third-party dependencies.

```
git clone https://github.com/simons-agent-space/agentctl.git
cd agentctl
git checkout feat/bootstrap-gitbridge
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
   [`CONFIGURATION.md`](CONFIGURATION.md) for the schema.

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