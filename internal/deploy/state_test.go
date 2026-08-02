package deploy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validDeployment returns a fully-derived Deployment that satisfies
// every identity check.
func validDeployment(app, commit string, hostPort, containerPort int) Deployment {
	return Deployment{
		App:           app,
		Commit:        commit,
		Image:         deriveImage(app, commit),
		ContainerName: deriveContainerName(app, commit),
		HostPort:      hostPort,
		ContainerPort: containerPort,
		Hostname:      app + ".apps.simonontheweb.de",
		Upstream:      "127.0.0.1:49152",
		DeployedAt:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
	}
}

func TestSaveDeployment_FirstDeployment(t *testing.T) {
	dir := t.TempDir()
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	dep.Upstream = "127.0.0.1:49152" // matches hostPort

	if err := SaveDeployment(dir, dep); err != nil {
		t.Fatalf("SaveDeployment: %v", err)
	}

	state, err := LoadDeploymentState(dir, "myapp")
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
	dir := t.TempDir()
	first := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	first.DeployedAt = time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	if err := SaveDeployment(dir, first); err != nil {
		t.Fatalf("SaveDeployment(first): %v", err)
	}

	second := validDeployment("myapp", strings.Repeat("b", 40), 49153, 8080)
	second.DeployedAt = time.Date(2026, 8, 2, 10, 5, 0, 0, time.UTC)
	if err := SaveDeployment(dir, second); err != nil {
		t.Fatalf("SaveDeployment(second): %v", err)
	}

	state, err := LoadDeploymentState(dir, "myapp")
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
	dir := t.TempDir()
	first := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	if err := SaveDeployment(dir, first); err != nil {
		t.Fatalf("SaveDeployment(first): %v", err)
	}

	second := validDeployment("myapp", strings.Repeat("b", 40), 49153, 8080)
	if err := SaveDeployment(dir, second); err != nil {
		t.Fatalf("SaveDeployment(second): %v", err)
	}

	// The temp file pattern must leave no temp files behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".write") {
			t.Errorf("unexpected leftover temp file: %s", name)
		}
		if name != "myapp.state.json" {
			t.Errorf("unexpected file in state dir: %s", name)
		}
	}

	// The file must contain the new deployment as current.
	state, err := LoadDeploymentState(dir, "myapp")
	if err != nil {
		t.Fatalf("LoadDeploymentState: %v", err)
	}
	if state.Current.Commit != second.Commit {
		t.Errorf("current.commit = %q, want %q", state.Current.Commit, second.Commit)
	}
}

func TestLoadDeploymentState_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "myapp.state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(dir, "myapp")
	if !errors.Is(err, ErrCorruptDeploymentState) {
		t.Errorf("expected ErrCorruptDeploymentState, got %v", err)
	}
}

func TestLoadDeploymentState_UnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
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
		},
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "myapp.state.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(dir, "myapp")
	if !errors.Is(err, ErrUnsupportedStateVersion) {
		t.Errorf("expected ErrUnsupportedStateVersion, got %v", err)
	}
}

func TestLoadDeploymentState_FabricatedIdentity(t *testing.T) {
	dir := t.TempDir()
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
		},
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "myapp.state.json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadDeploymentState(dir, "myapp")
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState, got %v", err)
	}
}

func TestLoadDeploymentState_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "myapp.state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := LoadDeploymentState(dir, "myapp")
	if !errors.Is(err, ErrSymlinkedStateFile) {
		t.Errorf("expected ErrSymlinkedStateFile, got %v", err)
	}
}

func TestSaveDeployment_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "myapp.state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	err := SaveDeployment(dir, dep)
	if !errors.Is(err, ErrSymlinkedStateFile) {
		t.Errorf("expected ErrSymlinkedStateFile, got %v", err)
	}
}

func TestLoadDeploymentState_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadDeploymentState(dir, "myapp")
	if !errors.Is(err, ErrDeploymentStateNotFound) {
		t.Errorf("expected ErrDeploymentStateNotFound, got %v", err)
	}
}

func TestSaveDeployment_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	dep := validDeployment("../etc", strings.Repeat("a", 40), 49152, 8080)
	err := SaveDeployment(dir, dep)
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for path traversal app, got %v", err)
	}
}

func TestSaveDeployment_RejectsFabricatedInput(t *testing.T) {
	dir := t.TempDir()
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	dep.Image = "agentctl/myapp:deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	err := SaveDeployment(dir, dep)
	if !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for fabricated image, got %v", err)
	}
}

func TestDeleteDeploymentState_SafeDeletion(t *testing.T) {
	dir := t.TempDir()
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	if err := SaveDeployment(dir, dep); err != nil {
		t.Fatalf("SaveDeployment: %v", err)
	}

	// Successful deletion.
	if err := DeleteDeploymentState(dir, "myapp"); err != nil {
		t.Fatalf("DeleteDeploymentState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "myapp.state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should be gone, stat err = %v", err)
	}

	// Deleting again returns ErrDeploymentStateNotFound (safe: not
	// a silent success on a missing file).
	err := DeleteDeploymentState(dir, "myapp")
	if !errors.Is(err, ErrDeploymentStateNotFound) {
		t.Errorf("expected ErrDeploymentStateNotFound on second delete, got %v", err)
	}

	// Path traversal app name rejected.
	if err := DeleteDeploymentState(dir, "../etc"); !errors.Is(err, ErrInvalidDeploymentState) {
		t.Errorf("expected ErrInvalidDeploymentState for traversal app, got %v", err)
	}
}

func TestDeleteDeploymentState_SymlinkNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "myapp.state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := DeleteDeploymentState(dir, "myapp"); !errors.Is(err, ErrSymlinkedStateFile) {
		t.Errorf("expected ErrSymlinkedStateFile, got %v", err)
	}
	// Target must still exist (we refused to follow the symlink).
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target should still exist: %v", err)
	}
}

func TestSaveDeployment_OverwritesCorruptStateRefused(t *testing.T) {
	// If the existing state file is corrupt, SaveDeployment must
	// refuse to overwrite it — otherwise a buggy save could silently
	// destroy the only record of the previous deployment.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "myapp.state.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dep := validDeployment("myapp", strings.Repeat("a", 40), 49152, 8080)
	err := SaveDeployment(dir, dep)
	if !errors.Is(err, ErrCorruptDeploymentState) {
		t.Errorf("expected ErrCorruptDeploymentState, got %v", err)
	}
	// The corrupt file must still be there.
	data, err := os.ReadFile(filepath.Join(dir, "myapp.state.json"))
	if err != nil {
		t.Fatalf("read corrupt file: %v", err)
	}
	if string(data) != "{not valid json" {
		t.Errorf("corrupt file was overwritten")
	}
}
