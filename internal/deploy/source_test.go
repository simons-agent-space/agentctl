package deploy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupRemote creates a local non-bare Git repository to act as the
// trusted remote. It contains a `main` branch with one commit and a
// `feature` branch with one additional commit. Returns the remote path
// (suitable for cfg.OriginURL), the main SHA, and the feature SHA.
func setupRemote(t *testing.T) (remotePath, mainSHA, featureSHA string) {
	t.Helper()
	dir := t.TempDir()
	remotePath = filepath.Join(dir, "remote")
	if err := os.MkdirAll(remotePath, 0o755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}

	runCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = remotePath
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), remotePath, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runCmd("init", "-q", "-b", "main", remotePath)
	runCmd("config", "user.email", "test@example.com")
	runCmd("config", "user.name", "Test User")
	runCmd("commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA = runCmd("rev-parse", "HEAD")

	runCmd("checkout", "-q", "-b", "feature")
	runCmd("commit", "--allow-empty", "-q", "-m", "feature commit")
	featureSHA = runCmd("rev-parse", "HEAD")
	runCmd("checkout", "-q", "main")

	return remotePath, mainSHA, featureSHA
}

func newConfig(t *testing.T, originURL string) SourceConfig {
	t.Helper()
	return SourceConfig{
		AllowedOrg:     "myorg",
		RepositoryRoot: t.TempDir(),
		OriginURL:      originURL,
	}
}

// mirrorPath returns the trusted mirror path for the given config and repo.
func mirrorPath(cfg SourceConfig, repo string) string {
	return filepath.Join(cfg.RepositoryRoot, repo+".git")
}

func TestCheckoutSource_ValidSHA(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	result, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *result)

	if result.Organisation != "myorg" || result.Repository != "myrepo" || result.Commit != mainSHA {
		t.Errorf("unexpected result: %+v", result)
	}
	if result.MirrorPath != mirrorPath(cfg, "myrepo") {
		t.Errorf("MirrorPath = %q, want %q", result.MirrorPath, mirrorPath(cfg, "myrepo"))
	}
}

func TestCheckoutSource_ShortSHARejected(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	shortSHA := mainSHA[:39]
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", shortSHA); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for short SHA, got %v", err)
	}
}

func TestCheckoutSource_UppercaseSHARejected(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	upperSHA := strings.ToUpper(mainSHA)
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", upperSHA); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for uppercase SHA, got %v", err)
	}
}

func TestCheckoutSource_NonHexSHARejected(t *testing.T) {
	remote, _, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	nonHex := strings.Repeat("z", 40)
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", nonHex); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for non-hex SHA, got %v", err)
	}
}

func TestCheckoutSource_InvalidRepoNameRejected(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "MyRepo", mainSHA); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for invalid repo name, got %v", err)
	}
}

func TestCheckoutSource_RepoOriginMismatchRejected(t *testing.T) {
	remote1, mainSHA, _ := setupRemote(t)
	remote2, _, _ := setupRemote(t)
	cfg := newConfig(t, remote1)

	// First checkout creates the mirror pointing at remote1.
	first, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *first)

	// Repoint the existing mirror's origin at a different remote.
	cmd := exec.Command("git", "-C", first.MirrorPath, "remote", "set-url", "origin", remote2)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set-url: %v: %s", err, out)
	}

	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA); !errors.Is(err, ErrRepoMismatch) {
		t.Errorf("expected ErrRepoMismatch, got %v", err)
	}
}

func TestCheckoutSource_NonBareMirrorRejected(t *testing.T) {
	cfg := newConfig(t, "file:///nonexistent")

	// Pre-create a non-bare repository at the expected mirror path.
	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatalf("mkdir mirror: %v", err)
	}
	cmd := exec.Command("git", "init", "-q", mirror)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", strings.Repeat("a", 40)); !errors.Is(err, ErrNotBare) {
		t.Errorf("expected ErrNotBare, got %v", err)
	}
}

func TestCheckoutSource_MissingCommitRejected(t *testing.T) {
	remote, _, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	bogusSHA := strings.Repeat("0", 40)
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", bogusSHA); !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

func TestCheckoutSource_NonCommitObjectRejected(t *testing.T) {
	remote, _, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	// Run a first checkout so the bare mirror is created; the tree SHA
	// is then resolvable in the mirror.
	first, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mustCommit(t, remote))
	if err != nil {
		t.Fatalf("setup checkout: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *first)

	cmd := exec.Command("git", "-C", first.MirrorPath, "rev-parse", "HEAD^{tree}")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("get tree: %v", err)
	}
	treeSHA := strings.TrimSpace(string(out))

	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", treeSHA); !errors.Is(err, ErrNotCommitObject) {
		t.Errorf("expected ErrNotCommitObject, got %v", err)
	}
}

func TestCheckoutSource_CommitOnMainAccepted(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	result, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *result)
}

func TestCheckoutSource_CommitNotReachableFromMainRejected(t *testing.T) {
	remote, _, featureSHA := setupRemote(t)
	cfg := newConfig(t, remote)

	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", featureSHA); !errors.Is(err, ErrUnreachableCommit) {
		t.Errorf("expected ErrUnreachableCommit, got %v", err)
	}
}

func TestCheckoutSource_DetachedCheckoutPointsAtCommit(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	result, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *result)

	cmd := exec.Command("git", "-C", result.CheckoutPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if strings.TrimSpace(string(out)) != mainSHA {
		t.Errorf("HEAD does not match expected commit")
	}
}

func TestCheckoutSource_OriginURLRequired(t *testing.T) {
	cfg := SourceConfig{
		AllowedOrg:     "myorg",
		RepositoryRoot: t.TempDir(),
		// OriginURL intentionally omitted.
	}
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", strings.Repeat("a", 40)); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing OriginURL, got %v", err)
	}
}

func TestCleanupCheckout_AllowsReuseAfterCleanup(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	first, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	if err := CleanupCheckout(context.Background(), cfg, *first); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	if _, err := os.Stat(first.CheckoutPath); err == nil {
		t.Fatalf("checkout still exists after cleanup")
	}

	second, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("second checkout after cleanup: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *second)
	if second.CheckoutPath != first.CheckoutPath {
		t.Errorf("second checkout path = %q, want %q", second.CheckoutPath, first.CheckoutPath)
	}
}

func TestCleanupCheckout_RejectsCheckoutPathEqualToRoot(t *testing.T) {
	cfg := newConfig(t, "file:///nonexistent")
	commit := strings.Repeat("a", 40)

	// Verify the root is a real directory before the call so we can
	// assert afterwards that it has not been deleted.
	if _, err := os.Stat(cfg.RepositoryRoot); err != nil {
		t.Fatalf("repository root missing before test: %v", err)
	}

	bad := SourceResult{
		Repository:   "myrepo",
		Commit:       commit,
		CheckoutPath: cfg.RepositoryRoot,
	}
	if err := CleanupCheckout(context.Background(), cfg, bad); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for CheckoutPath == RepositoryRoot, got %v", err)
	}
	if _, err := os.Stat(cfg.RepositoryRoot); err != nil {
		t.Errorf("repository root was deleted by CleanupCheckout: %v", err)
	}
}

func TestCleanupCheckout_RejectsDifferentCheckoutPath(t *testing.T) {
	cfg := newConfig(t, "file:///nonexistent")
	commit := strings.Repeat("a", 40)

	// Pre-create a path inside the root that is not the derived
	// <root>/<repo>-checkouts/<commit> path.
	bogusPath := filepath.Join(cfg.RepositoryRoot, "not-the-real-checkout")
	if err := os.MkdirAll(bogusPath, 0o755); err != nil {
		t.Fatalf("mkdir bogus: %v", err)
	}

	bad := SourceResult{
		Repository:   "myrepo",
		Commit:       commit,
		CheckoutPath: bogusPath,
	}
	if err := CleanupCheckout(context.Background(), cfg, bad); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for different checkout path, got %v", err)
	}
	if _, err := os.Stat(bogusPath); err != nil {
		t.Errorf("alleged checkout path was deleted despite rejection: %v", err)
	}
}

func TestCleanupCheckout_RejectsPathTraversalRepositoryName(t *testing.T) {
	cfg := newConfig(t, "file:///nonexistent")
	commit := strings.Repeat("a", 40)

	cases := []struct {
		name string
		repo string
	}{
		{"empty", ""},
		{"uppercase-prefix", "Myrepo"},
		{"path-traversal-dotdot", "../../../etc"},
		{"path-traversal-absolute", "/etc/passwd"},
		{"trailing-dash", "myrepo-"},
		{"uppercase-infix", "myREPO"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := SourceResult{
				Repository:   tc.repo,
				Commit:       commit,
				CheckoutPath: filepath.Join(cfg.RepositoryRoot, tc.repo, "checkouts", commit),
			}
			if err := CleanupCheckout(context.Background(), cfg, bad); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput for repository %q, got %v", tc.repo, err)
			}
		})
	}
}

func TestCleanupCheckout_RejectsInvalidCommitSHA(t *testing.T) {
	cfg := newConfig(t, "file:///nonexistent")

	cases := []struct {
		name   string
		commit string
	}{
		{"empty", ""},
		{"too-short", "abc123"},
		{"too-long", strings.Repeat("a", 41)},
		{"uppercase", strings.Repeat("A", 40)},
		{"non-hex", strings.Repeat("z", 40)},
		{"mixed-case", strings.Repeat("a", 20) + strings.Repeat("A", 20)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := SourceResult{
				Repository:   "myrepo",
				Commit:       tc.commit,
				CheckoutPath: filepath.Join(cfg.RepositoryRoot, "myrepo-checkouts", tc.commit),
			}
			if err := CleanupCheckout(context.Background(), cfg, bad); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput for commit %q, got %v", tc.commit, err)
			}
		})
	}
}

func TestCleanupCheckout_RefusesPathsOutsideRoot(t *testing.T) {
	cfg := newConfig(t, "file:///nonexistent")
	commit := strings.Repeat("a", 40)
	outside := "/this-path-does-not-exist-and-is-outside-the-temp-root"

	bad := SourceResult{
		Repository:   "myrepo",
		Commit:       commit,
		CheckoutPath: outside,
	}
	if err := CleanupCheckout(context.Background(), cfg, bad); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for path outside root, got %v", err)
	}
}

func mustCommit(t *testing.T, remote string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", remote, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}
