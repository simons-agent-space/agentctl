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

// setupBareMirrorWithRefs creates a local source repo with main and feature
// branches, clones it as a bare mirror, populates refs/remotes/origin/*,
// then renames the mirror to the path expected by CheckoutSource. Returns
// the mirror path, the main commit SHA, and the feature commit SHA.
func setupBareMirrorWithRefs(t *testing.T) (mirrorPath, mainSHA, featureSHA string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}

	runCmd := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runCmd(src, "init", "-q", "-b", "main", src)
	runCmd(src, "config", "user.email", "test@example.com")
	runCmd(src, "config", "user.name", "Test User")
	runCmd(src, "commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA = runCmd(src, "rev-parse", "HEAD")

	runCmd(src, "checkout", "-q", "-b", "feature")
	runCmd(src, "commit", "--allow-empty", "-q", "-m", "feature commit")
	featureSHA = runCmd(src, "rev-parse", "HEAD")
	runCmd(src, "checkout", "-q", "main")

	mirrorPath = filepath.Join(dir, "myrepo.git")
	cmd := exec.Command("git", "clone", "--bare", "-q", src, mirrorPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone bare: %v: %s", err, out)
	}
	// Populate refs/remotes/origin/* from the local source so merge-base works
	// even when the configured origin URL is unreachable (e.g. tests).
	runCmd(mirrorPath, "fetch", src, "+refs/heads/*:refs/remotes/origin/*")

	// Set origin to the expected GitHub URL (string only; no network access).
	runCmd(mirrorPath, "remote", "set-url", "origin", "https://github.com/myorg/myrepo.git")

	return mirrorPath, mainSHA, featureSHA
}

// setupBareMirrorWrongOrigin creates a mirror whose origin points at a
// different local source than expected.
func setupBareMirrorWrongOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src1 := filepath.Join(dir, "src1")
	src2 := filepath.Join(dir, "src2")

	runCmd := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
	}

	for _, d := range []string{src1, src2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		runCmd(d, "init", "-q", "-b", "main", d)
		runCmd(d, "config", "user.email", "test@example.com")
		runCmd(d, "config", "user.name", "Test")
		runCmd(d, "commit", "--allow-empty", "-q", "-m", "c")
	}

	mirror := filepath.Join(dir, "myrepo.git")
	cmd := exec.Command("git", "clone", "--bare", "-q", src1, mirror)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone bare: %v: %s", err, out)
	}
	// Repoint origin at the wrong source.
	runCmd(mirror, "remote", "set-url", "origin", src2)
	return mirror
}

func newConfig(t *testing.T) (SourceConfig, string) {
	t.Helper()
	root := t.TempDir()
	return SourceConfig{
		AllowedOrg:     "myorg",
		RepositoryRoot: root,
		SkipFetch:      true, // tests run offline; refs are already populated
	}, root
}

func TestCheckoutSource_ValidSHA(t *testing.T) {
	mirror, mainSHA, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	expectedMirror := filepath.Join(root, "myrepo.git")
	if err := os.Rename(mirror, expectedMirror); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	result, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}
	defer CleanupCheckout(cfg, result.CheckoutPath)

	if result.Organisation != "myorg" || result.Repository != "myrepo" || result.Commit != mainSHA {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestCheckoutSource_ShortSHARejected(t *testing.T) {
	mirror, mainSHA, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	shortSHA := mainSHA[:39]
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", shortSHA); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for short SHA, got %v", err)
	}
}

func TestCheckoutSource_UppercaseSHARejected(t *testing.T) {
	mirror, mainSHA, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	upperSHA := strings.ToUpper(mainSHA)
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", upperSHA); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for uppercase SHA, got %v", err)
	}
}

func TestCheckoutSource_NonHexSHARejected(t *testing.T) {
	mirror, _, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	nonHex := strings.Repeat("z", 40)
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", nonHex); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for non-hex SHA, got %v", err)
	}
}

func TestCheckoutSource_InvalidRepoNameRejected(t *testing.T) {
	mirror, mainSHA, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "MyRepo", mainSHA); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for invalid repo name, got %v", err)
	}
}

func TestCheckoutSource_RepoOriginMismatchRejected(t *testing.T) {
	mirror := setupBareMirrorWrongOrigin(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", strings.Repeat("a", 40)); !errors.Is(err, ErrRepoMismatch) {
		t.Errorf("expected ErrRepoMismatch, got %v", err)
	}
}

func TestCheckoutSource_MissingCommitRejected(t *testing.T) {
	mirror, _, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	bogusSHA := strings.Repeat("0", 40)
	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", bogusSHA); !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

func TestCheckoutSource_NonCommitObjectRejected(t *testing.T) {
	mirror, _, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	mirrorPath := filepath.Join(root, "myrepo.git")
	if err := os.Rename(mirror, mirrorPath); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	// Get a tree SHA (a tree object, not a commit).
	cmd := exec.Command("git", "-C", mirrorPath, "rev-parse", "HEAD^{tree}")
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
	mirror, mainSHA, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	result, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}
	defer CleanupCheckout(cfg, result.CheckoutPath)
}

func TestCheckoutSource_CommitNotReachableFromMainRejected(t *testing.T) {
	mirror, _, featureSHA := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	if _, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", featureSHA); !errors.Is(err, ErrUnreachableCommit) {
		t.Errorf("expected ErrUnreachableCommit, got %v", err)
	}
}

func TestCheckoutSource_DetachedCheckoutPointsAtCommit(t *testing.T) {
	mirror, mainSHA, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	result, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}
	defer CleanupCheckout(cfg, result.CheckoutPath)

	cmd := exec.Command("git", "-C", result.CheckoutPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if strings.TrimSpace(string(out)) != mainSHA {
		t.Errorf("HEAD does not match expected commit")
	}
}

func TestCleanupCheckout_RemovesCheckout(t *testing.T) {
	mirror, mainSHA, _ := setupBareMirrorWithRefs(t)
	cfg, root := newConfig(t)
	if err := os.Rename(mirror, filepath.Join(root, "myrepo.git")); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	result, err := CheckoutSource(context.Background(), cfg, "myorg", "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}

	if err := CleanupCheckout(cfg, result.CheckoutPath); err != nil {
		t.Fatalf("CleanupCheckout: %v", err)
	}
	if _, err := os.Stat(result.CheckoutPath); err == nil {
		t.Errorf("checkout still exists after cleanup")
	}
}

func TestCleanupCheckout_RefusesPathsOutsideRoot(t *testing.T) {
	cfg, _ := newConfig(t)
	outside := "/this-path-does-not-exist-and-is-outside-the-temp-root"
	if err := CleanupCheckout(cfg, outside); err == nil {
		t.Errorf("expected error for path outside root")
	}
}
