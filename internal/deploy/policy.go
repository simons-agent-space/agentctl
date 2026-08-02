package deploy

import (
	"fmt"
	"strings"
)

// Validate enforces project policy on a parsed manifest. expectedRepo
// is the repository short name the manifest is being deployed for
// (e.g. "agentctl"); it must match the app-name regex and must equal
// m.App. Returns nil when all checks pass, otherwise a descriptive
// error.
//
// Versions accepted:
//
//	1 — single HTTP container, no data mount (legacy contract).
//	2 — single HTTP container; optional data mount declared by the
//	    "data" field. When the "data" field is absent the deployment
//	    is identical to version 1 in behaviour. When it is present,
//	    the deployment opts into the per-app persistent data
//	    directory; see docs/DEPLOYMENT.md for the host-side layout.
//
// A version-1 manifest that contains a "data" field is rejected:
// version 1 explicitly did not support data mounts, so silently
// accepting it would be a contract change.
func Validate(m *Manifest, expectedRepo string) error {
	if m == nil {
		return fmt.Errorf("nil manifest")
	}
	if m.Version != 1 && m.Version != 2 {
		return fmt.Errorf("unsupported version %d (only versions 1 and 2 are accepted)", m.Version)
	}
	if m.Version == 1 && m.Data != nil {
		return fmt.Errorf("data field is not allowed in version 1 manifests (bump to version 2)")
	}
	if !appNameRe.MatchString(expectedRepo) {
		return fmt.Errorf("expected repo %q does not match app-name format", expectedRepo)
	}
	if !appNameRe.MatchString(m.App) {
		return fmt.Errorf("manifest app %q does not match app-name format", m.App)
	}
	if m.App != expectedRepo {
		return fmt.Errorf("manifest app %q does not match expected repo %q", m.App, expectedRepo)
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
	return nil
}
