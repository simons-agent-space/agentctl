package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupTestGitRepo creates a local bare origin and a working
// repository with two commits. Each commit contains a Dockerfile.
// Returns the origin URL (file://) and the two commit SHAs. The
// origin is usable by SourceConfig.OriginURL.
func setupTestGitRepo(t *testing.T) (originURL, commitA, commitB string) {
	t.Helper()

	originDir := t.TempDir()
	orchRunGit(t, "", "init", "--bare", "--initial-branch=main", originDir)

	workDir := t.TempDir()
	orchRunGit(t, workDir, "init", "--initial-branch=main")
	orchRunGit(t, workDir, "config", "user.email", "test@example.com")
	orchRunGit(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	orchRunGit(t, workDir, "add", "Dockerfile")
	orchRunGit(t, workDir, "commit", "-m", "first commit")

	outA, err := orchRunGitOut(t, workDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	commitA = strings.TrimSpace(string(outA))

	if err := os.WriteFile(filepath.Join(workDir, "Dockerfile"), []byte("FROM scratch\nRUN echo v2\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile v2: %v", err)
	}
	orchRunGit(t, workDir, "add", "Dockerfile")
	orchRunGit(t, workDir, "commit", "-m", "second commit")

	outB, err := orchRunGitOut(t, workDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	commitB = strings.TrimSpace(string(outB))

	orchRunGit(t, workDir, "remote", "add", "origin", originDir)
	orchRunGit(t, workDir, "push", "origin", "main")

	return originDir, commitA, commitB
}

func orchRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func orchRunGitOut(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// dockerDeployRunner is a fake commandRunner for the deploy tests.
// It routes docker invocations by subcommand to handlers registered
// via onBuild / onRun / onInspect / onRm / onStart. Unhandled
// commands return an error so tests fail loudly if the orchestrator
// makes an unexpected call.
type dockerDeployRunner struct {
	mu       sync.Mutex
	calls    [][]string
	handlers map[string]func(args []string) (string, error)
}

func newDockerDeployRunner() *dockerDeployRunner {
	return &dockerDeployRunner{
		handlers: map[string]func(args []string) (string, error){},
	}
}

func (r *dockerDeployRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	full := append([]string{name}, args...)
	r.calls = append(r.calls, append([]string(nil), full...))
	if len(args) == 0 {
		return "", fmt.Errorf("dockerDeployRunner: empty args")
	}
	if h, ok := r.handlers[args[0]]; ok {
		return h(args)
	}
	return "", fmt.Errorf("dockerDeployRunner: unexpected command: %v", full)
}

func (r *dockerDeployRunner) Calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func (r *dockerDeployRunner) onBuild(fn func(args []string) (string, error)) {
	r.handlers["build"] = fn
}
func (r *dockerDeployRunner) onRun(fn func(args []string) (string, error)) { r.handlers["run"] = fn }
func (r *dockerDeployRunner) onInspect(fn func(args []string) (string, error)) {
	r.handlers["inspect"] = fn
}
func (r *dockerDeployRunner) onRm(fn func(args []string) (string, error)) { r.handlers["rm"] = fn }
func (r *dockerDeployRunner) onStart(fn func(args []string) (string, error)) {
	r.handlers["start"] = fn
}

// deployFixture holds the test setup for orchestrator tests.
type deployFixture struct {
	cfg         DeployConfig
	healthSrv   *httptest.Server
	docker      *dockerDeployRunner
	caddy       *fakeCaddyRunner
	originURL   string
	commit      string // commit to deploy; defaults to commitA
	commitA     string
	commitB     string
	expectedApp string
}

func newDeployFixture(t *testing.T) *deployFixture {
	t.Helper()

	originURL, commitA, commitB := setupTestGitRepo(t)
	healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthSrv.Close)

	stateDir := t.TempDir()
	caddyDir := t.TempDir()
	repoRoot := t.TempDir()
	rootConfig := filepath.Join(caddyDir, "Caddyfile")
	if err := os.WriteFile(rootConfig, []byte("import "+filepath.Join(caddyDir, "*.caddy")+"\n"), 0o644); err != nil {
		t.Fatalf("seed root config: %v", err)
	}

	port := parseHTTPPort(healthSrv.URL)

	cfg := DeployConfig{
		Source: SourceConfig{
			AllowedOrg:     "testorg",
			RepositoryRoot: repoRoot,
			OriginURL:      originURL,
		},
		Runtime: RuntimeConfig{
			PortRangeStart: port,
			PortRangeEnd:   port,
			HealthTimeout:  2 * time.Second,
			RepositoryRoot: repoRoot,
		},
		Caddy: CaddyConfig{
			BaseDomain:     testBaseDomain,
			ConfigDir:      caddyDir,
			RootConfigPath: rootConfig,
			CaddyBinary:    "caddy",
		},
		State: StateConfig{
			StateDir:   stateDir,
			BaseDomain: testBaseDomain,
		},
	}

	docker := newDockerDeployRunner()
	caddy := healthyCaddy(rootConfig)

	// Override the port allocator so it does not bind a real TCP
	// listener (which would conflict with the health server
	// already listening on `port`). The allocator just returns
	// the start port.
	oldAllocate := allocatePortFunc
	allocatePortFunc = func(start, end int) (int, error) {
		return start, nil
	}
	t.Cleanup(func() { allocatePortFunc = oldAllocate })

	return &deployFixture{
		cfg:         cfg,
		healthSrv:   healthSrv,
		docker:      docker,
		caddy:       caddy,
		originURL:   originURL,
		commit:      commitA,
		commitA:     commitA,
		commitB:     commitB,
		expectedApp: "myapp",
	}
}

func (f *deployFixture) validManifest() Manifest {
	return Manifest{
		Version:       1,
		App:           f.expectedApp,
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
}

// dockerInspectAbsent returns a handler that reports the named
// container as absent (matching real docker's stderr output for a
// non-existent container). The inspectContainerStatus helper keys
// off the "No such object" string in the captured output.
func dockerInspectAbsent(containerName string) func(args []string) (string, error) {
	return func(args []string) (string, error) {
		return "Error: No such object: " + containerName, errors.New("exit 1")
	}
}

func (f *deployFixture) dockerAllSuccess(containerName string) {
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	f.docker.onRm(func(args []string) (string, error) { return "", nil })
}

func TestDeploy_FirstDeployment(t *testing.T) {
	f := newDeployFixture(t)
	f.dockerAllSuccess(deriveContainerName(f.expectedApp, f.commit))

	manifest := f.validManifest()
	result, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if result.App != f.expectedApp || result.Commit != f.commit {
		t.Errorf("unexpected result: %+v", result)
	}

	state, err := LoadDeploymentState(f.cfg.State, f.expectedApp)
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil || state.Current.Commit != f.commit {
		t.Errorf("state.Current.commit = %q, want %q", state.Current.Commit, f.commit)
	}
	if state.Previous != nil {
		t.Errorf("state.Previous should be nil on first deploy, got %+v", state.Previous)
	}
}

func TestDeploy_ReplacementDeployment(t *testing.T) {
	f := newDeployFixture(t)

	// Seed the state with the first deployment so the new one
	// is a replacement.
	seedState(t, f.cfg.State, Deployment{
		App: f.expectedApp, Commit: f.commitA,
		Image:         deriveImage(f.expectedApp, f.commitA),
		ContainerName: deriveContainerName(f.expectedApp, f.commitA),
		HostPort:      parseHTTPPort(f.healthSrv.URL), ContainerPort: 8080,
		Hostname:   f.expectedApp + "." + testBaseDomain,
		Upstream:   fmt.Sprintf("127.0.0.1:%d", parseHTTPPort(f.healthSrv.URL)),
		DeployedAt: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
	})

	// Deploy the second commit.
	f.commit = f.commitB
	newContainer := deriveContainerName(f.expectedApp, f.commitB)
	f.dockerAllSuccess(newContainer)

	manifest := f.validManifest()
	result, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if result.Commit != f.commitB {
		t.Errorf("result.Commit = %q, want %q", result.Commit, f.commitB)
	}

	state, err := LoadDeploymentState(f.cfg.State, f.expectedApp)
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil || state.Current.Commit != f.commitB {
		t.Errorf("state.Current.commit = %q, want %q", state.Current.Commit, f.commitB)
	}
	if state.Previous == nil || state.Previous.Commit != f.commitA {
		t.Errorf("state.Previous.commit = %q, want %q", state.Previous.Commit, f.commitA)
	}
}

func TestDeploy_BuildFailure(t *testing.T) {
	f := newDeployFixture(t)
	f.docker.onInspect(dockerInspectAbsent(deriveContainerName(f.expectedApp, f.commit)))
	f.docker.onBuild(func(args []string) (string, error) {
		return "build error output", errors.New("exit 1")
	})
	f.docker.onRun(func(args []string) (string, error) {
		return "container", nil
	})
	f.docker.onRm(func(args []string) (string, error) { return "", nil })

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err == nil {
		t.Fatalf("expected build failure")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Errorf("expected ErrDeploymentFailed, got %v", err)
	}
}

func TestDeploy_HealthFailure(t *testing.T) {
	f := newDeployFixture(t)
	f.healthSrv.Close()
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	f.docker.onRm(func(args []string) (string, error) { return "", nil })

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err == nil {
		t.Fatalf("expected health failure")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Errorf("expected ErrDeploymentFailed, got %v", err)
	}
	stateFile := filepath.Join(f.cfg.State.StateDir, f.expectedApp+".state.json")
	if _, err := os.Stat(stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file must not exist after failed deploy: stat err = %v", err)
	}
}

func TestDeploy_CaddyFailure(t *testing.T) {
	f := newDeployFixture(t)
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	f.docker.onRm(func(args []string) (string, error) { return "", nil })

	caddy := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", f.cfg.Caddy.RootConfigPath), resp: caddyResponse{err: errors.New("validate failed")}},
	)

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Fatalf("expected ErrDeploymentFailed, got %v", err)
	}

	sawRm := false
	for _, call := range f.docker.Calls() {
		if len(call) >= 4 && call[1] == "rm" && call[2] == "--force" && call[3] == containerName {
			sawRm = true
		}
	}
	if !sawRm {
		t.Errorf("expected docker rm --force for candidate container %s", containerName)
	}

	stateFile := filepath.Join(f.cfg.State.StateDir, f.expectedApp+".state.json")
	if _, err := os.Stat(stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file must not exist after failed deploy: stat err = %v", err)
	}
}

func TestDeploy_StateFailure(t *testing.T) {
	f := newDeployFixture(t)
	seedState(t, f.cfg.State, Deployment{
		App: f.expectedApp, Commit: f.commitA,
		Image:         deriveImage(f.expectedApp, f.commitA),
		ContainerName: deriveContainerName(f.expectedApp, f.commitA),
		HostPort:      parseHTTPPort(f.healthSrv.URL), ContainerPort: 8080,
		Hostname:   f.expectedApp + "." + testBaseDomain,
		Upstream:   fmt.Sprintf("127.0.0.1:%d", parseHTTPPort(f.healthSrv.URL)),
		DeployedAt: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
	})
	f.commit = f.commitB
	newContainer := deriveContainerName(f.expectedApp, f.commitB)
	f.docker.onInspect(dockerInspectAbsent(newContainer))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return newContainer, nil })
	f.docker.onRm(func(args []string) (string, error) { return "", nil })

	if err := os.Chmod(f.cfg.State.StateDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.cfg.State.StateDir, 0o755) })

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Fatalf("expected ErrDeploymentFailed, got %v", err)
	}

	sawRm := false
	for _, call := range f.docker.Calls() {
		if len(call) >= 4 && call[1] == "rm" && call[2] == "--force" && call[3] == newContainer {
			sawRm = true
		}
	}
	if !sawRm {
		t.Errorf("expected docker rm --force for candidate container %s", newContainer)
	}

	state, err := LoadDeploymentState(f.cfg.State, f.expectedApp)
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil || state.Current.Commit != f.commitA {
		t.Errorf("state.Current.commit = %q, want %q (unchanged)", state.Current.Commit, f.commitA)
	}
}

func TestDeploy_CleanupFailureStillSucceeds(t *testing.T) {
	f := newDeployFixture(t)
	seedState(t, f.cfg.State, Deployment{
		App: f.expectedApp, Commit: f.commitA,
		Image:         deriveImage(f.expectedApp, f.commitA),
		ContainerName: deriveContainerName(f.expectedApp, f.commitA),
		HostPort:      parseHTTPPort(f.healthSrv.URL), ContainerPort: 8080,
		Hostname:   f.expectedApp + "." + testBaseDomain,
		Upstream:   fmt.Sprintf("127.0.0.1:%d", parseHTTPPort(f.healthSrv.URL)),
		DeployedAt: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
	})
	f.commit = f.commitB
	newContainer := deriveContainerName(f.expectedApp, f.commitB)
	oldContainer := deriveContainerName(f.expectedApp, f.commitA)
	f.docker.onInspect(dockerInspectAbsent(newContainer))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return newContainer, nil })
	// rm --force for the old container fails. Deploy should
	// still succeed (state is correct, Caddy serves new).
	f.docker.onRm(func(args []string) (string, error) {
		for _, a := range args {
			if a == oldContainer {
				return "rm failed", errors.New("exit 1")
			}
		}
		return "", nil
	})

	manifest := f.validManifest()
	result, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err != nil {
		t.Fatalf("deploy should succeed despite cleanup failure: %v", err)
	}
	if result == nil || result.Commit != f.commitB {
		t.Errorf("unexpected result: %+v", result)
	}

	state, err := LoadDeploymentState(f.cfg.State, f.expectedApp)
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current.Commit != f.commitB || state.Previous.Commit != f.commitA {
		t.Errorf("state not swapped: current=%q previous=%q", state.Current.Commit, state.Previous.Commit)
	}
}

func TestDeploy_CallerCancellationUsesRecoveryContext(t *testing.T) {
	f := newDeployFixture(t)
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	f.docker.onRm(func(args []string) (string, error) { return "", nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The cancelling wrapper cancels the caller context on the
	// first caddy call. We make validate succeed but reload fail
	// so the deploy returns an error (the fake caddy runner
	// does not observe the cancelled context itself).
	caddyInner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", f.cfg.Caddy.RootConfigPath), resp: caddyResponse{}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", f.cfg.Caddy.RootConfigPath), resp: caddyResponse{err: errors.New("reload failed")}},
	)
	caddy := &cancellingCaddyRunner{inner: caddyInner, cancel: cancel}

	manifest := f.validManifest()
	_, err := deploy(ctx, f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}

	// The candidate container must be removed by the bounded
	// recovery context (the caller context is already cancelled).
	sawRm := false
	for _, call := range f.docker.Calls() {
		if len(call) >= 4 && call[1] == "rm" && call[2] == "--force" && call[3] == containerName {
			sawRm = true
		}
	}
	if !sawRm {
		t.Errorf("expected docker rm --force for candidate container after cancellation")
	}
}

func TestDeploy_RetryIsSafe(t *testing.T) {
	f := newDeployFixture(t)
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.dockerAllSuccess(containerName)

	manifest := f.validManifest()

	result1, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	// Second deploy with the same commit: the candidate
	// container is already running from the first deploy.
	// removeIfStopped inspects, sees the container is running
	// (we change the inspect handler to return "true"), and
	// returns ErrContainerRunning, which the runtime turns
	// into a failure. Deploy returns an error; state remains
	// consistent.
	f.docker.onInspect(func(args []string) (string, error) {
		return "true", nil
	})
	_, err = deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err == nil {
		t.Fatalf("expected error from second deploy (container already running)")
	}

	state, err := LoadDeploymentState(f.cfg.State, f.expectedApp)
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil || state.Current.Commit != result1.Commit {
		t.Errorf("state.Current.commit = %q, want %q", state.Current.Commit, result1.Commit)
	}
}

func seedState(t *testing.T, cfg StateConfig, dep Deployment) {
	t.Helper()
	if err := SaveDeployment(cfg, dep); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}
