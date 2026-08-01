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
// names: a lowercase letter followed by 1–31 of [a-z0-9-], total
// length 2–32.
var appNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Manifest is the deploy.json shape. Version 1 describes a single HTTP
// container; see docs/DEPLOYMENT.md for the full contract and scope.
type Manifest struct {
	Version       int    `json:"version"`
	App           string `json:"app"`
	ContainerPort int    `json:"container_port"`
	HealthPath    string `json:"health_path"`
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
	return &m, nil
}
