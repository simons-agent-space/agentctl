package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RuntimeConfig holds host-side runtime configuration for the
// candidate-container phase. All fields are trusted host configuration,
// not caller-supplied.
type RuntimeConfig struct {
	// PortRangeStart and PortRangeEnd define the inclusive range of
	// localhost ports the runtime may allocate. Both must be in
	// [1024, 65535] and PortRangeStart must not exceed PortRangeEnd.
	PortRangeStart int
	PortRangeEnd   int
	// HealthTimeout is the total poll budget for the health check.
	HealthTimeout time.Duration
	// RepositoryRoot is the trusted host path used to derive the
	// expected checkout path for validation.
	RepositoryRoot string
}

// CandidateResult describes a successfully health-checked candidate
// container. The values are derived entirely from validated inputs and
// the fixed naming rules; callers cannot inject arbitrary names.
type CandidateResult struct {
	App           string
	Commit        string
	Image         string
	ContainerName string
	HostPort      int
	ContainerPort int
	HealthURL     string
}

// Sentinel errors returned by the runtime layer.
var (
	ErrInvalidRuntimeConfig = errors.New("invalid runtime configuration")
	ErrInvalidInputs        = errors.New("invalid inputs")
	ErrDockerfileMissing    = errors.New("Dockerfile missing")
	ErrDockerfileSymlink    = errors.New("Dockerfile is a symlink")
	ErrImageBuildFailed     = errors.New("image build failed")
	ErrNoAvailablePort      = errors.New("no available port in range")
	ErrContainerRunning     = errors.New("container already running")
	ErrContainerStartFailed = errors.New("container start failed")
	ErrHealthCheckFailed    = errors.New("health check failed")
	ErrInvalidCandidate     = errors.New("invalid candidate identity")
)

// commandRunner abstracts command execution so tests can substitute a
// fake runner that records invocations.
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// dockerRunner is the production runner: it executes the given program
// via exec.CommandContext. Arguments are passed as a discrete argv; no
// shell is involved.
type dockerRunner struct{}

func (dockerRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// StartCandidate builds the image from source.CheckoutPath, runs the
// candidate on a localhost-only port, health-checks it, and returns a
// fully-derived CandidateResult. The function never logs, prints, or
// returns Docker output beyond a small bounded prefix.
//
// If the manifest opts into the per-app persistent data directory
// (Manifest.Data.Mount is true), the host-side data directory is
// ensured to exist BEFORE docker run so the bind mount target is
// present. The data root comes from cfg (trusted host configuration),
// never from the manifest; the in-container target is the fixed
// constant dataContainerPath.
func StartCandidate(ctx context.Context, cfg RuntimeConfig, data DataConfig, manifest Manifest, source SourceResult, envFile string) (*CandidateResult, error) {
	return startCandidate(ctx, cfg, data, manifest, source, dockerRunner{}, envFile)
}

func startCandidate(ctx context.Context, cfg RuntimeConfig, data DataConfig, manifest Manifest, source SourceResult, runner commandRunner, envFile string) (*CandidateResult, error) {
	if err := validateRuntimeInputs(cfg, manifest, source); err != nil {
		return nil, err
	}

	image := deriveImage(manifest.App, source.Commit)
	containerName := deriveContainerName(manifest.App, source.Commit)

	if err := buildImage(ctx, runner, image, source.CheckoutPath); err != nil {
		return nil, err
	}

	if err := removeIfStopped(ctx, runner, containerName); err != nil {
		return nil, err
	}

	// If the manifest opts into the per-app data mount, ensure the
	// host-side data directory exists before docker run so the bind
	// mount target is present. EnsureAppDataDir is idempotent and
	// never deletes or replaces existing data. The returned
	// AppData is passed to the runtime so docker run can include
	// the matching --mount flag.
	var appData *AppData
	if manifest.Data != nil && manifest.Data.Mount {
		var err error
		appData, err = EnsureAppDataDir(data, manifest.App, manifest.Data)
		if err != nil {
			return nil, fmt.Errorf("ensure app data dir: %w", err)
		}
	}

	return startWithPortRetry(ctx, cfg, runner, containerName, image, manifest.ContainerPort, manifest.HealthPath, manifest.App, source.Commit, appData, envFile)
}

// RemoveCandidate stops and removes the candidate container identified
// by the supplied result. The result's app and commit are validated and
// the image and container names are re-derived; the supplied values
// must match exactly.
func RemoveCandidate(ctx context.Context, cfg RuntimeConfig, candidate CandidateResult) error {
	return removeCandidate(ctx, candidate, dockerRunner{})
}

func removeCandidate(ctx context.Context, candidate CandidateResult, runner commandRunner) error {
	if !appNameRe.MatchString(candidate.App) {
		return fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidCandidate, candidate.App)
	}
	if !shaRe.MatchString(candidate.Commit) {
		return fmt.Errorf("%w: commit %q is not exactly 40 lowercase hex characters", ErrInvalidCandidate, candidate.Commit)
	}

	expectedImage := deriveImage(candidate.App, candidate.Commit)
	expectedContainer := deriveContainerName(candidate.App, candidate.Commit)
	if candidate.Image != expectedImage {
		return fmt.Errorf("%w: image %q does not match derived %q", ErrInvalidCandidate, candidate.Image, expectedImage)
	}
	if candidate.ContainerName != expectedContainer {
		return fmt.Errorf("%w: container name %q does not match derived %q", ErrInvalidCandidate, candidate.ContainerName, expectedContainer)
	}

	out, err := runner.Run(ctx, "docker", "rm", "--force", expectedContainer)
	if err != nil && !strings.Contains(out, "No such container") {
		return fmt.Errorf("docker rm --force %s: %w", expectedContainer, err)
	}
	return nil
}

func validateRuntimeInputs(cfg RuntimeConfig, manifest Manifest, source SourceResult) error {
	if cfg.PortRangeStart < 1024 || cfg.PortRangeStart > 65535 {
		return fmt.Errorf("%w: PortRangeStart %d not in [1024, 65535]", ErrInvalidRuntimeConfig, cfg.PortRangeStart)
	}
	if cfg.PortRangeEnd < 1024 || cfg.PortRangeEnd > 65535 {
		return fmt.Errorf("%w: PortRangeEnd %d not in [1024, 65535]", ErrInvalidRuntimeConfig, cfg.PortRangeEnd)
	}
	if cfg.PortRangeStart > cfg.PortRangeEnd {
		return fmt.Errorf("%w: PortRangeStart %d > PortRangeEnd %d", ErrInvalidRuntimeConfig, cfg.PortRangeStart, cfg.PortRangeEnd)
	}
	if cfg.HealthTimeout <= 0 {
		return fmt.Errorf("%w: HealthTimeout must be positive", ErrInvalidRuntimeConfig)
	}
	if cfg.RepositoryRoot == "" {
		return fmt.Errorf("%w: RepositoryRoot is required", ErrInvalidRuntimeConfig)
	}

	if err := Validate(&manifest); err != nil {
		return fmt.Errorf("%w: manifest: %v", ErrInvalidInputs, err)
	}
	// App and Repository are independent fields: app drives the
	// API path / hostname / state key, repository drives the
	// source mirror. The legacy v1/v2 invariant (app == repo)
	// is gone; the runtime no longer reconciles them.
	_ = manifest.App
	_ = source.Repository
	if !shaRe.MatchString(source.Commit) {
		return fmt.Errorf("%w: source.Commit %q is not a valid SHA", ErrInvalidInputs, source.Commit)
	}

	expectedPath := filepath.Join(cfg.RepositoryRoot, source.Repository+"-checkouts", source.Commit)
	absExpected, err := filepath.Abs(expectedPath)
	if err != nil {
		return fmt.Errorf("%w: resolve expected checkout path: %v", ErrInvalidInputs, err)
	}
	absCheckout, err := filepath.Abs(source.CheckoutPath)
	if err != nil {
		return fmt.Errorf("%w: resolve source.CheckoutPath: %v", ErrInvalidInputs, err)
	}
	if absExpected != absCheckout {
		return fmt.Errorf("%w: source.CheckoutPath %s does not match expected %s", ErrInvalidInputs, absCheckout, absExpected)
	}

	info, err := os.Stat(absCheckout)
	if err != nil {
		return fmt.Errorf("%w: checkout directory %s: %v", ErrInvalidInputs, absCheckout, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: checkout path %s is not a directory", ErrInvalidInputs, absCheckout)
	}

	dockerfile := filepath.Join(absCheckout, "Dockerfile")
	dInfo, err := os.Lstat(dockerfile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrDockerfileMissing, dockerfile)
		}
		return fmt.Errorf("%w: stat Dockerfile: %v", ErrDockerfileMissing, err)
	}
	if dInfo.Mode()&os.ModeSymlink != 0 {
		return ErrDockerfileSymlink
	}
	if !dInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrDockerfileMissing, dockerfile)
	}
	return nil
}

func deriveImage(app, commit string) string {
	return fmt.Sprintf("agentctl/%s:%s", app, commit)
}

func deriveContainerName(app, commit string) string {
	return fmt.Sprintf("agentctl-%s-%s", app, commit[:12])
}

func buildImage(ctx context.Context, runner commandRunner, image, checkoutPath string) error {
	out, err := runner.Run(ctx, "docker", "build", "--pull", "--tag", image, checkoutPath)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrImageBuildFailed, truncateForError(out))
	}
	return nil
}

func removeIfStopped(ctx context.Context, runner commandRunner, containerName string) error {
	out, err := runner.Run(ctx, "docker", "inspect", "--format", "{{.State.Running}}", containerName)
	if err != nil {
		// Docker returns exit code 1 with "No such object" when the
		// container does not exist. Treat that as "nothing to remove".
		if strings.Contains(out, "No such object") || strings.Contains(out, "No such container") {
			return nil
		}
		return fmt.Errorf("docker inspect %s: %w", containerName, err)
	}
	if strings.TrimSpace(out) == "true" {
		return fmt.Errorf("%w: %s", ErrContainerRunning, containerName)
	}
	if _, err := runner.Run(ctx, "docker", "rm", containerName); err != nil {
		return fmt.Errorf("remove stopped container %s: %w", containerName, err)
	}
	return nil
}

// allocatePortFunc is the port allocation function used by the
// runtime. It is overridable so tests can inject a deterministic
// allocator without binding real listeners (which would conflict with
// the test HTTP servers they spin up for health checks).
var allocatePortFunc = defaultAllocatePort

// defaultAllocatePort finds an available localhost port in the
// inclusive range [start, end]. It binds a temporary TCP listener on
// 127.0.0.1:<port> to test availability and closes the listener
// immediately. Returns ErrNoAvailablePort if no port in the range is
// free.
func defaultAllocatePort(start, end int) (int, error) {
	for port := start; port <= end; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("%w: %d..%d", ErrNoAvailablePort, start, end)
}

func startWithPortRetry(ctx context.Context, cfg RuntimeConfig, runner commandRunner, containerName, image string, containerPort int, healthPath, app, commit string, appData *AppData, envFile string) (*CandidateResult, error) {
	port, err := allocatePortFunc(cfg.PortRangeStart, cfg.PortRangeEnd)
	if err != nil {
		return nil, err
	}
	for {
		startErr := startContainer(ctx, runner, containerName, image, port, containerPort, appData, envFile)
		if startErr != nil {
			if isPortInUse(startErr) {
				// A port-binding failure can leave a stopped container with
				// the derived name behind. Remove it (best-effort) before
				// retrying so the next docker run does not hit a name
				// conflict.
				_ = removeContainer(ctx, runner, containerName)
				next, err := nextAvailablePort(cfg.PortRangeStart, cfg.PortRangeEnd, port)
				if err != nil {
					return nil, err
				}
				port = next
				continue
			}
			return nil, startErr
		}

		healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, healthPath)
		if hErr := pollHealth(ctx, healthURL, cfg.HealthTimeout); hErr != nil {
			// Health polling failed. Build, inspect, startup, and polling
			// used the caller context; now switch to a bounded cleanup
			// context derived from context.Background() so cleanup runs
			// even when the caller context is already cancelled.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			logs, _ := containerLogs(cleanupCtx, runner, containerName, 100)
			_ = removeContainer(cleanupCtx, runner, containerName)
			return nil, &healthCheckFailure{
				sentinel: ErrHealthCheckFailed,
				cause:    hErr,
				logs:     truncateForError(logs),
			}
		}

		return &CandidateResult{
			App:           app,
			Commit:        commit,
			Image:         image,
			ContainerName: containerName,
			HostPort:      port,
			ContainerPort: containerPort,
			HealthURL:     healthURL,
		}, nil
	}
}

// nextAvailablePort returns the next port after `after` in the inclusive
// range [start, end]. Returns ErrNoAvailablePort if there is no port
// after `after` within the range.
func nextAvailablePort(start, end, after int) (int, error) {
	if after+1 > end {
		return 0, fmt.Errorf("%w: %d..%d", ErrNoAvailablePort, start, end)
	}
	return after + 1, nil
}

func startContainer(ctx context.Context, runner commandRunner, containerName, image string, hostPort, containerPort int, appData *AppData, envFile string) error {
	args := buildDockerRunArgs(containerName, image, hostPort, containerPort, appData, envFile)
	out, err := runner.Run(ctx, "docker", args...)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrContainerStartFailed, truncateForError(out))
	}
	return nil
}

// buildDockerRunArgs constructs the fixed argv for `docker run`.
// The runtime layer (startContainer) and the rollback layer
// (runContainerFromImage) both use this function so the docker
// invocation stays in lockstep: every container agentctl starts
// uses the same security and resource options.
//
// When appData is non-nil the call adds a bind mount of
// appData.HostPath at the fixed in-container target /data with
// the read-only flag carried on appData.
func buildDockerRunArgs(containerName, image string, hostPort, containerPort int, appData *AppData, envFile string) []string {
	args := []string{
		"run",
		"--detach",
		"--name", containerName,
		"--restart", "unless-stopped",
		"--memory", "256m",
		"--cpus", "0.5",
		"--pids-limit", "128",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort),
	}
	if appData != nil {
		args = append(args, "--mount", buildDataMountArg(appData))
	}
	// envFile is a path produced by MaterializeEnvFile. When
	// non-empty, it is passed to docker as --env-file instead of
	// any --env flags; the caller (deploy / rollback) is
	// responsible for deleting the file after docker run returns.
	// An empty envFile means the deployment has no env entries
	// and the container is started without env injection.
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, image)
	return args
}

// buildDataMountArg constructs the --mount flag value for a
// per-app data bind mount. The mount uses the canonical Docker
// --mount syntax with type=bind; the host source is the validated
// AppData.HostPath and the in-container target is the fixed
// constant dataContainerPath. Read-only is taken from appData.
func buildDataMountArg(appData *AppData) string {
	return fmt.Sprintf("type=bind,source=%s,target=%s,readonly=%t", appData.HostPath, appData.ContainerPath, appData.ReadOnly)
}

func removeContainer(ctx context.Context, runner commandRunner, containerName string) error {
	out, err := runner.Run(ctx, "docker", "rm", "--force", containerName)
	if err != nil && !strings.Contains(out, "No such container") {
		return fmt.Errorf("docker rm --force %s: %w", containerName, err)
	}
	return nil
}

func containerLogs(ctx context.Context, runner commandRunner, containerName string, tail int) (string, error) {
	return runner.Run(ctx, "docker", "logs", "--tail", fmt.Sprintf("%d", tail), containerName)
}

func isPortInUse(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "already in use") || strings.Contains(s, "bind: address")
}

func pollHealth(ctx context.Context, url string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	deadline := time.Now().Add(timeout)
	delay := 100 * time.Millisecond
	const maxBytes = 4096

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out after %v", timeout)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("create health request: %w", err)
		}
		resp, doErr := client.Do(req)
		healthy := false
		if doErr == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				healthy = true
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBytes))
			_ = resp.Body.Close()
		}
		if healthy {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// healthCheckFailure wraps both ErrHealthCheckFailed (as the
// operation sentinel) and the underlying poll error (commonly
// context.Canceled or context.DeadlineExceeded) so that
// errors.Is(err, ErrHealthCheckFailed) and errors.Is(err, X) for any
// X reachable from the poll error both succeed.
//
// This is implemented as a struct (not fmt.Errorf with multiple %w)
// because the module targets Go 1.19, which supports only a single
// unwrap target per error.
type healthCheckFailure struct {
	sentinel error
	cause    error
	logs     string
}

func (e *healthCheckFailure) Error() string {
	return fmt.Sprintf("%s: %s (logs: %s)", e.sentinel, e.cause, e.logs)
}

// Unwrap exposes the underlying poll error so errors.Is finds
// context.Canceled (or any other cancellation cause) in the chain.
func (e *healthCheckFailure) Unwrap() error {
	return e.cause
}

// Is returns true when comparing against the sentinel error.
func (e *healthCheckFailure) Is(target error) bool {
	return target == e.sentinel
}

func truncateForError(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}
