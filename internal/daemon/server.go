package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/simons-agent-space/agentctl/internal/audit"
	"github.com/simons-agent-space/agentctl/internal/deploy"
)

// appNameRe mirrors the deploy package's regex so the daemon can
// reject malformed URLs before parsing the body. We deliberately
// duplicate the regex rather than import the internal one to keep
// the daemon's input-validation rules visible at the package
// boundary.
var appNameRe = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`)

// shaRe matches exactly 40 lowercase hexadecimal characters.
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Default socket permissions. Mode 0660 keeps the daemon
// reachable by the daemon process group while excluding "other".
// The group is supplied via configuration and resolved to a GID
// by the entrypoint before listen.
const defaultSocketMode os.FileMode = 0o660

// default request-size cap (64 KiB). Every JSON request body
// for this API is well under that; rejecting larger bodies
// protects the daemon from accidental abuse or memory pressure.
const defaultMaxRequestBytes int64 = 1 << 16

// default HTTP server timeouts. ReadHeaderTimeout is short so a
// stalled peer cannot tie up the socket; ReadTimeout is long
// enough to amortise a large request body. WriteTimeout must be at
// least as long as the longest operation the daemon runs (see
// OperationTimeout below) so a 30-minute deploy that ends in a
// small JSON response is not cut off mid-write. We derive
// WriteTimeout from OperationTimeout + WriteTimeoutMargin so it
// automatically tracks whatever operation budget the operator
// configures.
const (
	defaultReadHeaderTimeout  = 5 * time.Second
	defaultReadTimeout        = 60 * time.Second
	defaultWriteTimeout       = 0
	defaultIdleTimeout        = 120 * time.Second
	defaultOperationTimeout   = 30 * time.Minute
	defaultWriteTimeoutMargin = 5 * time.Minute
	defaultSocketProbeTimeout = 500 * time.Millisecond
)

// Config is the daemon's trusted host-side configuration. All
// fields except SocketPath, SocketGroup, MaxRequestBytes,
// OperationTimeout, SocketProbeTimeout, and WriteTimeoutMargin are
// forwarders into the deploy package's config types. The daemon
// does not mutate these structs after construction.
type Config struct {
	// SocketPath is the absolute path of the Unix domain socket
	// the daemon will listen on.
	SocketPath string
	// SocketGroup is the optional group name or numeric GID that
	// the socket is chowned to after creation. Empty means
	// "leave the group as the daemon process's primary group".
	// An unresolvable value is a fatal configuration error at
	// startup.
	SocketGroup string
	// SocketMode overrides the default socket permissions.
	// Zero value means defaultSocketMode.
	SocketMode os.FileMode
	// MaxRequestBytes caps the request body. Zero means
	// defaultMaxRequestBytes.
	MaxRequestBytes int64
	// OperationTimeout caps the wall-clock duration of a deploy or
	// rollback handler. Defaults to defaultOperationTimeout (30
	// minutes) when zero. The HTTP WriteTimeout is derived from this
	// value so an operation that completes inside its budget can
	// always write its response.
	OperationTimeout time.Duration
	// SocketProbeTimeout is the dial timeout used when probing
	// a pre-existing socket file at startup. Zero means
	// defaultSocketProbeTimeout.
	SocketProbeTimeout time.Duration
	// WriteTimeoutMargin is added to OperationTimeout when
	// computing the HTTP server's WriteTimeout. Zero means
	// defaultWriteTimeoutMargin.
	WriteTimeoutMargin time.Duration
	// Source is the trusted source-resolution configuration.
	Source deploy.SourceConfig
	// Runtime is the trusted runtime configuration.
	Runtime deploy.RuntimeConfig
	// Caddy is the trusted Caddy configuration.
	Caddy deploy.CaddyConfig
	// State is the trusted state-storage configuration.
	State deploy.StateConfig
	// Data is the trusted persistent-data configuration.
	Data deploy.DataConfig
}

// Validate checks that Config is well-formed before listen.
func (c *Config) Validate() error {
	if c.SocketPath == "" {
		return fmt.Errorf("%w: SocketPath is required", ErrInvalidRequest)
	}
	if !filepath.IsAbs(c.SocketPath) {
		return fmt.Errorf("%w: SocketPath %q must be absolute", ErrInvalidRequest, c.SocketPath)
	}
	if c.Source.RepositoryRoot == "" || c.Source.OriginURL == "" || c.Source.AllowedOrg == "" {
		return fmt.Errorf("%w: Source configuration is incomplete", ErrInvalidRequest)
	}
	if c.Runtime.PortRangeStart == 0 || c.Runtime.PortRangeEnd == 0 {
		return fmt.Errorf("%w: Runtime port range is required", ErrInvalidRequest)
	}
	if c.Caddy.BaseDomain == "" {
		return fmt.Errorf("%w: Caddy.BaseDomain is required", ErrInvalidRequest)
	}
	if c.State.StateDir == "" || c.State.BaseDomain == "" {
		return fmt.Errorf("%w: State configuration is incomplete", ErrInvalidRequest)
	}
	return nil
}

// SocketMode returns the effective socket mode (default applied).
func (c *Config) SocketModeOrDefault() os.FileMode {
	if c.SocketMode != 0 {
		return c.SocketMode
	}
	return defaultSocketMode
}

// MaxRequestBytesOrDefault returns the effective body cap.
func (c *Config) MaxRequestBytesOrDefault() int64 {
	if c.MaxRequestBytes > 0 {
		return c.MaxRequestBytes
	}
	return defaultMaxRequestBytes
}

// OperationTimeoutOrDefault returns the effective operation
// timeout. The default is large enough for a slow first-time build
// of an app image because the daemon's HTTP server derives its
// WriteTimeout from this value: a 30-minute deploy followed by a
// small JSON response must reach the client.
func (c *Config) OperationTimeoutOrDefault() time.Duration {
	if c.OperationTimeout > 0 {
		return c.OperationTimeout
	}
	return defaultOperationTimeout
}

// SocketProbeTimeoutOrDefault returns the dial timeout used when
// probing a pre-existing socket file at startup.
func (c *Config) SocketProbeTimeoutOrDefault() time.Duration {
	if c.SocketProbeTimeout > 0 {
		return c.SocketProbeTimeout
	}
	return defaultSocketProbeTimeout
}

// WriteTimeoutMarginOrDefault returns the margin added to the
// operation timeout when computing the HTTP server's WriteTimeout.
func (c *Config) WriteTimeoutMarginOrDefault() time.Duration {
	if c.WriteTimeoutMargin > 0 {
		return c.WriteTimeoutMargin
	}
	return defaultWriteTimeoutMargin
}

// WriteTimeout returns the HTTP WriteTimeout derived from the
// operation timeout and its margin. Returns 0 (no timeout) when
// the operation timeout is 0; otherwise returns operation + margin
// so a deploy that runs up to its budget can still write its
// response.
func (c *Config) WriteTimeout() time.Duration {
	op := c.OperationTimeoutOrDefault()
	if op <= 0 {
		return 0
	}
	return op + c.WriteTimeoutMarginOrDefault()
}

// dockerRunner abstracts `docker inspect` so tests can substitute
// a fake. Production uses execDockerRunner.
type dockerRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// execDockerRunner invokes a binary through os/exec.
type execDockerRunner struct{}

func (execDockerRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	return string(out), err
}

// Server is the UDS listener, HTTP router, and per-app lock
// manager. It owns the lifecycle of the listener and the
// graceful-shutdown signal.
//
// The deployer field is an indirection over deploy.Deploy and
// deploy.RollbackDeployment so handler tests can substitute a
// fake without touching real Docker / Caddy / filesystem state.
// It defaults to realDeployer{} in production.
type Server struct {
	cfg      *Config
	log      *audit.Logger
	locks    *appLocker
	deployer Deployer
	docker   dockerRunner

	// shuttingDown is set to true when Shutdown is called. While
	// set, new mutating requests are rejected with
	// ErrShuttingDown; in-flight requests are allowed to finish.
	shuttingDown atomic.Bool
}

// Deployer is the abstraction the handlers use to invoke the
// deploy / rollback pipeline. Production wires realDeployer{};
// tests wire a recording fake.
type Deployer interface {
	Deploy(ctx context.Context, cfg deploy.DeployConfig, manifest deploy.Manifest, commit string) (*deploy.DeployResult, error)
	Rollback(ctx context.Context, cfg deploy.RollbackConfig) error
}

// realDeployer delegates to the deploy package.
type realDeployer struct{}

func (realDeployer) Deploy(ctx context.Context, cfg deploy.DeployConfig, manifest deploy.Manifest, commit string) (*deploy.DeployResult, error) {
	return deploy.Deploy(ctx, cfg, manifest, commit)
}

func (realDeployer) Rollback(ctx context.Context, cfg deploy.RollbackConfig) error {
	return deploy.RollbackDeployment(ctx, cfg)
}

// NewServer prepares a Server from a validated config and an
// audit logger. It does not bind the socket; call ListenAndServe.
func NewServer(cfg *Config, log *audit.Logger) (*Server, error) {
	return NewServerWithDeployer(cfg, log, realDeployer{}, execDockerRunner{})
}

// NewServerWithDeployer is like NewServer but allows callers
// (tests) to substitute the deployer and docker runner.
func NewServerWithDeployer(cfg *Config, log *audit.Logger, dep Deployer, dr dockerRunner) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		return nil, fmt.Errorf("audit logger is required")
	}
	if dep == nil {
		return nil, fmt.Errorf("deployer is required")
	}
	if dr == nil {
		return nil, fmt.Errorf("docker runner is required")
	}
	return &Server{
		cfg:      cfg,
		log:      log,
		locks:    newAppLocker(),
		deployer: dep,
		docker:   dr,
	}, nil
}

// Shutdown marks the server as draining. New mutating requests
// are rejected with 503; the underlying http.Server's Shutdown
// is called separately by the entrypoint. Safe to call multiple
// times.
func (s *Server) Shutdown() {
	s.shuttingDown.Store(true)
}

// IsShuttingDown reports whether Shutdown has been called.
func (s *Server) IsShuttingDown() bool {
	return s.shuttingDown.Load()
}

// prepareSocket handles the pre-bind path at the configured socket
// location. It is the inverse of the previous "removeStaleSocket":
//
//   - If the path does not exist, it does nothing.
//   - If the path is occupied by a regular file (or anything that is
//     not a Unix socket), it refuses to touch it; that signals
//     operator misconfiguration rather than a stale socket.
//   - If the path is occupied by a socket, it probes whether a
//     daemon is actively listening. A successful probe means
//     "another agentctld is running here, refuse to start" and is
//     returned as ErrSocketInUse so the entrypoint exits non-zero
//     with a clear message. A failed probe (connection refused)
//     means the socket file is stale and is safe to unlink.
//
// The probe-then-unlink order is the difference between this
// implementation and a naive "always Remove" helper: removing a
// socket file out from under a running daemon disconnects its
// clients without warning and races bind attempts, while here we
// only remove the file when nothing is bound to it. The remaining
// race between probe and Listen is unavoidable and surfaces as a
// bind error if another process grabs the path in the gap.
func prepareSocket(path string, probeTimeout time.Duration) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket file at %s", path)
	}
	if probeSocket(path, probeTimeout) {
		return fmt.Errorf("%w: %s", ErrSocketInUse, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

// probeSocket returns true when a daemon is actively accepting
// connections on path. A short dial timeout avoids hanging on a
// listener that has accepted its queue but stopped processing; the
// caller's definition of "stale" is "not accepting a connection
// inside the probe budget".
func probeSocket(path string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = defaultSocketProbeTimeout
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// requireSocketParent verifies the parent directory of path exists
// and is a directory. We do not create it: the process supervisor
// (systemd RuntimeDirectory=, runit, OpenRC, ...) owns that
// directory's ownership and permissions, and the daemon must never
// silently override them.
func requireSocketParent(path string) error {
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrSocketParentMissing, parent)
		}
		return fmt.Errorf("stat socket parent %s: %w", parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrSocketParentMissing, parent)
	}
	return nil
}

// ListenAndServe binds the socket and serves until ctx is
// cancelled or a non-recoverable error occurs. Stale sockets are
// removed before bind. The socket is chmod'd after listen. On
// return the socket is closed and removed.
//
// The pre-bind sequence is:
//
//  1. requireSocketParent: refuse to start if the configured
//     socket's parent directory is missing or is not a directory.
//     The daemon does not create it; the process supervisor owns
//     that directory. There is no operator override: the contract
//     is fixed because creating the parent inside the daemon
//     would mask supervisor misconfiguration.
//  2. prepareSocket: probe a pre-existing socket, fail when a
//     daemon is actively listening, otherwise unlink it.
//
// The HTTP server's WriteTimeout is derived from
// cfg.OperationTimeout + cfg.WriteTimeoutMargin so an operation
// that completes inside its budget can always write its response.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := requireSocketParent(s.cfg.SocketPath); err != nil {
		return err
	}
	if err := prepareSocket(s.cfg.SocketPath, s.cfg.SocketProbeTimeoutOrDefault()); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.SocketPath, err)
	}
	defer listener.Close()
	defer func() { _ = os.Remove(s.cfg.SocketPath) }()

	if err := os.Chmod(s.cfg.SocketPath, s.cfg.SocketModeOrDefault()); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}
	if gid, ok, err := ResolveSocketGroup(s.cfg.SocketGroup); err != nil {
		return err
	} else if ok {
		if err := os.Chown(s.cfg.SocketPath, -1, gid); err != nil {
			return fmt.Errorf("chown socket to group %s: %w", s.cfg.SocketGroup, err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/apps", s.handleApps)
	mux.HandleFunc("/v1/apps/", s.handleAppByName)
	mux.HandleFunc("/v1/inspect", s.handleInspect)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout(),
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    1 << 16,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	s.log.Info("daemon-listening", map[string]any{
		"socket_path":  s.cfg.SocketPath,
		"socket_mode":  s.cfg.SocketModeOrDefault().String(),
		"socket_group": s.cfg.SocketGroup,
	})

	select {
	case <-ctx.Done():
		s.Shutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// ResolveSocketGroup converts a group name or numeric GID string
// to a numeric GID. Empty input returns ok=false so the caller
// skips Chown. An unresolvable value is returned as an error
// rather than panicking so the entrypoint can surface it as a
// fatal configuration error and exit non-zero. Exported so the
// entrypoint can pre-validate the configured group before bind.
func ResolveSocketGroup(name string) (int, bool, error) {
	if name == "" {
		return 0, false, nil
	}
	if gid, err := strconv.Atoi(name); err == nil {
		return gid, true, nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, false, fmt.Errorf("socket group %q: %w", name, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, false, fmt.Errorf("socket group %q has non-numeric gid %q: %w", name, g.Gid, err)
	}
	return gid, true, nil
}

// handleHealthz returns 200 OK for liveness/readiness checks.
// The daemon is considered ready as soon as ListenAndServe binds
// the socket; there is no warm-up period because every handler
// either serves from in-process state or delegates to the deploy
// layer, both of which are ready from the first request.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleApps routes GET /v1/apps to the list handler.
func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	apps, err := listManagedApps(s.cfg.State.StateDir)
	if err != nil {
		s.writeDaemonError(w, http.StatusInternalServerError, "internal", err)
		return
	}
	writeJSON(w, http.StatusOK, ListAppsResponse{Apps: apps})
}

// handleAppByName dispatches /v1/apps/{app}/<action> to the
// right handler based on the URL suffix.
func (s *Server) handleAppByName(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1/apps/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	app := parts[0]
	if !appNameRe.MatchString(app) {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, "app name does not match app-name format")
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "":
		http.NotFound(w, r)
	case "state":
		s.handleState(w, r, app)
	case "status":
		s.handleStatus(w, r, app)
	case "deploy":
		s.handleDeploy(w, r, app)
	case "rollback":
		s.handleRollback(w, r, app)
	default:
		http.NotFound(w, r)
	}
}

// handleState returns the persisted current and previous
// deployment for app. Returns 404 when the app is not managed.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	state, err := deploy.LoadDeploymentState(s.cfg.State, app)
	if err != nil {
		if errors.Is(err, deploy.ErrDeploymentStateNotFound) {
			writeError(w, http.StatusNotFound, "not_found", ErrAppNotManaged, "no state for app")
			return
		}
		s.writeDaemonError(w, http.StatusInternalServerError, "internal", err)
		return
	}
	writeJSON(w, http.StatusOK, StateResponse{
		App:      app,
		Current:  summaryFromDeployment(state.Current),
		Previous: summaryFromDeployment(state.Previous),
	})
}

// handleStatus returns the persisted state and the runtime
// container status. Container status is a best-effort docker
// inspect that never blocks the response when the container is
// absent.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	resp := StatusResponse{App: app}
	state, err := deploy.LoadDeploymentState(s.cfg.State, app)
	if err != nil {
		if !errors.Is(err, deploy.ErrDeploymentStateNotFound) {
			s.writeDaemonError(w, http.StatusInternalServerError, "internal", err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Current = summaryFromDeployment(state.Current)
	resp.Previous = summaryFromDeployment(state.Previous)

	if state.Current != nil {
		status, statusErr := inspectContainerStatus(r.Context(), s.docker, state.Current.ContainerName)
		switch {
		case statusErr == nil:
			resp.ContainerStatus = status
		case errors.Is(statusErr, errContainerAbsent):
			resp.ContainerStatus = "absent"
		default:
			resp.StatusCheckError = statusErr.Error()
			resp.ContainerStatus = "unknown"
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleInspect validates a manifest and resolves the commit
// against the trusted source mirror without performing any
// deployment. Manifest validation failures produce 200 with
// Valid=false so callers can surface them to a human reviewer.
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if s.IsShuttingDown() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", ErrShuttingDown, "daemon is draining")
		return
	}
	req, err := decodeInspectRequest(w, r, s.cfg.MaxRequestBytesOrDefault())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, err.Error())
		return
	}

	resp := InspectResponse{
		App:    req.App,
		Commit: req.Commit,
		Valid:  true,
	}

	manifest, perr := parseStrictManifest(req.Manifest)
	if perr != nil {
		resp.Valid = false
		resp.Errors = []string{perr.Error()}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.ManifestVersion = manifest.Version

	if verr := deploy.Validate(manifest, req.App); verr != nil {
		resp.Valid = false
		resp.Errors = append(resp.Errors, verr.Error())
	}

	// Acquire the per-app lock so a concurrent deploy does not
	// race the source resolution. We use TryAcquire because
	// inspect is a "pre-flight" check; a 409 here tells the
	// caller to retry once the in-flight deploy/rollback has
	// completed.
	release, lerr := s.locks.TryAcquire(req.App)
	if lerr != nil {
		writeError(w, http.StatusConflict, "conflict", ErrConcurrentOperation, lerr.Error())
		return
	}
	defer release()

	// Only resolve the source when the manifest is valid; an
	// invalid proposal must never produce side effects.
	if resp.Valid {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		// VerifyCommit is the side-effect-free counterpart of
		// CheckoutSource: it validates the commit against the
		// trusted mirror without creating a worktree, so the
		// handler does not have to schedule a CleanupCheckout
		// pass. ensureMirror still creates the bare mirror on
		// first use; that mirror is shared with deploys and
		// therefore not a per-inspect side effect.
		if verr := deploy.VerifyCommit(ctx, s.cfg.Source, s.cfg.Source.AllowedOrg, req.App, req.Commit); verr != nil {
			resp.SourceReachable = false
			resp.Valid = false
			resp.Errors = append(resp.Errors, fmt.Sprintf("source: %v", verr))
		} else {
			resp.SourceReachable = true
		}
	}

	if state, err := deploy.LoadDeploymentState(s.cfg.State, req.App); err == nil {
		resp.Current = summaryFromDeployment(state.Current)
		resp.Previous = summaryFromDeployment(state.Previous)
	} else if !errors.Is(err, deploy.ErrDeploymentStateNotFound) {
		resp.Errors = append(resp.Errors, fmt.Sprintf("load state: %v", err))
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleDeploy runs Deploy for an approved commit and manifest.
// The per-app lock prevents two deploys from racing; an in-flight
// deploy is reported as 409 Conflict.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if s.IsShuttingDown() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", ErrShuttingDown, "daemon is draining")
		return
	}
	req, err := decodeDeployRequest(w, r, s.cfg.MaxRequestBytesOrDefault())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, err.Error())
		return
	}
	if !appNameRe.MatchString(app) || !shaRe.MatchString(req.Commit) {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, "app or commit malformed")
		return
	}

	manifest, perr := parseStrictManifest(req.Manifest)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, perr.Error())
		return
	}
	if manifest.App != app {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, "manifest app does not match URL app")
		return
	}
	if verr := deploy.Validate(manifest, app); verr != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, verr.Error())
		return
	}

	release, lerr := s.locks.TryAcquire(app)
	if lerr != nil {
		writeError(w, http.StatusConflict, "conflict", ErrConcurrentOperation, lerr.Error())
		return
	}
	defer release()

	if s.IsShuttingDown() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", ErrShuttingDown, "daemon is draining")
		return
	}

	deployCfg := deploy.DeployConfig{
		Source:  s.cfg.Source,
		Runtime: s.cfg.Runtime,
		Caddy:   s.cfg.Caddy,
		State:   s.cfg.State,
		Data:    s.cfg.Data,
	}
	// Use a fresh background context for the deploy itself so a
	// cancelled HTTP request cannot interrupt an in-flight
	// deployment mid-pipeline. Graceful shutdown cooperates by
	// calling Shutdown on the server which cancels new requests
	// and waits for in-flight handlers to drain via the
	// http.Server.Shutdown path. The timeout is tied to the
	// configured OperationTimeout (which also drives the HTTP
	// server's WriteTimeout).
	deployCtx, cancel := context.WithTimeout(context.Background(), s.cfg.OperationTimeoutOrDefault())
	defer cancel()

	res, derr := s.deployer.Deploy(deployCtx, deployCfg, *manifest, req.Commit)
	if derr != nil {
		s.log.Error("deploy-failed", map[string]any{
			"app":    app,
			"commit": req.Commit,
			"error":  derr.Error(),
		})
		code := "internal"
		if errors.Is(derr, deploy.ErrInvalidDeployInput) || errors.Is(derr, deploy.ErrInvalidInput) {
			code = "invalid_request"
		}
		writeDeployError(w, derr, code)
		return
	}

	s.log.Info("deploy-succeeded", map[string]any{
		"app":            res.App,
		"commit":         res.Commit,
		"container_name": res.ContainerName,
		"host_port":      res.HostPort,
	})

	resp := DeployResponse{
		App:           res.App,
		Commit:        res.Commit,
		Image:         res.Image,
		ContainerName: res.ContainerName,
		HostPort:      res.HostPort,
		ContainerPort: res.ContainerPort,
		Hostname:      res.Hostname,
		Upstream:      res.Upstream,
		DeployedAt:    res.DeployedAt,
	}
	for _, w := range res.Warnings {
		resp.Warnings = append(resp.Warnings, w.Error())
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRollback runs RollbackDeployment for app. HealthPath is
// required because the persisted state does not include it.
// Concurrent deploys / rollbacks for the same app are serialized
// by the per-app lock.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if s.IsShuttingDown() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", ErrShuttingDown, "daemon is draining")
		return
	}
	req, err := decodeRollbackRequest(w, r, s.cfg.MaxRequestBytesOrDefault())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, err.Error())
		return
	}
	if req.HealthPath == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrHealthPathRequired, "health_path is required")
		return
	}
	if !strings.HasPrefix(req.HealthPath, "/") || strings.ContainsAny(req.HealthPath, "?#") {
		writeError(w, http.StatusBadRequest, "invalid_request", ErrInvalidRequest, "health_path must start with / and contain no ? or #")
		return
	}

	release, lerr := s.locks.TryAcquire(app)
	if lerr != nil {
		writeError(w, http.StatusConflict, "conflict", ErrConcurrentOperation, lerr.Error())
		return
	}
	defer release()

	if s.IsShuttingDown() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", ErrShuttingDown, "daemon is draining")
		return
	}

	rollbackCfg := deploy.RollbackConfig{
		App:        app,
		State:      s.cfg.State,
		Runtime:    s.cfg.Runtime,
		Caddy:      s.cfg.Caddy,
		Data:       s.cfg.Data,
		HealthPath: req.HealthPath,
	}

	rollbackCtx, cancel := context.WithTimeout(context.Background(), s.cfg.OperationTimeoutOrDefault())
	defer cancel()
	if rerr := s.deployer.Rollback(rollbackCtx, rollbackCfg); rerr != nil {
		s.log.Error("rollback-failed", map[string]any{
			"app":   app,
			"error": rerr.Error(),
		})
		// A rollback of an app that has never had a previous
		// deployment is a normal operational outcome (e.g. first
		// deployment got rolled back to itself), not an internal
		// fault. Surface it as 409 Conflict so callers can branch
		// without parsing the error string.
		code := "internal"
		if errors.Is(rerr, deploy.ErrNoPreviousDeployment) {
			code = "no_previous"
		}
		writeDeployError(w, rerr, code)
		return
	}

	state, lerr := deploy.LoadDeploymentState(s.cfg.State, app)
	if lerr != nil {
		s.writeDaemonError(w, http.StatusInternalServerError, "internal", lerr)
		return
	}
	if state.Current == nil {
		s.writeDaemonError(w, http.StatusInternalServerError, "internal", fmt.Errorf("rollback succeeded but state has no Current"))
		return
	}
	d := *state.Current
	resp := RollbackResponse{
		App:           d.App,
		Commit:        d.Commit,
		Image:         d.Image,
		ContainerName: d.ContainerName,
		HostPort:      d.HostPort,
		ContainerPort: d.ContainerPort,
		Hostname:      d.Hostname,
		Upstream:      d.Upstream,
		DeployedAt:    d.DeployedAt,
	}
	s.log.Info("rollback-succeeded", map[string]any{
		"app":            d.App,
		"commit":         d.Commit,
		"container_name": d.ContainerName,
	})
	writeJSON(w, http.StatusOK, resp)
}

// listManagedApps reads the state directory and returns the
// sorted app names for which a valid state file exists. Entries
// that do not pass the app-name regex are silently skipped (they
// cannot have been produced by SaveDeployment). Sub-directories
// and non-state files are skipped.
func listManagedApps(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	apps := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".state.json") {
			continue
		}
		app := strings.TrimSuffix(name, ".state.json")
		if !appNameRe.MatchString(app) {
			continue
		}
		apps = append(apps, app)
	}
	for i := 1; i < len(apps); i++ {
		for j := i; j > 0 && apps[j-1] > apps[j]; j-- {
			apps[j-1], apps[j] = apps[j], apps[j-1]
		}
	}
	return apps, nil
}

// decodeInspectRequest reads and validates a JSON body for an
// inspect request.
func decodeInspectRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (*InspectRequest, error) {
	body, err := readCappedBody(w, r, maxBytes)
	if err != nil {
		return nil, err
	}
	var req InspectRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after body")
	}
	if !appNameRe.MatchString(req.App) {
		return nil, fmt.Errorf("app %q does not match app-name format", req.App)
	}
	if !shaRe.MatchString(req.Commit) {
		return nil, fmt.Errorf("commit %q is not exactly 40 lowercase hex characters", req.Commit)
	}
	if len(req.Manifest) == 0 {
		return nil, fmt.Errorf("manifest is required")
	}
	return &req, nil
}

// decodeDeployRequest reads and validates a JSON body for a
// deploy request.
func decodeDeployRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (*DeployRequest, error) {
	body, err := readCappedBody(w, r, maxBytes)
	if err != nil {
		return nil, err
	}
	var req DeployRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after body")
	}
	if !shaRe.MatchString(req.Commit) {
		return nil, fmt.Errorf("commit %q is not exactly 40 lowercase hex characters", req.Commit)
	}
	if len(req.Manifest) == 0 {
		return nil, fmt.Errorf("manifest is required")
	}
	return &req, nil
}

// decodeRollbackRequest reads and validates a JSON body for a
// rollback request.
func decodeRollbackRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (*RollbackRequest, error) {
	body, err := readCappedBody(w, r, maxBytes)
	if err != nil {
		return nil, err
	}
	var req RollbackRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after body")
	}
	return &req, nil
}

// readCappedBody returns the request body as bytes, capped at
// maxBytes. http.MaxBytesReader surfaces an oversize body as a
// read error that the daemon reports as ErrInvalidRequest.
func readCappedBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil {
		return nil, fmt.Errorf("body is required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return b, nil
}

// parseStrictManifest decodes a raw manifest JSON body with
// DisallowUnknownFields + no-trailing-data rules matching the
// deploy package's Load rules.
func parseStrictManifest(raw json.RawMessage) (*deploy.Manifest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("manifest is required")
	}
	var m deploy.Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after manifest")
	}
	if m.Version == 1 && m.Data != nil {
		return nil, fmt.Errorf("data field is not allowed in version 1 manifests (bump to version 2)")
	}
	return &m, nil
}

// writeJSON marshals v as JSON and writes it with the given
// status code.
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeMethodNotAllowed replies with a 405 and a small JSON
// body.
func writeMethodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", ErrInvalidRequest, "method not allowed")
}

// writeError writes a structured error response. sentinel is
// preserved so callers can branch on errors.Is; it is also
// surfaced as a Sentinels entry for callers that prefer string
// comparison.
func writeError(w http.ResponseWriter, code int, label string, sentinel error, message string) {
	writeJSON(w, code, ErrorResponse{
		Error:     message,
		Code:      label,
		Sentinels: []string{sentinel.Error()},
	})
}

// writeDeployError walks the error chain and exposes every
// distinct error message in Sentinels.
func writeDeployError(w http.ResponseWriter, err error, label string) {
	sentinels := collectSentinels(err)
	code := http.StatusInternalServerError
	switch label {
	case "invalid_request":
		code = http.StatusBadRequest
	case "no_previous":
		code = http.StatusConflict
	}
	writeJSON(w, code, ErrorResponse{
		Error:     err.Error(),
		Code:      label,
		Sentinels: sentinels,
	})
}

// writeDaemonError is a thin wrapper used by read-only handlers
// that do not wrap a deploy-layer error directly.
func (s *Server) writeDaemonError(w http.ResponseWriter, code int, label string, err error) {
	writeJSON(w, code, ErrorResponse{
		Error:     err.Error(),
		Code:      label,
		Sentinels: collectSentinels(err),
	})
}

// collectSentinels walks err's chain and returns the Error()
// strings of every distinct error. Duplicates are dropped.
func collectSentinels(err error) []string {
	if err == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	cur := err
	for cur != nil {
		msg := cur.Error()
		if _, ok := seen[msg]; !ok {
			seen[msg] = struct{}{}
			out = append(out, msg)
		}
		cur = errors.Unwrap(cur)
	}
	return out
}

// errContainerAbsent is the sentinel returned by
// inspectContainerStatus when the container does not exist.
var errContainerAbsent = errors.New("container is absent")

// inspectContainerStatus shells out to `docker inspect` and
// maps the output to a short status string. "No such container"
// / "No such object" is mapped to errContainerAbsent so callers
// can distinguish "absent" from a real inspect failure.
func inspectContainerStatus(ctx context.Context, runner dockerRunner, containerName string) (string, error) {
	if !appNameRe.MatchString(containerName) {
		return "unknown", fmt.Errorf("container name %q is not a valid app name", containerName)
	}
	out, err := runner.Run(ctx, "docker", "inspect", "--format", "{{.State.Running}}", containerName)
	if err != nil {
		if strings.Contains(out, "No such container") || strings.Contains(out, "No such object") {
			return "absent", errContainerAbsent
		}
		return "unknown", fmt.Errorf("docker inspect %s: %v", containerName, err)
	}
	if strings.TrimSpace(out) == "true" {
		return "running", nil
	}
	return "stopped", nil
}
