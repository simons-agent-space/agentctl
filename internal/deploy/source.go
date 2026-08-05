package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// shaRe matches exactly 40 lowercase hexadecimal characters.
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Sentinel errors returned by CheckoutSource and CleanupCheckout.
var (
	ErrInvalidInput      = errors.New("invalid input")
	ErrRepoMismatch      = errors.New("repository origin mismatch")
	ErrNotBare           = errors.New("mirror is not a bare repository")
	ErrCommitNotFound    = errors.New("commit not found")
	ErrNotCommitObject   = errors.New("not a commit object")
	ErrUnreachableCommit = errors.New("commit not reachable from origin/main")
	ErrCheckoutExists    = errors.New("checkout path already exists")
)

// originFetchRefspec is the explicit refspec used to populate the
// remote-tracking branch that source resolution validates against.
// Bare clones do not create refs/remotes/<remote>/* automatically; this
// refspec is the canonical way to ensure refs/remotes/origin/main exists.
const originFetchRefspec = "+refs/heads/main:refs/remotes/origin/main"

// SourceConfig holds the trusted host-side configuration for source
// resolution. The repository root and the allowed organisation are
// supplied by trusted host configuration, not by the caller.
//
// The trust boundary is fixed: the caller's repository short name
// is verified against appNameRe, and the public origin URL is
// derived internally from AllowedOrg + repository. The caller
// never supplies a URL, an org, or a base URL. There is no
// per-repository allowlist: any syntactically valid repository
// in the configured org is selectable, and selection outside the
// org is impossible by construction.
type SourceConfig struct {
	AllowedOrg     string // GitHub org; the org component of the derived origin URL
	RepositoryRoot string // trusted host path; mirrors and checkouts live under here
	// OriginURLOverride is a test-only seam. When non-empty it
	// replaces the derived origin URL. The field is unexported
	// so production code (the daemon, the CLI, every external
	// entry point) cannot set it; only tests in this package
	// can. The override exists because the source-layer tests
	// need to point at a local file remote while the production
	// derivation is the literal "https://github.com/<org>/<repo>.git".
	OriginURLOverride string
}

// SourceResult is the outcome of a successful source resolution.
type SourceResult struct {
	Repository   string
	Commit       string
	CheckoutPath string
	MirrorPath   string
}

// originURL returns the trusted public origin URL for the given
// repository in the configured org. The literal "<org>" and
// "<repository>" segments are url-path-safe to embed between
// the fixed "https://github.com/" prefix and the trailing ".git"
// suffix; the regex in appNameRe (^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$)
// excludes every character that would need escaping (slash, colon,
// query, fragment, percent, whitespace, control, ...).
//
// The organisation is read from the trusted SourceConfig, not
// from the caller; the repository is the validated short name.
func (c SourceConfig) originURL(repository string) string {
	if c.OriginURLOverride != "" {
		return c.OriginURLOverride
	}
	return "https://github.com/" + c.AllowedOrg + "/" + repository + ".git"
}

// CheckoutSource resolves the repository, validates the commit, and
// produces a detached checkout directory containing exactly that commit.
//
// Caller-supplied inputs: repository, commit.
// Trusted host configuration: cfg.AllowedOrg, cfg.RepositoryRoot.
//
// The origin URL is derived inside CheckoutSource from the
// trusted org and the validated repository short name. The
// caller never provides a URL, a base URL, or an org.
//
// The function never logs, prints, or returns credentials or
// tokens.
func CheckoutSource(ctx context.Context, cfg SourceConfig, repository, commit string) (*SourceResult, error) {
	if err := validateSourceInputs(cfg, repository, commit); err != nil {
		return nil, err
	}

	mirrorPath := filepath.Join(cfg.RepositoryRoot, repository+".git")

	if err := ensureMirror(ctx, mirrorPath, cfg, repository); err != nil {
		return nil, err
	}

	if err := validateCommit(ctx, mirrorPath, commit); err != nil {
		return nil, err
	}

	checkoutPath := filepath.Join(cfg.RepositoryRoot, repository+"-checkouts", commit)
	if err := createDetachedCheckout(ctx, mirrorPath, checkoutPath, commit); err != nil {
		return nil, err
	}

	if err := stripCredentials(checkoutPath); err != nil {
		_ = removeCheckout(ctx, mirrorPath, checkoutPath)
		return nil, fmt.Errorf("strip credentials: %w", err)
	}

	return &SourceResult{
		Repository:   repository,
		Commit:       commit,
		CheckoutPath: checkoutPath,
		MirrorPath:   mirrorPath,
	}, nil
}

func validateSourceInputs(cfg SourceConfig, repository, commit string) error {
	if cfg.AllowedOrg == "" {
		return fmt.Errorf("%w: allowed org is required (host configuration)", ErrInvalidInput)
	}
	if cfg.RepositoryRoot == "" {
		return fmt.Errorf("%w: repository root is required (host configuration)", ErrInvalidInput)
	}
	if !appNameRe.MatchString(repository) {
		return fmt.Errorf("%w: repository %q does not match app-name format", ErrInvalidInput, repository)
	}
	if !shaRe.MatchString(commit) {
		return fmt.Errorf("%w: commit %q is not exactly 40 lowercase hex characters", ErrInvalidInput, commit)
	}
	return nil
}

// ensureMirror ensures the bare mirror exists, is bare, has the
// expected origin URL, and has refs/remotes/origin/main populated.
//
// On first use: create the parent directory, clone-bare from the
// expected origin, then run the explicit fetch refspec.
//
// On subsequent use: verify the mirror is bare, verify the configured
// remote.origin.url matches the derived origin URL, then run the
// explicit fetch refspec. A failed fetch is fatal — there is no
// fallback that accepts stale refs/remotes/origin/main in lieu of a
// successful fetch.
//
// The expected origin URL is derived inside this function from
// cfg.AllowedOrg + repository. The caller never supplies a URL.
// A pre-existing mirror whose origin URL points at a different org
// or a different repository is rejected with ErrRepoMismatch, so
// a deployed mirror for one repo cannot be silently reused by
// another repo even if both happen to be in the same org.
func ensureMirror(ctx context.Context, mirrorPath string, cfg SourceConfig, repository string) error {
	expectedOrigin := cfg.originURL(repository)

	headPath := filepath.Join(mirrorPath, "HEAD")
	if _, err := os.Stat(headPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check mirror at %s: %w", mirrorPath, err)
		}
		// HEAD is missing. The mirror either does not exist or exists as a
		// non-bare repository (which has no HEAD in the expected location).
		// `git clone --bare` refuses to overwrite an existing non-empty
		// directory, so detect that case explicitly and reject it.
		if _, dirErr := os.Stat(mirrorPath); dirErr == nil {
			return fmt.Errorf("%w: %s exists but is not a bare repository", ErrNotBare, mirrorPath)
		}
		// Mirror does not exist: create the parent directory and clone bare.
		if err := os.MkdirAll(filepath.Dir(mirrorPath), 0o755); err != nil {
			return fmt.Errorf("create repository root %s: %w", filepath.Dir(mirrorPath), err)
		}
		if err := runGit(ctx, "", "clone", "--bare", expectedOrigin, mirrorPath); err != nil {
			return fmt.Errorf("clone bare mirror from %s to %s: %w", expectedOrigin, mirrorPath, err)
		}
	} else {
		// Existing mirror: verify it is bare and points at the expected origin.
		if err := verifyBare(ctx, mirrorPath); err != nil {
			return err
		}
		out, err := gitOutput(ctx, mirrorPath, "config", "--get", "remote.origin.url")
		if err != nil {
			return fmt.Errorf("read origin URL of %s: %w", mirrorPath, err)
		}
		actual := normalizeGitURL(strings.TrimSpace(out))
		expected := normalizeGitURL(expectedOrigin)
		if actual != expected {
			return fmt.Errorf("%w: have %q, want %q", ErrRepoMismatch, strings.TrimSpace(out), expectedOrigin)
		}
	}

	// Explicitly populate refs/remotes/origin/main. Bare clones do not
	// create remote-tracking branches on their own. A failed fetch is fatal.
	if err := runGit(ctx, mirrorPath, "fetch", "--prune", "origin", originFetchRefspec); err != nil {
		return fmt.Errorf("fetch origin/main: %w", err)
	}

	return nil
}

func verifyBare(ctx context.Context, mirrorPath string) error {
	out, err := gitOutput(ctx, mirrorPath, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("verify bare repository at %s: %w", mirrorPath, err)
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf("%w: %s", ErrNotBare, mirrorPath)
	}
	return nil
}

func validateCommit(ctx context.Context, mirrorPath, commit string) error {
	objType, err := gitOutput(ctx, mirrorPath, "cat-file", "-t", commit)
	if err != nil {
		return fmt.Errorf("%w: cat-file -t %s: %v", ErrCommitNotFound, commit, err)
	}
	if strings.TrimSpace(objType) != "commit" {
		return fmt.Errorf("%w: got %q for %s", ErrNotCommitObject, objType, commit)
	}
	if err := runGit(ctx, mirrorPath, "merge-base", "--is-ancestor", commit, "refs/remotes/origin/main"); err != nil {
		return fmt.Errorf("%w: %s not reachable from refs/remotes/origin/main", ErrUnreachableCommit, commit)
	}
	return nil
}

// VerifyCommit validates that commit exists in the trusted mirror
// and is reachable from refs/remotes/origin/main, without creating a
// worktree. It is the side-effect-free counterpart of CheckoutSource:
//
//   - No worktree is added under cfg.RepositoryRoot, so callers do
//     not have to schedule a CleanupCheckout pass.
//   - On first use, ensureMirror still creates the bare mirror and
//     populates refs/remotes/origin/main; that is unavoidable because
//     validateCommit needs a local mirror with a current
//     refs/remotes/origin/main to test reachability against. It is,
//     however, the same mirror creation that any first-use deploy
//     would do, so VerifyCommit does not introduce a new persistent
//     side effect beyond the shared bare mirror.
//
// Use VerifyCommit for inspect / preflight calls. Use CheckoutSource
// when the caller actually needs a working tree (deploy build steps,
// rollback inspection, etc.).
func VerifyCommit(ctx context.Context, cfg SourceConfig, repository, commit string) error {
	if err := validateSourceInputs(cfg, repository, commit); err != nil {
		return err
	}
	mirrorPath := filepath.Join(cfg.RepositoryRoot, repository+".git")
	if err := ensureMirror(ctx, mirrorPath, cfg, repository); err != nil {
		return err
	}
	return validateCommit(ctx, mirrorPath, commit)
}

func createDetachedCheckout(ctx context.Context, mirrorPath, checkoutPath, commit string) error {
	if _, err := os.Stat(checkoutPath); err == nil {
		return fmt.Errorf("%w: %s", ErrCheckoutExists, checkoutPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check checkout path %s: %w", checkoutPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(checkoutPath), 0o755); err != nil {
		return fmt.Errorf("create checkout parent dir: %w", err)
	}
	if err := runGit(ctx, mirrorPath, "worktree", "add", "--detach", checkoutPath, commit); err != nil {
		return fmt.Errorf("create worktree at %s: %w", checkoutPath, err)
	}
	return nil
}

// stripCredentials removes any credential-bearing URLs from the worktree's
// Git config files. The worktree's .git is a file pointing to its gitdir;
// the actual config lives at <gitdir>/config.
func stripCredentials(checkoutPath string) error {
	gitFile := filepath.Join(checkoutPath, ".git")
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", gitFile, err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir: ") {
		return nil
	}
	worktreeGitDir := strings.TrimPrefix(line, "gitdir: ")
	return sanitizeConfigFile(filepath.Join(worktreeGitDir, "config"))
}

func sanitizeConfigFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // config may not exist; nothing to sanitize
	}
	credsRe := regexp.MustCompile(`(https?)://[^/@\s]+:[^/@\s]+@`)
	sanitized := credsRe.ReplaceAllString(string(data), `$1://`)
	if sanitized == string(data) {
		return nil
	}
	return os.WriteFile(path, []byte(sanitized), 0o644)
}

// CleanupCheckout removes the temporary checkout directory and clears
// its worktree registration from the trusted mirror.
//
// The identity of the checkout (repository, commit) is validated
// against the existing app-name and SHA rules. The expected checkout
// path is derived from the trusted repository root and the validated
// identity; the caller-supplied CheckoutPath must match it exactly.
//
// The trusted mirror path is derived from cfg.RepositoryRoot and the
// validated result.Repository — the caller never supplies it.
func CleanupCheckout(ctx context.Context, cfg SourceConfig, result SourceResult) error {
	if !appNameRe.MatchString(result.Repository) {
		return fmt.Errorf("%w: repository %q does not match app-name format", ErrInvalidInput, result.Repository)
	}
	if !shaRe.MatchString(result.Commit) {
		return fmt.Errorf("%w: commit %q is not exactly 40 lowercase hex characters", ErrInvalidInput, result.Commit)
	}

	expectedPath := filepath.Join(cfg.RepositoryRoot, result.Repository+"-checkouts", result.Commit)
	absExpected, err := filepath.Abs(expectedPath)
	if err != nil {
		return fmt.Errorf("resolve expected checkout path: %w", err)
	}
	absCheckout, err := filepath.Abs(result.CheckoutPath)
	if err != nil {
		return fmt.Errorf("%w: resolve checkout path %s: %v", ErrInvalidInput, result.CheckoutPath, err)
	}
	if absExpected != absCheckout {
		return fmt.Errorf("%w: checkout path %s does not match expected %s", ErrInvalidInput, absCheckout, absExpected)
	}

	mirrorPath := filepath.Join(cfg.RepositoryRoot, result.Repository+".git")
	return removeCheckout(ctx, mirrorPath, absCheckout)
}

// removeCheckout clears the worktree's Git registration and removes
// the checkout directory. The worktree-remove and prune steps are
// best-effort: a missing mirror, an already-unregistered worktree, or
// any other pre-condition failure must not prevent the directory
// removal that follows.
func removeCheckout(ctx context.Context, mirrorPath, checkoutPath string) error {
	_ = runGit(ctx, mirrorPath, "worktree", "remove", "--force", checkoutPath)
	_ = runGit(ctx, mirrorPath, "worktree", "prune")
	return os.RemoveAll(checkoutPath)
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return string(out), nil
}

func normalizeGitURL(u string) string {
	return strings.TrimSuffix(strings.TrimSpace(u), ".git")
}
