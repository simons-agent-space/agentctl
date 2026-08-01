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
func Validate(m *Manifest, expectedRepo string) error {
	if m == nil {
		return fmt.Errorf("nil manifest")
	}
	if m.Version != 1 {
		return fmt.Errorf("unsupported version %d (only version 1 is accepted)", m.Version)
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