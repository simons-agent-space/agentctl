package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// caddyResponse matches a caddy subcommand invocation.
type caddyResponse struct {
	out string
	err error
}

// fakeCaddyRunner records every caddy invocation and returns canned
// (out, err) pairs based on the subcommand.
type fakeCaddyRunner struct {
	mu        sync.Mutex
	calls     [][]string
	responses []fakeCaddyEntry
}

type fakeCaddyEntry struct {
	match func(args []string) bool
	resp  caddyResponse
}

func newFakeCaddyRunner(entries ...fakeCaddyEntry) *fakeCaddyRunner {
	return &fakeCaddyRunner{responses: entries}
}

func (f *fakeCaddyRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := append([]string{name}, args...)
	f.calls = append(f.calls, append([]string(nil), full...))
	for _, e := range f.responses {
		if e.match(args) {
			return e.resp.out, e.resp.err
		}
	}
	return "", fmt.Errorf("fakeCaddyRunner: unexpected command: %v", full)
}

func (f *fakeCaddyRunner) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func matchCaddy(args ...string) func([]string) bool {
	return func(a []string) bool {
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

// validCandidate returns a CandidateResult that satisfies all
// identity checks for the given app+commit.
func validCandidate(app, commit string, hostPort int) CandidateResult {
	return CandidateResult{
		App:           app,
		Commit:        commit,
		Image:         deriveImage(app, commit),
		ContainerName: deriveContainerName(app, commit),
		HostPort:      hostPort,
		ContainerPort: 8080,
		HealthURL:     fmt.Sprintf("http://127.0.0.1:%d/healthz", hostPort),
	}
}

func defaultCaddyConfig(t *testing.T) CaddyConfig {
	t.Helper()
	return CaddyConfig{
		BaseDomain:  "apps.simonontheweb.de",
		ConfigDir:   t.TempDir(),
		CaddyBinary: "caddy",
	}
}

func TestPromote_ValidPromotion(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)

	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", filepath.Join(cfg.ConfigDir, "myapp.caddy.tmp")), resp: caddyResponse{}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", filepath.Join(cfg.ConfigDir, "myapp.caddy.tmp")), resp: caddyResponse{}},
	)

	result, err := promote(context.Background(), cfg, candidate, runner)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if result.App != "myapp" || result.Commit != commit {
		t.Errorf("unexpected result: %+v", result)
	}
	if result.Hostname != "myapp.apps.simonontheweb.de" {
		t.Errorf("hostname = %q", result.Hostname)
	}
	if result.Upstream != "127.0.0.1:49152" {
		t.Errorf("upstream = %q", result.Upstream)
	}
	expectedPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")
	if result.ConfigPath != expectedPath {
		t.Errorf("config path = %q, want %q", result.ConfigPath, expectedPath)
	}

	body, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	want := "myapp.apps.simonontheweb.de {\n\treverse_proxy 127.0.0.1:49152\n}\n"
	if string(body) != want {
		t.Errorf("config body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func TestPromote_FabricatedCandidateRejected(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)

	cases := []struct {
		name    string
		mutate  func(*CandidateResult)
		wantErr error
	}{
		{"empty-app", func(c *CandidateResult) { c.App = "" }, ErrInvalidCandidate},
		{"bad-app", func(c *CandidateResult) { c.App = "MyApp" }, ErrInvalidCandidate},
		{"path-traversal-app", func(c *CandidateResult) { c.App = "../../etc" }, ErrInvalidCandidate},
		{"empty-commit", func(c *CandidateResult) { c.Commit = "" }, ErrInvalidCandidate},
		{"wrong-image", func(c *CandidateResult) { c.Image = "agentctl/myapp:deadbeef" }, ErrInvalidCandidate},
		{"wrong-container", func(c *CandidateResult) { c.ContainerName = "evil" }, ErrInvalidCandidate},
		{"bad-port", func(c *CandidateResult) { c.HostPort = 0 }, ErrInvalidCandidate},
		{"no-health-url", func(c *CandidateResult) { c.HealthURL = "" }, ErrInvalidCandidate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := validCandidate("myapp", commit, 49152)
			tc.mutate(&candidate)
			runner := newFakeCaddyRunner()
			_, err := promote(context.Background(), cfg, candidate, runner)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestPromote_InvalidDomainRejected(t *testing.T) {
	cases := []string{
		"",
		"no-tld",
		"has space.com",
		"has\tnewline.com",
		"-leading.com",
		"trailing-.com",
	}
	for _, d := range cases {
		t.Run(fmt.Sprintf("domain=%q", d), func(t *testing.T) {
			cfg := defaultCaddyConfig(t)
			cfg.BaseDomain = d
			candidate := validCandidate("myapp", strings.Repeat("a", 40), 49152)
			runner := newFakeCaddyRunner()
			_, err := promote(context.Background(), cfg, candidate, runner)
			if !errors.Is(err, ErrInvalidDomain) && !errors.Is(err, ErrInvalidCaddyConfig) {
				t.Errorf("expected ErrInvalidDomain or ErrInvalidCaddyConfig, got %v", err)
			}
		})
	}
}

func TestPromote_AtomicWrite(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)

	tempPath := filepath.Join(cfg.ConfigDir, "myapp.caddy.tmp")
	finalPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")

	// Snapshot existing temp at start; the test asserts it is removed
	// after a failed validate, and renamed to final after success.
	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", tempPath), resp: caddyResponse{err: errors.New("bad config")}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", tempPath), resp: caddyResponse{}},
	)

	_, err := promote(context.Background(), cfg, candidate, runner)
	if !errors.Is(err, ErrCaddyValidateFailed) {
		t.Fatalf("expected ErrCaddyValidateFailed, got %v", err)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file should have been removed after validate failure, stat err = %v", err)
	}
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("final file should not exist after validate failure, stat err = %v", err)
	}
}

func TestPromote_ValidationFailurePreservesPreviousConfig(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)
	finalPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")

	previous := []byte("previous contents\n")
	if err := os.WriteFile(finalPath, previous, 0o644); err != nil {
		t.Fatalf("seed previous: %v", err)
	}

	tempPath := filepath.Join(cfg.ConfigDir, "myapp.caddy.tmp")
	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", tempPath), resp: caddyResponse{out: "invalid Caddyfile", err: errors.New("exit 1")}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", tempPath), resp: caddyResponse{}},
	)

	_, err := promote(context.Background(), cfg, candidate, runner)
	if !errors.Is(err, ErrCaddyValidateFailed) {
		t.Fatalf("expected ErrCaddyValidateFailed, got %v", err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != string(previous) {
		t.Errorf("previous config was overwritten:\n--- got ---\n%s\n--- want ---\n%s", got, previous)
	}
}

func TestPromote_ReloadFailurePreservesPreviousConfig(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)
	finalPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")

	previous := []byte("previous contents\n")
	if err := os.WriteFile(finalPath, previous, 0o644); err != nil {
		t.Fatalf("seed previous: %v", err)
	}

	tempPath := filepath.Join(cfg.ConfigDir, "myapp.caddy.tmp")
	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", tempPath), resp: caddyResponse{}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", tempPath), resp: caddyResponse{out: "reload failed", err: errors.New("exit 1")}},
	)

	_, err := promote(context.Background(), cfg, candidate, runner)
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Fatalf("expected ErrCaddyReloadFailed, got %v", err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != string(previous) {
		t.Errorf("previous config was overwritten on reload failure:\n--- got ---\n%s\n--- want ---\n%s", got, previous)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file should have been removed after reload failure, stat err = %v", err)
	}
}

func TestPromote_UpstreamIsExactLocalhost(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)

	tempPath := filepath.Join(cfg.ConfigDir, "myapp.caddy.tmp")
	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", tempPath), resp: caddyResponse{}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", tempPath), resp: caddyResponse{}},
	)

	_, err := promote(context.Background(), cfg, candidate, runner)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(cfg.ConfigDir, "myapp.caddy"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(body), "reverse_proxy 127.0.0.1:49152") {
		t.Errorf("config does not contain the exact localhost upstream: %s", body)
	}
	if strings.Contains(string(body), "0.0.0.0") {
		t.Errorf("config must not bind to 0.0.0.0: %s", body)
	}
}

func TestRemovePromotion_SafeRemoval(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	configPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")
	if err := os.WriteFile(configPath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("reload", "--config", configPath), resp: caddyResponse{}},
	)

	if err := removePromotion(context.Background(), cfg, "myapp", runner); err != nil {
		t.Fatalf("removePromotion: %v", err)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config file should have been removed, stat err = %v", err)
	}
}

func TestRemovePromotion_RejectsInvalidApp(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	cases := []string{"", "MyApp", "../../etc", "myapp-"}
	for _, app := range cases {
		t.Run(fmt.Sprintf("app=%q", app), func(t *testing.T) {
			runner := newFakeCaddyRunner()
			err := removePromotion(context.Background(), cfg, app, runner)
			if !errors.Is(err, ErrInvalidCandidate) {
				t.Errorf("expected ErrInvalidCandidate, got %v", err)
			}
		})
	}
}

func TestRemovePromotion_NotFound(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	runner := newFakeCaddyRunner()
	err := removePromotion(context.Background(), cfg, "myapp", runner)
	if !errors.Is(err, ErrPromotionNotFound) {
		t.Errorf("expected ErrPromotionNotFound, got %v", err)
	}
}

func TestRemovePromotion_DoesNotTouchOtherApps(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	target := filepath.Join(cfg.ConfigDir, "myapp.caddy")
	other := filepath.Join(cfg.ConfigDir, "other.caddy")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.WriteFile(other, []byte("other"), 0o644); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("reload", "--config", target), resp: caddyResponse{}},
	)

	if err := removePromotion(context.Background(), cfg, "myapp", runner); err != nil {
		t.Fatalf("removePromotion: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("other app's config was disturbed: %v", err)
	}
}
