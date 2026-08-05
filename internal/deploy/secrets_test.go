package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSecret writes content to secretDir/<ref> with the given mode and
// returns the absolute path. Caller is responsible for the directory.
func writeSecret(t *testing.T, dir, ref string, content []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, ref)
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// os.WriteFile is subject to umask; chmod explicitly so the test
	// sees the exact mode regardless of the test runner's umask.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}

func TestLoadSecret_ValidFileWith0600(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o600)

	got, err := LoadSecret(dir, "foo.key")
	if err != nil {
		t.Fatalf("LoadSecret: %v", err)
	}
	if got != "supersecret" {
		t.Errorf("LoadSecret = %q, want %q (trailing newline trimmed)", got, "supersecret")
	}
}

func TestLoadSecret_PreservesNoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret"), 0o600)
	got, err := LoadSecret(dir, "foo.key")
	if err != nil {
		t.Fatalf("LoadSecret: %v", err)
	}
	if got != "supersecret" {
		t.Errorf("LoadSecret = %q, want %q", got, "supersecret")
	}
}

func TestLoadSecret_AcceptsStricterPerms(t *testing.T) {
	// 0o400 (read-only by owner) is stricter than 0o600 and must be accepted.
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o400)
	got, err := LoadSecret(dir, "foo.key")
	if err != nil {
		t.Fatalf("LoadSecret: %v", err)
	}
	if got != "supersecret" {
		t.Errorf("LoadSecret = %q, want %q", got, "supersecret")
	}
}

func TestLoadSecret_RejectsPermsOpenGroup(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o640)
	_, err := LoadSecret(dir, "foo.key")
	if !errors.Is(err, ErrSecretPermissions) {
		t.Errorf("expected ErrSecretPermissions, got %v", err)
	}
}

func TestLoadSecret_RejectsPermsOpenOther(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o604)
	_, err := LoadSecret(dir, "foo.key")
	if !errors.Is(err, ErrSecretPermissions) {
		t.Errorf("expected ErrSecretPermissions, got %v", err)
	}
}

func TestLoadSecret_RejectsPerms0666(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o666)
	_, err := LoadSecret(dir, "foo.key")
	if !errors.Is(err, ErrSecretPermissions) {
		t.Errorf("expected ErrSecretPermissions, got %v", err)
	}
}

func TestLoadSecret_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "real-secret")
	if err := os.WriteFile(target, []byte("supersecret\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "foo.key")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := LoadSecret(dir, "foo.key")
	if !errors.Is(err, ErrSecretSymlink) {
		t.Errorf("expected ErrSecretSymlink, got %v", err)
	}
}

func TestLoadSecret_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "foo.key"), 0o600); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := LoadSecret(dir, "foo.key")
	if !errors.Is(err, ErrSecretNotRegular) {
		t.Errorf("expected ErrSecretNotRegular, got %v", err)
	}
}

func TestLoadSecret_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadSecret(dir, "does-not-exist.key")
	if !errors.Is(err, ErrSecretMissing) {
		t.Errorf("expected ErrSecretMissing, got %v", err)
	}
}

func TestLoadSecret_RejectsBadSecretRef(t *testing.T) {
	dir := t.TempDir()
	cases := []string{
		"../escape",
		"with/slash",
		"..dotdot",
		".dot",
		"",
		strings.Repeat("a", 65),
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			_, err := LoadSecret(dir, ref)
			if err == nil {
				t.Errorf("expected error for secret_ref %q", ref)
			}
		})
	}
}

func TestIsSecretConfigured_TrueForValidFile(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o600)
	if !IsSecretConfigured(dir, "foo.key") {
		t.Errorf("IsSecretConfigured = false, want true")
	}
}

func TestIsSecretConfigured_FalseForMissing(t *testing.T) {
	dir := t.TempDir()
	if IsSecretConfigured(dir, "does-not-exist.key") {
		t.Errorf("IsSecretConfigured = true, want false")
	}
}

func TestIsSecretConfigured_FalseForSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "real-secret")
	if err := os.WriteFile(target, []byte("supersecret\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "foo.key")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if IsSecretConfigured(dir, "foo.key") {
		t.Errorf("IsSecretConfigured = true, want false (symlink)")
	}
}

func TestIsSecretConfigured_FalseForBadPerms(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o644)
	if IsSecretConfigured(dir, "foo.key") {
		t.Errorf("IsSecretConfigured = true, want false (perms 0o644)")
	}
}

func TestIsSecretConfigured_FalseForBadSecretRef(t *testing.T) {
	dir := t.TempDir()
	if IsSecretConfigured(dir, "../escape") {
		t.Errorf("IsSecretConfigured = true, want false (bad secret_ref)")
	}
	if IsSecretConfigured(dir, "") {
		t.Errorf("IsSecretConfigured = true, want false (empty secret_ref)")
	}
}

func TestValidateSecretDir_AcceptsExistingDirectory(t *testing.T) {
	// ValidateSecretDir now enforces 0700-or-stricter on the
	// directory; t.TempDir() is 0755 so we must chmod it for the
	// happy path to pass. In production the daemon's loadConfig
	// creates its runtime/secret dirs at 0700.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	if err := ValidateSecretDir(dir); err != nil {
		t.Errorf("ValidateSecretDir = %v, want nil", err)
	}
}

func TestValidateSecretDir_AcceptsEmpty(t *testing.T) {
	// Empty SecretDir is the legacy "no secrets" case and must not
	// crash startup. It is only meaningful once a manifest opts
	// into env; the deploy layer fails the deploy in that case.
	if err := ValidateSecretDir(""); err != nil {
		t.Errorf("ValidateSecretDir(\"\") = %v, want nil", err)
	}
}

func TestValidateSecretDir_RejectsMissingPath(t *testing.T) {
	err := ValidateSecretDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if !errors.Is(err, ErrSecretDirInvalid) {
		t.Errorf("expected ErrSecretDirInvalid, got %v", err)
	}
}

func TestValidateSecretDir_RejectsRegularFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := ValidateSecretDir(f)
	if !errors.Is(err, ErrSecretDirInvalid) {
		t.Errorf("expected ErrSecretDirInvalid, got %v", err)
	}
}

// TestValidateSecretDir_RejectsSymlink verifies the directory
// itself must not be a symlink. The check uses Lstat (not Stat)
// so the kernel does not follow a redirected secret root into an
// attacker-controlled tree.
func TestValidateSecretDir_RejectsSymlink(t *testing.T) {
	linkParent := t.TempDir()
	target := t.TempDir()
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatalf("chmod target: %v", err)
	}
	link := filepath.Join(linkParent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := ValidateSecretDir(link)
	if !errors.Is(err, ErrSecretDirSymlink) {
		t.Errorf("expected ErrSecretDirSymlink, got %v", err)
	}
	// The symlink target must be untouched.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target was modified: %v", err)
	}
}

// TestValidateSecretDir_RejectsWorldWritable verifies the
// directory must be 0700 or stricter (no group, no other access
// at all). The spec test name is RejectsWorldWritable; the
// underlying check is "any group/other bit set", so 0o755 (group
// + other readable) and 0o777 (everything) are both rejected.
func TestValidateSecretDir_RejectsWorldWritable(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o775, 0o777} {
		t.Run(fmt.Sprintf("mode_%#o", mode), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatalf("chmod %s %s: %v", dir, mode, err)
			}
			err := ValidateSecretDir(dir)
			if !errors.Is(err, ErrSecretDirPermissions) {
				t.Errorf("expected ErrSecretDirPermissions for mode %#o, got %v", mode, err)
			}
		})
	}
}

// TestValidateSecretDir_RejectsGroupReadable verifies 0750 is also
// rejected for the same reason: an unprivileged user in the
// owning group could read secret files.
func TestValidateSecretDir_RejectsGroupReadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	err := ValidateSecretDir(dir)
	if !errors.Is(err, ErrSecretDirPermissions) {
		t.Errorf("expected ErrSecretDirPermissions, got %v", err)
	}
}

// TestValidateSecretDir_RejectsUntrustedOwner verifies the
// trusted-owner check. In production this catches an unprivileged
// user who creates the secret directory and plants a known
// secret_ref filename. We exercise the rejection path by
// overriding the currentUID hook — the sandbox cannot actually
// chown to a different uid without CAP_SETUID.
func TestValidateSecretDir_RejectsUntrustedOwner(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	orig := currentUID
	currentUID = func() int { return 1000 }
	t.Cleanup(func() { currentUID = orig })
	// The dir was created by the test process so its real owner
	// is the test uid. With the hook returning 1000 the file is
	// "trusted"; with the hook returning a *different* uid the
	// file is "untrusted". Use 2000 (also must not match the
	// test runner's actual uid; the test would otherwise be a
	// silent no-op).
	currentUID = func() int { return 2000 }
	err := ValidateSecretDir(dir)
	if !errors.Is(err, ErrSecretDirOwner) {
		t.Errorf("expected ErrSecretDirOwner, got %v", err)
	}
}

// TestValidateSecretDir_AcceptsOwningUid verifies the trusted-
// owner rule: a directory owned by the same uid as the daemon
// passes the check. This is the case in production when the
// daemon created (or systemd provisioned) the secret dir.
func TestValidateSecretDir_AcceptsOwningUid(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Use the temp dir's actual owner uid so this test passes
	// regardless of which uid the test runner runs as. CI
	// runners (GitHub Actions ubuntu-24.04) run tests as
	// uid 1001; local dev is often uid 1000; other CI images
	// pick yet other uids. Hardcoding 1000 couples the test
	// to a specific host layout and breaks on every other
	// uid — see PR #15 CI failure ("uid 1001 ... only uid 0
	// or 1000 accepted").
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	ownerUID := uidFromFileInfo(info)
	orig := currentUID
	currentUID = func() int { return ownerUID }
	t.Cleanup(func() { currentUID = orig })
	if err := ValidateSecretDir(dir); err != nil {
		t.Errorf("ValidateSecretDir (own-uid trusted): %v, want nil", err)
	}
}

// TestLoadSecret_RejectsUntrustedOwner verifies the per-file
// trusted-owner check. Same hook-based test as the directory
// variant.
func TestLoadSecret_RejectsUntrustedOwner(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o600)
	orig := currentUID
	currentUID = func() int { return 2000 }
	t.Cleanup(func() { currentUID = orig })
	_, err := LoadSecret(dir, "foo.key")
	if !errors.Is(err, ErrSecretFileOwner) {
		t.Errorf("expected ErrSecretFileOwner, got %v", err)
	}
}

// TestIsSecretConfigured_FalseForUntrustedOwner verifies the
// inspect-side projection of the trusted-owner check.
func TestIsSecretConfigured_FalseForUntrustedOwner(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("supersecret\n"), 0o600)
	orig := currentUID
	currentUID = func() int { return 2000 }
	t.Cleanup(func() { currentUID = orig })
	if IsSecretConfigured(dir, "foo.key") {
		t.Errorf("IsSecretConfigured = true, want false (untrusted owner)")
	}
}

// TestLoadSecretOptional_MissingReturnsFalse verifies the new
// optional loader: missing files come back with found=false and
// no error.
func TestLoadSecretOptional_MissingReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	v, found, err := LoadSecretOptional(dir, "absent.key")
	if err != nil {
		t.Errorf("LoadSecretOptional: %v, want nil", err)
	}
	if found {
		t.Errorf("LoadSecretOptional found=true, want false")
	}
	if v != "" {
		t.Errorf("LoadSecretOptional value=%q, want empty", v)
	}
}

// TestLoadSecretOptional_PresentBrokenReturnsError verifies a
// present-but-broken file is reported with found=true AND an
// error. The orchestrator uses this to refuse optional secrets
// that exist but fail their security checks.
func TestLoadSecretOptional_PresentBrokenReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "bad.key", []byte("x"), 0o644) // wrong perms
	_, found, err := LoadSecretOptional(dir, "bad.key")
	if err == nil {
		t.Errorf("LoadSecretOptional err=nil, want ErrSecretPermissions")
	}
	if !errors.Is(err, ErrSecretPermissions) {
		t.Errorf("LoadSecretOptional err=%v, want errors.Is(ErrSecretPermissions)", err)
	}
	if !found {
		t.Errorf("LoadSecretOptional found=false, want true (file IS present)")
	}
}

// ---------- MaterializeEnvFile ----------

func TestMaterializeEnvFile_CreatesFileWith0600(t *testing.T) {
	runtimeDir := t.TempDir()
	// Runtime dir is hardened to 0700 so EnsureRuntimeDir accepts it.
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	secretDir := t.TempDir()
	writeSecret(t, secretDir, "foo.key", []byte("foo-value\n"), 0o600)
	writeSecret(t, secretDir, "bar.key", []byte("bar-value\n"), 0o600)

	path, err := MaterializeEnvFile(runtimeDir, []EnvEntry{
		{Name: "FOO", SecretRef: "foo.key", Required: true},
		{Name: "BAR", SecretRef: "bar.key", Required: true},
	}, secretDir)
	if err != nil {
		t.Fatalf("MaterializeEnvFile: %v", err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("env file perms = %#o, want 0o600", info.Mode().Perm())
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("env file is a symlink")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "FOO=foo-value\nBAR=bar-value\n"
	if string(content) != want {
		t.Errorf("env file content = %q, want %q", content, want)
	}
	// The env file MUST live under runtimeDir, not anywhere else.
	if !strings.HasPrefix(path, runtimeDir+string(os.PathSeparator)) {
		t.Errorf("env file path %q is not under runtimeDir %q", path, runtimeDir)
	}
}

func TestMaterializeEnvFile_FailsBeforeCreateWhenRequiredSecretMissing(t *testing.T) {
	runtimeDir := t.TempDir()
	// Runtime dir is hardened to 0700 so EnsureRuntimeDir accepts it.
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	secretDir := t.TempDir()
	writeSecret(t, secretDir, "foo.key", []byte("foo-value\n"), 0o600)
	// bar.key missing AND required -> ErrSecretMissing in chain.

	path, err := MaterializeEnvFile(runtimeDir, []EnvEntry{
		{Name: "FOO", SecretRef: "foo.key", Required: true},
		{Name: "BAR", SecretRef: "bar.key", Required: true},
	}, secretDir)
	if err == nil {
		_ = os.Remove(path)
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, ErrSecretMissing) {
		t.Errorf("expected ErrSecretMissing, got %v", err)
	}
	// No file should have been created in runtimeDir.
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("runtimeDir has %d entries after failed materialize, want 0", len(entries))
	}
}

// TestMaterializeEnvFile_SkipsOptionalMissing verifies the
// required/optional split: an env entry with Required=false
// whose secret file is missing is silently skipped (no entry
// written for it, no error returned).
func TestMaterializeEnvFile_SkipsOptionalMissing(t *testing.T) {
	runtimeDir := t.TempDir()
	// Runtime dir is hardened to 0700 so EnsureRuntimeDir accepts it.
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	secretDir := t.TempDir()
	writeSecret(t, secretDir, "foo.key", []byte("foo-value\n"), 0o600)
	// bar.key missing AND optional -> silently skipped.

	path, err := MaterializeEnvFile(runtimeDir, []EnvEntry{
		{Name: "FOO", SecretRef: "foo.key", Required: true},
		{Name: "BAR", SecretRef: "bar.key"}, // default Required=false
	}, secretDir)
	if err != nil {
		t.Fatalf("MaterializeEnvFile: %v", err)
	}
	defer os.Remove(path)

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "FOO=foo-value\n" // BAR omitted entirely
	if string(content) != want {
		t.Errorf("env file content = %q, want %q (BAR must be omitted)", content, want)
	}
	// Belt-and-braces: no BAR=... line at all.
	if strings.Contains(string(content), "BAR=") {
		t.Errorf("env file unexpectedly contains BAR= line: %s", content)
	}
}

// TestMaterializeEnvFile_RejectsInsecureOptional verifies that
// an optional secret that is PRESENT but fails the security
// checks still fails the materialize — present-but-broken is a
// config bug, not a missing file.
func TestMaterializeEnvFile_RejectsInsecureOptional(t *testing.T) {
	runtimeDir := t.TempDir()
	// Runtime dir is hardened to 0700 so EnsureRuntimeDir accepts it.
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	secretDir := t.TempDir()
	writeSecret(t, secretDir, "foo.key", []byte("foo-value\n"), 0o644) // world-readable
	// foo.key is present but insecure: regardless of Required,
	// the materialize must fail.
	_, err := MaterializeEnvFile(runtimeDir, []EnvEntry{
		{Name: "FOO", SecretRef: "foo.key"}, // optional
	}, secretDir)
	if err == nil {
		t.Fatalf("expected error for insecure optional secret")
	}
	if !errors.Is(err, ErrSecretPermissions) {
		t.Errorf("expected ErrSecretPermissions, got %v", err)
	}
}

func TestMaterializeEnvFile_EmptyEntriesReturnsEmptyPath(t *testing.T) {
	runtimeDir := t.TempDir()
	// Runtime dir is hardened to 0700 so EnsureRuntimeDir accepts it.
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	path, err := MaterializeEnvFile(runtimeDir, nil, t.TempDir())
	if err != nil {
		t.Fatalf("MaterializeEnvFile: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
}

func TestMaterializeEnvFile_RejectsBadSecretRef(t *testing.T) {
	runtimeDir := t.TempDir()
	// Runtime dir is hardened to 0700 so EnsureRuntimeDir accepts it.
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	_, err := MaterializeEnvFile(runtimeDir, []EnvEntry{
		{Name: "FOO", SecretRef: "../escape"},
	}, t.TempDir())
	if err == nil {
		t.Errorf("expected error for bad secret_ref")
	}
}

// TestMaterializeEnvFile_SafeCreatesMissingRuntimeDir verifies
// the safe-create path: a runtime directory that does not exist
// is created at mode 0700 and validated before use. The parent
// must already exist.
func TestMaterializeEnvFile_SafeCreatesMissingRuntimeDir(t *testing.T) {
	parent := t.TempDir()
	runtimeDir := filepath.Join(parent, "newly-created")
	secretDir := t.TempDir()
	writeSecret(t, secretDir, "foo.key", []byte("foo-value\n"), 0o600)

	path, err := MaterializeEnvFile(runtimeDir, []EnvEntry{
		{Name: "FOO", SecretRef: "foo.key", Required: true},
	}, secretDir)
	if err != nil {
		t.Fatalf("MaterializeEnvFile: %v", err)
	}
	defer os.Remove(path)

	info, err := os.Stat(runtimeDir)
	if err != nil {
		t.Fatalf("stat runtime dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("runtime dir is not a directory")
	}
	if perms := info.Mode().Perm(); perms != 0o700 {
		t.Errorf("runtime dir perms = %#o, want 0o700", perms)
	}
}

// TestMaterializeEnvFile_RejectsMissingParent verifies the
// safe-create refuses when the runtime dir's parent does not
// exist: only the leaf is created, never an arbitrary tree.
func TestMaterializeEnvFile_RejectsMissingParent(t *testing.T) {
	secretDir := t.TempDir()
	writeSecret(t, secretDir, "foo.key", []byte("foo-value\n"), 0o600)
	runtimeDir := "/tmp/agentctl-missing-parent-please-does-not-exist/runtime"
	_, err := MaterializeEnvFile(runtimeDir, []EnvEntry{
		{Name: "FOO", SecretRef: "foo.key", Required: true},
	}, secretDir)
	if !errors.Is(err, ErrRuntimeDirMissing) {
		t.Errorf("expected ErrRuntimeDirMissing, got %v", err)
	}
}

// TestMaterializeEnvFile_RuntimeDirNotWorktree verifies the env
// file is created in runtimeDir, not anywhere else (specifically
// not in a worktree-style path).
func TestMaterializeEnvFile_RuntimeDirNotWorktree(t *testing.T) {
	runtimeDir := t.TempDir()
	// Runtime dir is hardened to 0700 so EnsureRuntimeDir accepts it.
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	worktree := t.TempDir()
	secretDir := t.TempDir()
	writeSecret(t, secretDir, "foo.key", []byte("foo-value\n"), 0o600)
	path, err := MaterializeEnvFile(runtimeDir, []EnvEntry{
		{Name: "FOO", SecretRef: "foo.key", Required: true},
	}, secretDir)
	if err != nil {
		t.Fatalf("MaterializeEnvFile: %v", err)
	}
	defer os.Remove(path)
	if !strings.HasPrefix(path, runtimeDir+string(os.PathSeparator)) {
		t.Errorf("env file path %q is not under runtimeDir %q", path, runtimeDir)
	}
	if strings.HasPrefix(path, worktree) {
		t.Errorf("env file path %q leaked under worktree %q", path, worktree)
	}
}

// -- RuntimeDir validation tests (added per final review) -----------------

// TestEnsureRuntimeDir_RejectsUntrustedOwner verifies the
// trusted-owner rule for the runtime directory. Same hook-based
// technique as the secret-dir equivalent: the runtime dir is
// created by the test runner, but the test injects a different
// uid via currentUID() so the dir appears "untrusted".
func TestEnsureRuntimeDir_RejectsUntrustedOwner(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	orig := currentUID
	currentUID = func() int { return 2000 }
	t.Cleanup(func() { currentUID = orig })
	err := EnsureRuntimeDir(dir)
	if !errors.Is(err, ErrRuntimeDirOwner) {
		t.Errorf("expected ErrRuntimeDirOwner, got %v", err)
	}
}

func TestEnsureRuntimeDir_AcceptsValidDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := EnsureRuntimeDir(dir); err != nil {
		t.Errorf("EnsureRuntimeDir(%q) = %v, want nil", dir, err)
	}
}

func TestEnsureRuntimeDir_RejectsSymlink(t *testing.T) {
	parent := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := EnsureRuntimeDir(link); err == nil {
		t.Errorf("EnsureRuntimeDir on symlink expected error, got nil")
	} else if !errors.Is(err, ErrRuntimeDirSymlink) {
		t.Errorf("error = %v, want ErrRuntimeDirSymlink", err)
	}
}

func TestEnsureRuntimeDir_RejectsSymlinkInParent(t *testing.T) {
	parent := t.TempDir()
	realSub := filepath.Join(parent, "realsub")
	if err := os.Mkdir(realSub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkParent := filepath.Join(parent, "linkdir")
	if err := os.Symlink(realSub, linkParent); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target := filepath.Join(linkParent, "env")
	err := EnsureRuntimeDir(target)
	if err == nil {
		t.Errorf("EnsureRuntimeDir under symlinked parent expected error, got nil")
	} else if !errors.Is(err, ErrRuntimeDirSymlink) {
		t.Errorf("error = %v, want ErrRuntimeDirSymlink", err)
	}
}

func TestEnsureRuntimeDir_RejectsOverlyPermissiveMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	err := EnsureRuntimeDir(dir)
	if err == nil {
		t.Errorf("EnsureRuntimeDir on 0o755 expected error, got nil")
	} else if !errors.Is(err, ErrRuntimeDirPermissions) {
		t.Errorf("error = %v, want ErrRuntimeDirPermissions", err)
	}
}

func TestEnsureRuntimeDir_CreatesMissingDirectory(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	missing := filepath.Join(parent, "will-be-created")
	if _, err := os.Stat(missing); err == nil {
		t.Fatalf("path already exists: %s", missing)
	}
	if err := EnsureRuntimeDir(missing); err != nil {
		t.Errorf("EnsureRuntimeDir(%q) = %v, want nil", missing, err)
	}
	info, err := os.Stat(missing)
	if err != nil {
		t.Fatalf("post-create stat: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("post-create is not a dir")
	}
	if perms := info.Mode().Perm(); perms&0o077 != 0 {
		t.Errorf("post-create perms = %#o, must be 0o700 or stricter", perms)
	}
}

// -- env-secret single-line enforcement (added per final review) -------

func TestLoadSecretOptional_RejectsEmbeddedNewline(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("ab\ncd\n"), 0o600)
	_, found, err := LoadSecretOptional(dir, "foo.key")
	if err == nil {
		t.Fatalf("expected error for embedded newline, got nil (found=%v)", found)
	}
	if !errors.Is(err, ErrSecretMultiline) {
		t.Errorf("error = %v, want ErrSecretMultiline", err)
	}
}

func TestLoadSecretOptional_RejectsEmbeddedCarriageReturn(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "foo.key", []byte("ab\rcd\n"), 0o600)
	_, found, err := LoadSecretOptional(dir, "foo.key")
	if err == nil {
		t.Fatalf("expected error for embedded CR, got nil (found=%v)", found)
	}
	if !errors.Is(err, ErrSecretMultiline) {
		t.Errorf("error = %v, want ErrSecretMultiline", err)
	}
}
