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

// appNameRe matches both manifest "app" values and repository short
// names. A lowercase letter, then 0–30 of [a-z0-9-], then a final
// lowercase letter or digit. Total length 2–32. The leading and
// trailing rules prevent values like "demo-" that would produce
// invalid derived hostnames.
var appNameRe = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])$`)

// Manifest is the deploy.json shape. Version 1 describes a single HTTP
// container; version 2 adds an optional per-app persistent data
// mount; see docs/DEPLOYMENT.md for the full contract and scope.
//
// When Data is non-nil the manifest opts the deployment into the
// per-app persistent data directory. The host path is derived from
// the trusted DataConfig (host configuration), never from the
// manifest, and the container-side mount target is the fixed
// constant dataContainerPath. Read-only is set via Data.ReadOnly.
type Manifest struct {
	Version       int           `json:"version"`
	App           string        `json:"app"`
	ContainerPort int           `json:"container_port"`
	HealthPath    string        `json:"health_path"`
	Data          *ManifestData `json:"data,omitempty"`
}

// ManifestData is the value of a manifest's optional "data" field.
// It declares whether the deployment mounts the per-app persistent
// data directory and whether the in-container mount is read-only.
// The host path and the in-container target are NOT part of this
// struct: the host path is derived from a trusted host root plus
// the validated app name, and the in-container target is a fixed
// constant.
type ManifestData struct {
	Mount    bool `json:"mount"`
	ReadOnly bool `json:"read_only"`
}

// Load reads and decodes a deploy.json file from path. Unknown fields
// and any trailing data after the manifest object are rejected.
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
	// A second Decode must return io.EOF: anything else means the
	// file contained trailing data after the manifest object.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after deploy manifest")
	}
	// A version-1 manifest must not declare a data field: the v1
	// contract explicitly did not support per-app persistent data,
	// and silently accepting it on v1 would be a silent contract
	// change. Reject it at parse time so every consumer of Load
	// (including callers that skip Validate) sees the failure.
	if m.Version == 1 && m.Data != nil {
		return nil, fmt.Errorf("data field is not allowed in version 1 manifests (bump to version 2)")
	}
	return &m, nil
}
