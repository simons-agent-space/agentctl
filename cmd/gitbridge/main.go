// Command gitbridge is a small security-focused GitHub App token broker.
// It listens on a Unix domain socket, accepts a request containing a
// repository name and the fixed "builder" permission profile, and returns
// a short-lived GitHub installation token scoped to that repository.
//
// The broker is designed to run on a host that is more trusted than the
// agent sandbox: it owns the GitHub App's private key and never exposes
// it across the trust boundary. The sandbox only ever talks to the
// broker over a local UDS socket.
//
// Configuration is loaded from a single JSON file whose path is given
// by --config (or the GITBRIDGE_CONFIG env var).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/simons-agent-space/agentctl/internal/audit"
	"github.com/simons-agent-space/agentctl/internal/gitbridge"
	gh "github.com/simons-agent-space/agentctl/internal/github"
)

func main() {
	configPath := flag.String("config", envOr("GITBRIDGE_CONFIG", "/etc/gitbridge/config.json"), "path to JSON config file")
	logPath := flag.String("log", envOr("GITBRIDGE_LOG", ""), "path to audit log file (default stderr)")
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

	cfg, err := gitbridge.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	ghClient := gh.NewClient()

	srv, err := gitbridge.NewServer(cfg, ghClient, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init server: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("startup", map[string]any{
		"socket_path":         cfg.SocketPath,
		"allowed_org":         cfg.AllowedOrg,
		"allowed_repos_count": len(cfg.AllowedRepos),
	})

	if err := srv.ListenAndServe(ctx); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

func openLogWriter(path string) (writer *os.File, closer func() error, err error) {
	if path == "" {
		return os.Stderr, nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
