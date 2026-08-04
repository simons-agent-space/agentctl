package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testBaseDomain is the trusted base domain every test derives
// hostname and upstream from. Keeping it in one place keeps the
// derived values consistent across tests.
const testBaseDomain = "apps.simonontheweb.de"

// validDeployment returns a fully-derived Deployment that satisfies
// every identity check against testBaseDomain.
func validDeployment(app, commit string, hostPort, containerPort int) Deployment {
	return Deployment{
		App:           app,
		Commit:        commit,
		Image:         deriveImage(app, commit),
		ContainerName: deriveContainerName(app, commit),
		HostPort:      hostPort,
		ContainerPort: containerPort,
		Hostname:      app + "." + testBaseDomain,
		Upstream:      fmt.Sprintf("127.0.0.1:%d", hostPort),
		DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
	}
}

// validStateConfig returns a StateConfig that points at a fresh
// temp directory and carries the test base domain.
func validStateConfig(t *testing.T) StateConfig {
	t.Helper()
	return StateConfig{
		StateDir:   t.TempDir(),
		BaseDomain: testBaseDomain,
	}
}

func TestSaveDeployment_FirstDeployment(t *testing.T) {
	cfg := validStateConfig(t)
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)

	if err := SaveDeployment(cfg, dep); err != nil {
		t.Fatalf("SaveDeployment: %v", err)
	}

	state, err := LoadDeploymentState(cfg, "myapp")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Version != deploymentStateVersion {
		t.Errorf("version = %d, want %d", state.Version, deploymentStateVersion)
	}
	if state.App != "myapp" {
		t.Errorf("app = %q, want myapp", state.App)
	}
	if state.Current == nil {
		t.Fatalf("current is nil")
	}
	if state.Previous != nil {
		t.Errorf("previous should be nil on first deployment, got %+v", state.Previous)
	}
	if state.Current.App != dep.App || state.Current.Commit != dep.Commit || state.Current.Image != dep.Image {
		t.Errorf("current mismatch: %+v", state.Current)
	}
}

func TestSaveDeployment_SecondDeploymentMovesCurrentToPrevious(t *testing.T) {
	cfg := validStateConfig(t)
	first := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	first.DeployedAt = time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	if err := SaveDeployment(cfg, first); err != nil {
		t.Fatalf("SaveDeployment(first): %v", err)
	}

	second := validDeployment("myapp", strings.Repeat("b", 40), 49153, 8080)
	second.DeployedAt = time.Date(2026, 8, 2, 10, 5, 0, 0, time.UTC)
	if err := SaveDeployment(cfg, second); err != nil {
		t.Fatalf("SaveDeployment(second): %v", err)
	}

	state, err := LoadDeploymentState(cfg, "myapp")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil || state.Previous == nil {
		t.Fatalf("current=%+v previous=%+v", state.Current, state.Previous)
	}
	if state.Current.Commit != second.Commit {
		t.Errorf("current.commit = %q, want %q", state.Current.Commit, second.Commit)
	}
	if state.Previous.Commit != first.Commit {
		t.Errorf("previous.commit = %q, want %q", state.Previous.Commit, first.Commit)
	}
}

func TestSaveDeployment_AtomicReplacementNoTempLeft(t *testing.T) {
	cfg := validStateConfig(t)
	first := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	if err := SaveDeployment(cfg, first); err != nil {
		t.Fatalf("SaveDeployment(first): %v", err)
	}

	second := validDeployment("myapp", strings.Repeat("b", 40), 49153, 8080)
	if err := SaveDeployment(cfg, second); err != nil {
		t.Fatalf("SaveDeployment(second): %v", err)
	}

	// The temp file pattern must leave no temp files behind.
	entries, err := os.ReadDir(cfg.StateDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "state-") || strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".write") {
			t.Errorf("unexpected leftover temp file: %s", name)
		}
		if name != "myapp.state.json" {
			t.Errorf("unexpected file in state dir: %s", name)
		}
	}

	state, err := LoadDeploymentState(cfg, "myapp")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current.Commit != second.Commit {
		t.Errorf("current.commit = %q, want %q", state.Current.Commit, second.Commit)
	}
}

func TestLoadDeploymentState_CorruptJSON(t *testing.T) {
	cfg := validStateConfig(t)
	path := filepath.Join(cfg.StateDir, "myapp.state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrCorruptDeploymentState) {
		t.Errorf("expected ErrCorruptDeploymentState, got %v", err)
	}
}

func TestLoadDeploymentState_UnsupportedVersion(t *testing.T) {
	cfg := validStateConfig(t)
	state := DeploymentState{
		Version: 99,
		App:     "myapp",
		Current: &Deployment{
			App:           "myapp",
			Commit:        strings.Repeat("a", 40),
			Image:         deriveImage("myapp", strings.Repeat("a", 40)),
			ContainerName: deriveContainerName("myapp", strings.Repeat("a", 40)),
			HostPort:      49152,
			ContainerPort: 8080,
			Hostname:      "myapp." + testBaseDomain,
			Upstream:      "127.0.0.1:49152",
			DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
		},
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "myapp.state.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrUnsupportedStateVersion) {
		t.Errorf("expected ErrUnsupportedStateVersion, got %v", err)
	}
}

func TestLoadDeploymentState_FabricatedIdentity(t *testing.T) {
	cfg := validStateConfig(t)
	commit := strings.Repeat("a", 40)
	// Tamper: image does not equal the derived value for the
	// declared app+commit.
	state := DeploymentState{
		Version: deploymentStateVersion,
		App:     "myapp",
		Current: &Deployment{
			App:           "myapp",
			Commit:        commit,
			Image:         "agentctl/myapp:deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			ContainerName: deriveContainerName("myapp", commit),
			HostPort:      49152,
			ContainerPort: 8080,
			Hostname:      "myapp." + testBaseDomain,
			Upstream:      "127.0.0.1:49152",
			DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
		},
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "myapp.state.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState, got %v", err)
	}
}

func TestSaveDeployment_FabricatedUpstreamRejected(t *testing.T) {
	cfg := validStateConfig(t)
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	dep.Upstream = "0.0.0.0:49152" // does not match 127.0.0.1:HostPort
	err := SaveDeployment(cfg, dep)
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for fabricated upstream, got %v", err)
	}
	// Disk must be untouched.
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "myapp.state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file must not exist after rejected save, stat err = %v", err)
	}
}

func TestSaveDeployment_FabricatedHostnameRejected(t *testing.T) {
	cfg := validStateConfig(t)
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	dep.Hostname = "evil." + testBaseDomain // hostname must equal app+"."+baseDomain
	err := SaveDeployment(cfg, dep)
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for fabricated hostname, got %v", err)
	}
}

func TestSaveDeployment_ZeroTimestampRejected(t *testing.T) {
	cfg := validStateConfig(t)
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	dep.DeployedAt = time.Time{} // zero value
	err := SaveDeployment(cfg, dep)
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for zero timestamp, got %v", err)
	}
}

func TestLoadDeploymentState_FabricatedUpstreamRejected(t *testing.T) {
	cfg := validStateConfig(t)
	commit := strings.Repeat("a", 40)
	state := DeploymentState{
		Version: deploymentStateVersion,
		App:     "myapp",
		Current: &Deployment{
			App:           "myapp",
			Commit:        commit,
			Image:         deriveImage("myapp", commit),
			ContainerName: deriveContainerName("myapp", commit),
			HostPort:      49152,
			ContainerPort: 8080,
			Hostname:      "myapp." + testBaseDomain,
			Upstream:      "0.0.0.0:49152", // fabricated
			DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
		},
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "myapp.state.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for fabricated upstream, got %v", err)
	}
}

func TestLoadDeploymentState_FabricatedHostnameRejected(t *testing.T) {
	cfg := validStateConfig(t)
	commit := strings.Repeat("a", 40)
	state := DeploymentState{
		Version: deploymentStateVersion,
		App:     "myapp",
		Current: &Deployment{
			App:           "myapp",
			Commit:        commit,
			Image:         deriveImage("myapp", commit),
			ContainerName: deriveContainerName("myapp", commit),
			HostPort:      49152,
			ContainerPort: 8080,
			Hostname:      "evil." + testBaseDomain, // fabricated
			Upstream:      "127.0.0.1:49152",
			DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
		},
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "myapp.state.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for fabricated hostname, got %v", err)
	}
}

func TestLoadDeploymentState_ZeroTimestampRejected(t *testing.T) {
	cfg := validStateConfig(t)
	commit := strings.Repeat("a", 40)
	state := DeploymentState{
		Version: deploymentStateVersion,
		App:     "myapp",
		Current: &Deployment{
			App:           "myapp",
			Commit:        commit,
			Image:         deriveImage("myapp", commit),
			ContainerName: deriveContainerName("myapp", commit),
			HostPort:      49152,
			ContainerPort: 8080,
			Hostname:      "myapp." + testBaseDomain,
			Upstream:      "127.0.0.1:49152",
			DeployedAt:    time.Time{}, // zero
		},
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "myapp.state.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for zero timestamp, got %v", err)
	}
}

func TestLoadDeploymentState_PreviousValidatedAgainstBaseDomain(t *testing.T) {
	cfg := validStateConfig(t)
	commit1 := strings.Repeat("a", 40)
	commit2 := strings.Repeat("b", 40)
	// Previous was persisted with a hostname that does not match
	// the current trusted base domain. Load must reject.
	state := DeploymentState{
		Version: deploymentStateVersion,
		App:     "myapp",
		Current: &Deployment{
			App:           "myapp",
			Commit:        commit2,
			Image:         deriveImage("myapp", commit2),
			ContainerName: deriveContainerName("myapp", commit2),
			HostPort:      49153,
			ContainerPort: 8080,
			Hostname:      "myapp." + testBaseDomain,
			Upstream:      "127.0.0.1:49153",
			DeployedAt:    time.Date(2026, 8, 2, 10, 5, 0, 0, time.UTC),
		},
		Previous: &Deployment{
			App:           "myapp",
			Commit:        commit1,
			Image:         deriveImage("myapp", commit1),
			ContainerName: deriveContainerName("myapp", commit1),
			HostPort:      49152,
			ContainerPort: 8080,
			Hostname:      "myapp.evil.example.com", // fabricated
			Upstream:      "127.0.0.1:49152",
			DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
		},
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "myapp.state.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for fabricated previous hostname, got %v", err)
	}
}

func TestLoadDeploymentState_SymlinkRejected(t *testing.T) {
	cfg := validStateConfig(t)
	target := filepath.Join(cfg.StateDir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(cfg.StateDir, "myapp.state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrSymlinkedStateFile) {
		t.Errorf("expected ErrSymlinkedStateFile, got %v", err)
	}
}

func TestSaveDeployment_SymlinkRejected(t *testing.T) {
	cfg := validStateConfig(t)
	target := filepath.Join(cfg.StateDir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(cfg.StateDir, "myapp.state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	err := SaveDeployment(cfg, dep)
	if !errors.Is(err, ErrSymlinkedStateFile) {
		t.Errorf("expected ErrSymlinkedStateFile, got %v", err)
	}
}

func TestLoadDeploymentState_Missing(t *testing.T) {
	cfg := validStateConfig(t)
	_, err := LoadDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrDeploymentStateNotFound) {
		t.Errorf("expected ErrDeploymentStateNotFound, got %v", err)
	}
}

func TestSaveDeployment_RejectsPathTraversal(t *testing.T) {
	cfg := validStateConfig(t)
	dep := validDeployment("../etc", strings.Repeat("a", 40), 49152, 8080)
	err := SaveDeployment(cfg, dep)
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for path traversal app, got %v", err)
	}
}

func TestSaveDeployment_RejectsFabricatedInput(t *testing.T) {
	cfg := validStateConfig(t)
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	dep.Image = "agentctl/myapp:deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	err := SaveDeployment(cfg, dep)
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for fabricated image, got %v", err)
	}
}

func TestSaveDeployment_RequiresStateConfig(t *testing.T) {
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	if err := SaveDeployment(StateConfig{}, dep); !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for empty cfg, got %v", err)
	}
	if err := SaveDeployment(StateConfig{StateDir: "/tmp", BaseDomain: "no-tld"}, dep); !errors.Is(err, ErrInvalidDomain) {
		t.Errorf("expected ErrInvalidDomain for invalid base domain, got %v", err)
	}
}

func TestSaveDeployment_PredictableTempSymlinkNotFollowed(t *testing.T) {
	// The old atomic-write path used <file>.tmp as a predictable
	// temp file name. An attacker who pre-placed that path as a
	// symlink could redirect the write. The new path uses
	// os.CreateTemp (random suffix), so the predictable path must
	// never be opened.
	cfg := validStateConfig(t)
	sentinel := filepath.Join(cfg.StateDir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	// Pre-create the predictable temp path as a symlink to the
	// sentinel. If atomicWriteSync opens this path with
	// O_CREATE|O_TRUNC, the sentinel would be truncated.
	predictableTmp := filepath.Join(cfg.StateDir, "myapp.state.json.tmp")
	if err := os.Symlink(sentinel, predictableTmp); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	if err := SaveDeployment(cfg, dep); err != nil {
		t.Fatalf("SaveDeployment: %v", err)
	}

	// The sentinel must be untouched: the save never wrote through
	// the predictable symlink.
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(got) != "untouched" {
		t.Errorf("sentinel was modified via predictable temp symlink: %q", got)
	}

	// The state file must exist with the expected content.
	state, err := LoadDeploymentState(cfg, "myapp")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil || state.Current.Commit != dep.Commit {
		t.Errorf("state mismatch: %+v", state.Current)
	}
}

func TestDeleteDeploymentState_SafeDeletion(t *testing.T) {
	cfg := validStateConfig(t)
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	if err := SaveDeployment(cfg, dep); err != nil {
		t.Fatalf("SaveDeployment: %v", err)
	}

	if err := DeleteDeploymentState(cfg, "myapp"); err != nil {
		t.Fatalf("DeleteDeploymentState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "myapp.state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should be gone, stat err = %v", err)
	}

	err := DeleteDeploymentState(cfg, "myapp")
	if !errors.Is(err, ErrDeploymentStateNotFound) {
		t.Errorf("expected ErrDeploymentStateNotFound on second delete, got %v", err)
	}

	if err := DeleteDeploymentState(cfg, "../etc"); !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for traversal app, got %v", err)
	}
}

func TestDeleteDeploymentState_SymlinkNotFollowed(t *testing.T) {
	cfg := validStateConfig(t)
	target := filepath.Join(cfg.StateDir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(cfg.StateDir, "myapp.state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := DeleteDeploymentState(cfg, "myapp"); !errors.Is(err, ErrSymlinkedStateFile) {
		t.Errorf("expected ErrSymlinkedStateFile, got %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target should still exist: %v", err)
	}
}

func TestSaveDeployment_OverwritesCorruptStateRefused(t *testing.T) {
	cfg := validStateConfig(t)
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "myapp.state.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	err := SaveDeployment(cfg, dep)
	if !errors.Is(err, ErrCorruptDeploymentState) {
		t.Errorf("expected ErrCorruptDeploymentState, got %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, "myapp.state.json"))
	if err != nil {
		t.Fatalf("read corrupt file: %v", err)
	}
	if string(data) != "{not valid json" {
		t.Errorf("corrupt file was overwritten")
	}
}

func TestSaveDeployment_PersistsMountDataAndReadOnly(t *testing.T) {
	cfg := validStateConfig(t)
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	dep.MountData = true
	dep.MountReadOnly = true

	if err := SaveDeployment(cfg, dep); err != nil {
		t.Fatalf("SaveDeployment: %v", err)
	}

	state, err := LoadDeploymentState(cfg, "myapp")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil {
		t.Fatalf("current is nil")
	}
	if !state.Current.MountData || !state.Current.MountReadOnly {
		t.Errorf("current mount fields not preserved: %+v", state.Current)
	}

	// And the raw JSON on disk should contain both fields.
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, "myapp.state.json"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if !strings.Contains(string(data), `"mount_data": true`) {
		t.Errorf("expected mount_data:true in JSON, got: %s", data)
	}
	if !strings.Contains(string(data), `"mount_read_only": true`) {
		t.Errorf("expected mount_read_only:true in JSON, got: %s", data)
	}
}

func TestLoadDeploymentState_BackwardCompatibleWithoutDataFields(t *testing.T) {
	// A state file written by an older agentctl that did not know
	// about the data mount fields must still load with MountData=false
	// and MountReadOnly=false (the natural zero values). The version
	// number is unchanged; the new fields are optional on read.
	cfg := validStateConfig(t)
	legacy := `{
  "version": 1,
  "app": "myapp",
  "current": {
    "app": "myapp",
    "commit": "` + strings.Repeat("a", 40) + `",
    "image": "agentctl/myapp:` + strings.Repeat("a", 40) + `",
    "container_name": "agentctl-myapp-` + strings.Repeat("a", 12) + `",
    "host_port": 49152,
    "container_port": 8080,
    "hostname": "myapp.apps.simonontheweb.de",
    "upstream": "127.0.0.1:49152",
    "deployed_at": "2026-08-02T10:00:00Z"
  }
}`
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "myapp.state.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("seed legacy state: %v", err)
	}
	state, err := LoadDeploymentState(cfg, "myapp")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current == nil {
		t.Fatalf("current is nil")
	}
	if state.Current.MountData || state.Current.MountReadOnly {
		t.Errorf("legacy state should have mount fields false, got %+v", state.Current)
	}
}
