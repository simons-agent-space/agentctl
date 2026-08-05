package deploy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// setupRemote creates a local non-bare Git repository to act as the
// trusted remote. It contains a `main` branch with one commit and a
// `feature` branch with one additional commit. Returns the remote
// path, the main SHA, and the feature SHA. The remote path is used
// to seed the mirror's origin URL by overriding the URL derivation
// for the test (the production test config uses a per-test
// RepositoryRoot and AllowedOrg; the origin URL is derived from
// "https://github.com/<org>/<repo>.git", but the tests that need
// to point at a local file do so by reaching into the in-memory
// config and adjusting the derivation).
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

// newConfig returns a SourceConfig that derives the origin URL from
// the configured org + repository. Tests that need to point the
// mirror at a local file remote pass the remote path as the
// optional originURLOverride; the override is the test-only seam
// in deploy.SourceConfig that lets suite-level tests point the
// mirror at a local file remote while the production derivation
// is the real "https://github.com/<org>/<repo>.git".
func newConfig(t *testing.T, originURLOverride ...string) SourceConfig {
	t.Helper()
	cfg := SourceConfig{
		AllowedOrg:     "myorg",
		RepositoryRoot: t.TempDir(),
	}
	if len(originURLOverride) > 0 {
		cfg.originURLOverride = originURLOverride[0]
	}
	return cfg
}

// mirrorPath returns the trusted mirror path for the given config and repo.
func mirrorPath(cfg SourceConfig, repo string) string {
	return filepath.Join(cfg.RepositoryRoot, repo+".git")
}

// overrideOriginURL writes config.OriginURL for the duration of a
// test by directly rewriting the mirror's remote.origin.url after
// the bare clone is created. This is the test-side channel for
// pointing the mirror at a local file remote: production code
// derives the URL from the configured org + repository, so the
// AllowedOrg in the test config ("myorg") is treated as a stand-in
// org and the derivation is short-circuited by the override.
//
// The override is a test-only mechanism. The point of the
// production design is that originURL is never caller-supplied; in
// the tests we fake the derived URL by replacing it on disk after
// the bare clone.
func overrideOriginURL(t *testing.T, mirrorPath, url string) {
	t.Helper()
	cmd := exec.Command("git", "-C", mirrorPath, "remote", "set-url", "origin", url)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set-url: %v: %s", err, out)
	}
}

// dummyRepoURL returns the file:// URL the local setupRemote
// fixture produced. Tests that need the mirror to point at the
// fixture use this constant together with overrideOriginURL.
const dummyRepoURL = "file://" // combined with the actual remotePath in the test

func TestCheckoutSource_ValidSHA(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	// Point the derived mirror at the local fixture. The
	// production design derives the URL from the org + repo;
	// here we replace the bare clone's origin URL after the
	// clone to use the local file URL.
	//
	// The "right" way to test this without overrideOriginURL
	// would be to run a real local HTTPS server fronting the
	// remote; for the layer-under-test, the override is
	// sufficient because production code only ever writes to
	// remote.origin.url once (during clone) and reads it
	// thereafter for the mirror validation.
	//
	// We achieve the override by configuring the source layer
	// temporarily: we let CheckoutSource clone from the
	// derived URL into a bogus mirror, then repoint the mirror
	// to the real local remote, then call CheckoutSource again.
	// This is awkward; the cleaner approach is to expose a
	// test seam in source.go for the URL derivation. Since
	// adding that seam only for tests is a smell, we instead
	// hand-craft the mirror by cloning with the right URL
	// here and re-using the existing ensureMirror path.
	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed bare clone: %v: %s", err, out)
	}

	result, err := CheckoutSource(context.Background(), cfg, "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *result)

	if result.Repository != "myrepo" || result.Commit != mainSHA {
		t.Errorf("unexpected result: %+v", result)
	}
	if result.MirrorPath != mirror {
		t.Errorf("MirrorPath = %q, want %q", result.MirrorPath, mirror)
	}
}

func TestCheckoutSource_ShortSHARejected(t *testing.T) {
	cfg := newConfig(t)
	shortSHA := strings.Repeat("a", 39)
	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", shortSHA); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for short SHA, got %v", err)
	}
}

func TestCheckoutSource_UppercaseSHARejected(t *testing.T) {
	cfg := newConfig(t)
	upperSHA := strings.Repeat("A", 40)
	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", upperSHA); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for uppercase SHA, got %v", err)
	}
}

func TestCheckoutSource_NonHexSHARejected(t *testing.T) {
	cfg := newConfig(t)
	nonHex := strings.Repeat("z", 40)
	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", nonHex); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for non-hex SHA, got %v", err)
	}
}

func TestCheckoutSource_InvalidRepoNameRejected(t *testing.T) {
	cfg := newConfig(t)
	for _, name := range []string{"MyRepo", "myrepo-", "1myrepo", "with/slash", "with:colon", "with?query", "with#frag"} {
		t.Run(name, func(t *testing.T) {
			if _, err := CheckoutSource(context.Background(), cfg, name, strings.Repeat("a", 40)); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput for repository %q, got %v", name, err)
			}
		})
	}
}

func TestCheckoutSource_RepoOriginMismatchRejected(t *testing.T) {
	remote1, mainSHA, _ := setupRemote(t)
	remote2, _, _ := setupRemote(t)
	// The source layer derives the origin URL from the override
	// (which the test sets to remote1). The mirror is then
	// re-pointed at remote2 so the derivation no longer matches
	// the mirror. ensureMirror must reject the mismatch with
	// ErrRepoMismatch.
	cfg := newConfig(t, remote1)

	// Seed a mirror pointing at remote1.
	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote1, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	// Repoint the mirror at remote2 — a different URL than the
	// override. CheckoutSource must reject this with ErrRepoMismatch.
	overrideOriginURL(t, mirror, remote2)

	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", mainSHA); !errors.Is(err, ErrRepoMismatch) {
		t.Errorf("expected ErrRepoMismatch, got %v", err)
	}
}

// TestCheckoutSource_ExistingMirrorWithCorrectOriginAccepted proves
// that a pre-existing mirror whose remote.origin.url matches the
// derived origin URL is accepted. The source layer must verify the
// match (so a mirror created for one repo cannot be silently reused
// by another repo) and then proceed with the fetch.
func TestCheckoutSource_ExistingMirrorWithCorrectOriginAccepted(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	// Seed a mirror pointing at the remote that matches the
	// derivation. The fixture's newConfig wires the override to
	// the same remote, so the mirror's URL matches the derived
	// URL exactly.
	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	result, err := CheckoutSource(context.Background(), cfg, "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource: expected success for matching origin, got %v", err)
	}
	if result.Repository != "myrepo" || result.Commit != mainSHA {
		t.Errorf("unexpected result: %+v", result)
	}
	if result.MirrorPath != mirror {
		t.Errorf("MirrorPath = %q, want %q", result.MirrorPath, mirror)
	}
}

func TestCheckoutSource_NonBareMirrorRejected(t *testing.T) {
	cfg := newConfig(t)

	// Pre-create a non-bare repository at the expected mirror path.
	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatalf("mkdir mirror: %v", err)
	}
	cmd := exec.Command("git", "init", "-q", mirror)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", strings.Repeat("a", 40)); !errors.Is(err, ErrNotBare) {
		t.Errorf("expected ErrNotBare, got %v", err)
	}
}

func TestCheckoutSource_MissingCommitRejected(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	bogusSHA := strings.Repeat("0", 40)
	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", bogusSHA); !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
	_ = mainSHA
}

func TestCheckoutSource_NonCommitObjectRejected(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	cmd := exec.Command("git", "-C", mirror, "rev-parse", "HEAD^{tree}")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("get tree: %v", err)
	}
	treeSHA := strings.TrimSpace(string(out))

	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", treeSHA); !errors.Is(err, ErrNotCommitObject) {
		t.Errorf("expected ErrNotCommitObject, got %v", err)
	}
	_ = mainSHA
}

func TestCheckoutSource_CommitOnMainAccepted(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", mainSHA); err != nil {
		t.Fatalf("CheckoutSource: %v", err)
	}
}

func TestCheckoutSource_CommitNotReachableFromMainRejected(t *testing.T) {
	remote, _, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	// Add a feature branch commit that is not reachable from main.
	featCmd := exec.Command("git", "-C", mirror, "rev-parse", "refs/heads/feature")
	fout, err := featCmd.Output()
	if err != nil {
		t.Fatalf("get feature SHA: %v", err)
	}
	featureSHA := strings.TrimSpace(string(fout))

	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", featureSHA); !errors.Is(err, ErrUnreachableCommit) {
		t.Errorf("expected ErrUnreachableCommit, got %v", err)
	}
}

func TestCheckoutSource_DetachedCheckoutPointsAtCommit(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	result, err := CheckoutSource(context.Background(), cfg, "myrepo", mainSHA)
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

func TestCheckoutSource_AllowedOrgRequired(t *testing.T) {
	cfg := SourceConfig{
		// AllowedOrg intentionally omitted.
		RepositoryRoot: t.TempDir(),
	}
	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", strings.Repeat("a", 40)); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing AllowedOrg, got %v", err)
	}
}

func TestCheckoutSource_RepositoryRootRequired(t *testing.T) {
	cfg := SourceConfig{
		AllowedOrg: "myorg",
		// RepositoryRoot intentionally omitted.
	}
	if _, err := CheckoutSource(context.Background(), cfg, "myrepo", strings.Repeat("a", 40)); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing RepositoryRoot, got %v", err)
	}
}

func TestOriginURL_DerivedFromOrgAndRepository(t *testing.T) {
	cfg := SourceConfig{AllowedOrg: "simons-agent-space", RepositoryRoot: t.TempDir()}
	got := cfg.originURL("cron-dashboard")
	want := "https://github.com/simons-agent-space/cron-dashboard.git"
	if got != want {
		t.Errorf("originURL = %q, want %q", got, want)
	}
}

func TestSourceConfig_OriginURLUsesConfiguredOrgOnly(t *testing.T) {
	// Two configs that differ only in AllowedOrg must produce
	// different origin URLs for the same repository. This is
	// the structural proof that the org component is trusted
	// host configuration and cannot be selected by the caller.
	a := SourceConfig{AllowedOrg: "alpha-org", RepositoryRoot: t.TempDir()}
	b := SourceConfig{AllowedOrg: "beta-org", RepositoryRoot: t.TempDir()}
	ua := a.originURL("same-repo")
	ub := b.originURL("same-repo")
	if ua == ub {
		t.Errorf("originURL must differ between orgs: both = %q", ua)
	}
	if ua != "https://github.com/alpha-org/same-repo.git" {
		t.Errorf("alpha originURL = %q", ua)
	}
	if ub != "https://github.com/beta-org/same-repo.git" {
		t.Errorf("beta originURL = %q", ub)
	}
}

func TestCheckoutSource_SeparateRepositoriesUseSeparateMirrors(t *testing.T) {
	// Two distinct repositories in the same org must produce two
	// distinct mirrors and two distinct checkout paths. This is
	// the structural proof that the source layer does not
	// collapse "app" and "repository" into a single key.
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	// Seed two bare mirrors pointing at the same remote but
	// under different names. CheckoutSource must then accept
	// both repos and keep their mirrors/checkouts separate.
	for _, repo := range []string{"repo-a", "repo-b"} {
		mirror := mirrorPath(cfg, repo)
		if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
			t.Fatalf("mkdir mirror parent: %v", err)
		}
		cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
		if out, err := cloneCmd.CombinedOutput(); err != nil {
			t.Fatalf("seed clone %s: %v: %s", repo, err, out)
		}
	}

	resA, err := CheckoutSource(context.Background(), cfg, "repo-a", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource repo-a: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *resA)

	resB, err := CheckoutSource(context.Background(), cfg, "repo-b", mainSHA)
	if err != nil {
		t.Fatalf("CheckoutSource repo-b: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *resB)

	if resA.MirrorPath == resB.MirrorPath {
		t.Errorf("mirrors must be distinct: both = %q", resA.MirrorPath)
	}
	if resA.CheckoutPath == resB.CheckoutPath {
		t.Errorf("checkouts must be distinct: both = %q", resA.CheckoutPath)
	}
	if resA.MirrorPath != mirrorPath(cfg, "repo-a") {
		t.Errorf("resA.MirrorPath = %q, want %q", resA.MirrorPath, mirrorPath(cfg, "repo-a"))
	}
	if resB.MirrorPath != mirrorPath(cfg, "repo-b") {
		t.Errorf("resB.MirrorPath = %q, want %q", resB.MirrorPath, mirrorPath(cfg, "repo-b"))
	}
}

func TestCleanupCheckout_AllowsReuseAfterCleanup(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	first, err := CheckoutSource(context.Background(), cfg, "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	if err := CleanupCheckout(context.Background(), cfg, *first); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	if _, err := os.Stat(first.CheckoutPath); err == nil {
		t.Fatalf("checkout still exists after cleanup")
	}

	second, err := CheckoutSource(context.Background(), cfg, "myrepo", mainSHA)
	if err != nil {
		t.Fatalf("second checkout after cleanup: %v", err)
	}
	defer CleanupCheckout(context.Background(), cfg, *second)
	if second.CheckoutPath != first.CheckoutPath {
		t.Errorf("second checkout path = %q, want %q", second.CheckoutPath, first.CheckoutPath)
	}
}

func TestCleanupCheckout_RejectsCheckoutPathEqualToRoot(t *testing.T) {
	cfg := newConfig(t)
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
	cfg := newConfig(t)
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
	cfg := newConfig(t)
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
	cfg := newConfig(t)

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
	cfg := newConfig(t)
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

// TestVerifyCommit_ReachableOnMainAccepted verifies the happy path
// returns no error and creates no worktree on disk.
func TestVerifyCommit_ReachableOnMainAccepted(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	if err := VerifyCommit(context.Background(), cfg, "myrepo", mainSHA); err != nil {
		t.Fatalf("VerifyCommit: %v", err)
	}
}

// TestVerifyCommit_InvalidInputsRejected checks every validation
// rule the public helper must apply without ever reaching git. None
// of them touch the filesystem, so we use an origin path that does
// not exist.
func TestVerifyCommit_InvalidInputsRejected(t *testing.T) {
	cfg := newConfig(t)
	cases := []struct {
		name string
		repo string
		sha  string
		want error
	}{
		{"invalid repo name", "MyRepo", strings.Repeat("a", 40), ErrInvalidInput},
		{"short sha", "myrepo", strings.Repeat("a", 39), ErrInvalidInput},
		{"non-hex sha", "myrepo", strings.Repeat("z", 40), ErrInvalidInput},
		{"missing allowed org", "myrepo", strings.Repeat("a", 40), ErrInvalidInput},
		{"missing repository root", "myrepo", strings.Repeat("a", 40), ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cfg
			if tc.name == "missing allowed org" {
				cfg.AllowedOrg = ""
			}
			if tc.name == "missing repository root" {
				cfg.RepositoryRoot = ""
			}
			err := VerifyCommit(context.Background(), cfg, tc.repo, tc.sha)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

// TestVerifyCommit_MissingCommitRejected verifies that a well-formed
// but absent SHA is reported as ErrCommitNotFound, not ErrInvalidInput.
func TestVerifyCommit_MissingCommitRejected(t *testing.T) {
	remote, _, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	if err := VerifyCommit(context.Background(), cfg, "myrepo", strings.Repeat("0", 40)); !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("got %v, want errors.Is(_, ErrCommitNotFound)", err)
	}
}

// TestVerifyCommit_UnreachableRejected verifies that a commit in a
// non-main branch is reported as ErrUnreachableCommit.
func TestVerifyCommit_UnreachableRejected(t *testing.T) {
	remote, _, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	featCmd := exec.Command("git", "-C", mirror, "rev-parse", "refs/heads/feature")
	fout, err := featCmd.Output()
	if err != nil {
		t.Fatalf("get feature SHA: %v", err)
	}
	featureSHA := strings.TrimSpace(string(fout))

	if err := VerifyCommit(context.Background(), cfg, "myrepo", featureSHA); !errors.Is(err, ErrUnreachableCommit) {
		t.Errorf("got %v, want errors.Is(_, ErrUnreachableCommit)", err)
	}
}

// TestVerifyCommit_NonCommitObjectRejected verifies that a tree/blob
// SHA passes cat-file check on a real repo, so the function must rely
// on validateCommit's object-type rule. This guarantees VerifyCommit
// refuses to treat a tree or tag object as a commit.
func TestVerifyCommit_NonCommitObjectRejected(t *testing.T) {
	remote, _, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	// Materialise a tree SHA inside the mirror.
	hitCmd := exec.Command("git", "-C", mirror, "rev-parse", "HEAD^{tree}")
	hout, err := hitCmd.Output()
	if err != nil {
		t.Fatalf("get tree SHA: %v", err)
	}
	cleanTree := strings.TrimSpace(string(hout))

	if err := VerifyCommit(context.Background(), cfg, "myrepo", cleanTree); !errors.Is(err, ErrNotCommitObject) {
		t.Errorf("got %v, want errors.Is(_, ErrNotCommitObject)", err)
	}
}

// TestVerifyCommit_DoesNotCreateCheckout asserts the side-effect-free
// contract: after a successful VerifyCommit there must be no
// <repo>-checkouts/<commit> directory on disk. This is the
// regression test for the previous /v1/inspect handler, which
// called CheckoutSource and then dropped the result without calling
// CleanupCheckout.
func TestVerifyCommit_DoesNotCreateCheckout(t *testing.T) {
	remote, mainSHA, _ := setupRemote(t)
	cfg := newConfig(t, remote)

	mirror := mirrorPath(cfg, "myrepo")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	cloneCmd := exec.Command("git", "clone", "--bare", remote, mirror)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clone: %v: %s", err, out)
	}

	if err := VerifyCommit(context.Background(), cfg, "myrepo", mainSHA); err != nil {
		t.Fatalf("VerifyCommit: %v", err)
	}

	expected := filepath.Join(cfg.RepositoryRoot, "myrepo-checkouts", mainSHA)
	if _, err := os.Stat(expected); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout directory %s unexpectedly exists: %v", expected, err)
	}
}

func TestSourceConfig_NoExportedOriginOrBaseURLAPI(t *testing.T) {
	t.Helper()
	typ := reflect.TypeOf(SourceConfig{})

	// Exported fields.
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		lname := strings.ToLower(f.Name)
		for _, bad := range []string{"origin", "url", "base", "remote"} {
			if strings.Contains(lname, bad) {
				t.Errorf("SourceConfig exposes exported field %q (matches %q in name); rename or unexport it (production must not let a caller pick the origin URL, base URL, or alternate remote)", f.Name, bad)
				break
			}
		}
	}

	// Exported methods.
	ptr := reflect.PtrTo(typ)
	seen := map[string]bool{}
	for _, mt := range []reflect.Type{typ, ptr} {
		for i := 0; i < mt.NumMethod(); i++ {
			m := mt.Method(i)
			if !m.IsExported() {
				continue
			}
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			lname := strings.ToLower(m.Name)
			for _, bad := range []string{"origin", "url", "base", "remote", "withtest", "testorigin", "testurl"} {
				if strings.Contains(lname, bad) {
					t.Errorf("SourceConfig exposes exported method %q (matches %q in name); the production API must not let a caller pick the origin URL, base URL, or alternate remote (rename, unexport, or remove)", m.Name, bad)
					break
				}
			}
		}
	}
}
