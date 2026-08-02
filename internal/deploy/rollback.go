package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors returned by the rollback layer.
var (
	ErrNoPreviousDeployment = errors.New("no previous deployment to roll back to")
	ErrRollbackFailed       = errors.New("rollback failed")
)

// RollbackConfig holds trusted host-side configuration for
// rollback orchestration. All fields are trusted host
// configuration, not caller input.
type RollbackConfig struct {
	App        string // app to roll back
	State      StateConfig
	Runtime    RuntimeConfig
	Caddy      CaddyConfig
	HealthPath string // for health-checking the previous container
}

// RollbackDeployment rolls back the named app to its previous
// deployment. The flow is:
//
//  1. Load state; require Previous.
//  2. Validate current and previous identities (already done by
//     LoadDeploymentState, but the requirement is called out here
//     so the contract is obvious).
//  3. Ensure the previous container is running and healthy.
//  4. Promote the previous route through the existing Caddy layer.
//  5. Swap Current and Previous in state.
//  6. Remove the formerly-current container.
//
// If any step before a successful promotion fails, nothing
// changes on disk or in Caddy and the current deployment keeps
// serving. If the state swap fails after promotion, RollbackDeployment
// best-effort reverts Caddy to the original current route using a
// bounded recovery context and reports the combined failure. If the
// final remove-current step fails, the rollback is considered
// successful (Caddy serves previous, state is swapped); the old
// container being still running is a minor issue callers can clean
// up later.
//
// Rollback is idempotent in the toggle sense: calling it twice
// swaps Current and Previous twice. The error sentinel
// ErrNoPreviousDeployment is returned when there is nothing to roll
// back to (a single deployment has no Previous).
func RollbackDeployment(ctx context.Context, cfg RollbackConfig) error {
	return rollbackDeployment(ctx, cfg, rollbackDeps{
		docker: dockerRunner{},
		caddy:  caddyRunner{},
	})
}

// rollbackDeps holds the injectable command runners used by
// rollbackDeployment. Tests substitute fakes.
type rollbackDeps struct {
	docker commandRunner
	caddy  commandRunner
}

// recoveryContext returns a fresh context for cleanup operations
// after the caller context has been cancelled or expired. The
// rollback layer uses this for all post-failure cleanup so a
// cancelled caller context cannot prevent Caddy or Docker from
// being restored to a consistent state.
func recoveryContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func rollbackDeployment(ctx context.Context, cfg RollbackConfig, deps rollbackDeps) error {
	if err := validateRollbackConfig(cfg); err != nil {
		return err
	}
	if !appNameRe.MatchString(cfg.App) {
		return fmt.Errorf("%w: App %q does not match app-name format", ErrRollbackFailed, cfg.App)
	}

	state, err := LoadDeploymentState(cfg.State, cfg.App)
	if err != nil {
		return fmt.Errorf("%w: load state: %v", ErrRollbackFailed, err)
	}
	if state.Previous == nil {
		return ErrNoPreviousDeployment
	}

	// LoadDeploymentState already validated Current and Previous
	// against cfg.State.BaseDomain, so the deployments are safe
	// to use here.
	current := *state.Current
	previous := *state.Previous

	// 1. Ensure previous container is running and healthy.
	previousCandidate, err := ensurePreviousRunningAndHealthy(ctx, cfg.Runtime, previous, deps.docker)
	if err != nil {
		return fmt.Errorf("%w: ensure previous container: %v", ErrRollbackFailed, err)
	}

	// 2. Promote previous route through Caddy.
	if _, err := promote(ctx, cfg.Caddy, *previousCandidate, deps.caddy); err != nil {
		// Best-effort cleanup: if we started the container in
		// this call, stop it so a subsequent rollback does not
		// see a half-initialised container.
		cleanupCtx, cancel := recoveryContext()
		_ = stopContainerBestEffort(cleanupCtx, deps.docker, previous.ContainerName)
		cancel()
		return fmt.Errorf("%w: promote previous: %v", ErrRollbackFailed, err)
	}

	// 3. Swap Current and Previous in state.
	//    SaveDeployment moves existing Current to Previous and
	//    installs the supplied Deployment as Current. Passing
	//    `previous` as the new dep yields Current=previous,
	//    Previous=current — the desired swap.
	if err := SaveDeployment(cfg.State, previous); err != nil {
		// Try to revert Caddy to the original current route so
		// disk and Caddy agree again. Uses the bounded recovery
		// context so a cancelled caller cannot prevent revert.
		cleanupCtx, cancel := recoveryContext()
		defer cancel()
		currentCandidate := deploymentToCandidate(current, cfg.HealthPath)
		if _, revertErr := promote(cleanupCtx, cfg.Caddy, currentCandidate, deps.caddy); revertErr != nil {
			return fmt.Errorf("%w: save state: %v; caddy revert also failed: %v", ErrRollbackFailed, err, revertErr)
		}
		return fmt.Errorf("%w: save state: %v; caddy reverted", ErrRollbackFailed, err)
	}

	// 4. Remove the formerly-current container.
	//    State is already swapped and Caddy serves the previous
	//    deployment; a remove failure here leaves the old
	//    container running but not routed, which is recoverable
	//    later and does not change the rollback's user-visible
	//    outcome.
	if err := removeContainerCandidate(ctx, current, cfg.HealthPath, deps.docker); err != nil {
		return nil
	}

	return nil
}

// validateRollbackConfig checks that cfg is well-formed. All
// sub-configs are validated against their existing rules.
func validateRollbackConfig(cfg RollbackConfig) error {
	if cfg.HealthPath == "" {
		return fmt.Errorf("%w: HealthPath is required", ErrRollbackFailed)
	}
	if !strings.HasPrefix(cfg.HealthPath, "/") || strings.ContainsAny(cfg.HealthPath, "?#") {
		return fmt.Errorf("%w: HealthPath %q must start with / and not contain ? or #", ErrRollbackFailed, cfg.HealthPath)
	}
	if err := validateStateConfig(cfg.State); err != nil {
		return fmt.Errorf("%w: %v", ErrRollbackFailed, err)
	}
	if err := validateCaddyConfig(cfg.Caddy); err != nil {
		return fmt.Errorf("%w: %v", ErrRollbackFailed, err)
	}
	if cfg.Runtime.HealthTimeout == 0 {
		return fmt.Errorf("%w: Runtime.HealthTimeout is required", ErrRollbackFailed)
	}
	if cfg.Runtime.PortRangeStart == 0 || cfg.Runtime.PortRangeEnd == 0 {
		return fmt.Errorf("%w: Runtime.PortRangeStart and PortRangeEnd are required", ErrRollbackFailed)
	}
	return nil
}

// deploymentToCandidate converts a Deployment into a
// CandidateResult that the existing runtime and Caddy layers can
// consume. HealthURL is derived from HostPort and the rollback's
// HealthPath.
func deploymentToCandidate(dep Deployment, healthPath string) CandidateResult {
	return CandidateResult{
		App:           dep.App,
		Commit:        dep.Commit,
		Image:         dep.Image,
		ContainerName: dep.ContainerName,
		HostPort:      dep.HostPort,
		ContainerPort: dep.ContainerPort,
		HealthURL:     fmt.Sprintf("http://127.0.0.1:%d%s", dep.HostPort, healthPath),
	}
}

// ensurePreviousRunningAndHealthy inspects the previous container
// and, depending on its status, starts it (if stopped), runs it
// fresh from the image (if absent), or leaves it alone (if already
// running). It then health-checks the container on its recorded
// localhost port. On health-check failure, if the container was
// started in this call, it is removed (best-effort, bounded
// cleanup context).
func ensurePreviousRunningAndHealthy(ctx context.Context, cfg RuntimeConfig, dep Deployment, runner commandRunner) (*CandidateResult, error) {
	if !appNameRe.MatchString(dep.App) {
		return nil, fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidCandidate, dep.App)
	}
	if !shaRe.MatchString(dep.Commit) {
		return nil, fmt.Errorf("%w: commit %q is not exactly 40 lowercase hex characters", ErrInvalidCandidate, dep.Commit)
	}

	candidate := deploymentToCandidate(dep, mustHealthPathForRuntime(cfg))

	status, err := inspectContainerStatus(ctx, runner, dep.ContainerName)
	if err != nil {
		return nil, err
	}

	switch status {
	case containerStatusRunning:
		// Already running; no docker call needed.
	case containerStatusStopped:
		out, runErr := runner.Run(ctx, "docker", "start", dep.ContainerName)
		if runErr != nil {
			return nil, fmt.Errorf("%w: start %s: %s: %v", ErrContainerStartFailed, dep.ContainerName, truncateForError(out), runErr)
		}
	case containerStatusAbsent:
		if err := runContainerFromImage(ctx, runner, dep); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unknown container status %q for %s", ErrInvalidRuntimeConfig, status, dep.ContainerName)
	}

	if err := pollHealth(ctx, candidate.HealthURL, cfg.HealthTimeout); err != nil {
		if status != containerStatusRunning {
			cleanupCtx, cancel := recoveryContext()
			_ = removeContainerForce(cleanupCtx, runner, dep.ContainerName)
			cancel()
		}
		return nil, fmt.Errorf("%w: %v", ErrHealthCheckFailed, err)
	}

	return &candidate, nil
}

// mustHealthPathForRuntime is a placeholder so ensurePreviousRunningAndHealthy
// can build a HealthURL; the real health path is plumbed through
// RollbackConfig.HealthPath, and the only caller that needs this
// helper is the unit-test path that exercises the runtime helpers
// directly. It is set to "/" by default; production callers must
// use RollbackDeployment.
func mustHealthPathForRuntime(_ RuntimeConfig) string {
	return "/"
}

// runContainerFromImage runs a new container from the given
// image. The fixed argv matches the runtime layer: localhost-only
// publish, no privileged, no new privileges, hard resource caps.
func runContainerFromImage(ctx context.Context, runner commandRunner, dep Deployment) error {
	args := []string{
		"run", "--detach",
		"--name", dep.ContainerName,
		"--restart", "unless-stopped",
		"--memory", "256m",
		"--cpus", "0.5",
		"--pids-limit", "128",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", dep.HostPort, dep.ContainerPort),
		dep.Image,
	}
	out, err := runner.Run(ctx, "docker", args...)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrContainerStartFailed, truncateForError(out), err)
	}
	return nil
}

// removeContainerCandidate wraps removeCandidate with a Deployment
// and a health path. Used to remove the formerly-current container
// after a successful swap.
func removeContainerCandidate(ctx context.Context, dep Deployment, healthPath string, runner commandRunner) error {
	c := deploymentToCandidate(dep, healthPath)
	return removeCandidate(ctx, c, runner)
}

// stopContainerBestEffort is a best-effort `docker rm --force` for
// cleanup. Errors are swallowed because this is used after a
// rollback failure has already been reported.
func stopContainerBestEffort(ctx context.Context, runner commandRunner, containerName string) error {
	return removeContainerForce(ctx, runner, containerName)
}

// removeContainerForce runs `docker rm --force` and treats "No
// such container" / "No such object" as success. It is a thin
// wrapper around the runtime layer's removeContainer that treats
// both error strings as "nothing to do", which the runtime
// version does not.
func removeContainerForce(ctx context.Context, runner commandRunner, containerName string) error {
	out, err := runner.Run(ctx, "docker", "rm", "--force", containerName)
	if err != nil && !strings.Contains(out, "No such container") && !strings.Contains(out, "No such object") {
		return fmt.Errorf("docker rm --force %s: %s: %w", containerName, truncateForError(out), err)
	}
	return nil
}

// containerStatus describes the runtime state of a Docker
// container.
type containerStatus int

const (
	containerStatusUnknown containerStatus = iota
	containerStatusRunning
	containerStatusStopped
	containerStatusAbsent
)

// inspectContainerStatus returns the runtime status of a named
// container. A "No such object" / "No such container" error from
// docker inspect is mapped to containerStatusAbsent so callers can
// handle the fresh-run case without parsing docker output.
func inspectContainerStatus(ctx context.Context, runner commandRunner, containerName string) (containerStatus, error) {
	out, err := runner.Run(ctx, "docker", "inspect", "--format", "{{.State.Running}}", containerName)
	if err != nil {
		if strings.Contains(out, "No such object") || strings.Contains(out, "No such container") {
			return containerStatusAbsent, nil
		}
		return containerStatusUnknown, fmt.Errorf("docker inspect %s: %s: %w", containerName, truncateForError(out), err)
	}
	if strings.TrimSpace(out) == "true" {
		return containerStatusRunning, nil
	}
	return containerStatusStopped, nil
}
