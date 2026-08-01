package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeResponse captures one canned (out, err) pair for a command.
type fakeResponse struct {
	out string
	err error
}

// fakeRunner records every command invocation and returns canned
// responses that match a command/argument shape.
type fakeRunner struct {
	mu        sync.Mutex
	calls     [][]string
	responses []fakeResponseEntry
}

type fakeResponseEntry struct {
	match func(name string, args []string) bool
	resp  fakeResponse
}

func newFakeRunner(entries ...fakeResponseEntry) *fakeRunner {
	return &fakeRunner{responses: entries}
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := append([]string{name}, args...)
	f.calls = append(f.calls, append([]string(nil), full...))
	for _, e := range f.responses {
		if e.match(name, args) {
			return e.resp.out, e.resp.err
		}
	}
	return "", fmt.Errorf("fakeRunner: unexpected command: %v", full)
}

func (f *fakeRunner) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func match(name string, args ...string) func(string, []string) bool {
	return func(n string, a []string) bool {
		if n != name {
			return false
		}
		if len(a) != len(args) {
			return false
		}
		for i, want := range args {
			if a[i] != want {
				return false
			}
		}
		return true
	}
}

func matchAny(name string) func(string, []string) bool {
	return func(n string, a []string) bool { return n == name }
}

// setupValidCheckout creates a temp RepositoryRoot, then creates the
// checkout at the exact expected derived path <root>/<repo>-checkouts/<commit>
// with a real Dockerfile. Returns the absolute checkout path and a
// matching RuntimeConfig.
func setupValidCheckout(t *testing.T) (string, RuntimeConfig) {
	t.Helper()
	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	checkout := filepath.Join(root, "myapp-checkouts", commit)
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("mkdir checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	cfg := RuntimeConfig{
		PortRangeStart: 49152,
		PortRangeEnd:   49200,
		HealthTimeout:  3 * time.Second,
		RepositoryRoot: root,
	}
	return checkout, cfg
}

// runtimeSource returns a SourceResult matching the given checkout path.
func runtimeSource(checkout string) SourceResult {
	return SourceResult{
		Organisation: "myorg",
		Repository:   "myapp",
		Commit:       strings.Repeat("a", 40),
		CheckoutPath: checkout,
		MirrorPath:   "/some/mirror",
	}
}

// runtimeManifest returns a Manifest valid for runtime tests.
func runtimeManifest() Manifest {
	return Manifest{
		Version:       1,
		App:           "myapp",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
}

// freePort returns one available port on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// freePortRange returns an inclusive range of two consecutive free
// ports on 127.0.0.1, ordered so start <= end.
func freePortRange(t *testing.T) (int, int) {
	t.Helper()
	a := freePort(t)
	b := freePort(t)
	for b == a {
		b = freePort(t)
	}
	if a > b {
		a, b = b, a
	}
	return a, b
}

func TestValidateRuntimeConfig_RejectsBadPortRanges(t *testing.T) {
	checkout, cfg := setupValidCheckout(t)
	cases := []struct {
		name string
		mod  func(*RuntimeConfig)
	}{
		{"start-too-low", func(c *RuntimeConfig) { c.PortRangeStart = 80 }},
		{"end-too-high", func(c *RuntimeConfig) { c.PortRangeEnd = 70000 }},
		{"start-greater-than-end", func(c *RuntimeConfig) { c.PortRangeStart, c.PortRangeEnd = c.PortRangeEnd, c.PortRangeStart }},
		{"zero-health-timeout", func(c *RuntimeConfig) { c.HealthTimeout = 0 }},
		{"missing-repository-root", func(c *RuntimeConfig) { c.RepositoryRoot = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.mod(&cfg)
			_, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), newFakeRunner())
			if !errors.Is(err, ErrInvalidRuntimeConfig) {
				t.Errorf("expected ErrInvalidRuntimeConfig, got %v", err)
			}
		})
	}
}

func TestStartCandidate_MissingDockerfileRejected(t *testing.T) {
	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	checkout := filepath.Join(root, "myapp-checkouts", commit)
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := RuntimeConfig{PortRangeStart: 49152, PortRangeEnd: 49200, HealthTimeout: time.Second, RepositoryRoot: root}
	_, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), newFakeRunner())
	if !errors.Is(err, ErrDockerfileMissing) {
		t.Errorf("expected ErrDockerfileMissing, got %v", err)
	}
}

func TestStartCandidate_SymlinkedDockerfileRejected(t *testing.T) {
	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	checkout := filepath.Join(root, "myapp-checkouts", commit)
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	real := filepath.Join(checkout, "real-dockerfile")
	if err := os.WriteFile(real, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.Symlink(real, filepath.Join(checkout, "Dockerfile")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := RuntimeConfig{PortRangeStart: 49152, PortRangeEnd: 49200, HealthTimeout: time.Second, RepositoryRoot: root}
	_, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), newFakeRunner())
	if !errors.Is(err, ErrDockerfileSymlink) {
		t.Errorf("expected ErrDockerfileSymlink, got %v", err)
	}
}

func TestDeriveImageAndContainerName(t *testing.T) {
	commit := strings.Repeat("a", 40)
	img := deriveImage("myapp", commit)
	if img != "agentctl/myapp:"+commit {
		t.Errorf("image = %q", img)
	}
	cn := deriveContainerName("myapp", commit)
	if cn != "agentctl-myapp-"+commit[:12] {
		t.Errorf("container = %q", cn)
	}
}

func TestAllocatePort_Available(t *testing.T) {
	start, end := freePortRange(t)
	got, err := defaultAllocatePort(start, end)
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	if got < start || got > end {
		t.Errorf("port %d not in range [%d, %d]", got, start, end)
	}
}

func TestAllocatePort_Exhausted(t *testing.T) {
	// Use an invalid (descending) range. The function should refuse
	// without binding anything. This exercises the "no port available"
	// branch deterministically without depending on kernel-level
	// behaviour for occupied ports, which varies across sandboxes.
	if _, err := defaultAllocatePort(50000, 49999); !errors.Is(err, ErrNoAvailablePort) {
		t.Errorf("expected ErrNoAvailablePort, got %v", err)
	}
}

// withFixedPort overrides allocatePortFunc for the duration of a test
// so integration tests can drive startCandidate without binding real
// listeners (which would conflict with the test HTTP servers).
func withFixedPort(t *testing.T, port int) {
	t.Helper()
	old := allocatePortFunc
	allocatePortFunc = func(start, end int) (int, error) {
		return port, nil
	}
	t.Cleanup(func() { allocatePortFunc = old })
}

func TestStartCandidate_DockerBuildUsesExactCheckout(t *testing.T) {
	checkout, cfg := setupValidCheckout(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	var srvPort int
	fmt.Sscanf(srv.URL[len("http://127.0.0.1:"):], "%d", &srvPort)
	cfg.PortRangeStart = srvPort
	cfg.PortRangeEnd = srvPort
	withFixedPort(t, srvPort)

	image := deriveImage("myapp", strings.Repeat("a", 40))
	runner := newFakeRunner(
		fakeResponseEntry{match: match("docker", "build", "--pull", "--tag", image, checkout), resp: fakeResponse{out: ""}},
		fakeResponseEntry{match: matchAny("docker"), resp: fakeResponse{out: ""}},
	)
	if _, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), runner); err != nil {
		t.Fatalf("startCandidate: %v", err)
	}

	builds := 0
	for _, c := range runner.Calls() {
		if len(c) >= 2 && c[0] == "docker" && c[1] == "build" {
			builds++
			if c[len(c)-1] != checkout {
				t.Errorf("docker build context = %q, want %q", c[len(c)-1], checkout)
			}
		}
	}
	if builds != 1 {
		t.Errorf("expected exactly one docker build, got %d", builds)
	}
}

func TestStartCandidate_BuildFailurePreventsContainerStart(t *testing.T) {
	checkout, cfg := setupValidCheckout(t)
	runner := newFakeRunner(
		fakeResponseEntry{
			match: matchAny("docker"),
			resp:  fakeResponse{out: "build error output", err: errors.New("exit 1")},
		},
	)
	_, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), runner)
	if !errors.Is(err, ErrImageBuildFailed) {
		t.Errorf("expected ErrImageBuildFailed, got %v", err)
	}
	for _, c := range runner.Calls() {
		if len(c) >= 2 && c[0] == "docker" && c[1] == "run" {
			t.Errorf("docker run was called after build failure: %v", c)
		}
	}
}

func TestStartCandidate_RequiredDockerRestrictionsPresent(t *testing.T) {
	checkout, cfg := setupValidCheckout(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	var srvPort int
	fmt.Sscanf(srv.URL[len("http://127.0.0.1:"):], "%d", &srvPort)
	cfg.PortRangeStart = srvPort
	cfg.PortRangeEnd = srvPort
	withFixedPort(t, srvPort)

	runner := newFakeRunner(fakeResponseEntry{match: matchAny("docker"), resp: fakeResponse{out: ""}})
	if _, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), runner); err != nil {
		t.Fatalf("startCandidate: %v", err)
	}

	var runCall []string
	for _, c := range runner.Calls() {
		if len(c) >= 2 && c[0] == "docker" && c[1] == "run" {
			runCall = c
			break
		}
	}
	if runCall == nil {
		t.Fatal("docker run was not called")
	}

	joined := strings.Join(runCall, " ")
	required := []string{
		"--detach",
		"--restart unless-stopped",
		"--memory 256m",
		"--cpus 0.5",
		"--pids-limit 128",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		fmt.Sprintf("--publish 127.0.0.1:%d:8080", srvPort),
	}
	for _, want := range required {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run missing %q in %v", want, runCall)
		}
	}

	forbidden := []string{"--privileged", "--network host", "--cap-add", "/var/run/docker.sock"}
	for _, bad := range forbidden {
		if strings.Contains(joined, bad) {
			t.Errorf("docker run contains forbidden %q in %v", bad, runCall)
		}
	}
}

func TestStartCandidate_PortBindingFailureTriesNextPort(t *testing.T) {
	checkout, cfg := setupValidCheckout(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	var srvPort int
	fmt.Sscanf(srv.URL[len("http://127.0.0.1:"):], "%d", &srvPort)
	cfg.PortRangeStart = srvPort
	cfg.PortRangeEnd = srvPort + 2

	calls := 0
	old := allocatePortFunc
	allocatePortFunc = func(start, end int) (int, error) {
		calls++
		return srvPort + calls - 1, nil
	}
	t.Cleanup(func() { allocatePortFunc = old })

	attempt := 0
	runner := newFakeRunner(
		fakeResponseEntry{
			match: func(name string, args []string) bool {
				if name != "docker" || len(args) < 1 || args[0] != "run" {
					return false
				}
				attempt++
				return attempt == 1
			},
			resp: fakeResponse{out: "bind: address already in use", err: errors.New("exit 125")},
		},
		fakeResponseEntry{match: matchAny("docker"), resp: fakeResponse{out: ""}},
	)
	_, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), runner)
	// The health check is expected to fail because the HTTP server is
	// on the first port, not the retry port. What matters for this
	// test is that the retry happened: attempt must be >= 2.
	if attempt < 2 {
		t.Errorf("expected at least 2 docker run attempts (port retry), got %d", attempt)
	}
	if err != nil && !errors.Is(err, ErrHealthCheckFailed) {
		t.Errorf("unexpected startCandidate error: %v", err)
	}
}

func TestStartCandidate_ContainerAlreadyRunningRejected(t *testing.T) {
	checkout, cfg := setupValidCheckout(t)
	commit := strings.Repeat("a", 40)
	containerName := deriveContainerName("myapp", commit)

	runner := newFakeRunner(
		fakeResponseEntry{
			match: match("docker", "inspect", "--format", "{{.State.Running}}", containerName),
			resp:  fakeResponse{out: "true\n"},
		},
		fakeResponseEntry{match: matchAny("docker"), resp: fakeResponse{out: ""}},
	)
	_, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), runner)
	if !errors.Is(err, ErrContainerRunning) {
		t.Errorf("expected ErrContainerRunning, got %v", err)
	}
}

func TestPollHealth_Healthy200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := pollHealth(context.Background(), srv.URL, 2*time.Second); err != nil {
		t.Errorf("expected healthy, got %v", err)
	}
}

func TestPollHealth_Other2xx(t *testing.T) {
	for _, code := range []int{201, 202, 204, 299} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			if err := pollHealth(context.Background(), srv.URL, 2*time.Second); err != nil {
				t.Errorf("status %d: expected healthy, got %v", code, err)
			}
		})
	}
}

func TestPollHealth_3xx4xx5xxNotHealthy(t *testing.T) {
	for _, code := range []int{301, 302, 400, 404, 500} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			if err := pollHealth(context.Background(), srv.URL, 200*time.Millisecond); err == nil {
				t.Errorf("status %d: expected not healthy, got nil", code)
			}
		})
	}
}

func TestPollHealth_DoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com", http.StatusFound)
	}))
	defer srv.Close()
	if err := pollHealth(context.Background(), srv.URL, 200*time.Millisecond); err == nil {
		t.Errorf("redirect: expected not healthy, got nil")
	}
}

func TestPollHealth_ContextCancellationStopsPolling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := pollHealth(ctx, srv.URL, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestStartCandidate_HealthTimeoutRemovesCandidate(t *testing.T) {
	checkout, cfg := setupValidCheckout(t)
	cfg.HealthTimeout = 200 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	var srvPort int
	fmt.Sscanf(srv.URL[len("http://127.0.0.1:"):], "%d", &srvPort)
	cfg.PortRangeStart = srvPort
	cfg.PortRangeEnd = srvPort
	withFixedPort(t, srvPort)

	runner := newFakeRunner(fakeResponseEntry{match: matchAny("docker"), resp: fakeResponse{out: ""}})
	_, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), runner)
	if !errors.Is(err, ErrHealthCheckFailed) {
		t.Errorf("expected ErrHealthCheckFailed, got %v", err)
	}
	calls := runner.Calls()
	removed := false
	for _, c := range calls {
		if len(c) >= 4 && c[0] == "docker" && c[1] == "rm" && c[2] == "--force" {
			removed = true
		}
	}
	if !removed {
		t.Errorf("expected docker rm --force after health timeout, calls=%v", calls)
	}
}

func TestRemoveCandidate_DerivesAndValidatesIdentity(t *testing.T) {
	checkout, cfg := setupValidCheckout(t)
	commit := strings.Repeat("a", 40)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	var srvPort int
	fmt.Sscanf(srv.URL[len("http://127.0.0.1:"):], "%d", &srvPort)
	cfg.PortRangeStart = srvPort
	cfg.PortRangeEnd = srvPort
	withFixedPort(t, srvPort)

	runner := newFakeRunner(fakeResponseEntry{match: matchAny("docker"), resp: fakeResponse{out: ""}})
	first, err := startCandidate(context.Background(), cfg, runtimeManifest(), runtimeSource(checkout), runner)
	if err != nil {
		t.Fatalf("startCandidate: %v", err)
	}
	if first.App != "myapp" || first.Commit != commit {
		t.Errorf("unexpected first result: %+v", first)
	}
	if first.Image != deriveImage("myapp", commit) {
		t.Errorf("image = %q", first.Image)
	}
	if first.ContainerName != deriveContainerName("myapp", commit) {
		t.Errorf("container = %q", first.ContainerName)
	}

	fabricated := *first
	fabricated.ContainerName = "evil-name"
	if err := removeCandidate(context.Background(), fabricated, newFakeRunner()); !errors.Is(err, ErrInvalidCandidate) {
		t.Errorf("fabricated container: expected ErrInvalidCandidate, got %v", err)
	}
	fabricated = *first
	fabricated.Image = "agentctl/myapp:deadbeef"
	if err := removeCandidate(context.Background(), fabricated, newFakeRunner()); !errors.Is(err, ErrInvalidCandidate) {
		t.Errorf("fabricated image: expected ErrInvalidCandidate, got %v", err)
	}
	fabricated = *first
	fabricated.App = "Bad-App"
	if err := removeCandidate(context.Background(), fabricated, newFakeRunner()); !errors.Is(err, ErrInvalidCandidate) {
		t.Errorf("invalid app: expected ErrInvalidCandidate, got %v", err)
	}
	fabricated = *first
	fabricated.Commit = strings.Repeat("z", 40)
	if err := removeCandidate(context.Background(), fabricated, newFakeRunner()); !errors.Is(err, ErrInvalidCandidate) {
		t.Errorf("invalid commit: expected ErrInvalidCandidate, got %v", err)
	}
}

func TestRemoveCandidate_IdempotentWhenAbsent(t *testing.T) {
	commit := strings.Repeat("a", 40)
	candidate := CandidateResult{
		App:           "myapp",
		Commit:        commit,
		Image:         deriveImage("myapp", commit),
		ContainerName: deriveContainerName("myapp", commit),
	}
	runner := newFakeRunner(
		fakeResponseEntry{
			match: matchAny("docker"),
			resp:  fakeResponse{out: "Error: No such container: " + candidate.ContainerName, err: errors.New("exit 1")},
		},
	)
	if err := removeCandidate(context.Background(), candidate, runner); err != nil {
		t.Errorf("expected idempotent nil, got %v", err)
	}
}
