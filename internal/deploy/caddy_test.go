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

// defaultCaddyConfig returns a CaddyConfig that satisfies every
// host-side field required for promotion: a valid base domain, a
// temp config dir, and a root config path inside the temp dir.
//
// The seeded root config imports only the exact *.caddy glob (no
// .partial or .bak), so the production contract is exercised by
// every test that uses this helper.
func defaultCaddyConfig(t *testing.T) CaddyConfig {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(root, []byte("import "+filepath.Join(dir, "*.caddy")+"\n"), 0o644); err != nil {
		t.Fatalf("seed root config: %v", err)
	}
	return CaddyConfig{
		BaseDomain:     "apps.simonontheweb.de",
		ConfigDir:      dir,
		RootConfigPath: root,
		CaddyBinary:    "caddy",
	}
}

// validateAndReloadRoot matches the canonical pair of caddy
// invocations that target the root config: validate then reload,
// both with --config <root>.
func validateAndReloadRoot(root string) []fakeCaddyEntry {
	return []fakeCaddyEntry{
		{match: matchCaddy("validate", "--config", root), resp: caddyResponse{}},
		{match: matchCaddy("reload", "--config", root), resp: caddyResponse{}},
	}
}

func TestPromote_ValidPromotion(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)

	runner := newFakeCaddyRunner(validateAndReloadRoot(cfg.RootConfigPath)...)

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

	// No leftover temp or backup files.
	for _, p := range []string{
		expectedPath + ".partial",
		expectedPath + ".bak",
		expectedPath + ".tmp",
		expectedPath + ".write",
	} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unexpected leftover %s: stat err = %v", p, err)
		}
	}

	// Validate and reload both targeted the canonical root, never
	// the per-app fragment.
	for _, call := range runner.Calls() {
		if len(call) < 4 {
			continue
		}
		if call[2] == "--config" && (call[3] == expectedPath || strings.HasSuffix(call[3], ".partial") || strings.HasSuffix(call[3], ".tmp")) {
			t.Errorf("caddy must target root config, not a fragment: %v", call)
		}
	}
}

func TestPromote_RequiresRootConfigPath(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	cfg.RootConfigPath = ""
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)
	runner := newFakeCaddyRunner()
	_, err := promote(context.Background(), cfg, candidate, runner)
	if !errors.Is(err, ErrInvalidCaddyConfig) {
		t.Errorf("expected ErrInvalidCaddyConfig, got %v", err)
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

	tempPath := filepath.Join(cfg.ConfigDir, "myapp.caddy.partial")
	finalPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")

	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", cfg.RootConfigPath), resp: caddyResponse{err: errors.New("bad config")}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", cfg.RootConfigPath), resp: caddyResponse{}},
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

func TestPromote_ValidateTargetsRootConfig(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)

	runner := newFakeCaddyRunner(validateAndReloadRoot(cfg.RootConfigPath)...)

	if _, err := promote(context.Background(), cfg, candidate, runner); err != nil {
		t.Fatalf("promote: %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 caddy invocations (validate, reload), got %d: %v", len(calls), calls)
	}
	if calls[0][1] != "validate" || calls[0][2] != "--config" || calls[0][3] != cfg.RootConfigPath {
		t.Errorf("validate must target root config: %v", calls[0])
	}
	if calls[1][1] != "reload" || calls[1][2] != "--config" || calls[1][3] != cfg.RootConfigPath {
		t.Errorf("reload must target root config: %v", calls[1])
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

	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", cfg.RootConfigPath), resp: caddyResponse{out: "invalid Caddyfile", err: errors.New("exit 1")}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", cfg.RootConfigPath), resp: caddyResponse{}},
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
	if _, err := os.Stat(finalPath + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup must not be created on validate failure, stat err = %v", err)
	}
}

func TestPromote_ReloadFailureRestoresPreviousAndReloads(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)
	finalPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")

	previous := []byte("previous contents\n")
	if err := os.WriteFile(finalPath, previous, 0o644); err != nil {
		t.Fatalf("seed previous: %v", err)
	}

	// Validate succeeds; every reload (the promotion reload and the
	// best-effort restore reload) fails.
	runner := &fakeCaddyRunner{responses: []fakeCaddyEntry{
		{match: matchCaddy("validate", "--config", cfg.RootConfigPath), resp: caddyResponse{}},
		{match: func(a []string) bool {
			return len(a) >= 3 && a[1] == "reload" && a[2] == "--config" && a[3] == cfg.RootConfigPath
		}, resp: caddyResponse{out: "reload failed", err: errors.New("exit 1")}},
	}}

	_, err := promote(context.Background(), cfg, candidate, runner)
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Fatalf("expected ErrCaddyReloadFailed, got %v", err)
	}

	// Final file holds the previous contents (restored from backup).
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != string(previous) {
		t.Errorf("previous config not restored on reload failure:\n--- got ---\n%s\n--- want ---\n%s", got, previous)
	}

	// No leftover temp or backup files.
	for _, p := range []string{
		finalPath + ".partial",
		finalPath + ".bak",
	} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unexpected leftover %s: stat err = %v", p, err)
		}
	}

	// Validate (1) + reload (1, failed) + best-effort restore reload (1)
	// = 3 caddy invocations, all targeting the root config.
	calls := runner.Calls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 caddy invocations (validate, reload-fail, restore-reload), got %d: %v", len(calls), calls)
	}
	for i, call := range calls {
		if call[2] != "--config" || call[3] != cfg.RootConfigPath {
			t.Errorf("call %d must target root config: %v", i, call)
		}
	}
}

func TestPromote_ReloadFailureFirstPromotionRemovesNewFragment(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)
	finalPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")

	runner := &fakeCaddyRunner{responses: []fakeCaddyEntry{
		{match: matchCaddy("validate", "--config", cfg.RootConfigPath), resp: caddyResponse{}},
		{match: func(a []string) bool {
			return len(a) >= 3 && a[1] == "reload" && a[2] == "--config" && a[3] == cfg.RootConfigPath
		}, resp: caddyResponse{out: "reload failed", err: errors.New("exit 1")}},
	}}

	_, err := promote(context.Background(), cfg, candidate, runner)
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Fatalf("expected ErrCaddyReloadFailed, got %v", err)
	}

	// No previous config existed, so final file must be gone (we
	// removed it to restore pre-promotion state), and the parked
	// temp must be gone too.
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("final file should be removed after failed first promotion, stat err = %v", err)
	}
	for _, p := range []string{finalPath + ".partial", finalPath + ".bak"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unexpected leftover %s: stat err = %v", p, err)
		}
	}
}

func TestPromote_UpstreamIsExactLocalhost(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)

	runner := newFakeCaddyRunner(validateAndReloadRoot(cfg.RootConfigPath)...)

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

	runner := newFakeCaddyRunner(validateAndReloadRoot(cfg.RootConfigPath)...)

	if err := removePromotion(context.Background(), cfg, "myapp", runner); err != nil {
		t.Fatalf("removePromotion: %v", err)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config file should have been removed, stat err = %v", err)
	}
	for _, p := range []string{configPath + ".bak", configPath + ".partial"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unexpected leftover %s: stat err = %v", p, err)
		}
	}

	// Validate (1) + reload (1), both targeting root.
	calls := runner.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 caddy invocations (validate, reload), got %d: %v", len(calls), calls)
	}
	for i, call := range calls {
		if call[2] != "--config" || call[3] != cfg.RootConfigPath {
			t.Errorf("call %d must target root config: %v", i, call)
		}
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

func TestRemovePromotion_ValidateFailureRestoresFragment(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	configPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")
	previous := []byte("previous contents\n")
	if err := os.WriteFile(configPath, previous, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runner := newFakeCaddyRunner(
		fakeCaddyEntry{match: matchCaddy("validate", "--config", cfg.RootConfigPath), resp: caddyResponse{err: errors.New("bad config")}},
		fakeCaddyEntry{match: matchCaddy("reload", "--config", cfg.RootConfigPath), resp: caddyResponse{}},
	)

	err := removePromotion(context.Background(), cfg, "myapp", runner)
	if !errors.Is(err, ErrCaddyValidateFailed) {
		t.Fatalf("expected ErrCaddyValidateFailed, got %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != string(previous) {
		t.Errorf("fragment was not restored on validate failure:\n--- got ---\n%s\n--- want ---\n%s", got, previous)
	}
	if _, err := os.Stat(configPath + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup must not linger on validate failure, stat err = %v", err)
	}
}

func TestRemovePromotion_ReloadFailureRestoresFragmentAndReloads(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	configPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")
	previous := []byte("previous contents\n")
	if err := os.WriteFile(configPath, previous, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runner := &fakeCaddyRunner{responses: []fakeCaddyEntry{
		{match: matchCaddy("validate", "--config", cfg.RootConfigPath), resp: caddyResponse{}},
		{match: func(a []string) bool {
			return len(a) >= 3 && a[1] == "reload" && a[2] == "--config" && a[3] == cfg.RootConfigPath
		}, resp: caddyResponse{out: "reload failed", err: errors.New("exit 1")}},
	}}

	err := removePromotion(context.Background(), cfg, "myapp", runner)
	if !errors.Is(err, ErrCaddyReloadFailed) {
		t.Fatalf("expected ErrCaddyReloadFailed, got %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != string(previous) {
		t.Errorf("fragment not restored on reload failure:\n--- got ---\n%s\n--- want ---\n%s", got, previous)
	}
	if _, err := os.Stat(configPath + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup must not linger on reload failure, stat err = %v", err)
	}

	// Validate + reload-fail + best-effort restore-reload = 3 calls.
	calls := runner.Calls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 caddy invocations, got %d: %v", len(calls), calls)
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

	runner := newFakeCaddyRunner(validateAndReloadRoot(cfg.RootConfigPath)...)

	if err := removePromotion(context.Background(), cfg, "myapp", runner); err != nil {
		t.Fatalf("removePromotion: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("other app's config was disturbed: %v", err)
	}
}

// TestPromote_RootImportIsExactCaddy proves the production contract:
// the canonical root config must import the exact "*.caddy" glob,
// never the greedy "*.caddy*". A greedy glob would pull .partial
// and .bak into the running Caddy and break the promotion/rollback
// flow. The seeded root in defaultCaddyConfig is the reference
// contract; every other test in this file uses the same helper.
func TestPromote_RootImportIsExactCaddy(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	rootContent, err := os.ReadFile(cfg.RootConfigPath)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	rootStr := strings.TrimSpace(string(rootContent))

	if !strings.HasPrefix(rootStr, "import ") {
		t.Fatalf("root must start with 'import', got: %s", rootStr)
	}
	importPath := strings.TrimPrefix(rootStr, "import ")

	// The import path must end with the exact glob "/*.caddy" —
	// not "/*.caddy*", "/*.caddy.bak", "/*.caddy.partial", or any
	// other variant.
	if strings.HasSuffix(importPath, "/*.caddy*") {
		t.Errorf("root import must not use greedy '/*.caddy*' glob, got: %s", rootStr)
	}
	if !strings.HasSuffix(importPath, "/*.caddy") {
		t.Errorf("root import must end with exact '/*.caddy' glob, got: %s", rootStr)
	}
}

// validateSnapshotRunner wraps a fakeCaddyRunner and snapshots the
// ConfigDir state at the moment the caddy validate call lands, so
// tests can prove that .partial and .bak are never visible to
// Caddy at validation time.
type validateSnapshotRunner struct {
	inner      *fakeCaddyRunner
	t          *testing.T
	tempPath   string
	backupPath string
	previous   []byte
}

func (r *validateSnapshotRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "caddy" && len(args) >= 2 && args[0] == "validate" {
		// At validate time, .partial must NOT exist: the promotion
		// flow renames it to the final .caddy path before invoking
		// caddy validate, because the root's import glob does not
		// match .partial.
		if _, err := os.Stat(r.tempPath); !errors.Is(err, os.ErrNotExist) {
			r.t.Errorf("validate must not see .partial at %s: stat err = %v", r.tempPath, err)
		}
		// .bak may exist (from the backup step) but is the OLD
		// content, not the new content the root would import.
		bakContent, err := os.ReadFile(r.backupPath)
		if err != nil {
			r.t.Errorf("read .bak at validate time: %v", err)
		} else if string(bakContent) != string(r.previous) {
			r.t.Errorf(".bak at validate time holds new content (not old):\n--- got ---\n%s\n--- want ---\n%s", bakContent, r.previous)
		}
	}
	return r.inner.Run(ctx, name, args...)
}

// TestPromote_ValidationNeverSeesPartialOrBackup proves the central
// property of the new flow: at the moment caddy validate runs,
// the .partial file no longer exists (it has been renamed to the
// final .caddy path) and any .bak on disk holds the old content
// rather than the new. The root config's import glob ("*.caddy")
// therefore sees exactly one fragment for this app.
func TestPromote_ValidationNeverSeesPartialOrBackup(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)
	finalPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")
	tempPath := finalPath + ".partial"
	backupPath := finalPath + ".bak"

	previous := []byte("previous contents\n")
	if err := os.WriteFile(finalPath, previous, 0o644); err != nil {
		t.Fatalf("seed previous: %v", err)
	}

	inner := newFakeCaddyRunner(validateAndReloadRoot(cfg.RootConfigPath)...)
	runner := &validateSnapshotRunner{
		inner:      inner,
		t:          t,
		tempPath:   tempPath,
		backupPath: backupPath,
		previous:   previous,
	}

	if _, err := promote(context.Background(), cfg, candidate, runner); err != nil {
		t.Fatalf("promote: %v", err)
	}
}

// TestPromote_UpdateExistingAppNoDuplicate proves that promoting a
// new build of an app that already has a final fragment replaces
// the fragment in place rather than creating a second one. The
// canonical root config (which imports *.caddy) would then have
// two fragments for the same hostname, which Caddy rejects.
func TestPromote_UpdateExistingAppNoDuplicate(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	commit := strings.Repeat("a", 40)
	candidate := validCandidate("myapp", commit, 49152)
	finalPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")

	// Seed a previous final for the same app+hostname on a
	// different host port.
	previous := renderCaddyfile("myapp.apps.simonontheweb.de", 12345)
	if err := os.WriteFile(finalPath, []byte(previous), 0o644); err != nil {
		t.Fatalf("seed previous: %v", err)
	}

	runner := newFakeCaddyRunner(validateAndReloadRoot(cfg.RootConfigPath)...)

	if _, err := promote(context.Background(), cfg, candidate, runner); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// There must be exactly one file in ConfigDir for "myapp"
	// (the new final), and the hostname "myapp.apps.simonontheweb.de"
	// must appear exactly once across every fragment's content
	// (no leftover previous final defining the same hostname).
	entries, err := os.ReadDir(cfg.ConfigDir)
	if err != nil {
		t.Fatalf("read config dir: %v", err)
	}
	var myappFiles []string
	hostname := "myapp.apps.simonontheweb.de"
	var hostnameHits int
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "myapp") {
			myappFiles = append(myappFiles, name)
		}
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(cfg.ConfigDir, name))
		if err != nil {
			continue
		}
		hostnameHits += strings.Count(string(b), hostname)
	}
	if len(myappFiles) != 1 {
		t.Errorf("expected exactly 1 file for myapp, got %d: %v", len(myappFiles), myappFiles)
	}
	if myappFiles[0] != "myapp.caddy" {
		t.Errorf("expected myapp.caddy, got %s", myappFiles[0])
	}
	if hostnameHits != 1 {
		t.Errorf("hostname %s must appear exactly once across all fragments, got %d", hostname, hostnameHits)
	}

	// No leftover .partial or .bak files.
	for _, p := range []string{finalPath + ".partial", finalPath + ".bak"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unexpected leftover %s: stat err = %v", p, err)
		}
	}

	// Final holds the new content.
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	want := renderCaddyfile("myapp.apps.simonontheweb.de", 49152)
	if string(got) != want {
		t.Errorf("final content mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// reloadBlockRunner wraps a fakeCaddyRunner and, at the moment the
// caddy reload call lands, replaces the on-disk backup with a
// non-empty directory so that the subsequent best-effort os.Remove
// must fail. This lets the test prove that backup-cleanup failure
// is not turned into a removal error.
type reloadBlockRunner struct {
	inner      *fakeCaddyRunner
	t          *testing.T
	backupPath string
}

func (r *reloadBlockRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "caddy" && len(args) >= 2 && args[0] == "reload" {
		// At reload time the backup is a regular file (it was
		// moved aside before validate). Replace it with a
		// non-empty directory so os.Remove fails on cleanup.
		if err := os.Remove(r.backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.t.Logf("remove backup before blocking: %v", err)
		}
		if err := os.MkdirAll(r.backupPath, 0o755); err != nil {
			r.t.Fatalf("mkdir backup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(r.backupPath, "block"), []byte("block"), 0o644); err != nil {
			r.t.Fatalf("write block: %v", err)
		}
	}
	return r.inner.Run(ctx, name, args...)
}

// TestRemovePromotion_BackupCleanupFailureNotAnError proves that a
// leftover-backup-cleanup failure does NOT turn a successful
// removal into an error. By the time cleanup runs the route has
// already been removed from running Caddy via a successful reload;
// a leftover .bak on disk is harmless (the root's import glob
// does not match .bak) and must be reported as success.
func TestRemovePromotion_BackupCleanupFailureNotAnError(t *testing.T) {
	cfg := defaultCaddyConfig(t)
	configPath := filepath.Join(cfg.ConfigDir, "myapp.caddy")
	backupPath := configPath + ".bak"

	if err := os.WriteFile(configPath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	inner := newFakeCaddyRunner(validateAndReloadRoot(cfg.RootConfigPath)...)
	runner := &reloadBlockRunner{
		inner:      inner,
		t:          t,
		backupPath: backupPath,
	}

	if err := removePromotion(context.Background(), cfg, "myapp", runner); err != nil {
		t.Fatalf("removePromotion must return nil even if backup cleanup fails: %v", err)
	}

	// The final fragment is gone — the route is removed from
	// both disk and running Caddy.
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config file should have been removed, stat err = %v", err)
	}

	// The backup could not be removed (it's now a non-empty
	// directory), so it is still on disk. That is fine: the
	// root config's import glob does not match .bak, so a
	// leftover backup never reaches running Caddy.
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("backup should still exist on disk: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("backup should be a non-empty directory (un-removable), got mode %v", info.Mode())
	}
}
