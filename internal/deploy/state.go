package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// deploymentStateVersion is the only version this layer writes or
// accepts on read. A state file declaring any other version is
// rejected with ErrUnsupportedStateVersion so a future bump can
// migrate deliberately rather than silently misinterpret old data.
const deploymentStateVersion = 1

// Sentinel errors returned by the deployment-state layer.
var (
	ErrInvalidDeploymentState  = errors.New("invalid deployment state")
	ErrCorruptDeploymentState  = errors.New("corrupt deployment state")
	ErrUnsupportedStateVersion = errors.New("unsupported deployment state version")
	ErrDeploymentStateNotFound = errors.New("deployment state not found")
	ErrSymlinkedStateFile      = errors.New("deployment state file is a symlink")
)

// Deployment captures the identity of one deployed instance. Every
// field except DeployedAt is derived from validated inputs and the
// fixed naming rules; callers cannot inject arbitrary names.
type Deployment struct {
	App           string    `json:"app"`
	Commit        string    `json:"commit"`
	Image         string    `json:"image"`
	ContainerName string    `json:"container_name"`
	HostPort      int       `json:"host_port"`
	ContainerPort int       `json:"container_port"`
	Hostname      string    `json:"hostname"`
	Upstream      string    `json:"upstream"`
	DeployedAt    time.Time `json:"deployed_at"`
}

// DeploymentState is the persisted record for a single app. Exactly
// one file is written per app; the file holds both the current and
// the previous deployment so a future rollback layer can target
// either without re-deriving identity from the runtime layer.
type DeploymentState struct {
	Version  int         `json:"version"`
	App      string      `json:"app"`
	Current  *Deployment `json:"current"`
	Previous *Deployment `json:"previous,omitempty"`
}

// SaveDeployment persists dep as the current deployment for its app
// in dir. If a previous current deployment exists and passes
// identity validation, it is moved to the previous slot. The write
// is atomic (temp file + fsync + rename + directory fsync) and
// refuses to write through a symlinked state file.
//
// The supplied dep is validated before disk is touched: the app
// must match the app-name regex, the commit must be exactly 40
// lowercase hex characters, image and container name must equal the
// derived values, and the ports must be in range. Any mismatch
// returns ErrInvalidDeploymentState without touching disk.
func SaveDeployment(dir string, dep Deployment) error {
	if err := validateDeployment(dep); err != nil {
		return err
	}
	if dir == "" {
		return fmt.Errorf("%w: dir is required", ErrInvalidDeploymentState)
	}

	path, err := stateFilePath(dir, dep.App)
	if err != nil {
		return err
	}

	// Refuse to write through a symlinked state file. If the file
	// does not exist, that's fine — we'll create it.
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkedStateFile, path)
		}
	}

	// Load existing state (if any) to move current to previous.
	// A missing file is expected on first deployment; any other
	// load error (corrupt, fabricated, unsupported) aborts the
	// save so we never silently overwrite a state we can't read.
	var previous *Deployment
	if existing, err := LoadDeploymentState(dir, dep.App); err == nil {
		previous = existing.Current
	} else if !errors.Is(err, ErrDeploymentStateNotFound) {
		return err
	}

	state := DeploymentState{
		Version:  deploymentStateVersion,
		App:      dep.App,
		Current:  &dep,
		Previous: previous,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: marshal: %v", ErrInvalidDeploymentState, err)
	}
	data = append(data, '\n')

	if err := atomicWriteSync(path, data); err != nil {
		return fmt.Errorf("%w: write %s: %v", ErrInvalidDeploymentState, path, err)
	}
	return nil
}

// LoadDeploymentState reads the persisted state for app from dir.
// The state file must exist (ErrDeploymentStateNotFound otherwise),
// must not be a symlink, must be parseable JSON
// (ErrCorruptDeploymentState otherwise), must declare the supported
// version (ErrUnsupportedStateVersion otherwise), and must pass
// identity validation (ErrInvalidDeploymentState otherwise).
func LoadDeploymentState(dir string, app string) (*DeploymentState, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: dir is required", ErrInvalidDeploymentState)
	}
	if !appNameRe.MatchString(app) {
		return nil, fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidDeploymentState, app)
	}

	path, err := stateFilePath(dir, app)
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrDeploymentStateNotFound, path)
		}
		return nil, fmt.Errorf("%w: stat %s: %v", ErrInvalidDeploymentState, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrSymlinkedStateFile, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrDeploymentStateNotFound, path)
		}
		return nil, fmt.Errorf("%w: read %s: %v", ErrCorruptDeploymentState, path, err)
	}

	var state DeploymentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptDeploymentState, err)
	}

	if state.Version != deploymentStateVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedStateVersion, state.Version, deploymentStateVersion)
	}
	if state.App != app {
		return nil, fmt.Errorf("%w: file app %q does not match requested %q", ErrInvalidDeploymentState, state.App, app)
	}
	if state.Current == nil {
		return nil, fmt.Errorf("%w: current deployment is required", ErrInvalidDeploymentState)
	}
	if err := validateDeployment(*state.Current); err != nil {
		return nil, err
	}
	if state.Current.App != app {
		return nil, fmt.Errorf("%w: current.app %q does not match state app %q", ErrInvalidDeploymentState, state.Current.App, app)
	}
	if state.Previous != nil {
		if err := validateDeployment(*state.Previous); err != nil {
			return nil, err
		}
		if state.Previous.App != app {
			return nil, fmt.Errorf("%w: previous.app %q does not match state app %q", ErrInvalidDeploymentState, state.Previous.App, app)
		}
	}

	return &state, nil
}

// DeleteDeploymentState removes the state file for app from dir.
// The file must exist (ErrDeploymentStateNotFound otherwise) and
// must not be a symlink. The app name is validated against the
// app-name regex to prevent path traversal.
func DeleteDeploymentState(dir string, app string) error {
	if dir == "" {
		return fmt.Errorf("%w: dir is required", ErrInvalidDeploymentState)
	}
	if !appNameRe.MatchString(app) {
		return fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidDeploymentState, app)
	}

	path, err := stateFilePath(dir, app)
	if err != nil {
		return err
	}

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrDeploymentStateNotFound, path)
		}
		return fmt.Errorf("%w: stat %s: %v", ErrInvalidDeploymentState, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkedStateFile, path)
	}

	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrDeploymentStateNotFound, path)
		}
		return fmt.Errorf("%w: remove %s: %v", ErrInvalidDeploymentState, path, err)
	}
	return nil
}

// stateFilePath derives the state file path for an app in dir and
// confirms the resolved path stays within dir (defense in depth
// against a symlinked state directory).
func stateFilePath(dir, app string) (string, error) {
	if !appNameRe.MatchString(app) {
		return "", fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidDeploymentState, app)
	}
	path := filepath.Join(dir, app+".state.json")
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: abs dir: %v", ErrInvalidDeploymentState, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: abs path: %v", ErrInvalidDeploymentState, err)
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return "", fmt.Errorf("%w: rel: %v", ErrInvalidDeploymentState, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path traversal detected", ErrInvalidDeploymentState)
	}
	return path, nil
}

// validateDeployment checks that d's identity fields match the fixed
// naming rules. Used both before writing (to reject fabricated
// inputs) and after reading (to detect tampered state files).
func validateDeployment(d Deployment) error {
	if !appNameRe.MatchString(d.App) {
		return fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidDeploymentState, d.App)
	}
	if !shaRe.MatchString(d.Commit) {
		return fmt.Errorf("%w: commit %q is not exactly 40 lowercase hex characters", ErrInvalidDeploymentState, d.Commit)
	}
	if !hostPortRe.MatchString(strconv.Itoa(d.HostPort)) {
		return fmt.Errorf("%w: host port %d is not a valid port", ErrInvalidDeploymentState, d.HostPort)
	}
	if d.ContainerPort < 1024 || d.ContainerPort > 65535 {
		return fmt.Errorf("%w: container port %d out of range [1024, 65535]", ErrInvalidDeploymentState, d.ContainerPort)
	}
	expectedImage := deriveImage(d.App, d.Commit)
	if d.Image != expectedImage {
		return fmt.Errorf("%w: image %q does not match derived %q", ErrInvalidDeploymentState, d.Image, expectedImage)
	}
	expectedContainer := deriveContainerName(d.App, d.Commit)
	if d.ContainerName != expectedContainer {
		return fmt.Errorf("%w: container name %q does not match derived %q", ErrInvalidDeploymentState, d.ContainerName, expectedContainer)
	}
	return nil
}

// atomicWriteSync writes data to path atomically: open temp file,
// write, fsync, close, rename into place, then fsync the parent
// directory so the rename is durable across a crash. The temp file
// lives in the same directory as path so the rename is atomic.
func atomicWriteSync(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// fsync the parent directory so the rename is durable.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
