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

func TestConfigValidate_OK(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestConfigValidate_AppID(t *testing.T) {
	c := validConfig()
	c.AppID = 0
	if err := c.Validate(); err == nil {
		t.Errorf("expected error for app_id=0")
	}
}

func TestConfigValidate_InstallationID(t *testing.T) {
	c := validConfig()
	c.InstallationID = 0
	if err := c.Validate(); err == nil {
		t.Errorf("expected error for installation_id=0")
	}
}

func TestConfigValidate_NoPrivateKeyPath(t *testing.T) {
	c := validConfig()
	c.PrivateKeyPath = ""
	if err := c.Validate(); err == nil {
		t.Errorf("expected error for missing private_key_path")
	}
}

func TestConfigValidate_NoOrg(t *testing.T) {
	c := validConfig()
	c.AllowedOrg = ""
	if err := c.Validate(); err == nil {
		t.Errorf("expected error for missing allowed_org")
	}
}

func TestConfigValidate_EmptyAllowedRepos(t *testing.T) {
	c := validConfig()
	c.AllowedRepos = nil
	if err := c.Validate(); err == nil {
		t.Errorf("expected error for empty allowed_repositories")
	}
}

func TestConfigValidate_RepoWithSlash(t *testing.T) {
	c := validConfig()
	c.AllowedRepos = []string{"org/repo"}
	if err := c.Validate(); err == nil {
		t.Errorf("expected error for repo name with slash")
	}
}

func TestConfigValidate_NoSocketPath(t *testing.T) {
	c := validConfig()
	c.SocketPath = ""
	if err := c.Validate(); err == nil {
		t.Errorf("expected error for missing socket_path")
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
