#!/usr/bin/env bash
# agentctld-wait.sh — poll the agentctld /healthz endpoint over its Unix socket
# until it returns 200 OK or the timeout elapses. systemd may report
# agentctld.service as active before the socket is bound; this script handles
# that race by polling with exponential backoff.
#
# Usage: agentctld-wait.sh [-t SECONDS] [-s SOCKET_PATH]
#   -t SECONDS       Maximum time to wait (default: 30)
#   -s SOCKET_PATH   Unix socket path (default: /run/agentctld/socket)
#
# On timeout, prints 'systemctl --no-pager --full status agentctld.service'
# and the last 50 lines of 'journalctl -u agentctld.service', then exits 1.
#
# Exits 0 on first successful /healthz response, 1 on timeout, 2 on bad args.

set -euo pipefail

TIMEOUT=30
SOCKET=/run/agentctld/socket

usage() {
  cat <<EOF
Usage: agentctld-wait.sh [-t SECONDS] [-s SOCKET_PATH]

  -t SECONDS       Maximum time to wait (default: 30)
  -s SOCKET_PATH   Unix socket path (default: /run/agentctld/socket)

On timeout, prints 'systemctl --no-pager --full status agentctld.service'
and the last 50 lines of 'journalctl -u agentctld.service', then exits 1.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -t) TIMEOUT="$2"; shift 2 ;;
    -s) SOCKET="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if ! [[ "$TIMEOUT" =~ ^[0-9]+$ ]] || (( TIMEOUT <= 0 )); then
  echo "invalid timeout: $TIMEOUT (must be a positive integer)" >&2
  exit 2
fi

DEADLINE=$((SECONDS + TIMEOUT))
INTERVAL_MS=100
MAX_INTERVAL_MS=1000

while (( SECONDS < DEADLINE )); do
  if curl --fail --silent --max-time 2 \
       --unix-socket "$SOCKET" \
       http://localhost/healthz >/dev/null 2>&1; then
    exit 0
  fi
  sleep "$(awk -v ms=$INTERVAL_MS 'BEGIN { printf "%.3f", ms / 1000 }')"
  INTERVAL_MS=$(( INTERVAL_MS * 2 ))
  (( INTERVAL_MS > MAX_INTERVAL_MS )) && INTERVAL_MS=$MAX_INTERVAL_MS
done

# Timed out — print diagnostics and exit 1
echo "agentctld did not become ready within ${TIMEOUT}s (socket: $SOCKET)" >&2
echo >&2
echo "=== systemctl --no-pager --full status agentctld.service ===" >&2
systemctl --no-pager --full status agentctld.service >&2 || true
echo >&2
echo "=== journalctl -u agentctld.service (last 50 lines) ===" >&2
journalctl --no-pager -n 50 -u agentctld.service >&2 || true
exit 1
