// Package deploy parses and validates the deploy.json manifest consumed
// by agentctld. Parsing (Load) is kept separate from policy
// (Validate): Load reads and decodes the file strictly, Validate
// enforces the project's rules about its contents.
package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// appNameRe matches both manifest "app" values and manifest
// "repository" values. A lowercase letter, then 0–30 of [a-z0-9-],
// then a final lowercase letter or digit. Total length 2–32. The
// leading and trailing rules prevent values like "demo-" that would
// produce invalid derived hostnames.
//
// The manifest "app" and "repository" fields share the same regex:
// both are GitHub-influenced short names that must be safe to embed
// in URLs, hostnames, and filesystem paths. The manifest contract is
// the single source of truth for this shape; the daemon's
// counterpart (internal/daemon/server.go) duplicates the regex so
// HTTP input validation cannot accidentally drift.
var appNameRe = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`)

// secretRefRe is the allowlist shape for a secret_ref: lowercase
// letters, digits, dots, underscores, dashes; 1–64 chars; no path
// separators, no leading dot, no double dots. Used to reject
// traversal in secret_ref values before the secret loader joins
// the name onto the secret directory.
var secretRefRe = regexp.MustCompile(`^[a-z0-9_](?:[a-z0-9_.-]{0,62}[a-z0-9_])$`)

// Manifest is the deploy.json shape.
//
//   - Version 1: single HTTP container; no data, no env, no
//     repository (legacy).
//   - Version 2: adds an optional "data" field for per-app data
//     mounts.
//   - Version 3: adds a REQUIRED "repository" field (GitHub repo
//     short name; the daemon derives the trusted origin URL
//     internally from the configured org + repository) and an
//     optional "env" array for per-app secret injection.
//
// App and Repository are distinct roles:
//
//   - App is the deployment identity: it drives the API path
//     (/v1/apps/<app>/...), the Caddy hostname prefix
//     (<app>.<base-domain>), the per-app state key, the per-app
//     data directory, and the Docker container naming input.
//   - Repository is the GitHub repo short name used for source
//     resolution. The full trusted URL is derived internally as
//     https://github.com/<AGENTCTLD_SOURCE_ALLOWED_ORG>/<repository>.git
//     so the caller can never select a different org and can
//     never provide a full URL.
//
// App and Repository may differ (e.g. a renamed deployment identity
// for the same repo, or the same identity across multiple repos in
// the same org). When they are equal the manifest still declares
// both fields explicitly; nothing is implied.
//
// When Data is non-nil the manifest opts the deployment into a
// per-app data mount. When Env is non-empty the manifest opts the
// deployment into per-app secret injection. Neither feature is
// available on version 1; only "env" and "repository" require
// version 3.
type Manifest struct {
	Version       int           `json:"version"`
	App           string        `json:"app"`
	Repository    string        `json:"repository"`
	ContainerPort int           `json:"container_port"`
	HealthPath    string        `json:"health_path"`
	Data          *ManifestData `json:"data,omitempty"`
	Env           []EnvEntry    `json:"env,omitempty"`
}

// ManifestData is the value of the optional "data" field.
//
//   - Mount controls whether a data directory is mounted at all.
//   - ReadOnly sets the read-only flag for the mount; ignored
//     (always true) when HostSource is set, because host-source
//     mounts are required to be read-only.
//   - HostSource (v3) names an EXISTING host directory to mount
//     read-only. Must be present in the daemon-side
//     HostSourceAllowlist (DataConfig). Must be absolute, must
//     exist, must be a directory, must not be a symlink. The
//     daemon does not create or modify the directory.
//   - ContainerPath (v3) overrides the fixed in-container mount
//     target (`/data`) when HostSource is set. Must be absolute
//     and must not be a dangerous target (`/`, `/proc`, `/sys`,
//     `/dev`, `/run`). Defaults to `/data` when absent.
type ManifestData struct {
	Mount         bool   `json:"mount"`
	ReadOnly      bool   `json:"read_only"`
	HostSource    string `json:"host_source,omitempty"`
	ContainerPath string `json:"container_path,omitempty"`
}

// EnvEntry is one environment variable to inject into the container.
// SecretRef names a file under the daemon's secret directory
// (Config.SecretDir); the file's content becomes the env value.
// Secret values are never persisted; only the reference is stored.
type EnvEntry struct {
	Name      string `json:"name"`
	SecretRef string `json:"secret_ref"`
	Required  bool   `json:"required,omitempty"`
}

// Load reads and decodes a deploy.json file from path. Unknown
// fields and trailing data after the manifest object are rejected.
// Version gating:
//
//	v1: no data, no env, no repository
//	v2: data allowed, no env, no repository
//	v3: repository REQUIRED, data + env allowed
//
// Anything outside the v1/v2/v3 set is rejected at parse time so
// every consumer of Load sees the failure. A version 3 manifest
// without a "repository" field is rejected here too — the source
// layer has no other way to learn which repository to clone.
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read deploy manifest: %w", err)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse deploy manifest: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after deploy manifest")
	}
	switch m.Version {
	case 1:
		if m.Data != nil {
			return nil, fmt.Errorf("data field is not allowed in version 1 manifests (bump to version 2)")
		}
		if len(m.Env) > 0 {
			return nil, fmt.Errorf("env field is not allowed in version 1 manifests (bump to version 3)")
		}
		if m.Repository != "" {
			return nil, fmt.Errorf("repository field is not allowed in version 1 manifests (bump to version 3)")
		}
	case 2:
		if len(m.Env) > 0 {
			return nil, fmt.Errorf("env field requires manifest version 3 (current version is 2)")
		}
		if m.Repository != "" {
			return nil, fmt.Errorf("repository field requires manifest version 3 (current version is 2)")
		}
	case 3:
		if m.Repository == "" {
			return nil, fmt.Errorf("repository field is required for version 3 manifests")
		}
		// Repository shape (regex match, non-emptiness) is
		// enforced by Validate (which has the surrounding app
		// context for the error message); an empty repository is
		// the only parse-time concern.
	default:
		return nil, fmt.Errorf("unsupported manifest version %d (only versions 1, 2 and 3 are accepted)", m.Version)
	}
	for i, e := range m.Env {
		if e.Name == "" {
			return nil, fmt.Errorf("env[%d]: name is required", i)
		}
		if !secretRefRe.MatchString(e.SecretRef) {
			return nil, fmt.Errorf("env[%d]: secret_ref %q is invalid (must match %s)", i, e.SecretRef, secretRefRe.String())
		}
	}
	return &m, nil
}
