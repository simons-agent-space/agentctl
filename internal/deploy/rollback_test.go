package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDockerRunner records docker invocations and returns canned
// responses. The contexts slice records the context received for
// each call. The errs slice records the context's error at the
// moment of the call — not the current state, which may have been
// mutated by a later cancel() — so tests can prove that cleanup
// calls use a fresh (non-cancelled) context.
type fakeDockerRunner struct {
	mu        sync.Mutex
	calls     [][]string
	contexts  []context.Context
	errs      []error
	responses []fakeDockerEntry
}

type fakeDockerEntry struct {
	match func(args []string) bool
	resp  dockerResponse
}

type dockerResponse struct {
	out string
	err error
}

func (f *fakeDockerRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := append([]string{name}, args...)
	f.calls = append(f.calls, append([]string(nil), full...))
	f.contexts = append(f.contexts, ctx)
	f.errs = append(f.errs, ctx.Err())
	for _, e := range f.responses {
		if e.match(args) {
			return e.resp.out, e.resp.err
		}
	}
	return "", fmt.Errorf("fakeDockerRunner: unexpected command: %v", full)
}

func (f *fakeDockerRunner) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func (f *fakeDockerRunner) Contexts() []context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]context.Context{}, f.contexts...)
}

// CallErrors returns a copy of the error each context carried at
// the moment of the call. Tests should use this instead of
// calling Err() on the recorded contexts, because the contexts
// may be cancelled later by the rollback's own cleanup calls.
func (f *fakeDockerRunner) CallErrors() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error{}, f.errs...)
}

// matchDockerInspect matches `docker inspect --format {{.State.Running}} <name>`.
func matchDockerInspect(name string) func([]string) bool {
	return func(a []string) bool {
		return len(a) >= 4 && a[0] == "inspect" && a[1] == "--format" && a[len(a)-1] == name
	}
}

// matchDockerStart matches `docker start <name>`.
func matchDockerStart(name string) func([]string) bool {
	return func(a []string) bool {
		return len(a) >= 2 && a[0] == "start" && a[1] == name
	}
}

// matchDockerRm matches `docker rm --force <name>`.
func matchDockerRm(name string) func([]string) bool {
	return func(a []string) bool {
		return len(a) >= 3 && a[0] == "rm" && a[1] == "--force" && a[2] == name
	}
}

// matchDockerRun matches `docker run ... --name <name> ...`.
func matchDockerRun(name string) func([]string) bool {
	return func(a []string) bool {
		for i, arg := range a {
			if arg == "--name" && i+1 < len(a) && a[i+1] == name {
				return a[0] == "run"
			}
		}
		return false
	}
}

// cancellingCaddyRunner wraps a fakeCaddyRunner and cancels the
// caller's context on the first invocation. This lets tests
// simulate caller cancellation during the Caddy promotion phase.
type cancellingCaddyRunner struct {
	inner  *fakeCaddyRunner
	cancel context.CancelFunc
	mu     sync.Mutex
	called int
}

func (r *cancellingCaddyRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.called++
	first := r.called == 1
	r.mu.Unlock()
	if first {
		r.cancel()
	}
	return r.inner.Run(ctx, name, args...)
}

// rollbackFixture builds a RollbackConfig with a temp state dir,
// a temp Caddy root, and a localhost health-check server. It
// seeds the state with two valid deployments and returns the
// pieces tests need.
type rollbackFixture struct {
	cfg                   RollbackConfig
	healthSrv             *httptest.Server
	current               Deployment
	previous              Deployment
	previousContainerName string
}

func newRollbackFixture(t *testing.T) *rollbackFixture {
	t.Helper()

	// Health-check server: always returns 200.
	healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthSrv.Close)

	stateDir := t.TempDir()
	caddyDir := t.TempDir()
	rootConfig := filepath.Join(caddyDir, "Caddyfile")
	if err := os.WriteFile(rootConfig, []byte("import "+filepath.Join(caddyDir, "*.caddy")+"\n"), 0o644); err != nil {
		t.Fatalf("seed root config: %v", err)
	}

	cfg := RollbackConfig{
		App: "myapp",
		State: StateConfig{
			StateDir:   stateDir,
			BaseDomain: testBaseDomain,
		},
		Runtime: RuntimeConfig{
			PortRangeStart: 49152,
			PortRangeEnd:   65535,
			HealthTimeout:  2 * time.Second,
		},
		Caddy: CaddyConfig{
			BaseDomain:     testBaseDomain,
			ConfigDir:      caddyDir,
			RootConfigPath: rootConfig,
			CaddyBinary:    "caddy",
		},
		HealthPath: "/healthz",
	}

	// Both deployments use the health server's port so health
	// checks succeed during tests. The Caddy promotion constructs
	// upstream URLs from HostPort, so using the same port for both
	// is fine (the fake caddy runner does not validate URLs).
	port := parseHTTPPort(healthSrv.URL)

	commitA := strings.Repeat("a", 40)
	commitB := strings.Repeat("b", 40)

	// `previous` is the deployment that ends up in state.Previous:
	// it is saved first so the second save moves it into the
	// previous slot. `current` is the deployment that ends up in
	// state.Current: it is saved second so it becomes the live one.
	previous := Deployment{
		App:           "myapp",
		Commit:        commitA,
		Image:         deriveImage("myapp", commitA),
		ContainerName: deriveContainerName("myapp", commitA),
		HostPort:      port,
		ContainerPort: 8080,
		Hostname:      "myapp." + testBaseDomain,
		Upstream:      fmt.Sprintf("127.0.0.1:%d", port),
		DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
	}
	current := Deployment{
		App:           "myapp",
		Commit:        commitB,
		Image:         deriveImage("myapp", commitB),
		ContainerName: deriveContainerName("myapp", commitB),
		HostPort:      port,
		ContainerPort: 8080,
		Hostname:      "myapp." + testBaseDomain,
		Upstream:      fmt.Sprintf("127.0.0.1:%d", port),
		DeployedAt:    time.Date(2026, 8, 2, 10, 5, 0, 0, time.UTC),
	}

	// Seed state: previous first (becomes Current), then current
	// (becomes Current, previous moves to Previous). After this
	// the names match: state.Current = current, state.Previous = previous.
	if err := SaveDeployment(cfg.State, previous); err != nil {
		t.Fatalf("seed previous: %v", err)
	}
	if err := SaveDeployment(cfg.State, current); err != nil {
		t.Fatalf("seed current: %v", err)
	}

	return &rollbackFixture{
		cfg:                   cfg,
		healthSrv:             healthSrv,
		current:               current,
		previous:              previous,
		previousContainerName: previous.ContainerName,
	}
}

func parseHTTPPort(url string) int {
	idx := strings.LastIndex(url, ":")
	if idx < 0 {
		return 0
	}
	port, err := strconv.Atoi(url[idx+1:])
	if err != nil {
		return 0
	}
	return port
}

// dockerRunning returns a fakeDockerRunner that reports the named
// container as already running.
func dockerRunning(containerName string) *fakeDockerRunner {
	return newFakeDockerRunner(
		fakeDockerEntry{match: matchDockerInspect(containerName), resp: dockerResponse{out: "true"}},
	)
}

// dockerStopped returns a fakeDockerRunner that reports the named
// container as stopped and (optionally) fails or succeeds on
// `docker start`.
func dockerStopped(containerName string, startErr error) *fakeDockerRunner {
	entries := []fakeDockerEntry{
		{match: matchDockerInspect(containerName), resp: dockerResponse{out: "false"}},
	}
	if startErr != nil {
		entries = append(entries, fakeDockerEntry{
			match: matchDockerStart(containerName),
			resp:  dockerResponse{err: startErr},
		})
	} else {
		entries = append(entries, fakeDockerEntry{
			match: matchDockerStart(containerName),
			resp:  dockerResponse{out: containerName},
		})
	}
	return newFakeDockerRunner(entries...)
}

// dockerAbsent returns a fakeDockerRunner that reports the named
// container as absent and (optionally) fails or succeeds on
// `docker run`.
func dockerAbsent(containerName string, runErr error) *fakeDockerRunner {
	entries := []fakeDockerEntry{
		{match: matchDockerInspect(containerName), resp: dockerResponse{out: "", err: errors.New("No such object")}},
	}
	if runErr != nil {
		entries = append(entries, fakeDockerEntry{
			match: matchDockerRun(containerName),
			resp:  dockerResponse{err: runErr},
		})
	} else {
		entries = append(entries, fakeDockerEntry{
			match: matchDockerRun(containerName),
			resp:  dockerResponse{out: containerName},
		})
	}
	return newFakeDockerRunner(entries...)
}

// newFakeDockerRunner constructs a fakeDockerRunner with the given
// entries.
func newFakeDockerRunner(entries ...fakeDockerEntry) *fakeDockerRunner {
	return &fakeDockerRunner{responses: entries}
}

// healthyCaddy returns a fake caddy runner that succeeds on
// validate and reload against the given root config.
func healthyCaddy(root string) *fakeCaddyRunner {
	return newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", root), resp: caddyResponse{}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", root), resp: caddyResponse{}},
	)
}

func TestRollbackDeployment_Success(t *testing.T) {
	f := newRollbackFixture(t)

	docker := dockerRunning(f.previousContainerName)
	// Also handle the post-swap `docker rm --force <currentContainer>`.
	docker.responses = append(docker.responses,
		fakeDockerEntry{match: matchDockerRm(f.current.ContainerName), resp: dockerResponse{}},
	)
	caddy := healthyCaddy(f.cfg.Caddy.RootConfigPath)

	err := rollbackDeployment(context.Background(), f.cfg, rollbackDeps{docker: docker, caddy: caddy})
	if err != nil {
		t.Fatalf("rollbackDeployment: %v", err)
	}

	// State must be swapped: state.Current becomes what was
	// state.Previous before, and vice versa.
	state, err := LoadDeploymentState(f.cfg.State, "myapp")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil || state.Current.Commit != f.previous.Commit {
		t.Errorf("current.commit = %q, want %q", state.Current.Commit, f.previous.Commit)
	}
	if state.Previous == nil || state.Previous.Commit != f.current.Commit {
		t.Errorf("previous.commit = %q, want %q", state.Previous.Commit, f.current.Commit)
	}

	// The formerly-current container must have been removed.
	foundRm := false
	for _, call := range docker.Calls() {
		if len(call) >= 3 && call[1] == "rm" && call[2] == "--force" && call[3] == f.current.ContainerName {
			foundRm = true
			break
		}
	}
	if !foundRm {
		t.Errorf("expected docker rm --force for current container %s", f.current.ContainerName)
	}
}

func TestRollbackDeployment_MissingPrevious(t *testing.T) {
	f := newRollbackFixture(t)

	// Erase previous by swapping (calling rollback once on the
	// initial two-deployment state would clear previous, but we
	// want to simulate the "no previous" state directly). The
	// easiest way: seed only a single deployment.
	single := Deployment{
		App:           "solo",
		Commit:        strings.Repeat("c", 40),
		Image:         deriveImage("solo", strings.Repeat("c", 40)),
		ContainerName: deriveContainerName("solo", strings.Repeat("c", 40)),
		HostPort:      parseHTTPPort(f.healthSrv.URL),
		ContainerPort: 8080,
		Hostname:      "solo." + testBaseDomain,
		Upstream:      fmt.Sprintf("127.0.0.1:%d", parseHTTPPort(f.healthSrv.URL)),
		DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
	}
	if err := SaveDeployment(f.cfg.State, single); err != nil {
		t.Fatalf("seed solo: %v", err)
	}
	// Verify Previous is nil.
	state, err := LoadDeploymentState(f.cfg.State, "solo")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Previous != nil {
		t.Fatalf("test setup: expected Previous nil for solo app")
	}

	cfg := f.cfg
	cfg.App = "solo"

	docker := dockerRunning(single.ContainerName)
	caddy := healthyCaddy(f.cfg.Caddy.RootConfigPath)

	err = rollbackDeployment(context.Background(), cfg, rollbackDeps{docker: docker, caddy: caddy})
	if !errors.Is(err, ErrNoPreviousDeployment) {
		t.Errorf("expected ErrNoPreviousDeployment, got %v", err)
	}

	// State must be unchanged.
	state2, _ := LoadDeploymentState(f.cfg.State, "solo")
	if state2.Current == nil || state2.Current.Commit != single.Commit {
		t.Errorf("state changed unexpectedly")
	}
}

func TestRollbackDeployment_PreviousContainerStartFailure(t *testing.T) {
	f := newRollbackFixture(t)

	// Previous container is stopped; docker start fails.
	docker := dockerStopped(f.previousContainerName, errors.New("start failed"))
	caddy := healthyCaddy(f.cfg.Caddy.RootConfigPath)

	err := rollbackDeployment(context.Background(), f.cfg, rollbackDeps{docker: docker, caddy: caddy})
	if !errors.Is(err, ErrRollbackFailed) {
		t.Errorf("expected ErrRollbackFailed, got %v", err)
	}

	// State must be unchanged.
	state, _ := LoadDeploymentState(f.cfg.State, "myapp")
	if state.Current.Commit != f.current.Commit {
		t.Errorf("state changed: current.commit = %q, want %q", state.Current.Commit, f.current.Commit)
	}
}

func TestRollbackDeployment_HealthFailure(t *testing.T) {
	f := newRollbackFixture(t)

	// Close the health server so health checks fail.
	f.healthSrv.Close()

	docker := dockerRunning(f.previousContainerName)
	caddy := healthyCaddy(f.cfg.Caddy.RootConfigPath)

	err := rollbackDeployment(context.Background(), f.cfg, rollbackDeps{docker: docker, caddy: caddy})
	if !errors.Is(err, ErrRollbackFailed) {
		t.Errorf("expected ErrRollbackFailed, got %v", err)
	}

	// State must be unchanged.
	state, _ := LoadDeploymentState(f.cfg.State, "myapp")
	if state.Current.Commit != f.current.Commit {
		t.Errorf("state changed: current.commit = %q, want %q", state.Current.Commit, f.current.Commit)
	}
}

func TestRollbackDeployment_CaddyPromotionFailure(t *testing.T) {
	f := newRollbackFixture(t)

	docker := dockerRunning(f.previousContainerName)
	caddy := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", f.cfg.Caddy.RootConfigPath), resp: caddyResponse{}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", f.cfg.Caddy.RootConfigPath), resp: caddyResponse{err: errors.New("reload failed")}},
	)

	err := rollbackDeployment(context.Background(), f.cfg, rollbackDeps{docker: docker, caddy: caddy})
	if !errors.Is(err, ErrRollbackFailed) {
		t.Errorf("expected ErrRollbackFailed, got %v", err)
	}

	// State must be unchanged.
	state, _ := LoadDeploymentState(f.cfg.State, "myapp")
	if state.Current.Commit != f.current.Commit {
		t.Errorf("state changed: current.commit = %q, want %q", state.Current.Commit, f.current.Commit)
	}
}

func TestRollbackDeployment_StateWriteFailure(t *testing.T) {
	f := newRollbackFixture(t)

	docker := dockerRunning(f.previousContainerName)
	// Handle the post-swap rm (which won't happen because state save fails first,
	// but defensive).
	caddy := healthyCaddy(f.cfg.Caddy.RootConfigPath)

	// Make state dir read-only so SaveDeployment cannot write the temp file.
	if err := os.Chmod(f.cfg.State.StateDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.cfg.State.StateDir, 0o755) })

	err := rollbackDeployment(context.Background(), f.cfg, rollbackDeps{docker: docker, caddy: caddy})
	if !errors.Is(err, ErrRollbackFailed) {
		t.Errorf("expected ErrRollbackFailed, got %v", err)
	}
}

func TestRollbackDeployment_CleanupFailureRollsBackStillSucceeds(t *testing.T) {
	f := newRollbackFixture(t)

	docker := dockerRunning(f.previousContainerName)
	// Make the post-swap `docker rm --force` for the current
	// container fail. The rollback should still report success
	// because Caddy serves the previous deployment and state is
	// swapped.
	docker.responses = append(docker.responses,
		fakeDockerEntry{match: matchDockerRm(f.current.ContainerName), resp: dockerResponse{err: errors.New("rm failed")}},
	)
	caddy := healthyCaddy(f.cfg.Caddy.RootConfigPath)

	err := rollbackDeployment(context.Background(), f.cfg, rollbackDeps{docker: docker, caddy: caddy})
	if err != nil {
		t.Errorf("expected nil despite remove failure, got %v", err)
	}

	// State must still be swapped.
	state, _ := LoadDeploymentState(f.cfg.State, "myapp")
	if state.Current.Commit != f.previous.Commit {
		t.Errorf("state not swapped: current.commit = %q, want %q", state.Current.Commit, f.previous.Commit)
	}
}

func TestRollbackDeployment_CallerCancellationUsesRecoveryContext(t *testing.T) {
	f := newRollbackFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Previous container is running (no start needed). Health
	// check succeeds. Caddy validate succeeds but cancels the
	// caller context. Caddy reload then fails. The rollback must
	// best-effort clean up via a bounded recovery context that
	// is NOT cancelled.
	docker := dockerRunning(f.previousContainerName)
	// Register a response for the cleanup docker rm so the
	// rollback can run its best-effort cleanup.
	docker.responses = append(docker.responses,
		fakeDockerEntry{match: matchDockerRm(f.previousContainerName), resp: dockerResponse{}},
	)
	caddyInner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", f.cfg.Caddy.RootConfigPath), resp: caddyResponse{}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", f.cfg.Caddy.RootConfigPath), resp: caddyResponse{err: errors.New("reload failed")}},
	)
	caddy := &cancellingCaddyRunner{inner: caddyInner, cancel: cancel}

	err := rollbackDeployment(ctx, f.cfg, rollbackDeps{docker: docker, caddy: caddy})
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}

	// Every docker call must have used a non-cancelled context.
	// The cleanup rm runs under the bounded recovery context so
	// it is not affected by the caller cancellation. We check the
	// error recorded at the moment of the call (not the current
	// context state, which may have been mutated by the
	// rollback's own cleanup calls).
	for i, err := range docker.CallErrors() {
		if err != nil {
			t.Errorf("docker call %d used a cancelled context: %v", i, err)
		}
	}

	// State must be unchanged.
	state, _ := LoadDeploymentState(f.cfg.State, "myapp")
	if state.Current.Commit != f.current.Commit {
		t.Errorf("state changed: current.commit = %q, want %q", state.Current.Commit, f.current.Commit)
	}
}

func TestRollbackDeployment_RequiresConfig(t *testing.T) {
	f := newRollbackFixture(t)

	// Missing HealthPath.
	bad := f.cfg
	bad.HealthPath = ""
	if err := rollbackDeployment(context.Background(), bad, rollbackDeps{docker: dockerRunning(f.previousContainerName), caddy: healthyCaddy(f.cfg.Caddy.RootConfigPath)}); !errors.Is(err, ErrRollbackFailed) {
		t.Errorf("expected ErrRollbackFailed for missing HealthPath, got %v", err)
	}

	// Invalid BaseDomain in State.
	bad = f.cfg
	bad.State.BaseDomain = "no-tld"
	if err := rollbackDeployment(context.Background(), bad, rollbackDeps{docker: dockerRunning(f.previousContainerName), caddy: healthyCaddy(f.cfg.Caddy.RootConfigPath)}); !errors.Is(err, ErrRollbackFailed) {
		t.Errorf("expected ErrRollbackFailed for invalid base domain, got %v", err)
	}
}
