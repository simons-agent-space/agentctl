// Command agentctld is the host-side agentctl control plane.
//
// agentctld exposes a narrow local API surface over a Unix domain
// socket. Callers (the future Telegram approval flow and OpenClaw
// tools) send deploy proposals, approve and run deployments, inspect
// state, roll back managed apps, and list what is managed. The daemon
// never accepts arbitrary host paths, shell commands, Docker
// arguments, Caddy fragments, or container names: every filesystem
// path is derived from trusted host configuration, every candidate
// identity is checked against the fixed naming rules before any side
// effect.
//
// Configuration is loaded from AGENTCTLD_* environment variables,
// typically sourced from a systemd Environment= directive. The flag
// surface intentionally matches gitbridge's: --log selects the audit
// log path; everything else is environment-driven so a misconfigured
// unit cannot accidentally point the daemon at the wrong paths.
//
// Operational notes:
//   - The socket is created with mode 0660; the group is taken from
//     AGENTCTLD_SOCKET_GROUP and resolved to a numeric GID at startup.
//     An unresolvable group is a fatal configuration error.
//   - A pre-existing socket file at AGENTCTLD_SOCKET_PATH is probed
//     before bind: if a daemon is actively listening the daemon
//     refuses to start with an "already running" error; otherwise the
//     stale socket file is unlinked. A pre-existing non-socket file
//     at that path is a fatal startup error so the daemon never
//     silently overwrites unrelated files. The listen path must be
//     absolute.
//   - The parent directory of AGENTCTLD_SOCKET_PATH must already
//     exist. The daemon does not create it. The process supervisor
//     (systemd RuntimeDirectory=, runit, supervisord, ...) owns that
//     directory's ownership and permissions, and silently creating
//     it inside the daemon would mask supervisor misconfiguration.
//   - SIGINT and SIGTERM begin graceful shutdown. In-flight handlers
//     run to completion on their own background context; new
//     requests are rejected with 503.
//
// Environment variables:
//
//	AGENTCTLD_SOCKET_PATH          absolute path of the UDS listener (required)
//	AGENTCTLD_SOCKET_GROUP         group name or numeric GID for the socket (optional)
//	AGENTCTLD_SOCKET_MODE          socket mode in octal (optional, default 0660; e.g. 0660, 0o660)
//	AGENTCTLD_OPERATION_TIMEOUT    per-operation wall-clock budget (optional, default 30m). Drives both the deploy/rollback context timeout and the HTTP WriteTimeout, which is set to this value plus AGENTCTLD_WRITE_TIMEOUT_MARGIN.
//	AGENTCTLD_WRITE_TIMEOUT_MARGIN extra headroom added to the operation timeout to derive the HTTP WriteTimeout (optional, default 5m). Set to 0 to disable the headroom.
//	AGENTCTLD_MAX_REQUEST_BYTES    max request body in bytes (optional, default 65536)
//	AGENTCTLD_AUDIT_LOG            path for audit logs (default stderr)
//
// Source layer:
//
//	AGENTCTLD_SOURCE_REPOSITORY_ROOT    trusted host path (required)
//	AGENTCTLD_SOURCE_ORIGIN_URL         trusted remote URL (required)
//	AGENTCTLD_SOURCE_ALLOWED_ORG        expected GitHub org (required)
//
// Runtime layer:
//
//	AGENTCTLD_RUNTIME_PORT_RANGE_START  inclusive port-range start (required)
//	AGENTCTLD_RUNTIME_PORT_RANGE_END    inclusive port-range end (required)
//	AGENTCTLD_RUNTIME_HEALTH_TIMEOUT    total health-check budget (required, e.g. 30s)
//	AGENTCTLD_RUNTIME_REPOSITORY_ROOT   trusted host path (required)
//
// Caddy layer:
//
//	AGENTCTLD_CADDY_BASE_DOMAIN     parent domain (required)
//	AGENTCTLD_CADDY_CONFIG_DIR      managed config dir (required)
//	AGENTCTLD_CADDY_ROOT_PATH       canonical Caddyfile (required)
//	AGENTCTLD_CADDY_BINARY          path to caddy binary (optional, default "caddy")
//
// State layer:
//
//	AGENTCTLD_STATE_DIR             per-app state dir (required)
//	AGENTCTLD_STATE_BASE_DOMAIN     parent domain for state (required)
//
// Data layer:
//
//	AGENTCTLD_DATA_ROOT             persistent-data root (required)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/simons-agent-space/agentctl/internal/audit"
	"github.com/simons-agent-space/agentctl/internal/daemon"
	"github.com/simons-agent-space/agentctl/internal/deploy"
)

func main() {
	logPath := flag.String("log", os.Getenv("AGENTCTLD_AUDIT_LOG"), "path for audit logs (default stderr; AGENTCTLD_AUDIT_LOG when set)")
	flag.Parse()

	logWriter, closer, err := openLogWriter(*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		os.Exit(1)
	}
	if closer != nil {
		defer closer()
	}
	log := audit.New(logWriter)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	srv, err := daemon.NewServer(cfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init server: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("agentctld-startup", map[string]any{
		"socket_path":  cfg.SocketPath,
		"socket_group": cfg.SocketGroup,
		"allowed_org":  cfg.Source.AllowedOrg,
	})

	if err := srv.ListenAndServe(ctx); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
	log.Info("agentctld-stopped", nil)
}

// loadConfig builds the daemon.Config from AGENTCTLD_* environment
// variables and pre-validates every required field up front, so the
// entrypoint can exit cleanly on missing values without binding the
// socket first.
func loadConfig() (*daemon.Config, error) {
	cfg := &daemon.Config{
		SocketPath:  os.Getenv("AGENTCTLD_SOCKET_PATH"),
		SocketGroup: os.Getenv("AGENTCTLD_SOCKET_GROUP"),
		Source: deploy.SourceConfig{
			AllowedOrg:     os.Getenv("AGENTCTLD_SOURCE_ALLOWED_ORG"),
			RepositoryRoot: os.Getenv("AGENTCTLD_SOURCE_REPOSITORY_ROOT"),
			OriginURL:      os.Getenv("AGENTCTLD_SOURCE_ORIGIN_URL"),
		},
		Runtime: deploy.RuntimeConfig{
			RepositoryRoot: os.Getenv("AGENTCTLD_RUNTIME_REPOSITORY_ROOT"),
		},
		Caddy: deploy.CaddyConfig{
			BaseDomain:     os.Getenv("AGENTCTLD_CADDY_BASE_DOMAIN"),
			ConfigDir:      os.Getenv("AGENTCTLD_CADDY_CONFIG_DIR"),
			RootConfigPath: os.Getenv("AGENTCTLD_CADDY_ROOT_PATH"),
			CaddyBinary:    os.Getenv("AGENTCTLD_CADDY_BINARY"),
		},
		State: deploy.StateConfig{
			StateDir:   os.Getenv("AGENTCTLD_STATE_DIR"),
			BaseDomain: os.Getenv("AGENTCTLD_STATE_BASE_DOMAIN"),
		},
		Data: deploy.DataConfig{
			DataRoot: os.Getenv("AGENTCTLD_DATA_ROOT"),
		},
	}

	if v := os.Getenv("AGENTCTLD_SOCKET_MODE"); v != "" {
		m, err := parseOctalMode(v)
		if err != nil {
			return nil, fmt.Errorf("AGENTCTLD_SOCKET_MODE: %w", err)
		}
		cfg.SocketMode = m
	}
	if v := os.Getenv("AGENTCTLD_MAX_REQUEST_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("AGENTCTLD_MAX_REQUEST_BYTES: %w", err)
		}
		cfg.MaxRequestBytes = n
	}
	if v := os.Getenv("AGENTCTLD_RUNTIME_PORT_RANGE_START"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("AGENTCTLD_RUNTIME_PORT_RANGE_START: %w", err)
		}
		cfg.Runtime.PortRangeStart = n
	}
	if v := os.Getenv("AGENTCTLD_RUNTIME_PORT_RANGE_END"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("AGENTCTLD_RUNTIME_PORT_RANGE_END: %w", err)
		}
		cfg.Runtime.PortRangeEnd = n
	}
	if v := os.Getenv("AGENTCTLD_RUNTIME_HEALTH_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("AGENTCTLD_RUNTIME_HEALTH_TIMEOUT: %w", err)
		}
		cfg.Runtime.HealthTimeout = d
	}
	if v := os.Getenv("AGENTCTLD_OPERATION_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("AGENTCTLD_OPERATION_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("AGENTCTLD_OPERATION_TIMEOUT must be positive")
		}
		cfg.OperationTimeout = d
	}
	if v := os.Getenv("AGENTCTLD_WRITE_TIMEOUT_MARGIN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("AGENTCTLD_WRITE_TIMEOUT_MARGIN: %w", err)
		}
		if d < 0 {
			return nil, fmt.Errorf("AGENTCTLD_WRITE_TIMEOUT_MARGIN must not be negative")
		}
		// Always assign through a pointer so operators can
		// distinguish "unset" (nil pointer → default margin) from
		// "explicitly zero" (→ no headroom, WriteTimeout equals
		// OperationTimeout). The env-var docstring promises this
		// behaviour and the pointer is what makes it observable
		// inside the daemon.
		cfg.WriteTimeoutMargin = &d
	}
	if cfg.Caddy.CaddyBinary == "" {
		cfg.Caddy.CaddyBinary = "caddy"
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if _, _, err := daemon.ResolveSocketGroup(cfg.SocketGroup); err != nil {
		return nil, fmt.Errorf("AGENTCTLD_SOCKET_GROUP: %w", err)
	}
	if err := requireNonEmpty(map[string]string{
		"AGENTCTLD_SOURCE_REPOSITORY_ROOT":  cfg.Source.RepositoryRoot,
		"AGENTCTLD_SOURCE_ORIGIN_URL":       cfg.Source.OriginURL,
		"AGENTCTLD_SOURCE_ALLOWED_ORG":      cfg.Source.AllowedOrg,
		"AGENTCTLD_RUNTIME_REPOSITORY_ROOT": cfg.Runtime.RepositoryRoot,
		"AGENTCTLD_CADDY_BASE_DOMAIN":       cfg.Caddy.BaseDomain,
		"AGENTCTLD_CADDY_CONFIG_DIR":        cfg.Caddy.ConfigDir,
		"AGENTCTLD_CADDY_ROOT_PATH":         cfg.Caddy.RootConfigPath,
		"AGENTCTLD_STATE_DIR":               cfg.State.StateDir,
		"AGENTCTLD_STATE_BASE_DOMAIN":       cfg.State.BaseDomain,
		"AGENTCTLD_DATA_ROOT":               cfg.Data.DataRoot,
	}); err != nil {
		return nil, err
	}
	if cfg.Runtime.PortRangeStart <= 0 || cfg.Runtime.PortRangeEnd <= 0 {
		return nil, fmt.Errorf("AGENTCTLD_RUNTIME_PORT_RANGE_START and AGENTCTLD_RUNTIME_PORT_RANGE_END must be positive")
	}
	if cfg.Runtime.PortRangeStart > cfg.Runtime.PortRangeEnd {
		return nil, fmt.Errorf("AGENTCTLD_RUNTIME_PORT_RANGE_START (%d) exceeds AGENTCTLD_RUNTIME_PORT_RANGE_END (%d)", cfg.Runtime.PortRangeStart, cfg.Runtime.PortRangeEnd)
	}
	if cfg.Runtime.HealthTimeout <= 0 {
		return nil, fmt.Errorf("AGENTCTLD_RUNTIME_HEALTH_TIMEOUT must be positive")
	}
	if cfg.OperationTimeout < 0 {
		return nil, fmt.Errorf("AGENTCTLD_OPERATION_TIMEOUT must not be negative")
	}
	return cfg, nil
}

// requireNonEmpty reports the first empty named value so the
// operator gets a specific missing-key message rather than a
// downstream config-layer surprise. Numeric and duration fields
// are validated by the explicit range checks below; this helper
// only covers pure-string config so it stays free of value
// encoding.
func requireNonEmpty(values map[string]string) error {
	for name, v := range values {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}

// parseOctalMode parses a unix mode expressed as octal digits,
// accepting the common shapes "660", "0660", and the Go literal
// "0o660".
func parseOctalMode(s string) (os.FileMode, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty mode")
	}
	s = strings.TrimPrefix(s, "0o")
	s = strings.TrimPrefix(s, "0O")
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("expected an octal mode (e.g. 0660 or 0o660): %w", err)
	}
	return os.FileMode(n), nil
}

func openLogWriter(path string) (writer *os.File, closer func() error, err error) {
	if path == "" {
		return os.Stderr, nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}
