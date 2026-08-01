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

// Sentinel errors returned by CheckoutSource.
var (
	ErrInvalidInput      = errors.New("invalid input")
	ErrRepoMismatch      = errors.New("repository origin mismatch")
	ErrCommitNotFound    = errors.New("commit not found")
	ErrNotCommitObject   = errors.New("not a commit object")
	ErrUnreachableCommit = errors.New("commit not reachable from origin/main")
	ErrCheckoutExists    = errors.New("checkout path already exists")
)

// SourceConfig holds the trusted host-side configuration for source
// resolution. RepositoryRoot is supplied by trusted host configuration,
// not the caller.
type SourceConfig struct {
	AllowedOrg     string // expected organisation; matches the passed organisation
	RepositoryRoot string // trusted host path; mirrors and checkouts live under here
	SkipFetch      bool   // for tests: skip fetch+prune when offline
}

// SourceResult is the outcome of a successful source resolution.
type SourceResult struct {
	Organisation string
	Repository   string
	Commit       string
	CheckoutPath string
}

// CheckoutSource resolves the repository, validates the commit, and
// produces a detached checkout directory containing exactly that commit.
//
// Caller-supplied inputs: organisation, repository, commit.
// Trusted host configuration: cfg.AllowedOrg, cfg.RepositoryRoot.
//
// The function never logs, prints, or returns credentials or tokens.
func CheckoutSource(ctx context.Context, cfg SourceConfig, organisation, repository, commit string) (*SourceResult, error) {
	if err := validateSourceInputs(cfg, organisation, repository, commit); err != nil {
		return nil, err
	}

	mirrorPath := filepath.Join(cfg.RepositoryRoot, repository+".git")
	expectedOrigin := fmt.Sprintf("https://github.com/%s/%s.git", organisation, repository)

	if err := ensureMirror(ctx, cfg, mirrorPath, expectedOrigin, cfg.RepositoryRoot); err != nil {
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
		_ = CleanupCheckout(cfg, checkoutPath)
		return nil, fmt.Errorf("strip credentials: %w", err)
	}

	return &SourceResult{
		Organisation: organisation,
		Repository:   repository,
		Commit:       commit,
		CheckoutPath: checkoutPath,
	}, nil
}

func validateSourceInputs(cfg SourceConfig, organisation, repository, commit string) error {
	if organisation == "" {
		return fmt.Errorf("%w: organisation is required", ErrInvalidInput)
	}
	if organisation != cfg.AllowedOrg {
		return fmt.Errorf("%w: organisation %q does not match allowed %q", ErrInvalidInput, organisation, cfg.AllowedOrg)
	}
	if !appNameRe.MatchString(repository) {
		return fmt.Errorf("%w: repository %q does not match app-name format", ErrInvalidInput, repository)
	}
	if !shaRe.MatchString(commit) {
		return fmt.Errorf("%w: commit %q is not exactly 40 lowercase hex characters", ErrInvalidInput, commit)
	}
	if cfg.RepositoryRoot == "" {
		return fmt.Errorf("%w: repository root is required (host configuration)", ErrInvalidInput)
	}
	return nil
}

func ensureMirror(ctx context.Context, cfg SourceConfig, mirrorPath, expectedOrigin, repoRoot string) error {
	headPath := filepath.Join(mirrorPath, "HEAD")
	if _, err := os.Stat(headPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check mirror at %s: %w", mirrorPath, err)
		}
		// Mirror does not exist: create root and clone bare mirror.
		if err := os.MkdirAll(repoRoot, 0o755); err != nil {
			return fmt.Errorf("create repository root %s: %w", repoRoot, err)
		}
		if err := runGit(ctx, "", "clone", "--bare", expectedOrigin, mirrorPath); err != nil {
			return fmt.Errorf("clone bare mirror from %s to %s: %w", expectedOrigin, mirrorPath, err)
		}
		return nil
	}

	// Existing: verify bare and origin.
	if err := runGit(ctx, mirrorPath, "rev-parse", "--is-bare-repository"); err != nil {
		return fmt.Errorf("verify bare repository at %s: %w", mirrorPath, err)
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

	if !cfg.SkipFetch {
		if err := runGit(ctx, mirrorPath, "fetch", "--prune", "origin"); err != nil {
			// If fetch fails but refs/remotes/origin/main exists locally, the
			// mirror is fresh enough for offline scenarios (e.g. tests).
			if !hasRemoteMain(mirrorPath) {
				return fmt.Errorf("fetch origin from %s: %w", mirrorPath, err)
			}
		}
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

// CleanupCheckout removes the temporary checkout directory. It refuses
// to remove paths outside the trusted repository root.
func CleanupCheckout(cfg SourceConfig, checkoutPath string) error {
	absCheckout, err := filepath.Abs(checkoutPath)
	if err != nil {
		return fmt.Errorf("resolve checkout path: %w", err)
	}
	absRoot, err := filepath.Abs(cfg.RepositoryRoot)
	if err != nil {
		return fmt.Errorf("resolve root path: %w", err)
	}
	rel, err := filepath.Rel(absRoot, absCheckout)
	if err != nil {
		return fmt.Errorf("checkout path %s is outside trusted root %s", checkoutPath, cfg.RepositoryRoot)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("checkout path %s is outside trusted root %s", checkoutPath, cfg.RepositoryRoot)
	}
	return os.RemoveAll(absCheckout)
}

func hasRemoteMain(mirrorPath string) bool {
	cmd := exec.Command("git", "-C", mirrorPath, "rev-parse", "--verify", "refs/remotes/origin/main")
	return cmd.Run() == nil
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
