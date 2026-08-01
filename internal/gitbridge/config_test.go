package gitbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		AppID:          1,
		InstallationID: 2,
		PrivateKeyPath: "/tmp/key.pem",
		AllowedOrg:     "simons-agent-space",
		AllowedRepos:   []string{"agentctl", "another-repo"},
		SocketPath:     "/run/gitbridge/socket",
	}
}

func cloneConfig(c *Config) *Config {
	out := *c
	out.AllowedRepos = append([]string(nil), c.AllowedRepos...)
	return &out
}

func TestConfigValidate_Valid(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestConfigValidate_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
	}{
		{"AppID zero", func(c *Config) { c.AppID = 0 }},
		{"InstallationID zero", func(c *Config) { c.InstallationID = 0 }},
		{"missing PrivateKeyPath", func(c *Config) { c.PrivateKeyPath = "" }},
		{"missing AllowedOrg", func(c *Config) { c.AllowedOrg = "" }},
		{"empty AllowedRepos", func(c *Config) { c.AllowedRepos = nil }},
		{"repo with slash", func(c *Config) { c.AllowedRepos = []string{"org/repo"} }},
		{"missing SocketPath", func(c *Config) { c.SocketPath = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cloneConfig(validConfig())
			tc.mod(c)
			if err := c.Validate(); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestIsAllowed(t *testing.T) {
	c := validConfig()
	cases := []struct {
		slug string
		want bool
	}{
		{"simons-agent-space/agentctl", true},
		{"simons-agent-space/another-repo", true},
		{"simons-agent-space/other-repo", false},
		{"other-org/agentctl", false},
		{"agentctl", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := c.IsAllowed(tc.slug); got != tc.want {
			t.Errorf("IsAllowed(%q) = %v, want %v", tc.slug, got, tc.want)
		}
	}
}

func TestLoadConfig_RejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"app_id":1,"installation_id":2,"private_key_path":"/k","allowed_org":"o","allowed_repositories":["r"],"socket_path":"/s","unknown_field":true}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Errorf("expected error for unknown field")
	} else if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("expected 'unknown field' in error, got: %v", err)
	}
}

func TestLoadConfig_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"app_id":1,"installation_id":2,"private_key_path":"/k","allowed_org":"o","allowed_repositories":["r"],"socket_path":"/s"}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppID != 1 {
		t.Errorf("AppID = %d", cfg.AppID)
	}
}
