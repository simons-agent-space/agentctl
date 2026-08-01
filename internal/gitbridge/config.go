// Package gitbridge implements the broker: it validates a request, mints a
// GitHub App JWT, calls GitHub to mint an installation token restricted to
// a single repository, and returns the token + expiry to the UDS caller.
package gitbridge

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the JSON file shape consumed by the broker. It is loaded once
// at startup. Mutating it at runtime is not supported.
type Config struct {
	// AppID is the numeric GitHub App ID.
	AppID int64 `json:"app_id"`
	// InstallationID is the numeric installation ID for the org.
	InstallationID int64 `json:"installation_id"`
	// PrivateKeyPath is the on-disk path to the PEM-encoded RSA private key.
	// The key is read once at startup and zeroed when the broker exits.
	PrivateKeyPath string `json:"private_key_path"`
	// AllowedOrg is the single organisation that repositories must live in.
	AllowedOrg string `json:"allowed_org"`
	// AllowedRepos is the explicit list of allowed repository names
	// (without the org prefix). Anything else is rejected.
	AllowedRepos []string `json:"allowed_repositories"`
	// SocketPath is where the UDS listener is created.
	SocketPath string `json:"socket_path"`
	// SocketMode is the file mode applied to the socket (e.g. "0660").
	SocketMode string `json:"socket_mode,omitempty"`
	// SocketGroup is the optional group name applied to the socket.
	// If empty, the socket inherits the broker process's group.
	SocketGroup string `json:"socket_group,omitempty"`
}

// LoadConfig reads and validates the JSON file at path.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate ensures the config is internally consistent.
func (c *Config) Validate() error {
	if c.AppID <= 0 {
		return fmt.Errorf("app_id must be > 0")
	}
	if c.InstallationID <= 0 {
		return fmt.Errorf("installation_id must be > 0")
	}
	if c.PrivateKeyPath == "" {
		return fmt.Errorf("private_key_path is required")
	}
	if c.AllowedOrg == "" {
		return fmt.Errorf("allowed_org is required")
	}
	if len(c.AllowedRepos) == 0 {
		return fmt.Errorf("allowed_repositories must be non-empty")
	}
	if c.SocketPath == "" {
		return fmt.Errorf("socket_path is required")
	}
	for _, r := range c.AllowedRepos {
		if r == "" {
			return fmt.Errorf("allowed_repositories contains empty entry")
		}
		if strings.ContainsRune(r, '/') {
			return fmt.Errorf("allowed_repositories entry %q must not contain '/'", r)
		}
	}
	return nil
}

// IsAllowed reports whether fullSlug (org/name) is in the configured
// organisation and on the allowlist.
func (c *Config) IsAllowed(fullSlug string) bool {
	org, name, ok := splitSlug(fullSlug)
	if !ok {
		return false
	}
	if !strings.EqualFold(org, c.AllowedOrg) {
		return false
	}
	for _, r := range c.AllowedRepos {
		if strings.EqualFold(r, name) {
			return true
		}
	}
	return false
}

func splitSlug(slug string) (org, name string, ok bool) {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
