package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validDataConfig returns a DataConfig pointing at a fresh temp dir.
func validDataConfig(t *testing.T) DataConfig {
	t.Helper()
	return DataConfig{DataRoot: t.TempDir()}
}

func TestAppDataDir_DerivesExpectedPath(t *testing.T) {
	cfg := validDataConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.DataRoot, "myapp", "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := AppDataDir(cfg, "myapp")
	if err != nil {
		t.Fatalf("AppDataDir: %v", err)
	}
	want := filepath.Join(cfg.DataRoot, "myapp", "data")
	if got.HostPath != want {
		t.Errorf("HostPath = %q, want %q", got.HostPath, want)
	}
	if got.ContainerPath != "/data" {
		t.Errorf("ContainerPath = %q, want /data", got.ContainerPath)
	}
	if got.App != "myapp" {
		t.Errorf("App = %q, want myapp", got.App)
	}
}

func TestAppDataDir_RejectsEmptyDataRoot(t *testing.T) {
	if _, err := AppDataDir(DataConfig{}, "myapp"); !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestAppDataDir_RejectsBadAppName(t *testing.T) {
	cfg := validDataConfig(t)
	for _, bad := range []string{"", "Myapp", "-bad", "1bad", "bad-", "a/b", "..", "."} {
		if _, err := AppDataDir(cfg, bad); !errors.Is(err, ErrInvalidDataConfig) {
			t.Errorf("AppDataDir(%q): expected ErrInvalidDataConfig, got %v", bad, err)
		}
	}
}

func TestAppDataDir_AcceptsTwoCharName(t *testing.T) {
	cfg := validDataConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.DataRoot, "a1", "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := AppDataDir(cfg, "a1"); err != nil {
		t.Errorf("expected accept a1, got %v", err)
	}
}

func TestAppDataDir_AcceptsThirtyTwoCharName(t *testing.T) {
	cfg := validDataConfig(t)
	name := "a" + strings.Repeat("b", 30) + "1"
	if err := os.MkdirAll(filepath.Join(cfg.DataRoot, name, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := AppDataDir(cfg, name); err != nil {
		t.Errorf("expected accept 32-char name, got %v", err)
	}
}

func TestAppDataDir_RejectsThirtyThreeCharName(t *testing.T) {
	cfg := validDataConfig(t)
	name := "a" + strings.Repeat("b", 31) + "1"
	if _, err := AppDataDir(cfg, name); !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestEnsureAppDataDir_CreatesDir(t *testing.T) {
	cfg := validDataConfig(t)
	data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true})
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	info, err := os.Stat(data.HostPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected directory at %s", data.HostPath)
	}
	// Parent <DataRoot>/myapp should also exist.
	parent := filepath.Join(cfg.DataRoot, "myapp")
	if pInfo, err := os.Stat(parent); err != nil || !pInfo.IsDir() {
		t.Errorf("expected parent dir %s: %v", parent, err)
	}
}

func TestEnsureAppDataDir_Idempotent(t *testing.T) {
	cfg := validDataConfig(t)
	first, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true})
	if err != nil {
		t.Fatalf("first EnsureAppDataDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.HostPath, "marker.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	second, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true})
	if err != nil {
		t.Fatalf("second EnsureAppDataDir: %v", err)
	}
	if first.HostPath != second.HostPath {
		t.Errorf("path changed: %s vs %s", first.HostPath, second.HostPath)
	}
	if _, err := os.Stat(filepath.Join(second.HostPath, "marker.txt")); err != nil {
		t.Errorf("marker file lost across idempotent call: %v", err)
	}
}

func TestEnsureAppDataDir_LeavesExistingDataAlone(t *testing.T) {
	cfg := validDataConfig(t)
	host := filepath.Join(cfg.DataRoot, "myapp", "data")
	if err := os.MkdirAll(host, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(host, "user-data.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true}); err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	// Existing file must still be there with its original content.
	b, err := os.ReadFile(filepath.Join(host, "user-data.json"))
	if err != nil {
		t.Fatalf("read existing: %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("existing data modified: %q", b)
	}
}

func TestEnsureAppDataDir_RejectsSymlinkedDataDir(t *testing.T) {
	cfg := validDataConfig(t)
	host := filepath.Join(cfg.DataRoot, "myapp", "data")
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, host); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true})
	if !errors.Is(err, ErrSymlinkedAppData) {
		t.Errorf("expected ErrSymlinkedAppData, got %v", err)
	}
	// Target must be untouched.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target was modified: %v", err)
	}
}

func TestEnsureAppDataDir_RejectsRegularFileAtDataPath(t *testing.T) {
	cfg := validDataConfig(t)
	host := filepath.Join(cfg.DataRoot, "myapp", "data")
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(host, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true})
	if !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestRemoveAppDataDir_RequiresForceForNonEmpty(t *testing.T) {
	cfg := validDataConfig(t)
	data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true})
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(data.HostPath, "user-data.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = RemoveAppDataDir(cfg, "myapp", false)
	if !errors.Is(err, ErrAppDataNotEmpty) {
		t.Errorf("expected ErrAppDataNotEmpty, got %v", err)
	}
	// Dir and contents must still exist.
	if _, err := os.Stat(data.HostPath); err != nil {
		t.Errorf("data dir was removed without force: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data.HostPath, "user-data.json")); err != nil {
		t.Errorf("user-data was removed without force: %v", err)
	}
}

func TestRemoveAppDataDir_ForceRemovesContents(t *testing.T) {
	cfg := validDataConfig(t)
	data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true})
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(data.HostPath, "user-data.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := RemoveAppDataDir(cfg, "myapp", true); err != nil {
		t.Errorf("RemoveAppDataDir(force=true): %v", err)
	}
	if _, err := os.Stat(data.HostPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data dir still present after force remove: %v", err)
	}
}

func TestRemoveAppDataDir_EmptyDirSucceedsWithoutForce(t *testing.T) {
	cfg := validDataConfig(t)
	if _, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true}); err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if err := RemoveAppDataDir(cfg, "myapp", false); err != nil {
		t.Errorf("RemoveAppDataDir on empty dir: %v", err)
	}
}

func TestRemoveAppDataDir_NotFound(t *testing.T) {
	cfg := validDataConfig(t)
	err := RemoveAppDataDir(cfg, "myapp", true)
	if !errors.Is(err, ErrAppDataNotFound) {
		t.Errorf("expected ErrAppDataNotFound, got %v", err)
	}
}

func TestRemoveAppDataDir_RejectsSymlinkedDataDir(t *testing.T) {
	cfg := validDataConfig(t)
	host := filepath.Join(cfg.DataRoot, "myapp", "data")
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, host); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := RemoveAppDataDir(cfg, "myapp", true)
	if !errors.Is(err, ErrSymlinkedAppData) {
		t.Errorf("expected ErrSymlinkedAppData, got %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target was modified: %v", err)
	}
}

func TestRemoveAppDataDir_RejectsBadAppName(t *testing.T) {
	cfg := validDataConfig(t)
	if err := RemoveAppDataDir(cfg, "../escape", true); !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
	if err := RemoveAppDataDir(cfg, "BadName", true); !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestRemoveAppDataDir_RejectsEmptyDataRoot(t *testing.T) {
	if err := RemoveAppDataDir(DataConfig{}, "myapp", true); !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestDataContainerPathIsFixed(t *testing.T) {
	if dataContainerPath != "/data" {
		t.Errorf("dataContainerPath drifted: %q", dataContainerPath)
	}
}

func TestAppDataDir_AllowsSymlinkedDataRoot(t *testing.T) {
	// AppDataDir Lstats the derived path but does not create it.
	// A symlinked DataRoot changes the path it stats; it does
	// not change the result for an existing directory. The actual
	// symlink rejection happens in EnsureAppDataDir and
	// RemoveAppDataDir — see the *RejectsSymlinkedDataRoot tests
	// below.
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "myapp", "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "root")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := DataConfig{DataRoot: link}
	data, err := AppDataDir(cfg, "myapp")
	if err != nil {
		t.Fatalf("AppDataDir: %v", err)
	}
	want := filepath.Join(link, "myapp", "data")
	if data.HostPath != want {
		t.Errorf("HostPath = %q, want %q", data.HostPath, want)
	}
}

func TestEnsureAppDataDir_RejectsSymlinkedDataRoot(t *testing.T) {
	// A symlinked DataRoot could otherwise redirect MkdirAll
	// outside the trusted layout. The pre-MkdirAll symlink
	// check must catch it.
	real := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "root")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := DataConfig{DataRoot: link}
	if _, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true}); !errors.Is(err, ErrSymlinkedAppData) {
		t.Errorf("expected ErrSymlinkedAppData, got %v", err)
	}
	// Nothing must have been created inside the trusted real root.
	if _, err := os.Stat(filepath.Join(real, "myapp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data dir leaked into real root: %v", err)
	}
}

func TestEnsureAppDataDir_RejectsSymlinkedAppParent(t *testing.T) {
	// <DataRoot>/<app> as a symlink would let MkdirAll create
	// the data directory under the symlink target, outside the
	// trusted layout. The pre-MkdirAll check must catch it.
	dataDir := t.TempDir()
	appDir := filepath.Join(dataDir, "myapp")
	target := t.TempDir()
	if err := os.Symlink(target, appDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := DataConfig{DataRoot: dataDir}
	if _, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true}); !errors.Is(err, ErrSymlinkedAppData) {
		t.Errorf("expected ErrSymlinkedAppData, got %v", err)
	}
	// The symlink target must be untouched — no `data` directory
	// must have appeared inside it.
	if _, err := os.Stat(filepath.Join(target, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data dir leaked into symlink target: %v", err)
	}
}

func TestRemoveAppDataDir_RejectsSymlinkedDataRoot(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "myapp", "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "root")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := DataConfig{DataRoot: link}
	if err := RemoveAppDataDir(cfg, "myapp", true); !errors.Is(err, ErrSymlinkedAppData) {
		t.Errorf("expected ErrSymlinkedAppData, got %v", err)
	}
}

func TestRemoveAppDataDir_RejectsSymlinkedAppParent(t *testing.T) {
	// A symlinked <DataRoot>/<app> must not let RemoveAll
	// walk into the symlink target.
	dataDir := t.TempDir()
	appDir := filepath.Join(dataDir, "myapp")
	target := t.TempDir()
	if err := os.Symlink(target, appDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Seed the symlink target with a "data" directory that
	// must NOT be touched by RemoveAppDataDir.
	if err := os.MkdirAll(filepath.Join(target, "data"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "data", "user.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := DataConfig{DataRoot: dataDir}
	if err := RemoveAppDataDir(cfg, "myapp", true); !errors.Is(err, ErrSymlinkedAppData) {
		t.Errorf("expected ErrSymlinkedAppData, got %v", err)
	}
	// The user file inside the symlink target must survive.
	if _, err := os.Stat(filepath.Join(target, "data", "user.json")); err != nil {
		t.Errorf("user file was deleted through symlinked parent: %v", err)
	}
}

// ---------- host_source / v3 mounts ----------

// validDataConfigWithAllowlist is a helper that returns a DataConfig
// pointing at a fresh temp DataRoot plus an allowlist of additional
// host paths that may be mounted read-only.
func validDataConfigWithAllowlist(t *testing.T, allowlist ...string) DataConfig {
	t.Helper()
	return DataConfig{DataRoot: t.TempDir(), HostSourceAllowlist: allowlist}
}

func TestEnsureAppDataDir_LegacyMountUnchanged(t *testing.T) {
	// Sanity: a v2-style data field without host_source continues to
	// create the app-owned directory and respects md.ReadOnly.
	cfg := validDataConfig(t)
	data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, ReadOnly: false})
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if data.ReadOnly {
		t.Errorf("legacy mount: ReadOnly = true, want false (manifest has read_only=false)")
	}
	want := filepath.Join(cfg.DataRoot, "myapp", "data")
	if data.HostPath != want {
		t.Errorf("HostPath = %q, want %q", data.HostPath, want)
	}
}

func TestEnsureAppDataDir_ApprovedHostSourceMount(t *testing.T) {
	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "seed.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := validDataConfigWithAllowlist(t, hostDir)
	data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, HostSource: hostDir})
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if data.HostPath != hostDir {
		t.Errorf("HostPath = %q, want %q", data.HostPath, hostDir)
	}
	if !data.ReadOnly {
		t.Errorf("host-source mount: ReadOnly = false, want true")
	}
	// The host dir must be unchanged.
	if _, err := os.Stat(filepath.Join(hostDir, "seed.txt")); err != nil {
		t.Errorf("host_source was modified: %v", err)
	}
	// The app-owned directory must NOT have been created.
	legacyPath := filepath.Join(cfg.DataRoot, "myapp", "data")
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy app-owned data dir was created for host_source mount: %v", err)
	}
}

func TestEnsureAppDataDir_HostSourceWritableManifestStillForcesReadOnly(t *testing.T) {
	// Even when the manifest says read_only=false, host-source mounts
	// MUST be read-only. This is a hard invariant of the v3 contract.
	hostDir := t.TempDir()
	cfg := validDataConfigWithAllowlist(t, hostDir)
	data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{
		Mount:      true,
		HostSource: hostDir,
		ReadOnly:   false, // explicitly false; must be overridden
	})
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if !data.ReadOnly {
		t.Errorf("ReadOnly = false, want true (host-source mounts must always be read-only)")
	}
}

func TestEnsureAppDataDir_UnapprovedHostSourceRejected(t *testing.T) {
	hostDir := t.TempDir()
	// hostDir is NOT in the allowlist.
	cfg := validDataConfig(t)
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, HostSource: hostDir})
	if !errors.Is(err, ErrHostSourceDenied) {
		t.Errorf("expected ErrHostSourceDenied, got %v", err)
	}
}

func TestEnsureAppDataDir_HostSourceMissingRejected(t *testing.T) {
	missing := "/tmp/agentctl-does-not-exist-please-do-not-create-me"
	cfg := validDataConfigWithAllowlist(t, missing)
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, HostSource: missing})
	if !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestEnsureAppDataDir_HostSourceRegularFileRejected(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(hostFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := validDataConfigWithAllowlist(t, hostFile)
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, HostSource: hostFile})
	if !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestEnsureAppDataDir_HostSourceSymlinkRejected(t *testing.T) {
	real := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := validDataConfigWithAllowlist(t, link)
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, HostSource: link})
	if !errors.Is(err, ErrSymlinkedAppData) {
		t.Errorf("expected ErrSymlinkedAppData, got %v", err)
	}
	// The real directory must be untouched.
	if _, err := os.Stat(real); err != nil {
		t.Errorf("real directory was modified: %v", err)
	}
}

func TestEnsureAppDataDir_HostSourceSymlinkChainRejected(t *testing.T) {
	// Allowlist contains the symlink path, but the symlink points
	// at a directory that is NOT in the allowlist. The chain
	// rejection must still refuse the mount.
	real := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := validDataConfigWithAllowlist(t, link) // link allowed, real not
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, HostSource: link})
	if !errors.Is(err, ErrSymlinkedAppData) {
		t.Errorf("expected ErrSymlinkedAppData for symlink chain, got %v", err)
	}
}

func TestEnsureAppDataDir_HostSourceNonAbsoluteRejected(t *testing.T) {
	cfg := validDataConfigWithAllowlist(t, "relative/path")
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, HostSource: "relative/path"})
	if err == nil {
		t.Errorf("expected error for non-absolute host_source")
	}
}

func TestEnsureAppDataDir_ContainerPathDangerousRejected(t *testing.T) {
	hostDir := t.TempDir()
	cfg := validDataConfigWithAllowlist(t, hostDir)
	for _, p := range []string{"/", "/proc", "/sys", "/dev", "/run"} {
		t.Run(p, func(t *testing.T) {
			_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{
				Mount:         true,
				HostSource:    hostDir,
				ContainerPath: p,
			})
			if !errors.Is(err, ErrContainerPathBad) {
				t.Errorf("expected ErrContainerPathBad for %q, got %v", p, err)
			}
		})
	}
}

func TestEnsureAppDataDir_ContainerPathNonAbsoluteRejected(t *testing.T) {
	hostDir := t.TempDir()
	cfg := validDataConfigWithAllowlist(t, hostDir)
	_, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{
		Mount:         true,
		HostSource:    hostDir,
		ContainerPath: "data", // not absolute
	})
	if !errors.Is(err, ErrContainerPathBad) {
		t.Errorf("expected ErrContainerPathBad for non-absolute path, got %v", err)
	}
}

// TestValidateContainerPath_RejectsDescendants verifies the dangerous
// container-path deny list blocks both exact matches AND descendants
// of /proc, /sys, /dev, /run. The whole point of the deny list is
// to stop an operator from mounting a container path that hides a
// host kernel interface; descendant mounts would do exactly that,
// so they must be rejected too.
func TestValidateContainerPath_RejectsDescendants(t *testing.T) {
	for _, p := range []string{"/proc/sys", "/proc/1/cmdline", "/sys/fs/cgroup", "/dev/shm", "/dev/null", "/run/secrets", "/run/docker"} {
		t.Run(p, func(t *testing.T) {
			err := validateContainerPath(p)
			if !errors.Is(err, ErrContainerPathBad) {
				t.Errorf("expected ErrContainerPathBad for %q, got %v", p, err)
			}
		})
	}
}

// TestValidateContainerPath_AcceptsNonDangerousPaths verifies the
// counterpoint: ordinary container paths are accepted.
func TestValidateContainerPath_AcceptsNonDangerousPaths(t *testing.T) {
	for _, p := range []string{"/data", "/data/openclaw-state", "/srv/data", "/var/lib/app", "/openclaw"} {
		t.Run(p, func(t *testing.T) {
			if err := validateContainerPath(p); err != nil {
				t.Errorf("validateContainerPath(%q) = %v, want nil", p, err)
			}
		})
	}
}

func TestEnsureAppDataDir_ContainerPathSafeAccepted(t *testing.T) {
	hostDir := t.TempDir()
	cfg := validDataConfigWithAllowlist(t, hostDir)
	for _, p := range []string{"/data", "/srv/app", "/var/lib/data", "/openclaw-state", "/srv/openclaw/state/state"} {
		t.Run(p, func(t *testing.T) {
			data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{
				Mount:         true,
				HostSource:    hostDir,
				ContainerPath: p,
			})
			if err != nil {
				t.Errorf("expected accept %q, got %v", p, err)
			}
			if data == nil || data.ContainerPath != p {
				t.Errorf("ContainerPath = %v, want %q", data, p)
			}
		})
	}
}

func TestEnsureAppDataDir_HostSourceDefaultsContainerPathToData(t *testing.T) {
	hostDir := t.TempDir()
	cfg := validDataConfigWithAllowlist(t, hostDir)
	data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: true, HostSource: hostDir})
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if data.ContainerPath != "/data" {
		t.Errorf("ContainerPath = %q, want /data (default)", data.ContainerPath)
	}
}

func TestEnsureAppDataDir_NilMountNoOp(t *testing.T) {
	cfg := validDataConfig(t)
	data, err := EnsureAppDataDir(cfg, "myapp", nil)
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if data != nil {
		t.Errorf("data = %+v, want nil for nil manifest.Data", data)
	}
}

func TestEnsureAppDataDir_FalseMountNoOp(t *testing.T) {
	cfg := validDataConfig(t)
	data, err := EnsureAppDataDir(cfg, "myapp", &ManifestData{Mount: false})
	if err != nil {
		t.Fatalf("EnsureAppDataDir: %v", err)
	}
	if data != nil {
		t.Errorf("data = %+v, want nil for mount=false", data)
	}
}
