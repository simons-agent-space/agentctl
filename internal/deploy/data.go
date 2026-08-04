package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dataContainerPath is the DEFAULT in-container path at which the
// host data directory is mounted when the manifest does not set
// ContainerPath. The runtime, rollback, and data layers all use
// this constant for legacy mounts. Host-source mounts (v3) may
// override this per-manifest via ManifestData.ContainerPath after
// validateContainerPath has cleared it.
const dataContainerPath = "/data"

// dangerousContainerPaths is the set of in-container targets that
// the daemon refuses to mount over, OR any descendant of those
// targets. Mounting any of these (e.g. /proc, /sys, /dev, /run,
// /proc/sys, /dev/shm, /run/secrets) would hide or corrupt the
// host's view of those subsystems. The list is exact: a path
// equal to one of these OR starting with "<prefix>/" is rejected.
var dangerousContainerPaths = []string{"/", "/proc", "/sys", "/dev", "/run"}

// Sentinel errors returned by the data layer.
var (
	ErrInvalidDataConfig = errors.New("invalid data configuration")
	ErrAppDataNotFound   = errors.New("app data directory not found")
	ErrAppDataNotEmpty   = errors.New("app data directory not empty")
	ErrHostSourceDenied  = errors.New("host source denied by daemon allowlist")
	ErrContainerPathBad  = errors.New("container_path is not allowed")
	// ErrSymlinkedAppData is returned when any component of the
	// data layout — DataRoot, the existing <DataRoot>/<app>
	// parent, or the final data path — is a symlink. The symlink
	// target is never followed and is left untouched.
	ErrSymlinkedAppData = errors.New("app data path component is a symlink")
)

// DataConfig holds the trusted host-side configuration for the
// per-app data layer. DataRoot is supplied by trusted host
// configuration, not by the caller. The derived per-app path is
// <DataRoot>/<app>/data. HostSourceAllowlist is the daemon-side
// allowlist of EXISTING host directories that a manifest may
// mount as a read-only data source; it is empty in the legacy
// config and is only consulted when data.host_source is set.
type DataConfig struct {
	// DataRoot is the trusted host directory under which every
	// per-app data directory is created. The trailing slash is
	// not significant.
	DataRoot string

	// HostSourceAllowlist is the daemon-side allowlist of host
	// paths that may be mounted read-only as data sources. Paths
	// are matched after filepath.Clean; an exact match is
	// required (no prefix/glob match). The allowlist is operator-
	// supplied and must contain every existing host directory that
	// any manifest is permitted to mount.
	HostSourceAllowlist []string
}

// AppData describes the derived identity of one app's data
// directory. HostPath is the resolved absolute host path;
// ContainerPath is the in-container mount point; ReadOnly
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

// validateContainerPath returns nil iff p is an absolute, cleaned
// path that is not on the dangerous-container-path deny list AND
// is not a descendant of any of those paths. The block list is
// exactly `/`, `/proc`, `/sys`, `/dev`, `/run`; `/proc/sys`,
// `/dev/shm`, `/run/secrets`, etc. are also rejected because they
// are descendants of a blocked path. Examples that pass:
// `/data`, `/data/openclaw-state`, `/srv/data`. Examples that
// fail: `/`, `/proc`, `/proc/sys`, `/sys/fs/cgroup`, `/dev`,
// `/dev/shm`, `/run`, `/run/secrets`. The function does not touch
// the filesystem.
func validateContainerPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%w: %q is not absolute", ErrContainerPathBad, p)
	}
	cleaned := filepath.Clean(p)
	for _, d := range dangerousContainerPaths {
		if cleaned == d {
			return fmt.Errorf("%w: %q is a dangerous in-container target", ErrContainerPathBad, cleaned)
		}
		// Descendant check: a path equal to d+"/x" sits under the
		// blocked prefix. The prefix string already ends in "/"
		// for all entries (they are single-segment absolute paths),
		// so a bare prefix check is sufficient.
		if strings.HasPrefix(cleaned, d+"/") {
			return fmt.Errorf("%w: %q is a descendant of dangerous in-container target %q",
				ErrContainerPathBad, cleaned, d)
		}
	}
	return nil
}

// resolveContainerPath returns the in-container path to use. If the
// manifest overrides with ContainerPath, that value is validated;
// otherwise the fixed dataContainerPath is returned. The returned
// path is cleaned and absolute (the default is both).
func resolveContainerPath(md *ManifestData) (string, error) {
	if md == nil || md.ContainerPath == "" {
		return dataContainerPath, nil
	}
	if err := validateContainerPath(md.ContainerPath); err != nil {
		return "", err
	}
	return filepath.Clean(md.ContainerPath), nil
}

// EnsureAppDataDir derives one app's data directory according to
// the manifest's data field. Behavior is selected by the
// manifest:
//
//   - md == nil or md.Mount == false: no mount requested; returns
//     (nil, nil).
//   - md.Mount == true and md.HostSource == "": legacy app-owned
//     behavior. Ensure <DataRoot>/<app>/data exists, creating it
//     if necessary. The mount is ReadOnly if md.ReadOnly is set.
//   - md.Mount == true and md.HostSource != "": host-source mount.
//     The source path must exist, must be a directory, must not
//     be a symlink (at any path component), must be absolute, and
//     must appear (after Clean) in cfg.HostSourceAllowlist. The
//     daemon does not create or modify the directory. The mount
//     is ReadOnly regardless of md.ReadOnly — host-source mounts
//     are required to be read-only.
//
// ContainerPath, when set, overrides the fixed /data. It is
// validated against the dangerous-target deny list (including
// descendants) before any filesystem call.
//
// The legacy app-owned branch (md.HostSource == "") creates the
// directory hierarchy one level at a time, never with
// os.MkdirAll, and re-checks every newly-created directory to
// guarantee it is a real directory — not a symlink that appeared
// between the parent-walk pre-check and the mkdir. This is the
// v1/v2 symlink-safety contract; it must not be weakened by the
// v3 host-source work added above.
func EnsureAppDataDir(cfg DataConfig, app string, md *ManifestData) (*AppData, error) {
	if md == nil || !md.Mount {
		return nil, nil
	}

	containerPath, err := resolveContainerPath(md)
	if err != nil {
		return nil, err
	}

	if md.HostSource != "" {
		return ensureHostSource(cfg, app, md.HostSource, containerPath)
	}

	// Legacy app-owned behavior: create the data layout
	// one level at a time. Each call to ensureTrustedDir walks
	// the parent chain for symlinks and re-checks the created
	// directory is a real directory. Using os.MkdirAll here
	// would be unsafe because it would silently create
	// intermediate directories without checking them for
	// symlinks, which would let a redirect at <DataRoot>/<app>
	// land the data directory inside an attacker-controlled
	// tree.
	hostPath := filepath.Join(cfg.DataRoot, app, "data")
	appDir := filepath.Dir(hostPath)

	if err := noSymlinkAt(cfg.DataRoot, ErrSymlinkedAppData); err != nil {
		return nil, err
	}
	if err := ensureTrustedDir(appDir); err != nil {
		return nil, err
	}
	if err := ensureTrustedDir(hostPath); err != nil {
		return nil, err
	}

	return &AppData{
		App:           app,
		HostPath:      hostPath,
		ContainerPath: containerPath,
		ReadOnly:      md.ReadOnly,
	}, nil
}

// ensureTrustedDir creates dir (with mode 0755) if it does not
// exist, after confirming no parent component is a symlink. If
// dir already exists, verifies it is a real directory. The
// post-create Lstat guards against a TOCTOU race where a symlink
// appears between the pre-check and the mkdir. Used by
// EnsureAppDataDir for the v1/v2 app-owned branch.
func ensureTrustedDir(dir string) error {
	if err := noSymlinkAt(dir, ErrSymlinkedAppData); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return fmt.Errorf("%w: mkdir %s: %v", ErrInvalidDataConfig, dir, err)
		}
		info, err = os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("%w: post-mkdir stat %s: %v", ErrInvalidDataConfig, dir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s became a symlink after mkdir", ErrSymlinkedAppData, dir)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %s is not a directory after mkdir", ErrInvalidDataConfig, dir)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: stat %s: %v", ErrInvalidDataConfig, dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkedAppData, dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s exists but is not a directory", ErrInvalidDataConfig, dir)
	}
	return nil
}

// ensureHostSource validates an existing host directory against
// the daemon-side allowlist and returns the AppData describing it.
// The directory is NOT modified; the caller is responsible for
// any cleanup needed on the caller side.
//
// Symlink rejection is strict: every path component from the root
// down to (and including) the source path itself must be a real
// directory. A symlink anywhere in that chain is refused with
// ErrSymlinkedAppData.
func ensureHostSource(cfg DataConfig, app, source, containerPath string) (*AppData, error) {
	if !filepath.IsAbs(source) {
		return nil, fmt.Errorf("%w: host_source %q is not absolute", ErrInvalidDataConfig, source)
	}
	cleaned := filepath.Clean(source)

	// Allowlist: exact match only, against the cleaned path. The
	// allowlist is operator-supplied and not derived from the
	// manifest, so a manifest cannot grant itself access.
	if !pathInAllowlist(cleaned, cfg.HostSourceAllowlist) {
		return nil, fmt.Errorf("%w: %s is not in the daemon allowlist", ErrHostSourceDenied, cleaned)
	}

	// Resolve symlinks in every component so a redirected
	// component cannot point the mount at an unexpected path.
	if err := noSymlinkAt(cleaned, ErrSymlinkedAppData); err != nil {
		return nil, err
	}

	info, err := os.Stat(cleaned)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: host_source %s does not exist", ErrInvalidDataConfig, cleaned)
		}
		return nil, fmt.Errorf("%w: stat %s: %v", ErrInvalidDataConfig, cleaned, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: host_source %s is not a directory", ErrInvalidDataConfig, cleaned)
	}

	// Host-source mounts are always read-only — the daemon
	// ignores (and overrides) md.ReadOnly for these mounts. This
	// is a hard invariant of the host-source contract.
	return &AppData{
		App:           app,
		HostPath:      cleaned,
		ContainerPath: containerPath,
		ReadOnly:      true,
	}, nil
}

// pathInList reports whether cleaned is an exact match for any
// element of list (after filepath.Clean on each element). Used
// for the host-source allowlist check.
func pathInAllowlist(cleaned string, list []string) bool {
	for _, p := range list {
		if filepath.Clean(p) == cleaned {
			return true
		}
	}
	return false
}

// noSymlinkAt returns ErrSymlinkedAppData if path (or any parent
// directory leading to it) is a symlink. errFor is the sentinel
// returned on detection. Used both by the legacy app-owned
// branch and by ensureHostSource.
//
// The check walks up the directory chain from path to /. A
// symlink at any component fails the check. The component where
// the symlink was detected is reported in the error.
func noSymlinkAt(path string, errFor error) error {
	cleaned := filepath.Clean(path)
	cur := cleaned
	for {
		info, err := os.Lstat(cur)
		if err != nil {
			return nil // missing parents are handled by the caller
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", errFor, cur)
		}
		if cur == "/" || cur == "." {
			return nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}

// AppDataDir returns the derived AppData for an EXISTING app-owned
// data directory without creating it. Used by RemoveAppDataDir.
// Host-source paths cannot be reported by AppDataDir (there is no
// app-owned directory to describe); the function returns
// ErrAppDataNotFound in that case.
//
// Config and app-name validation runs BEFORE the filesystem check
// so an operator who passes a bad config or app name gets
// ErrInvalidDataConfig (the same sentinel RemoveAppDataDir returns)
// instead of a confusing ErrAppDataNotFound that points at a path
// that was never going to be examined.
func AppDataDir(cfg DataConfig, app string) (*AppData, error) {
	if cfg.DataRoot == "" {
		return nil, fmt.Errorf("%w: DataRoot is required", ErrInvalidDataConfig)
	}
	if !appNameRe.MatchString(app) {
		return nil, fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidDataConfig, app)
	}
	hostPath := filepath.Join(cfg.DataRoot, app, "data")
	info, err := os.Lstat(hostPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrAppDataNotFound
		}
		return nil, fmt.Errorf("%w: stat %s: %v", ErrInvalidDataConfig, hostPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrSymlinkedAppData, hostPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalidDataConfig, hostPath)
	}
	return &AppData{
		App:           app,
		HostPath:      hostPath,
		ContainerPath: dataContainerPath,
		ReadOnly:      false, // not meaningful for AppDataDir; see EnsureAppDataDir
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
//   - Symlinks at DataRoot, at an existing <DataRoot>/<app>
//     parent, or at the final data path are rejected with
//     ErrSymlinkedAppData before any ReadDir or RemoveAll runs,
//     so a redirected parent cannot redirect deletion outside
//     the trusted layout.
//   - If the directory does not exist, ErrAppDataNotFound is
//     returned (consistent with the state layer's behavior).
//   - If force is false and the directory contains any entries,
//     ErrAppDataNotEmpty is returned and nothing is removed.
//   - If force is true, the directory and its contents are
//     removed recursively.
//
// RemoveAppDataDir never accepts an arbitrary host path; the host
// path is derived from the trusted DataRoot and the validated app
// name.
func RemoveAppDataDir(cfg DataConfig, app string, force bool) error {
	data, err := AppDataDir(cfg, app)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(data.HostPath, filepath.Clean(cfg.DataRoot)+string(os.PathSeparator)) &&
		data.HostPath != filepath.Clean(cfg.DataRoot) {
		return fmt.Errorf("%w: %s escapes DataRoot", ErrInvalidDataConfig, data.HostPath)
	}
	if err := noSymlinkAt(data.HostPath, ErrSymlinkedAppData); err != nil {
		return err
	}
	entries, err := os.ReadDir(data.HostPath)
	if err != nil {
		return fmt.Errorf("%w: readdir %s: %v", ErrInvalidDataConfig, data.HostPath, err)
	}
	if len(entries) > 0 && !force {
		return ErrAppDataNotEmpty
	}
	if err := os.RemoveAll(data.HostPath); err != nil {
		return fmt.Errorf("%w: remove %s: %v", ErrInvalidDataConfig, data.HostPath, err)
	}
	return nil
}
