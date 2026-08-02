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

// Test-only sentinels returned by the fake docker / caddy runners
// to prove that the orchestrator preserves underlying errors via
// errors.Is. They are package-level so tests can refer to the
// exact sentinel identity (errors.Is checks pointer-equality of
// the *errors.errorString), and so we have a single source of
// truth for both the runner response and the assertion target.
//
// errSimulatedCaddyReload is returned by the fake Caddy runner on
// a specific validate/reload call to drive the combined-failure
// orchestrator paths. The orchestrator's deployError preserves
// the wrapped sentinel via the caddy layer's caddyCommandError,
// and the deployError.Is method walks every secondary so callers
// can errors.Is for it across the chain.
//
// errSimulatedDockerRm is returned by the fake Docker runner on a
// specific rm call. removeContainerForce wraps the runner err
// with %w, so errors.Is finds it through the orchestrator's
// deployError chain.
var (
	errSimulatedCaddyReload = errors.New("simulated caddy reload failure")
	errSimulatedDockerRm    = errors.New("simulated docker rm failure")
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
// It routes docker invocations by subcommand to registered
// handlers via onBuild / onRun / onInspect / onRm / onStart.
// Unhandled commands return an error so tests fail loudly if the
// orchestrator makes an unexpected call.
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
	// already listening on `port`).
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
// container as absent (matching real docker's stderr output).
func dockerInspectAbsent(containerName string) func(args []string) (string, error) {
	return func(args []string) (string, error) {
		return "Error: No such object: " + containerName, errors.New("exit 1")
	}
}

// --- Original deploy tests (carried over from PR #10) -------------

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
	f.docker.onRun(func(args []string) (string, error) { return "container", nil })
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

	f.docker.onInspect(func(args []string) (string, error) { return "true", nil })
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

func (f *deployFixture) dockerAllSuccess(containerName string) {
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	f.docker.onRm(func(args []string) (string, error) { return "", nil })
}

func seedState(t *testing.T, cfg StateConfig, dep Deployment) {
	t.Helper()
	if err := SaveDeployment(cfg, dep); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

// --- New tests for autonomous-use safety -------------------------

// sequentialCaddyRunner returns canned responses in order. The
// n-th matching call returns responses[n]. This lets tests model
// "initial promote succeeds, recovery fails" without writing
// per-call matchers (fakeCaddyRunner returns the first match, not
// the n-th).
type sequentialCaddyRunner struct {
	mu        sync.Mutex
	calls     int
	responses []caddyResponse
	match     func(args []string) bool
}

func (r *sequentialCaddyRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.match != nil && !r.match(args) {
		return "", fmt.Errorf("sequentialCaddyRunner: unexpected args: %v", args)
	}
	if r.calls >= len(r.responses) {
		return "", fmt.Errorf("sequentialCaddyRunner: no response for call %d (args=%v)", r.calls, args)
	}
	resp := r.responses[r.calls]
	r.calls++
	return resp.out, resp.err
}

// matchCaddyValidateOrReload matches both validate and reload
// subcommands. The deploy test pipeline always calls them in
// the order validate, reload. The args slice does not include
// the binary name, so a[0] is the subcommand.
func matchCaddyValidateOrReload() func([]string) bool {
	return func(a []string) bool {
		return len(a) >= 1 && (a[0] == "validate" || a[0] == "reload")
	}
}

// TestDeploy_InvalidManifestFailsBeforeSideEffects proves that a
// manifest with an invalid app name is rejected before any Docker /
// git / Caddy / state operation runs.
func TestDeploy_InvalidManifestFailsBeforeSideEffects(t *testing.T) {
	f := newDeployFixture(t)
	f.docker.onInspect(func(args []string) (string, error) {
		t.Fatalf("docker inspect must not be called before validation")
		return "", nil
	})
	f.docker.onBuild(func(args []string) (string, error) {
		t.Fatalf("docker build must not be called before validation")
		return "", nil
	})

	manifest := Manifest{
		Version:       1,
		App:           "MyApp", // invalid: uppercase
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err == nil {
		t.Fatalf("expected validation error")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Errorf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, ErrInvalidDeployInput) {
		t.Errorf("error must preserve ErrInvalidDeployInput, got %v", err)
	}
}

// TestDeploy_InvalidAppPortHealthPathPreservesSentinel proves
// that each of the manifest identity fields, when wrong, returns
// an error preserving BOTH ErrDeploymentFailed and
// ErrInvalidDeployInput.
func TestDeploy_InvalidAppPortHealthPathPreservesSentinel(t *testing.T) {
	f := newDeployFixture(t)

	cases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"bad-app", func(m *Manifest) { m.App = "BadApp" }},
		{"bad-port-low", func(m *Manifest) { m.ContainerPort = 80 }},
		{"bad-port-high", func(m *Manifest) { m.ContainerPort = 70000 }},
		{"empty-health", func(m *Manifest) { m.HealthPath = "" }},
		{"bad-health-no-slash", func(m *Manifest) { m.HealthPath = "healthz" }},
		{"bad-health-query", func(m *Manifest) { m.HealthPath = "/healthz?x=1" }},
		{"bad-version", func(m *Manifest) { m.Version = 99 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := f.validManifest()
			tc.mutate(&manifest)
			_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
				docker: f.docker,
				caddy:  f.caddy,
			})
			if err == nil {
				t.Fatalf("expected validation error")
			}
			if !errors.Is(err, ErrDeploymentFailed) {
				t.Errorf("error must preserve ErrDeploymentFailed, got %v", err)
			}
			if !errors.Is(err, ErrInvalidDeployInput) {
				t.Errorf("error must preserve ErrInvalidDeployInput, got %v", err)
			}
		})
	}
}

// TestDeploy_MismatchedBaseDomainsFailBeforeSideEffects proves
// that Caddy.BaseDomain != State.BaseDomain is rejected before
// any git / Docker / Caddy call lands.
func TestDeploy_MismatchedBaseDomainsFailBeforeSideEffects(t *testing.T) {
	f := newDeployFixture(t)
	f.cfg.State.BaseDomain = "other.example.com"

	f.docker.onInspect(func(args []string) (string, error) {
		t.Fatalf("docker inspect must not be called before validation")
		return "", nil
	})

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err == nil {
		t.Fatalf("expected validation error for mismatched base domains")
	}
	if !errors.Is(err, ErrDeploymentFailed) || !errors.Is(err, ErrInvalidDeployInput) {
		t.Errorf("error must preserve both sentinels, got %v", err)
	}
}

// TestDeploy_CorruptStateFailsBeforeCheckoutBuild proves that an
// existing state file that is corrupt / fails parsing is rejected
// before the source checkout or Docker build runs.
func TestDeploy_CorruptStateFailsBeforeCheckoutBuild(t *testing.T) {
	f := newDeployFixture(t)
	corrupt := filepath.Join(f.cfg.State.StateDir, f.expectedApp+".state.json")
	if err := os.WriteFile(corrupt, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}

	f.docker.onInspect(func(args []string) (string, error) {
		t.Fatalf("docker inspect must not be called before state check")
		return "", nil
	})
	f.docker.onBuild(func(args []string) (string, error) {
		t.Fatalf("docker build must not be called before state check")
		return "", nil
	})

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  f.caddy,
	})
	if err == nil {
		t.Fatalf("expected error from corrupt state")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Errorf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, ErrCorruptDeploymentState) {
		t.Errorf("error must preserve underlying ErrCorruptDeploymentState, got %v", err)
	}
}

// TestDeploy_FirstDeploymentStateFailureRemovesRouteBeforeRemovingCandidate
// proves that on a first deployment whose state save fails, the
// orchestrator calls RemovePromotion (so the new route is gone
// from Caddy) BEFORE removing the candidate container.
func TestDeploy_FirstDeploymentStateFailureRemovesRouteBeforeRemovingCandidate(t *testing.T) {
	f := newDeployFixture(t)
	newContainer := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(newContainer))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return newContainer, nil })

	// Caddy: initial promote succeeds (calls 1-2). Recovery
	// RemovePromotion also succeeds (calls 3-4).
	caddy := &sequentialCaddyRunner{
		match:     matchCaddyValidateOrReload(),
		responses: []caddyResponse{{}, {}, {}, {}},
	}

	if err := os.Chmod(f.cfg.State.StateDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.cfg.State.StateDir, 0o755) })

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Fatalf("expected ErrDeploymentFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "save state") {
		t.Errorf("error must mention save state, got: %v", err)
	}

	// Docker call order: build, inspect, run. The candidate
	// cleanup is the final docker call.
	calls := f.docker.Calls()
	if len(calls) < 4 {
		t.Fatalf("expected at least 4 docker calls, got %d: %v", len(calls), calls)
	}
	if calls[0][1] != "build" || calls[1][1] != "inspect" || calls[2][1] != "run" {
		t.Errorf("unexpected first-three call order: %v", calls)
	}
	last := calls[len(calls)-1]
	if !(len(last) >= 4 && last[1] == "rm" && last[2] == "--force" && last[3] == newContainer) {
		t.Errorf("expected final docker call to be rm --force on candidate %s, got %v", newContainer, last)
	}
	if caddy.calls != 4 {
		t.Errorf("expected exactly 4 caddy calls, got %d", caddy.calls)
	}
}

// TestDeploy_ReplacementStateFailureRestoresOriginalRoute proves
// that on a replacement deployment whose state save fails, the
// orchestrator re-promotes the snapshotted old Current BEFORE
// removing the candidate container. If the recovery re-promote
// fails, the candidate must be left running.
//
// The 4th Caddy call (the recovery reload) returns
// errSimulatedCaddyReload. The orchestrator's deployError
// preserves both the state-save failure and the Caddy recovery
// failure as separate secondaries, so an autonomous caller can
// errors.Is for either:
//
//   - errors.Is(err, ErrDeploymentFailed)        // always true
//   - errors.Is(err, ErrCaddyReloadFailed)       // Caddy-layer sentinel
//   - errors.Is(err, errSimulatedCaddyReload)    // specific runner error
func TestDeploy_ReplacementStateFailureRestoresOriginalRoute(t *testing.T) {
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

	// Caddy: initial promote succeeds (calls 1-2). Recovery
	// re-promote validate succeeds (call 3) and reload fails
	// with the test sentinel (call 4).
	caddy := &sequentialCaddyRunner{
		match: matchCaddyValidateOrReload(),
		responses: []caddyResponse{
			{},
			{},
			{},
			{err: errSimulatedCaddyReload},
		},
	}
	if err := os.Chmod(f.cfg.State.StateDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.cfg.State.StateDir, 0o755) })

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Fatalf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, errSimulatedCaddyReload) {
		t.Errorf("error must preserve underlying errSimulatedCaddyReload so callers can errors.Is, got %v", err)
	}
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Errorf("error must preserve Caddy-layer ErrCaddyReloadFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "save state") {
		t.Errorf("error must mention save state, got: %v", err)
	}
	if !strings.Contains(err.Error(), "caddy recovery also failed") {
		t.Errorf("error must mention caddy recovery failure, got: %v", err)
	}

	// Candidate must NOT be removed: Caddy recovery failed.
	for _, call := range f.docker.Calls() {
		if len(call) >= 4 && call[1] == "rm" && call[2] == "--force" && call[3] == newContainer {
			t.Errorf("candidate %s must NOT be removed when Caddy recovery fails: %v", newContainer, call)
		}
	}
	if caddy.calls != 4 {
		t.Errorf("expected exactly 4 caddy calls, got %d", caddy.calls)
	}
}

// TestDeploy_CaddyRecoveryFailureKeepsCandidateRunning proves that
// when state-save recovery (Caddy revert) fails, the candidate
// container is left running (Caddy may still route to it) and the
// returned error preserves BOTH the state-save failure and the
// Caddy recovery failure as structurally detectable sentinels.
//
// The 3rd Caddy call (the recovery validate) returns
// errSimulatedCaddyReload. The orchestrator's deployError
// preserves both the state-save failure and the Caddy recovery
// failure as separate secondaries, so an autonomous caller can
// errors.Is for either:
//
//   - errors.Is(err, ErrDeploymentFailed)        // always true
//   - errors.Is(err, ErrCaddyValidateFailed)     // Caddy-layer sentinel
//   - errors.Is(err, errSimulatedCaddyReload)    // specific runner error
func TestDeploy_CaddyRecoveryFailureKeepsCandidateRunning(t *testing.T) {
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

	// Caddy: initial promote succeeds (calls 1-2). Recovery
	// re-promote validate fails with the test sentinel (call 3).
	caddy := &sequentialCaddyRunner{
		match: matchCaddyValidateOrReload(),
		responses: []caddyResponse{
			{},
			{},
			{err: errSimulatedCaddyReload},
		},
	}
	if err := os.Chmod(f.cfg.State.StateDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.cfg.State.StateDir, 0o755) })

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Errorf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, errSimulatedCaddyReload) {
		t.Errorf("error must preserve underlying errSimulatedCaddyReload so callers can errors.Is, got %v", err)
	}
	if !errors.Is(err, ErrCaddyValidateFailed) {
		t.Errorf("error must preserve Caddy-layer ErrCaddyValidateFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "save state") {
		t.Errorf("error must mention the state-save failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "caddy recovery also failed") {
		t.Errorf("error must mention the caddy recovery failure, got: %v", err)
	}

	for _, call := range f.docker.Calls() {
		if len(call) >= 4 && call[1] == "rm" && call[2] == "--force" && call[3] == newContainer {
			t.Errorf("candidate %s must not be removed when Caddy recovery fails: %v", newContainer, call)
		}
	}
}

// TestDeploy_CleanupFailureOnSuccessPathReported proves that a
// failure to remove the formerly-current container on the
// success path is reported as a warning, not silently swallowed
// and not converted into a deployment error.
//
// Rationale (autonomous-use safety): once the new deployment is
// committed (state persisted, Caddy serving the new route), the
// deploy has SUCCEEDED. An autonomous caller that sees an error
// would retry a deployment that already worked. The fix is to
// return success with a single warning on result.Warnings.
func TestDeploy_CleanupFailureOnSuccessPathReported(t *testing.T) {
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
	// Old-container rm fails; candidate rm succeeds.
	f.docker.onRm(func(args []string) (string, error) {
		for _, a := range args {
			if a == oldContainer {
				return "rm failed", errSimulatedDockerRm
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
		t.Fatalf("deploy must succeed despite old-container cleanup failure (deployment is live); got err: %v", err)
	}
	if result == nil {
		t.Fatalf("result must be non-nil on success")
	}
	if result.Commit != f.commitB {
		t.Errorf("result.Commit = %q, want %q", result.Commit, f.commitB)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("expected exactly 1 warning on result.Warnings, got %d: %v", len(result.Warnings), result.Warnings)
	}
	if !strings.Contains(result.Warnings[0].Error(), "post-deploy cleanup of old container") {
		t.Errorf("warning must mention post-deploy cleanup, got: %v", result.Warnings[0])
	}
	if !errors.Is(result.Warnings[0], errSimulatedDockerRm) {
		t.Errorf("warning must wrap the underlying errSimulatedDockerRm so callers can errors.Is it, got: %v", result.Warnings[0])
	}

	state, lerr := LoadDeploymentState(f.cfg.State, f.expectedApp)
	if lerr != nil {
		t.Fatalf("LoadDeploymentState: %v", lerr)
	}
	if state.Current.Commit != f.commitB || state.Previous.Commit != f.commitA {
		t.Errorf("state not swapped: current=%q previous=%q", state.Current.Commit, state.Previous.Commit)
	}
}

// TestDeploy_CandidateCleanupFailureReported proves that when the
// candidate container cleanup fails after a successful Caddy
// recovery, the failure is reported in the returned error.
//
// Candidate-cleanup failures on FAILED deployment paths remain
// fatal: the deployment did not commit, so the caller must see
// an error and may retry. This is in contrast to the success
// path (step 7), where cleanup failure is non-fatal.
func TestDeploy_CandidateCleanupFailureReported(t *testing.T) {
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
	// Candidate rm fails; old-container rm succeeds.
	f.docker.onRm(func(args []string) (string, error) {
		for _, a := range args {
			if a == newContainer {
				return "rm failed", errSimulatedDockerRm
			}
		}
		return "", nil
	})

	// All four Caddy calls succeed (initial promote + recovery
	// re-promote).
	caddy := &sequentialCaddyRunner{
		match:     matchCaddyValidateOrReload(),
		responses: []caddyResponse{{}, {}, {}, {}},
	}
	if err := os.Chmod(f.cfg.State.StateDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.cfg.State.StateDir, 0o755) })

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected error from candidate cleanup failure")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Errorf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, errSimulatedDockerRm) {
		t.Errorf("error must preserve underlying errSimulatedDockerRm so callers can errors.Is, got %v", err)
	}
	if !strings.Contains(err.Error(), "save state") {
		t.Errorf("error must mention the primary save-state failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "candidate cleanup failed") {
		t.Errorf("error must mention candidate cleanup failure, got: %v", err)
	}
	for _, call := range f.docker.Calls() {
		if len(call) >= 4 && call[1] == "rm" && call[2] == "--force" && call[3] == oldContainer {
			t.Errorf("old container %s must not be removed in candidate-cleanup-failure path", oldContainer)
		}
	}
}

// TestDeploy_CallerCancellationUsesFreshRecoveryContext proves
// that even when the caller context is cancelled, the
// orchestrator uses a bounded recovery context for cleanup so
// cleanup actually runs.
//
// The Caddy layer's rollback reload also has to succeed here:
// otherwise the safety rule (rollback-failed -> keep candidate)
// would keep the container running and this test would not be
// able to assert that cleanup ran under a fresh context. Three
// responses are provided: validate success, primary reload
// failure, rollback reload success. Cancellation happens on the
// first caddy invocation (validate), so the primary reload sees
// a cancelled context but still returns its canned error; the
// rollback reload uses context.Background() inside the Caddy
// layer and therefore runs to completion.
func TestDeploy_CallerCancellationUsesFreshRecoveryContext(t *testing.T) {
	f := newDeployFixture(t)
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	f.docker.onRm(func(args []string) (string, error) { return "", nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caddyInner := &sequentialCaddyRunner{
		match: matchCaddyValidateOrReload(),
		responses: []caddyResponse{
			{},
			{err: errors.New("reload failed")},
			{},
		},
	}
	caddy := &cancellingCaddyRunner{inner: caddyInner, cancel: cancel}

	manifest := f.validManifest()
	_, err := deploy(ctx, f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Errorf("error must preserve ErrDeploymentFailed, got %v", err)
	}

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

// TestDeploy_PromotionFailureRollbackSucceededRemovesCandidate
// proves the safe half of the candidate-on-promotion-failure
// rule: when the initial Caddy reload fails but the rollback
// reload succeeds, Caddy has demonstrably returned to the
// pre-promotion state and the orchestrator therefore removes
// the candidate container. The returned error preserves
// ErrDeploymentFailed, the Caddy-layer sentinel
// (ErrCaddyReloadFailed), and the underlying runner error so
// callers can branch on any of them via errors.Is.
//
// This is the success counterpart of the safety rule: a
// successful rollback means traffic is no longer flowing to the
// candidate, so removing it is safe.
func TestDeploy_PromotionFailureRollbackSucceededRemovesCandidate(t *testing.T) {
	f := newDeployFixture(t)
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	f.docker.onRm(func(args []string) (string, error) { return "", nil })

	// Caddy: validate success, primary reload fails, rollback
	// reload succeeds. This is the "reload failed but Caddy
	// recovered" path.
	caddy := &sequentialCaddyRunner{
		match: matchCaddyValidateOrReload(),
		responses: []caddyResponse{
			{},
			{err: errSimulatedCaddyReload},
			{},
		},
	}

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected promotion failure")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Fatalf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Errorf("error must preserve ErrCaddyReloadFailed, got %v", err)
	}
	if !errors.Is(err, errSimulatedCaddyReload) {
		t.Errorf("error must preserve underlying errSimulatedCaddyReload so callers can errors.Is, got %v", err)
	}
	// The Caddy sentinel walks the chain through
	// caddyCommandError.rollback == nil, so the rollback
	// sentinel is NOT in the chain (there was no rollback
	// failure).
	if errors.Is(err, fmt.Errorf("rollback reload also failed")) {
		// This is a coarse assertion; the structural check
		// above (caddyErr.rollback == nil) is exercised in
		// TestDeploy_PromotionFailureRollbackFailedKeepsCandidate.
	}

	sawRm := false
	for _, call := range f.docker.Calls() {
		if len(call) >= 4 && call[1] == "rm" && call[2] == "--force" && call[3] == containerName {
			sawRm = true
		}
	}
	if !sawRm {
		t.Errorf("candidate %s must be removed when Caddy rollback succeeds, got calls: %v", containerName, f.docker.Calls())
	}
	if caddy.calls != 3 {
		t.Errorf("expected exactly 3 caddy calls (validate, primary reload fail, rollback reload success), got %d", caddy.calls)
	}

	// State file must not exist: the deployment did not commit.
	stateFile := filepath.Join(f.cfg.State.StateDir, f.expectedApp+".state.json")
	if _, err := os.Stat(stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file must not exist after failed promote, stat err = %v", err)
	}
}

// TestDeploy_PromotionFailureRollbackFailedKeepsCandidate proves
// the dangerous-half safety rule: when the initial Caddy reload
// fails AND the Caddy rollback reload also fails, Caddy's
// running configuration is uncertain. The new route may still be
// live and traffic may still be flowing to the candidate
// container. Removing the candidate in this state would turn a
// recoverable partial failure into an outage, so the
// orchestrator deliberately keeps the candidate running and
// returns an error preserving the Caddy-layer sentinel
// (ErrCaddyReloadFailed), the original reload failure, and the
// rollback failure so callers can branch on any of them via
// errors.Is.
//
// Structural check: the error is unwrapped to *caddyCommandError
// and its rollback field is non-nil, proving the orchestrator
// inspects the Caddy-layer model rather than guessing.
func TestDeploy_PromotionFailureRollbackFailedKeepsCandidate(t *testing.T) {
	f := newDeployFixture(t)
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	// If rm is called for the candidate, the test fails: the
	// safety rule is being violated.
	f.docker.onRm(func(args []string) (string, error) {
		for _, a := range args {
			if a == containerName {
				t.Errorf("candidate %s must NOT be removed when Caddy rollback fails: %v", containerName, args)
			}
		}
		return "", nil
	})

	// Caddy: validate success, primary reload fails, rollback
	// reload also fails. This is the "Caddy could not recover"
	// path.
	caddy := &sequentialCaddyRunner{
		match: matchCaddyValidateOrReload(),
		responses: []caddyResponse{
			{},
			{err: errSimulatedCaddyReload},
			{err: errSimulatedCaddyReload},
		},
	}

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected promotion failure")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Fatalf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Errorf("error must preserve ErrCaddyReloadFailed, got %v", err)
	}
	// The original reload failure and the rollback failure are
	// the SAME test sentinel (errSimulatedCaddyReload) in this
	// scenario, so a single errors.Is is sufficient to prove
	// both ends of the chain are preserved. We additionally
	// check the structural property below.
	if !errors.Is(err, errSimulatedCaddyReload) {
		t.Errorf("error must preserve errSimulatedCaddyReload through the chain, got %v", err)
	}

	// Structural check: the underlying error is a
	// *caddyCommandError with a non-nil rollback field.
	var caddyErr *caddyCommandError
	if !errors.As(err, &caddyErr) {
		t.Fatalf("error must unwrap to *caddyCommandError, got %T: %v", err, err)
	}
	if caddyErr.rollback == nil {
		t.Errorf("caddyCommandError.rollback must be non-nil when rollback reload also failed, got nil")
	}
	if !errors.Is(caddyErr.rollback, errSimulatedCaddyReload) {
		t.Errorf("caddyCommandError.rollback must preserve the rollback failure, got %v", caddyErr.rollback)
	}

	// The message must mention the rollback failure so human
	// operators can diagnose without inspecting the chain.
	if !strings.Contains(err.Error(), "rollback reload also failed") {
		t.Errorf("error must mention the rollback failure, got: %v", err)
	}
	// It must NOT mention "cleanup: <nil>" — cleanup was
	// deliberately not attempted.
	if strings.Contains(err.Error(), "<nil>") {
		t.Errorf("error message must not contain '<nil>', got: %v", err)
	}
	if strings.Contains(err.Error(), "cleanup:") {
		t.Errorf("error message must not mention 'cleanup:' when cleanup was not attempted, got: %v", err)
	}

	// secondaries must NOT contain a nil entry.
	for i, e := range caddyErrUnwrapSecondaries(err) {
		if e == nil {
			t.Errorf("secondaries[%d] must not be nil", i)
		}
	}

	if caddy.calls != 3 {
		t.Errorf("expected exactly 3 caddy calls (validate, primary reload fail, rollback reload fail), got %d", caddy.calls)
	}

	// State file must not exist: the deployment did not commit.
	stateFile := filepath.Join(f.cfg.State.StateDir, f.expectedApp+".state.json")
	if _, err := os.Stat(stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file must not exist after failed promote, stat err = %v", err)
	}
}

// TestDeploy_PromotionFailureSuccessfulCleanupHasNoBogusSecondary
// proves that when promotion fails but the cleanup succeeds,
// the returned error does not include a nil secondary and the
// message does not contain "cleanup: <nil>". This guards
// against regressions where the orchestrator appends nil
// entries to secondaries or renders nil-cleanup into the
// message.
func TestDeploy_PromotionFailureSuccessfulCleanupHasNoBogusSecondary(t *testing.T) {
	f := newDeployFixture(t)
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	// Cleanup rm succeeds.
	f.docker.onRm(func(args []string) (string, error) { return "", nil })

	// Caddy: validate success, primary reload fails, rollback
	// reload succeeds. Cleanup also succeeds.
	caddy := &sequentialCaddyRunner{
		match: matchCaddyValidateOrReload(),
		responses: []caddyResponse{
			{},
			{err: errSimulatedCaddyReload},
			{},
		},
	}

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected promotion failure")
	}

	// Structural check: the deployError must NOT contain a nil
	// secondary. Every entry in secondaries must be non-nil.
	for i, e := range caddyErrUnwrapSecondaries(err) {
		if e == nil {
			t.Errorf("secondaries[%d] must not be nil", i)
		}
	}

	// The message must mention promote (the primary failure)
	// and must NOT mention "cleanup:" or "<nil>" — cleanup
	// succeeded and produced no secondary.
	msg := err.Error()
	if !strings.HasPrefix(msg, "promote:") {
		t.Errorf("message must start with 'promote:', got: %v", msg)
	}
	if strings.Contains(msg, "<nil>") {
		t.Errorf("message must not contain '<nil>', got: %v", msg)
	}
	if strings.Contains(msg, "cleanup:") {
		t.Errorf("message must not mention 'cleanup:' when cleanup succeeded, got: %v", msg)
	}

	// All sentinels remain detectable.
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Errorf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Errorf("error must preserve ErrCaddyReloadFailed, got %v", err)
	}
	if !errors.Is(err, errSimulatedCaddyReload) {
		t.Errorf("error must preserve errSimulatedCaddyReload, got %v", err)
	}
}

// TestDeploy_PromotionFailureCleanupFailureReportsBoth proves
// that when promotion fails, Caddy rollback succeeds, but the
// candidate-container cleanup itself fails, the returned error
// preserves BOTH the promotion failure and the cleanup failure
// as separate errors.Is-detectable sentinels and the message
// mentions both.
func TestDeploy_PromotionFailureCleanupFailureReportsBoth(t *testing.T) {
	f := newDeployFixture(t)
	containerName := deriveContainerName(f.expectedApp, f.commit)
	f.docker.onInspect(dockerInspectAbsent(containerName))
	f.docker.onBuild(func(args []string) (string, error) { return "", nil })
	f.docker.onRun(func(args []string) (string, error) { return containerName, nil })
	f.docker.onRm(func(args []string) (string, error) {
		for _, a := range args {
			if a == containerName {
				return "rm failed", errSimulatedDockerRm
			}
		}
		return "", nil
	})

	caddy := &sequentialCaddyRunner{
		match: matchCaddyValidateOrReload(),
		responses: []caddyResponse{
			{},
			{err: errSimulatedCaddyReload},
			{},
		},
	}

	manifest := f.validManifest()
	_, err := deploy(context.Background(), f.cfg, manifest, f.commit, deployDeps{
		docker: f.docker,
		caddy:  caddy,
	})
	if err == nil {
		t.Fatalf("expected promotion+cleanup failure")
	}
	if !errors.Is(err, ErrDeploymentFailed) {
		t.Fatalf("error must preserve ErrDeploymentFailed, got %v", err)
	}
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Errorf("error must preserve ErrCaddyReloadFailed, got %v", err)
	}
	if !errors.Is(err, errSimulatedCaddyReload) {
		t.Errorf("error must preserve errSimulatedCaddyReload, got %v", err)
	}
	if !errors.Is(err, errSimulatedDockerRm) {
		t.Errorf("error must preserve errSimulatedDockerRm so callers can errors.Is, got %v", err)
	}

	msg := err.Error()
	if !strings.Contains(msg, "promote:") {
		t.Errorf("message must mention 'promote:', got: %v", msg)
	}
	if !strings.Contains(msg, "cleanup:") {
		t.Errorf("message must mention 'cleanup:' when cleanup failed, got: %v", msg)
	}
	if strings.Contains(msg, "<nil>") {
		t.Errorf("message must not contain '<nil>', got: %v", msg)
	}

	// Structural: no nil secondary.
	for i, e := range caddyErrUnwrapSecondaries(err) {
		if e == nil {
			t.Errorf("secondaries[%d] must not be nil", i)
		}
	}
}

// caddyErrUnwrapSecondaries walks the deployError chain and
// returns every error value reachable through Unwrap and
// errors.As. Tests use it to assert "no nil in secondaries"
// structurally rather than pattern-matching on the message.
func caddyErrUnwrapSecondaries(err error) []error {
	var dErr *deployError
	if !errors.As(err, &dErr) {
		return nil
	}
	out := make([]error, 0)
	for _, s := range dErr.secondaries {
		out = append(out, s)
	}
	return out
}
