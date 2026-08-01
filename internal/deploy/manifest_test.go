package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validManifest returns a Manifest that satisfies every Validate rule
// against the repo "agentctl". Tests clone it and mutate one field to
// exercise individual rejection paths.
func validManifest() *Manifest {
	return &Manifest{
		Version:       1,
		App:           "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
}

// writeDeploy writes body to a temp deploy.json and returns the path.
func writeDeploy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---------- Load ----------

func TestLoad_Valid(t *testing.T) {
	path := writeDeploy(t, `{"version":1,"app":"agentctl","container_port":8080,"health_path":"/healthz"}`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := *validManifest()
	if *got != want {
		t.Errorf("got %+v, want %+v", *got, want)
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	path := writeDeploy(t, `{"version":1,"app":"agentctl","container_port":8080,"health_path":"/healthz","extra":true}`)
	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("expected 'unknown field' in error, got: %v", err)
	}
}

func TestLoad_RejectsTrailingJSON(t *testing.T) {
	path := writeDeploy(t, `{"version":1,"app":"agentctl","container_port":8080,"health_path":"/healthz"}{"sneaky":"object"}`)
	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for trailing JSON")
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Errorf("expected 'trailing' in error, got: %v", err)
	}
}

func TestLoad_RejectsMalformedJSON(t *testing.T) {
	path := writeDeploy(t, `{"version":1,"app":"agentctl",`)
	if _, err := Load(path); err == nil {
		t.Errorf("expected error for malformed JSON")
	}
}

func TestLoad_RejectsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Errorf("expected error for missing file")
	}
}

// ---------- Validate ----------

func TestValidate_Valid(t *testing.T) {
	if err := Validate(validManifest(), "agentctl"); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidate_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*Manifest) // nil for the nil-manifest case
		repo    string
		wantSub string
	}{
		{"nil manifest", nil, "agentctl", "nil manifest"},
		{"version 0", func(m *Manifest) { m.Version = 0 }, "agentctl", "unsupported version"},
		{"version 2", func(m *Manifest) { m.Version = 2 }, "agentctl", "unsupported version"},
		{"version negative", func(m *Manifest) { m.Version = -1 }, "agentctl", "unsupported version"},
		{"app starts with digit", func(m *Manifest) { m.App = "1agentctl" }, "agentctl", "app-name format"},
		{"app starts with hyphen", func(m *Manifest) { m.App = "-agentctl" }, "agentctl", "app-name format"},
		{"app too long", func(m *Manifest) { m.App = strings.Repeat("a", 33) }, "agentctl", "app-name format"},
		{"app empty", func(m *Manifest) { m.App = "" }, "agentctl", "app-name format"},
		{"app uppercase", func(m *Manifest) { m.App = "Agentctl" }, "agentctl", "app-name format"},
		{"app underscore", func(m *Manifest) { m.App = "agent_ctl" }, "agentctl", "app-name format"},
		{"app mismatch expectedRepo", func(m *Manifest) { m.App = "otherapp" }, "agentctl", "does not match expected repo"},
		{"expectedRepo bad format", func(m *Manifest) {}, "BadFormat", "app-name format"},
		{"expectedRepo empty", func(m *Manifest) {}, "", "app-name format"},
		{"expectedRepo with slash", func(m *Manifest) {}, "org/repo", "app-name format"},
		{"container_port 0", func(m *Manifest) { m.ContainerPort = 0 }, "agentctl", "container_port"},
		{"container_port below 1024", func(m *Manifest) { m.ContainerPort = 1023 }, "agentctl", "container_port"},
		{"container_port above 65535", func(m *Manifest) { m.ContainerPort = 65536 }, "agentctl", "container_port"},
		{"container_port negative", func(m *Manifest) { m.ContainerPort = -1 }, "agentctl", "container_port"},
		{"health_path missing slash", func(m *Manifest) { m.HealthPath = "healthz" }, "agentctl", "must start with /"},
		{"health_path empty", func(m *Manifest) { m.HealthPath = "" }, "agentctl", "must start with /"},
		{"health_path query", func(m *Manifest) { m.HealthPath = "/healthz?ok=1" }, "agentctl", "query string or fragment"},
		{"health_path fragment", func(m *Manifest) { m.HealthPath = "/healthz#x" }, "agentctl", "query string or fragment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *Manifest
			if tc.name == "nil manifest" {
				m = nil
			} else {
				mm := validManifest()
				if tc.mut != nil {
					tc.mut(mm)
				}
				m = mm
			}
			err := Validate(m, tc.repo)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("expected error containing %q, got %q", tc.wantSub, err.Error())
			}
		})
	}
}

func TestValidate_AppNameFormatAppliesToExpectedRepo(t *testing.T) {
	m := validManifest()
	if err := Validate(m, "Agentctl"); err == nil {
		t.Errorf("expected error for uppercase expectedRepo")
	}
	if err := Validate(m, "1-agentctl"); err == nil {
		t.Errorf("expected error for digit-prefixed expectedRepo")
	}
	if err := Validate(m, "agentctl"); err != nil {
		t.Errorf("agentctl should validate: %v", err)
	}
}
