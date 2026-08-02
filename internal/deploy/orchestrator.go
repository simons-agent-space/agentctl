package deploy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// Sentinel errors returned by the deployment orchestrator.
var (
	ErrDeploymentFailed = errors.New("deployment failed")
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
}

// Deploy runs the full deployment pipeline:
//
//  1. prepare trusted source checkout
//  2. build and start candidate container (includes health check)
//  3. promote the candidate route through Caddy
//  4. persist deployment state (moves Current → Previous)
//  5. remove the formerly-current container
//
// On any failure the current live deployment is never touched: the
// pipeline only cleans up resources created by THIS deployment
// (the candidate container, the new Caddy route). Caddy promotion
// failure is contained by the Caddy layer's own atomic restore;
// state-save failure triggers best-effort Caddy revert before
// cleanup. Cleanup uses a bounded recovery context so a cancelled
// caller cannot strand the host with inconsistent state.
//
// The manifest is expected to be validated by the caller (the
// caller passes a validated deploy config plus validated
// deployment manifest and commit). The commit is re-validated by
// the source layer.
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

func deploy(ctx context.Context, cfg DeployConfig, manifest Manifest, commit string, deps deployDeps) (*DeployResult, error) {
	// 1. Prepare trusted source checkout. The source layer
	//    validates the commit (40 hex chars, reachable from
	//    origin/main) and produces a detached checkout directory.
	source, err := CheckoutSource(ctx, cfg.Source, cfg.Source.AllowedOrg, manifest.App, commit)
	if err != nil {
		return nil, fmt.Errorf("%w: source checkout: %v", ErrDeploymentFailed, err)
	}

	// 2. Build and start the candidate container. This step
	//    builds the Docker image from the checkout, removes any
	//    stopped container with the derived name, allocates a
	//    localhost port, runs the container, and health-checks
	//    it. On health-check failure the candidate container is
	//    removed (best-effort) before the error is returned.
	candidate, err := startCandidate(ctx, cfg.Runtime, manifest, *source, deps.docker)
	if err != nil {
		return nil, fmt.Errorf("%w: start candidate: %v", ErrDeploymentFailed, err)
	}

	// Track the container name so cleanup paths can target it
	// specifically without touching the current live deployment.
	createdContainer := candidate.ContainerName

	// 3. Promote the candidate route through Caddy. The Caddy
	//    layer handles its own atomic install + rollback on
	//    reload failure; on a returned error the previous live
	//    deployment is still serving (the .caddy file on disk
	//    is back to its pre-deploy content via the Caddy
	//    layer's own restore). We still need to clean up the
	//    candidate container we created.
	if _, err := promote(ctx, cfg.Caddy, *candidate, deps.caddy); err != nil {
		cleanupCtx, cancel := recoveryContext()
		_ = removeContainerForce(cleanupCtx, deps.docker, createdContainer)
		cancel()
		return nil, fmt.Errorf("%w: promote: %v", ErrDeploymentFailed, err)
	}

	// 4. Persist deployment state. SaveDeployment moves
	//    existing Current → Previous and installs the new
	//    Deployment as Current. On failure here, Caddy is
	//    already serving the new route; we best-effort revert
	//    Caddy to the old route and clean up the candidate
	//    container.
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
	if err := SaveDeployment(cfg.State, dep); err != nil {
		// Best-effort Caddy revert. Load the previous Current
		// from state (it was not overwritten by the failed
		// save) and promote it back. If revert fails we still
		// clean up the candidate container and return the
		// combined error.
		cleanupCtx, cancel := recoveryContext()
		defer cancel()
		if existing, loadErr := LoadDeploymentState(cfg.State, dep.App); loadErr == nil && existing.Current != nil {
			oldCandidate := deploymentToCandidate(*existing.Current, manifest.HealthPath)
			_, _ = promote(cleanupCtx, cfg.Caddy, oldCandidate, deps.caddy)
		}
		_ = removeContainerForce(cleanupCtx, deps.docker, createdContainer)
		return nil, fmt.Errorf("%w: save state: %v", ErrDeploymentFailed, err)
	}

	// 5. Remove the formerly-current container. State is
	//    already swapped (Current=new, Previous/old). Caddy is
	//    serving the new route. Removing the old container is
	//    best-effort: if it fails the deployment still
	//    succeeded from the user's perspective (state is
	//    correct, the route is live); the old container is
	//    just running but not routed and can be cleaned up
	//    later.
	state, err := LoadDeploymentState(cfg.State, dep.App)
	if err == nil && state.Previous != nil {
		_ = removeContainerCandidate(ctx, *state.Previous, manifest.HealthPath, deps.docker)
	}

	return &DeployResult{
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
	}, nil
}
