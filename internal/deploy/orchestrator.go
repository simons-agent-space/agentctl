package deploy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Sentinel errors returned by the deployment orchestrator.
var (
	ErrDeploymentFailed   = errors.New("deployment failed")
	ErrInvalidDeployInput = errors.New("invalid deploy input")
)

// DeployConfig holds the trusted host-side configuration for a
// deployment. All fields are trusted host configuration, not caller
// input.
type DeployConfig struct {
	Source  SourceConfig
	Runtime RuntimeConfig
	Caddy   CaddyConfig
	State   StateConfig
}

// DeployResult is the outcome of a successful deployment.
//
// Warnings collects non-fatal issues that occurred after the
// deployment was already committed and live (Caddy promotion and
// state persistence both succeeded). The deployment is considered
// successful: the caller should NOT retry. Each warning is a
// self-contained cleanup task the caller may want to schedule
// (e.g. remove the formerly-current container that is now
// unrouted but still running).
type DeployResult struct {
	App           string
	Commit        string
	Image         string
	ContainerName string
	HostPort      int
	ContainerPort int
	Hostname      string
	Upstream      string
	DeployedAt    time.Time
	StateFile     string
	Warnings      []error
}

// Deploy runs the full deployment pipeline. It is safe for
// autonomous use: every input is validated before any side effect,
// the existing state is snapshotted before checkout/build, and
// every failure path restores disk and Caddy to a consistent state
// before returning. The manifest is validated internally — Deploy
// does not rely on the caller having validated it.
//
// Flow:
//
//  1. validate the complete DeployConfig and manifest
//  2. snapshot the existing state (NotFound means first deploy;
//     corrupt/unsupported/invalid fails here, before any side effect)
//  3. prepare trusted source checkout
//  4. build and start the candidate container (includes health check)
//  5. promote the candidate route through Caddy
//  6. persist deployment state (moves Current → Previous)
//  7. remove the formerly-current container (NON-FATAL: a
//     cleanup failure here becomes a warning on the result)
//
// Failure semantics:
//
//   - The current live deployment is never touched before promotion
//     and state persistence both succeed.
//   - Cleanup targets only resources created by THIS deployment
//     (candidate container, new Caddy route).
//   - State-save failure triggers best-effort Caddy recovery:
//     re-promote the snapshotted old Current for replacement
//     deployments, or RemovePromotion for first deployments.
//   - Caddy recovery failure keeps the candidate container
//     running (Caddy may still route to it) and the returned
//     error preserves ErrDeploymentFailed, the state-save
//     failure, and the Caddy recovery failure as separate
//     sentinels detectable with errors.Is.
//   - Candidate-cleanup failures on failed deployment paths are
//     NOT silently swallowed: the returned error preserves
//     ErrDeploymentFailed and the primary and cleanup failures
//     as separate sentinels detectable with errors.Is.
//   - Old-container removal after successful state persistence
//     is NON-FATAL: the deployment is already committed and
//     live, so the failure is reported as a warning on the
//     result (not an error).
//   - Every post-failure cleanup runs under
//     context.Background() plus a 10s timeout so a cancelled
//     caller cannot strand the host with inconsistent state.
func Deploy(ctx context.Context, cfg DeployConfig, manifest Manifest, commit string) (*DeployResult, error) {
	return deploy(ctx, cfg, manifest, commit, deployDeps{
		docker: dockerRunner{},
		caddy:  caddyRunner{},
	})
}

// deployDeps holds the injectable command runners used by deploy.
// Tests substitute fakes.
type deployDeps struct {
	docker commandRunner
	caddy  commandRunner
}

// deployError preserves ErrDeploymentFailed (always) and one or
// more underlying errors (the primary and any cleanup or recovery
// failures). errors.Is works for ErrDeploymentFailed AND for every
// preserved underlying error, so callers can branch on any of
// them. Go 1.19 does not support multiple %w, so we implement the
// multi-sentinel semantics explicitly via the Is method and
// Unwrap (returns the first secondary so the standard library
// can walk the chain).
type deployError struct {
	primary     error
	secondaries []error
	message     string
}

func (e *deployError) Error() string { return e.message }

// Unwrap returns the first secondary so the standard library's
// errors.Is and errors.Unwrap walk the chain in the natural way.
// The Is method below gives callers access to every secondary.
func (e *deployError) Unwrap() error {
	if len(e.secondaries) > 0 {
		return e.secondaries[0]
	}
	return nil
}

func (e *deployError) Is(target error) bool {
	if target == e.primary {
		return true
	}
	for _, secondary := range e.secondaries {
		if errors.Is(secondary, target) {
			return true
		}
	}
	return false
}

// validateDeployConfig validates the complete DeployConfig and the
// supplied manifest before any side effect. Returns an error that
// preserves both ErrDeploymentFailed and the underlying validation
// sentinel so callers can branch on either.
//
// Sources of validation failure:
//
//   - Source.RepositoryRoot / OriginURL / AllowedOrg must be set.
//   - Runtime.PortRangeStart/End must be in [1024, 65535] and
//     Start must not exceed End. HealthTimeout must be positive.
//     RepositoryRoot must be set.
//   - Caddy config (delegated to validateCaddyConfig).
//   - State config (delegated to validateStateConfig).
//   - Caddy.BaseDomain must equal State.BaseDomain.
//   - Manifest: version 1, app matches appNameRe, container port
//     in [1024, 65535], health path starts with / and contains
//     neither ? nor #.
func validateDeployConfig(cfg DeployConfig, manifest Manifest) error {
	if cfg.Source.RepositoryRoot == "" {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     "Source.RepositoryRoot is required",
		}
	}
	if cfg.Source.OriginURL == "" {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     "Source.OriginURL is required",
		}
	}
	if cfg.Source.AllowedOrg == "" {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     "Source.AllowedOrg is required",
		}
	}
	if cfg.Runtime.PortRangeStart < 1024 || cfg.Runtime.PortRangeStart > 65535 {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidRuntimeConfig},
			message:     fmt.Sprintf("Runtime.PortRangeStart %d not in [1024, 65535]", cfg.Runtime.PortRangeStart),
		}
	}
	if cfg.Runtime.PortRangeEnd < 1024 || cfg.Runtime.PortRangeEnd > 65535 {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidRuntimeConfig},
			message:     fmt.Sprintf("Runtime.PortRangeEnd %d not in [1024, 65535]", cfg.Runtime.PortRangeEnd),
		}
	}
	if cfg.Runtime.PortRangeStart > cfg.Runtime.PortRangeEnd {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidRuntimeConfig},
			message:     fmt.Sprintf("Runtime.PortRangeStart %d > PortRangeEnd %d", cfg.Runtime.PortRangeStart, cfg.Runtime.PortRangeEnd),
		}
	}
	if cfg.Runtime.HealthTimeout <= 0 {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidRuntimeConfig},
			message:     "Runtime.HealthTimeout must be positive",
		}
	}
	if cfg.Runtime.RepositoryRoot == "" {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidRuntimeConfig},
			message:     "Runtime.RepositoryRoot is required",
		}
	}
	if err := validateCaddyConfig(cfg.Caddy); err != nil {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{err},
			message:     fmt.Sprintf("Caddy config: %v", err),
		}
	}
	if err := validateStateConfig(cfg.State); err != nil {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{err},
			message:     fmt.Sprintf("State config: %v", err),
		}
	}
	if cfg.Caddy.BaseDomain != cfg.State.BaseDomain {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     fmt.Sprintf("Caddy.BaseDomain %q != State.BaseDomain %q", cfg.Caddy.BaseDomain, cfg.State.BaseDomain),
		}
	}
	if manifest.App == "" {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     "manifest app is required",
		}
	}
	if !appNameRe.MatchString(manifest.App) {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     fmt.Sprintf("manifest app %q does not match app-name format", manifest.App),
		}
	}
	if manifest.Version != 1 {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     fmt.Sprintf("manifest version %d is not supported (only version 1)", manifest.Version),
		}
	}
	if manifest.ContainerPort < 1024 || manifest.ContainerPort > 65535 {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     fmt.Sprintf("manifest container_port %d out of range [1024, 65535]", manifest.ContainerPort),
		}
	}
	if !strings.HasPrefix(manifest.HealthPath, "/") {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     fmt.Sprintf("manifest health_path %q must start with /", manifest.HealthPath),
		}
	}
	if strings.ContainsAny(manifest.HealthPath, "?#") {
		return &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{ErrInvalidDeployInput},
			message:     fmt.Sprintf("manifest health_path %q must not contain ? or #", manifest.HealthPath),
		}
	}
	return nil
}

func deploy(ctx context.Context, cfg DeployConfig, manifest Manifest, commit string, deps deployDeps) (*DeployResult, error) {
	// 1. Validate config and manifest. Fails before any side effect.
	if err := validateDeployConfig(cfg, manifest); err != nil {
		return nil, err
	}

	// 2. Snapshot the existing state (if any). NotFound means this
	//    is a first deployment. Corrupt / unsupported / invalid
	//    state fails here, before checkout/build.
	existing, err := LoadDeploymentState(cfg.State, manifest.App)
	if err != nil && !errors.Is(err, ErrDeploymentStateNotFound) {
		return nil, &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{err},
			message:     fmt.Sprintf("load existing state: %v", err),
		}
	}
	var snapshotCurrent *Deployment
	if err == nil {
		snapshotCurrent = existing.Current
	}

	// 3. Prepare trusted source checkout. The source layer
	//    validates the commit (40 hex chars, reachable from
	//    origin/main) and produces a detached checkout directory.
	source, err := CheckoutSource(ctx, cfg.Source, cfg.Source.AllowedOrg, manifest.App, commit)
	if err != nil {
		return nil, &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{err},
			message:     fmt.Sprintf("source checkout: %v", err),
		}
	}

	// 4. Build and start the candidate container (includes health
	//    check). On health-check failure the candidate container
	//    is removed (best-effort) before the error is returned.
	candidate, err := startCandidate(ctx, cfg.Runtime, manifest, *source, deps.docker)
	if err != nil {
		return nil, &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{err},
			message:     fmt.Sprintf("start candidate: %v", err),
		}
	}
	createdContainer := candidate.ContainerName

	// 5. Promote the candidate route through Caddy. The Caddy
	//    layer handles its own atomic install + rollback on
	//    reload failure; on a returned error the previous live
	//    deployment is still serving — but only if the Caddy
	//    layer's rollback reload itself succeeded. promote() can
	//    fail in two structurally different states:
	//
	//      (a) initial Caddy reload failed, but rollback reload
	//          succeeded. Caddy's running configuration no longer
	//          routes to the candidate. Safe to remove candidate.
	//      (b) initial Caddy reload failed, AND rollback reload
	//          also failed. Caddy's running configuration is
	//          uncertain; the new route may still be live and
	//          traffic may still be flowing to the candidate.
	//          Removing the candidate in this state would turn a
	//          recoverable partial failure into an outage, so we
	//          deliberately keep it running.
	//
	//    Distinguish (a) from (b) via the caddyCommandError
	//    returned by promote: its rollback field is nil when the
	//    rollback reload succeeded and non-nil when it also
	//    failed. Non-caddy errors (e.g. ErrInvalidCaddyConfig,
	//    ErrInvalidCandidate, ErrAtomicWriteFailed, validate
	//    failures) all leave Caddy in its pre-promotion state and
	//    take the safe-to-remove path. errors.As returns false
	//    for them, so rollbackFailed stays false.
	if _, err := promote(ctx, cfg.Caddy, *candidate, deps.caddy); err != nil {
		var caddyErr *caddyCommandError
		rollbackFailed := errors.As(err, &caddyErr) && caddyErr.rollback != nil

		var removeErr error
		if rollbackFailed {
			// Caddy recovery failed. Keep the candidate container
			// running so it remains routable if Caddy still has
			// the new route loaded. The caller must investigate
			// and clean up manually.
		} else {
			// Caddy recovered (no reload was attempted, or the
			// rollback reload succeeded). It is safe to remove
			// the candidate container; the previous deployment
			// is still serving. The cleanup uses a fresh bounded
			// recovery context so a cancelled caller cannot
			// strand the host with a running candidate.
			cleanupCtx, cancel := recoveryContext()
			removeErr = removeContainerForce(cleanupCtx, deps.docker, createdContainer)
			cancel()
		}

		// Build the returned error. primary is always
		// ErrDeploymentFailed; secondaries carry the underlying
		// promote error and (only when attempted) the cleanup
		// error. Nil entries are dropped so callers never see
		// "cleanup: <nil>" in the message or a spurious nil
		// sentinel in secondaries.
		secondaries := make([]error, 0, 2)
		parts := make([]string, 0, 2)
		secondaries = append(secondaries, err)
		parts = append(parts, fmt.Sprintf("promote: %v", err))
		if removeErr != nil {
			secondaries = append(secondaries, removeErr)
			parts = append(parts, fmt.Sprintf("cleanup: %v", removeErr))
		}
		return nil, &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: secondaries,
			message:     strings.Join(parts, "; "),
		}
	}

	// 6. Persist deployment state. SaveDeployment moves existing
	//    Current → Previous and installs the new Deployment as
	//    Current. On failure, Caddy is already serving the new
	//    route; we must recover Caddy BEFORE touching the
	//    candidate container, and we must do so under a bounded
	//    recovery context so a cancelled caller cannot prevent
	//    the recovery.
	dep := Deployment{
		App:           candidate.App,
		Commit:        candidate.Commit,
		Image:         candidate.Image,
		ContainerName: candidate.ContainerName,
		HostPort:      candidate.HostPort,
		ContainerPort: candidate.ContainerPort,
		Hostname:      candidate.App + "." + cfg.Caddy.BaseDomain,
		Upstream:      fmt.Sprintf("127.0.0.1:%d", candidate.HostPort),
		DeployedAt:    time.Now().UTC(),
	}
	saveErr := SaveDeployment(cfg.State, dep)
	if saveErr != nil {
		cleanupCtx, cancel := recoveryContext()
		defer cancel()
		var recoverErr error
		if snapshotCurrent != nil {
			// Replacement deployment: re-promote the snapshotted
			// old Current so Caddy returns to the old route.
			oldCandidate := deploymentToCandidate(*snapshotCurrent, manifest.HealthPath)
			_, recoverErr = promote(cleanupCtx, cfg.Caddy, oldCandidate, deps.caddy)
		} else {
			// First deployment: there is no old route to
			// re-promote. Remove the new route so Caddy returns
			// to the pre-deployment "absent" state.
			recoverErr = RemovePromotionWithRunner(cleanupCtx, cfg.Caddy, manifest.App, deps.caddy)
		}
		if recoverErr != nil {
			// Caddy recovery failed. Keep the candidate container
			// running because Caddy may still be serving the new
			// route. Return an error preserving ErrDeploymentFailed
			// that includes both failures.
			return nil, &deployError{
				primary:     ErrDeploymentFailed,
				secondaries: []error{saveErr, recoverErr},
				message:     fmt.Sprintf("save state: %v; caddy recovery also failed: %v", saveErr, recoverErr),
			}
		}
		// Caddy recovery succeeded. Now (and only now) it is safe
		// to remove the candidate container. Do NOT silently
		// ignore a cleanup failure.
		removeErr := removeContainerForce(cleanupCtx, deps.docker, createdContainer)
		if removeErr != nil {
			return nil, &deployError{
				primary:     ErrDeploymentFailed,
				secondaries: []error{saveErr, removeErr},
				message:     fmt.Sprintf("save state: %v; candidate cleanup failed: %v", saveErr, removeErr),
			}
		}
		return nil, &deployError{
			primary:     ErrDeploymentFailed,
			secondaries: []error{saveErr},
			message:     fmt.Sprintf("save state: %v", saveErr),
		}
	}

	// 7. Remove the formerly-current container. State is already
	//    swapped (Current=new, Previous/old). Caddy is serving
	//    the new route. A cleanup failure here is NON-FATAL: the
	//    deployment is already committed and live, so the
	//    failure is reported as a warning on the result (not an
	//    error). This prevents an autonomous caller from
	//    retrying a deployment that actually succeeded.
	result := &DeployResult{
		App:           dep.App,
		Commit:        dep.Commit,
		Image:         dep.Image,
		ContainerName: dep.ContainerName,
		HostPort:      dep.HostPort,
		ContainerPort: dep.ContainerPort,
		Hostname:      dep.Hostname,
		Upstream:      dep.Upstream,
		DeployedAt:    dep.DeployedAt,
		StateFile:     filepath.Join(cfg.State.StateDir, dep.App+".state.json"),
	}
	if snapshotCurrent != nil {
		if removeErr := removeContainerCandidate(ctx, *snapshotCurrent, manifest.HealthPath, deps.docker); removeErr != nil {
			result.Warnings = append(result.Warnings, fmt.Errorf("post-deploy cleanup of old container: %w", removeErr))
		}
	}

	return result, nil
}
