// Secret loading for per-app env injection. Secret files live under
// Config.SecretDir; their filenames match the EnvEntry.SecretRef
// after the secretRefRe validation in Load(). This file enforces
// the ownership, permission, and shape rules so secret values are
// never trusted from an attacker-controlled file.
//
// Env-secret values are strictly single-line: Docker --env-file
// cannot safely represent arbitrary multi-line values with the
// current KEY=value writer, so the loader rejects any embedded
// newline or carriage return after trimming one trailing newline.
// The runtime directory used to stage the temp env file is held
// to the same standard as the secret directory: no symlinks at
// any path component, real directory, owned by root or the
// daemon's own uid, mode 0700-or-stricter.
package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Sentinel errors returned by the secret loader.
var (
	ErrSecretDirInvalid     = errors.New("invalid secret directory")
	ErrSecretMissing        = errors.New("required secret file is missing")
	ErrSecretSymlink        = errors.New("secret file must not be a symlink")
	ErrSecretPermissions    = errors.New("secret file permissions too open")
	ErrSecretNotRegular     = errors.New("secret file is not a regular file")
	ErrSecretDirSymlink     = errors.New("secret directory must not be a symlink")
	ErrSecretDirPermissions = errors.New("secret directory permissions too open")
	ErrSecretDirOwner       = errors.New("secret directory is not owned by a trusted user")
	ErrSecretFileOwner      = errors.New("secret file is not owned by a trusted user")
)

// ErrRuntimeDirMissing is returned by MaterializeEnvFile when the
// supplied runtime directory does not exist or is not a directory.
// In normal operation the daemon's loadConfig pre-creates
// RuntimeDir at startup, so this is a defensive check that
// catches a configuration bug (operator pointed RuntimeDir at a
// path that disappeared after startup).
var ErrRuntimeDirMissing = errors.New("env file: runtime directory is missing or not a directory")

var (
	ErrRuntimeDirSymlink     = errors.New("env file: runtime directory must not be a symlink")
	ErrRuntimeDirPermissions = errors.New("env file: runtime directory permissions too open")
	ErrRuntimeDirOwner       = errors.New("env file: runtime directory is not owned by a trusted user")
	ErrSecretMultiline       = errors.New("secret value must not contain embedded newlines")
)

// currentUID is wrapped in a package-level var so tests can verify
// the "untrusted owner" rejection path without needing CAP_SETUID.
// Production always reads from os.Geteuid(); tests swap it via a
// saved/restored hook.
var currentUID = os.Geteuid

// trustedOwner reports whether uid is one the daemon is willing to
// load secrets for: uid 0 (root) or the daemon's own uid. Other
// uids mean an unprivileged user has tampered with the secret
// layout; refusing the load is the safe default.
func trustedOwner(uid int) bool {
	return uid == 0 || uid == currentUID()
}

// ValidateSecretDir sanity-checks the configured secret directory
// once at daemon startup. It does not enumerate the directory.
// An empty dir is accepted and means "no secrets configured" —
// manifests that opt into env will fail their deploy when
// SecretDir is empty, but the daemon itself must start
// regardless so operators can roll out secret support on the
// same daemon without a restart loop.
//
// On a non-empty dir the loader rejects:
//   - symlinks at any component of the directory path (defends
//     against a redirected secret root pointing at an
//     attacker-controlled tree);
//   - group/other access on the directory itself (0700 or stricter
//     only);
//   - an untrusted owner (root or the daemon's uid only).
func ValidateSecretDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := noSymlinkAt(dir, ErrSecretDirSymlink); err != nil {
		return err
	}
	// noSymlinkAt already checked `dir` for being a symlink at every
	// component; a re-Lstat here would only catch the missing-dir
	// case (noSymlinkAt returns nil for missing parents so the caller
	// can decide). Stat (not Lstat) so we see the directory through
	// the symlink-free path.
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrSecretDirInvalid, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrSecretDirInvalid, dir)
	}
	uid := uidFromFileInfo(info)
	if !trustedOwner(uid) {
		return fmt.Errorf("%w: %s has uid %d (only uid 0 or %d accepted)",
			ErrSecretDirOwner, dir, uid, currentUID())
	}
	if perms := info.Mode().Perm(); perms&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %#o (must be %s or stricter)",
			ErrSecretDirPermissions, dir, perms, "0700")
	}
	return nil
}

// IsSecretConfigured reports whether a secret is present and would
// pass the loader's per-file checks, WITHOUT reading the value.
// Used by /v1/inspect so the response can say "configured: true/
// false" without ever pulling the secret into memory. Mirrors the
// LoadSecret checks: no symlink, regular file, 0o600-or-stricter,
// trusted owner.
func IsSecretConfigured(secretDir, secretRef string) bool {
	_, found, err := LoadSecretOptional(secretDir, secretRef)
	return found && err == nil
}

// LoadSecret reads a secret file from secretDir, validates ownership
// and permissions, and trims a single trailing newline. The
// returned string is sensitive; the caller is responsible for not
// logging it, not persisting it, and not exposing it in inspect
// output or error messages. Missing-file is reported via
// ErrSecretMissing in the returned error so required-secret
// callers can branch; optional-secret callers should use
// LoadSecretOptional instead, which distinguishes "missing" from
// "present-but-broken".
func LoadSecret(secretDir, secretRef string) (string, error) {
	v, found, err := LoadSecretOptional(secretDir, secretRef)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrSecretMissing, secretRef)
	}
	return v, nil
}

// LoadSecretOptional is the deploy-time entry point used by the
// orchestrator's required/optional split. It returns:
//
//   - (value, true, nil)  — the secret loaded successfully.
//   - ("",     false, nil) — no file at secretDir/secretRef. This
//     is NOT an error; the orchestrator treats it as "skip this
//     env entry" when the entry is optional, and "fail the
//     deploy" when the entry is required.
//   - ("",     true,  err) — a file was found but is malformed or
//     fails a security check (symlink, wrong perms, bad owner,
//     unreadable, multi-line value). This is always an error;
//     even an optional entry must not be silently skipped when
//     the file is present-but-broken, because that would mask a
//     config bug.
//
// LoadSecret is a thin wrapper around LoadSecretOptional that
// collapses the "missing" case into ErrSecretMissing for callers
// that don't need the distinction.
func LoadSecretOptional(secretDir, secretRef string) (string, bool, error) {
	if !secretRefRe.MatchString(secretRef) {
		// secretRefRe was already validated by Load(), but defend
		// in depth — direct callers of LoadSecretOptional must
		// not be able to smuggle "../foo" into the path.
		return "", false, fmt.Errorf("invalid secret_ref %q", secretRef)
	}
	path := filepath.Join(secretDir, secretRef)
	// Lstat (not Stat) so symlinks are detected, not followed.
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat secret %s: %w", secretRef, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", true, fmt.Errorf("%w: %s", ErrSecretSymlink, secretRef)
	}
	if !info.Mode().IsRegular() {
		return "", true, fmt.Errorf("%w: %s", ErrSecretNotRegular, secretRef)
	}
	uid := uidFromFileInfo(info)
	if !trustedOwner(uid) {
		return "", true, fmt.Errorf("%w: %s has uid %d (only uid 0 or %d accepted)",
			ErrSecretFileOwner, secretRef, uid, currentUID())
	}
	if perms := info.Mode().Perm(); perms&0o077 != 0 {
		return "", true, fmt.Errorf("%w: %s has mode %#o (must be %s or stricter)",
			ErrSecretPermissions, secretRef, perms, "0600")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", true, fmt.Errorf("read secret %s: %w", secretRef, err)
	}
	// Trim only ONE trailing newline. After that, the value MUST
	// be single-line: Docker --env-file cannot safely represent
	// arbitrary multi-line values with the current KEY=value
	// writer, so embedded newlines or carriage returns are a
	// rejected configuration. The error names the secret_ref
	// but never the value.
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	if strings.ContainsAny(s, "\n\r") {
		return "", true, fmt.Errorf("%w: %s", ErrSecretMultiline, secretRef)
	}
	return s, true, nil
}

// envFilePerm is the permission of the temp env file we hand to
// docker --env-file. The file is created in the daemon-owned
// runtime directory (Config.RuntimeDir) with mode 0600 and removed
// after `docker create` succeeds; even if it lingers, only the
// owning user can read it.
const envFilePerm = 0o600

// MaterializeEnvFile writes the supplied env values to a temp file
// inside runtimeDir with mode 0600 and returns its path. The file
// is intended to be passed to `docker run --env-file <path>` and
// deleted immediately afterwards (caller's responsibility). Secret
// values reach the file in plaintext but the file's lifetime is
// bounded to the deploy call.
//
// The file lives in runtimeDir — a dedicated daemon-owned
// directory outside any per-deploy worktree — so it is not
// affected by worktree cleanup, not on a shared mount that may be
// reaped by another process, and not alongside source code that
// might be copied or backed up before the deploy finishes.
//
// Optional-secret semantics: an env entry with Required=false
// whose secret file is MISSING is silently skipped (no entry is
// written for it, no error is returned). An env entry whose
// secret file is present but malformed or insecure is always
// rejected, regardless of Required — that is a configuration bug
// the operator must fix.
func MaterializeEnvFile(runtimeDir string, entries []EnvEntry, secretDir string) (path string, err error) {
	if len(entries) == 0 {
		return "", nil
	}
	if runtimeDir == "" {
		return "", fmt.Errorf("env file: empty runtimeDir")
	}
	// Defensive: the daemon pre-creates RuntimeDir at startup.
	// The full security check (no symlinks incl. parent
	// components, trusted owner, mode 0700-or-stricter, safe-
	// create on missing path) lives in EnsureRuntimeDir. An
	// "unsafe but existing" runtime directory is rejected with
	// the matching sentinel — never silently chmod'd or chown'd.
	if err := EnsureRuntimeDir(runtimeDir); err != nil {
		return "", err
	}
	var buf strings.Builder
	for _, e := range entries {
		v, found, lerr := LoadSecretOptional(secretDir, e.SecretRef)
		switch {
		case lerr != nil:
			// Present-but-broken: always fail, regardless of
			// Required. A malformed secret is a configuration
			// bug that the operator must fix; silently skipping
			// it would mask the bug.
			return "", fmt.Errorf("env %s: %w", e.Name, lerr)
		case !found:
			if e.Required {
				return "", fmt.Errorf("env %s: %w", e.Name, ErrSecretMissing)
			}
			// Optional + missing: skip silently (no entry written).
			continue
		}
		// The values are joined without quoting. Docker's
		// --env-file parser interprets lines as KEY=value
		// (single-line). Values are taken verbatim up to the
		// first newline, which the loader has already trimmed;
		// embedded newlines and carriage returns are rejected
		// at load time. Internal whitespace is preserved.
		fmt.Fprintf(&buf, "%s=%s\n", e.Name, v)
	}
	// Use os.CreateTemp in the trusted runtimeDir so the name is
	// unique across concurrent deploys and the file is created
	// with the exact permission we want (no separate chmod). The
	// runtimeDir is owned by the agentctld user, so 0o600 gives
	// the file the tightest possible default visibility. Docker
	// reads the file as the same user during docker run.
	f, err := os.CreateTemp(runtimeDir, ".agentctld-env-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create env file: %w", err)
	}
	if err := f.Chmod(envFilePerm); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("chmod env file: %w", err)
	}
	name := f.Name()
	if _, err := f.WriteString(buf.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("write env file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close env file: %w", err)
	}
	return name, nil
}

// EnsureRuntimeDir sanity-checks the daemon's runtime directory
// (a per-deploy scratch space that briefly contains plaintext
// secret values during a deploy call) and safe-creates it on
// first use. The runtime dir is held to the same standard as the
// secret directory: no symlinks at the path or any parent
// component, real directory, owned by root or the daemon's own
// uid, mode 0700-or-stricter. A missing directory is safe-
// created at 0700 — only the leaf, never MkdirAll — and the
// parent must already exist; otherwise the call fails with
// ErrRuntimeDirMissing. After a successful create the directory
// is re-stat'd and re-validated so a symlink inserted between
// the pre-check and the mkdir is caught. Existing unsafe
// directories are rejected with the matching sentinel rather
// than silently chmod'd/chown'd.
//
// EnsureRuntimeDir is the only place the daemon creates the
// runtime directory: cmd/agentctld calls it at startup and
// MaterializeEnvFile re-calls it on every deploy so a runtime
// dir that drifted out of compliance after startup is caught
// on the next deploy rather than silently used.
func EnsureRuntimeDir(runtimeDir string) error {
	if runtimeDir == "" {
		return fmt.Errorf("env file: empty runtimeDir")
	}
	if err := noSymlinkAt(runtimeDir, ErrRuntimeDirSymlink); err != nil {
		return err
	}
	info, err := os.Stat(runtimeDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s: %v", ErrRuntimeDirMissing, runtimeDir, err)
		}
		// Safe-create: only the leaf; refuse if any parent is
		// missing so we cannot materialise an arbitrary tree.
		// Also reject symlinks at any parent component: creating
		// through one would land the new dir in an attacker-
		// controlled location. (noSymlinkAt's missing-path
		// early-return only fires when the *target* itself is
		// missing, so we must check the parent explicitly here.)
		parent := filepath.Dir(runtimeDir)
		if err := noSymlinkAt(parent, ErrRuntimeDirSymlink); err != nil {
			return err
		}
		if _, perr := os.Stat(parent); perr != nil {
			return fmt.Errorf("%w: %s: %v", ErrRuntimeDirMissing, runtimeDir, perr)
		}
		if mkErr := os.Mkdir(runtimeDir, 0o700); mkErr != nil {
			return fmt.Errorf("%w: %s: %v", ErrRuntimeDirMissing, runtimeDir, mkErr)
		}
		info, err = os.Stat(runtimeDir)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrRuntimeDirMissing, runtimeDir, err)
		}
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrRuntimeDirMissing, runtimeDir)
	}
	uid := uidFromFileInfo(info)
	if !trustedOwner(uid) {
		return fmt.Errorf("%w: %s has uid %d (only uid 0 or %d accepted)",
			ErrRuntimeDirOwner, runtimeDir, uid, currentUID())
	}
	if perms := info.Mode().Perm(); perms&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %#o (must be 0700 or stricter)",
			ErrRuntimeDirPermissions, runtimeDir, perms)
	}
	return nil
}

// uidFromFileInfo extracts the uid from a FileInfo's Sys(). The
// supported shape on unix-like systems is syscall.Stat_t, whose
// Uid field exposes the numeric owner. Platforms that don't
// carry a uid (e.g. Windows) fall through to -1, which always
// fails trustedOwner; the daemon does not run on Windows today.
func uidFromFileInfo(info os.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
