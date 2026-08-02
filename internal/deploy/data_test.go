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
	got, err := AppDataDir(cfg, "myapp", false)
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
	if _, err := AppDataDir(DataConfig{}, "myapp", false); !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestAppDataDir_RejectsBadAppName(t *testing.T) {
	cfg := validDataConfig(t)
	for _, bad := range []string{"", "Myapp", "-bad", "1bad", "bad-", "a/b", "..", "."} {
		if _, err := AppDataDir(cfg, bad, false); !errors.Is(err, ErrInvalidDataConfig) {
			t.Errorf("AppDataDir(%q): expected ErrInvalidDataConfig, got %v", bad, err)
		}
	}
}

func TestAppDataDir_AcceptsTwoCharName(t *testing.T) {
	cfg := validDataConfig(t)
	if _, err := AppDataDir(cfg, "a1", false); err != nil {
		t.Errorf("expected accept a1, got %v", err)
	}
}

func TestAppDataDir_AcceptsThirtyTwoCharName(t *testing.T) {
	cfg := validDataConfig(t)
	name := "a" + strings.Repeat("b", 30) + "1"
	if _, err := AppDataDir(cfg, name, false); err != nil {
		t.Errorf("expected accept 32-char name, got %v", err)
	}
}

func TestAppDataDir_RejectsThirtyThreeCharName(t *testing.T) {
	cfg := validDataConfig(t)
	name := "a" + strings.Repeat("b", 31) + "1"
	if _, err := AppDataDir(cfg, name, false); !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestEnsureAppDataDir_CreatesDir(t *testing.T) {
	cfg := validDataConfig(t)
	data, err := EnsureAppDataDir(cfg, "myapp", false)
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
	first, err := EnsureAppDataDir(cfg, "myapp", false)
	if err != nil {
		t.Fatalf("first EnsureAppDataDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.HostPath, "marker.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	second, err := EnsureAppDataDir(cfg, "myapp", false)
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
	if _, err := EnsureAppDataDir(cfg, "myapp", false); err != nil {
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
	_, err := EnsureAppDataDir(cfg, "myapp", false)
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
	_, err := EnsureAppDataDir(cfg, "myapp", false)
	if !errors.Is(err, ErrInvalidDataConfig) {
		t.Errorf("expected ErrInvalidDataConfig, got %v", err)
	}
}

func TestRemoveAppDataDir_RequiresForceForNonEmpty(t *testing.T) {
	cfg := validDataConfig(t)
	data, err := EnsureAppDataDir(cfg, "myapp", false)
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
	data, err := EnsureAppDataDir(cfg, "myapp", false)
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
	if _, err := EnsureAppDataDir(cfg, "myapp", false); err != nil {
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
	// AppDataDir is a pure-derive helper: it does not touch
	// disk. A symlinked DataRoot does not change the path it
	// returns. The actual symlink rejection happens in
	// EnsureAppDataDir and RemoveAppDataDir — see the
	// *RejectsSymlinkedDataRoot tests below.
	real := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "root")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := DataConfig{DataRoot: link}
	data, err := AppDataDir(cfg, "myapp", false)
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
	if _, err := EnsureAppDataDir(cfg, "myapp", false); !errors.Is(err, ErrSymlinkedAppData) {
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
	if _, err := EnsureAppDataDir(cfg, "myapp", false); !errors.Is(err, ErrSymlinkedAppData) {
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
