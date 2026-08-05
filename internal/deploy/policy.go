package deploy

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Validate enforces project policy on a parsed manifest. The
// manifest is the single source of truth for both the deployment
// identity (App) and the source-resolution repository (Repository);
// Validate never takes a separate "expected repo" argument and
// never derives the repository from the app name.
//
// Versions accepted:
//
//	1 — single HTTP container, no data mount, no env, no repository
//	    (legacy contract). The repository field is rejected at
//	    parse time by Load, so a v1 manifest that reaches Validate
//	    has Repository == "".
//	2 — single HTTP container; optional data mount declared by the
//	    "data" field. When the "data" field is absent the deployment
//	    is identical to version 1 in behaviour. When it is present,
//	    the deployment opts into the per-app persistent data
//	    directory; see docs/DEPLOYMENT.md for the host-side layout.
//	3 — adds a REQUIRED "repository" field (GitHub repo short name;
//	    the daemon derives the trusted origin URL internally from
//	    the configured org + repository) and an optional "env" array
//	    for per-app secret injection.
//
// A version-1 manifest that contains a "data", "env", or
// "repository" field is rejected by Load before Validate sees it.
// Similarly, a version-2 manifest with an "env" or "repository"
// field is rejected by Load. Validate therefore only checks the
// shape of fields that are valid for the declared version.
func Validate(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("nil manifest")
	}
	if m.Version != 1 && m.Version != 2 && m.Version != 3 {
		return fmt.Errorf("unsupported version %d (only versions 1, 2 and 3 are accepted)", m.Version)
	}
	if m.Version == 1 && m.Data != nil {
		return fmt.Errorf("data field is not allowed in version 1 manifests (bump to version 2)")
	}
	if len(m.Env) > 0 {
		if m.Version == 1 {
			return fmt.Errorf("env field is not allowed in version 1 manifests (bump to version 3)")
		}
		if m.Version == 2 {
			return fmt.Errorf("env field requires manifest version 3 (current version is 2)")
		}
	}
	if m.Version == 3 && m.Repository == "" {
		// Defensive: Load already rejects this. Kept here so a
		// manifest constructed in-memory (tests, future code
		// paths) cannot bypass the requirement.
		return fmt.Errorf("repository field is required for version 3 manifests")
	}
	if !appNameRe.MatchString(m.App) {
		return fmt.Errorf("manifest app %q does not match app-name format", m.App)
	}
	// Repository is required for v3; for v1/v2 it is empty and
	// the regex check is skipped. App and Repository are allowed
	// to differ; both names must individually match the safe
	// short-name regex so they can be embedded in URLs, hostnames,
	// and filesystem paths.
	if m.Repository != "" && !appNameRe.MatchString(m.Repository) {
		return fmt.Errorf("manifest repository %q does not match app-name format", m.Repository)
	}
	if m.ContainerPort < 1024 || m.ContainerPort > 65535 {
		return fmt.Errorf("container_port %d out of range [1024, 65535]", m.ContainerPort)
	}
	if !strings.HasPrefix(m.HealthPath, "/") {
		return fmt.Errorf("health_path %q must start with /", m.HealthPath)
	}
	if strings.ContainsAny(m.HealthPath, "?#") {
		return fmt.Errorf("health_path %q must not contain a query string or fragment", m.HealthPath)
	}
	// Env-entry shape: each entry has a non-empty Name, a
	// secret_ref that already passed the secretRefRe check in
	// Load, and the names are unique within the manifest.
	seenNames := map[string]struct{}{}
	for i, e := range m.Env {
		if _, dup := seenNames[e.Name]; dup {
			return fmt.Errorf("env[%d]: duplicate name %q", i, e.Name)
		}
		seenNames[e.Name] = struct{}{}
	}
	// Data-mount shape: ContainerPath (if present and non-empty)
	// must be a safe absolute path. An absent/empty ContainerPath
	// keeps the default in-container target (/data); HostSource is
	// checked separately by ValidateDataMount because it requires
	// the daemon's HostSourceAllowlist config.
	if m.Data != nil && m.Data.ContainerPath != "" {
		if err := validateContainerPath(m.Data.ContainerPath); err != nil {
			return fmt.Errorf("data.container_path: %w", err)
		}
	}
	return nil
}

// ValidateDataMount validates the data mount configuration against
// the daemon-side DataConfig. It is non-side-effecting: it does not
// touch the filesystem. The caller (EnsureAppDataDir) re-runs the
// same checks at deploy time and produces a real AppData.
//
// This function lives in policy.go (not data.go) because the caller
// is Validate / inspect, and the import cycle would otherwise
// happen the other way: data.go -> policy.go is a natural edge
// already (data.go already imports policy.go's appNameRe). The
// allowlist check belongs near the manifest contract, not near the
// filesystem layer.
func ValidateDataMount(m *Manifest, cfg DataConfig) error {
	if m == nil || m.Data == nil {
		return nil
	}
	if !m.Data.Mount {
		return nil
	}
	if m.Data.HostSource != "" {
		if err := validateHostSourceReference(m.Data.HostSource, cfg); err != nil {
			return err
		}
	}
	// ContainerPath was checked by Validate (it has no config
	// dependency). ReadOnly is ignored when HostSource is set
	// (host-source mounts are always read-only); we don't reject
	// a misleading "read_only": false on a host-source mount here
	// because the runtime enforces the override.
	return nil
}

// validateHostSourceReference performs the non-side-effecting
// portion of ensureHostSource: it checks the allowlist and the
// shape. The filesystem side (Lstat, IsDir, symlink-chain) is
// done in EnsureAppDataDir at deploy time and is not duplicated
// here.
func validateHostSourceReference(src string, cfg DataConfig) error {
	if src == "" {
		return nil
	}
	if !filepath.IsAbs(src) {
		return fmt.Errorf("%w: host_source %q is not absolute", ErrInvalidDataConfig, src)
	}
	cleaned := filepath.Clean(src)
	if !pathInAllowlist(cleaned, cfg.HostSourceAllowlist) {
		return fmt.Errorf("%w: %s is not in the daemon allowlist", ErrHostSourceDenied, cleaned)
	}
	return nil
}
