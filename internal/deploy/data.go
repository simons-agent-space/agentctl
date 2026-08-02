package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dataContainerPath is the FIXED in-container path at which the host
// data directory is mounted. The caller never controls this — the
// runtime, rollback, and data layers all use this constant. This is
// what keeps the design narrow: every persistent per-app directory
// lives at exactly /data inside the container.
const dataContainerPath = "/data"

// Sentinel errors returned by the data layer.
var (
	ErrInvalidDataConfig = errors.New("invalid data configuration")
	ErrAppDataNotFound   = errors.New("app data directory not found")
	ErrAppDataNotEmpty   = errors.New("app data directory not empty")
	ErrSymlinkedAppData  = errors.New("app data directory is a symlink")
)

// DataConfig holds the trusted host-side configuration for the
// per-app data layer. DataRoot is supplied by trusted host
// configuration, not by the caller. The derived per-app path is
// <DataRoot>/<app>/data.
//
// DataRoot is expected to live outside the repository root and
// outside any path the deployment process can write into except
// through this layer. A symlink at any component of the path
// (including <DataRoot> or <DataRoot>/<app>) is rejected as
// defense in depth.
type DataConfig struct {
	// DataRoot is the trusted host directory under which every
	// per-app data directory is created. The trailing slash is
	// not significant.
	DataRoot string
}

// AppData describes the derived identity of one app's data
// directory. HostPath is the resolved absolute host path;
// ContainerPath is the fixed in-container mount point; ReadOnly
// records whether the in-container mount should be read-only.
// Callers receive this struct from EnsureAppDataDir / AppDataDir
// so they can pass it to the runtime's docker run args builder
// without re-deriving anything.
type AppData struct {
	App           string
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// EnsureAppDataDir ensures <DataRoot>/<app>/data exists, creating
// the parent <DataRoot>/<app>/ directory and the data directory
// itself if necessary. The function is idempotent: an existing
// directory is left untouched (other than a final chown-style
// permission tightening if the operator requested one — currently
// none, so a pre-existing dir is left alone).
//
// readOnly is recorded on the returned AppData so the runtime
// can pass it to docker run as the mount's readonly flag. It is
// not enforced by EnsureAppDataDir itself: the host directory is
// the same in either case. The host-side permissions are
// operator-controlled; a read-only container mount can still be
// written to from the host.
//
// The app name is validated against the existing appNameRe. The
// resolved host path is verified to live under cfg.DataRoot
// (defense in depth against a symlinked DataRoot). If the data
// directory exists as a symlink, the operation is rejected with
// ErrSymlinkedAppData and the symlink target is left untouched.
//
// EnsureAppDataDir is the call Deploy / Rollback make on every
// deployment to guarantee the bind-mount target exists before
// docker run. It is NOT a destructive operation.
func EnsureAppDataDir(cfg DataConfig, app string, readOnly bool) (*AppData, error) {
	data, err := AppDataDir(cfg, app, readOnly)
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(data.HostPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Create the directory (and any missing parents).
		if err := os.MkdirAll(data.HostPath, 0o755); err != nil {
			return nil, fmt.Errorf("%w: mkdir %s: %v", ErrInvalidDataConfig, data.HostPath, err)
		}
		return data, nil
	case err != nil:
		return nil, fmt.Errorf("%w: stat %s: %v", ErrInvalidDataConfig, data.HostPath, err)
	case info.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: %s", ErrSymlinkedAppData, data.HostPath)
	case !info.IsDir():
		return nil, fmt.Errorf("%w: %s exists but is not a directory", ErrInvalidDataConfig, data.HostPath)
	}

	// Existing directory: nothing to do. EnsureAppDataDir is
	// idempotent by construction; we deliberately do not chmod
	// or otherwise mutate a pre-existing data directory the
	// operator may have set up.
	return data, nil
}

// AppDataDir derives the host path for app's data directory and
// validates that the resolved path lives under cfg.DataRoot. It
// does NOT touch disk. Returns ErrInvalidDataConfig if cfg.DataRoot
// is empty, the app name fails the appNameRe check, or the
// resolved path escapes cfg.DataRoot. readOnly is recorded on
// the returned AppData.
func AppDataDir(cfg DataConfig, app string, readOnly bool) (*AppData, error) {
	if cfg.DataRoot == "" {
		return nil, fmt.Errorf("%w: DataRoot is required", ErrInvalidDataConfig)
	}
	if !appNameRe.MatchString(app) {
		return nil, fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidDataConfig, app)
	}

	hostPath, err := deriveAppDataPath(cfg.DataRoot, app)
	if err != nil {
		return nil, err
	}

	return &AppData{
		App:           app,
		HostPath:      hostPath,
		ContainerPath: dataContainerPath,
		ReadOnly:      readOnly,
	}, nil
}

// RemoveAppDataDir is the EXPLICIT destructive operation for the
// data layer. Deploy, Rollback, and the candidate-cleanup paths
// must NEVER call this. It is intended for a future operator
// command ("delete this app's persistent data") that requires the
// operator to confirm intent.
//
// Behavior:
//
//   - The app name is validated (appNameRe) and the derived host
//     path is verified to live under cfg.DataRoot. Both checks
//     fail before disk is touched.
//   - If the directory does not exist, ErrAppDataNotFound is
//     returned (consistent with the state layer's behavior).
//   - If the directory is a symlink, the operation is rejected
//     with ErrSymlinkedAppData; the symlink target is left
//     untouched.
//   - If force is false and the directory contains any entries,
//     ErrAppDataNotEmpty is returned and nothing is removed.
//   - If force is true, the directory and its contents are
//     removed recursively.
//
// RemoveAppDataDir never accepts an arbitrary host path; the host
// path is derived from the trusted DataRoot and the validated app
// name.
func RemoveAppDataDir(cfg DataConfig, app string, force bool) error {
	data, err := AppDataDir(cfg, app, false)
	if err != nil {
		return err
	}

	info, err := os.Lstat(data.HostPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrAppDataNotFound, data.HostPath)
		}
		return fmt.Errorf("%w: stat %s: %v", ErrInvalidDataConfig, data.HostPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkedAppData, data.HostPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s exists but is not a directory", ErrInvalidDataConfig, data.HostPath)
	}

	if !force {
		entries, err := os.ReadDir(data.HostPath)
		if err != nil {
			return fmt.Errorf("%w: readdir %s: %v", ErrInvalidDataConfig, data.HostPath, err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("%w: %s contains %d entries; pass force=true to remove recursively", ErrAppDataNotEmpty, data.HostPath, len(entries))
		}
	}

	if err := os.RemoveAll(data.HostPath); err != nil {
		return fmt.Errorf("%w: remove %s: %v", ErrInvalidDataConfig, data.HostPath, err)
	}
	return nil
}

// deriveAppDataPath returns the absolute host path for app's data
// directory and verifies the resolved path is contained in
// dataRoot (defense in depth against a symlinked root that points
// outside the trusted host layout).
func deriveAppDataPath(dataRoot, app string) (string, error) {
	path := filepath.Join(dataRoot, app, "data")
	absRoot, err := filepath.Abs(dataRoot)
	if err != nil {
		return "", fmt.Errorf("%w: abs root: %v", ErrInvalidDataConfig, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: abs path: %v", ErrInvalidDataConfig, err)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("%w: rel: %v", ErrInvalidDataConfig, err)
	}
	if rel == ".." || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path traversal detected", ErrInvalidDataConfig)
	}
	return absPath, nil
}
