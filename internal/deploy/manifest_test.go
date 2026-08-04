package deploy

import (
	"fmt"
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
	want := validManifest()
	if got.Version != want.Version ||
		got.App != want.App ||
		got.ContainerPort != want.ContainerPort ||
		got.HealthPath != want.HealthPath ||
		got.Data != nil || want.Data != nil ||
		len(got.Env) != 0 {
		t.Errorf("got %+v, want %+v", *got, *want)
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
		{"version negative", func(m *Manifest) { m.Version = -1 }, "agentctl", "unsupported version"},
		{"app starts with digit", func(m *Manifest) { m.App = "1agentctl" }, "agentctl", "app-name format"},
		{"app starts with hyphen", func(m *Manifest) { m.App = "-agentctl" }, "agentctl", "app-name format"},
		{"app ends with hyphen", func(m *Manifest) { m.App = "demo-" }, "agentctl", "app-name format"},
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

func TestValidate_AcceptsTwoCharName(t *testing.T) {
	m := &Manifest{
		Version:       1,
		App:           "a1",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
	if err := Validate(m, "a1"); err != nil {
		t.Errorf("two-character name a1 should validate: %v", err)
	}
}

func TestValidate_AcceptsThirtyTwoCharName(t *testing.T) {
	// 32-char name: 'a' + 30 x 'b' + '1'
	name := "a" + strings.Repeat("b", 30) + "1"
	if len(name) != 32 {
		t.Fatalf("test bug: expected 32 chars, got %d", len(name))
	}
	m := &Manifest{
		Version:       1,
		App:           name,
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
	if err := Validate(m, name); err != nil {
		t.Errorf("32-character name should validate: %v", err)
	}
}

func TestValidate_RejectsThirtyThreeCharName(t *testing.T) {
	name := "a" + strings.Repeat("b", 31) + "1" // 33 chars
	if len(name) != 33 {
		t.Fatalf("test bug: expected 33 chars, got %d", len(name))
	}
	m := &Manifest{
		Version:       1,
		App:           name,
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
	if err := Validate(m, name); err == nil {
		t.Errorf("33-character name should be rejected")
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

func TestValidate_AcceptsVersion2WithoutData(t *testing.T) {
	m := &Manifest{
		Version:       2,
		App:           "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
	if err := Validate(m, "agentctl"); err != nil {
		t.Errorf("version 2 without data should validate: %v", err)
	}
}

func TestValidate_AcceptsVersion2WithDataMount(t *testing.T) {
	cases := []struct {
		name     string
		mount    bool
		readOnly bool
	}{
		{"mount rw", true, false},
		{"mount ro", true, true},
		{"no mount", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{
				Version:       2,
				App:           "agentctl",
				ContainerPort: 8080,
				HealthPath:    "/healthz",
				Data:          &ManifestData{Mount: tc.mount, ReadOnly: tc.readOnly},
			}
			if err := Validate(m, "agentctl"); err != nil {
				t.Errorf("version 2 with data %+v should validate: %v", tc, err)
			}
		})
	}
}

func TestValidate_RejectsVersion1WithDataField(t *testing.T) {
	m := &Manifest{
		Version:       1,
		App:           "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
		Data:          &ManifestData{Mount: true},
	}
	err := Validate(m, "agentctl")
	if err == nil {
		t.Fatalf("expected error for version 1 with data field")
	}
	if !strings.Contains(err.Error(), "version 1") || !strings.Contains(err.Error(), "data") {
		t.Errorf("expected error mentioning version 1 and data, got %q", err.Error())
	}
}

func TestLoad_Version2WithoutData(t *testing.T) {
	path := writeDeploy(t, `{"version":2,"app":"agentctl","container_port":8080,"health_path":"/healthz"}`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != 2 {
		t.Errorf("Version = %d, want 2", got.Version)
	}
	if got.Data != nil {
		t.Errorf("Data = %+v, want nil", got.Data)
	}
}

func TestLoad_Version2WithData(t *testing.T) {
	path := writeDeploy(t, `{"version":2,"app":"agentctl","container_port":8080,"health_path":"/healthz","data":{"mount":true,"read_only":true}}`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Data == nil {
		t.Fatalf("Data = nil, want non-nil")
	}
	if !got.Data.Mount || !got.Data.ReadOnly {
		t.Errorf("Data = %+v, want {mount:true, read_only:true}", got.Data)
	}
}

func TestLoad_RejectsVersion1WithUnknownDataField(t *testing.T) {
	// A v1 manifest with a "data" field must fail Load (unknown
	// field) AND fail Validate (v1 + data is a contract change).
	path := writeDeploy(t, `{"version":1,"app":"agentctl","container_port":8080,"health_path":"/healthz","data":{"mount":true}}`)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected Load to reject unknown data field on version 1")
	}
}

func TestLoad_RejectsUnknownFieldInData(t *testing.T) {
	path := writeDeploy(t, `{"version":2,"app":"agentctl","container_port":8080,"health_path":"/healthz","data":{"mount":true,"host_path":"/etc"}}`)
	if _, err := Load(path); err == nil {
		t.Errorf("expected Load to reject unknown field inside data")
	}
}

// ---------- v3 / env / host_source ----------

func TestLoad_Version3WithoutDataOrEnv(t *testing.T) {
	path := writeDeploy(t, `{"version":3,"app":"agentctl","container_port":8080,"health_path":"/healthz"}`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != 3 {
		t.Errorf("Version = %d, want 3", got.Version)
	}
	if got.Data != nil {
		t.Errorf("Data = %+v, want nil", got.Data)
	}
	if len(got.Env) != 0 {
		t.Errorf("Env = %+v, want []", got.Env)
	}
}

func TestLoad_Version3WithEnv(t *testing.T) {
	path := writeDeploy(t, `{"version":3,"app":"agentctl","container_port":8080,"health_path":"/healthz","env":[{"name":"FOO","secret_ref":"foo.key"}]}`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Env) != 1 {
		t.Fatalf("Env length = %d, want 1", len(got.Env))
	}
	e := got.Env[0]
	if e.Name != "FOO" || e.SecretRef != "foo.key" || e.Required {
		t.Errorf("Env[0] = %+v, want name=FOO secret_ref=foo.key required=false", e)
	}
}

func TestLoad_Version3WithDataAndEnv(t *testing.T) {
	path := writeDeploy(t, `{"version":3,"app":"agentctl","container_port":8080,"health_path":"/healthz","data":{"mount":true,"host_source":"/srv/data","container_path":"/srv"},"env":[{"name":"FOO","secret_ref":"foo.key","required":true}]}`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Data == nil {
		t.Fatalf("Data = nil, want non-nil")
	}
	if got.Data.HostSource != "/srv/data" {
		t.Errorf("HostSource = %q, want /srv/data", got.Data.HostSource)
	}
	if got.Data.ContainerPath != "/srv" {
		t.Errorf("ContainerPath = %q, want /srv", got.Data.ContainerPath)
	}
	if len(got.Env) != 1 || got.Env[0].SecretRef != "foo.key" || !got.Env[0].Required {
		t.Errorf("Env = %+v", got.Env)
	}
}

func TestLoad_RejectsVersion2WithEnv(t *testing.T) {
	path := writeDeploy(t, `{"version":2,"app":"agentctl","container_port":8080,"health_path":"/healthz","env":[{"name":"FOO","secret_ref":"foo.key"}]}`)
	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for v2 with env")
	}
	if !strings.Contains(err.Error(), "version 3") {
		t.Errorf("expected error mentioning version 3, got %q", err.Error())
	}
}

func TestLoad_EnvEntryRejectsEmptyName(t *testing.T) {
	path := writeDeploy(t, `{"version":3,"app":"agentctl","container_port":8080,"health_path":"/healthz","env":[{"name":"","secret_ref":"foo.key"}]}`)
	if _, err := Load(path); err == nil {
		t.Errorf("expected error for empty env name")
	}
}

func TestLoad_EnvEntryRejectsBadSecretRef(t *testing.T) {
	cases := []string{
		"../escape",
		"with/slash",
		".dot",
		"..dotdot",
		"x",
		// Uppercase is rejected by secretRefRe.
		"UPPER",
		// Empty is rejected.
		"",
		// Names longer than 64 chars are rejected.
		strings.Repeat("a", 65),
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			body := fmt.Sprintf(`{"version":3,"app":"agentctl","container_port":8080,"health_path":"/healthz","env":[{"name":"X","secret_ref":%q}]}`, ref)
			path := writeDeploy(t, body)
			if _, err := Load(path); err == nil {
				t.Errorf("expected error for secret_ref %q", ref)
			}
		})
	}
}

func TestLoad_EnvEntryAcceptsValidSecretRefs(t *testing.T) {
	// secretRefRe: ^[a-z0-9_](?:[a-z0-9_.-]{0,62}[a-z0-9_])$
	// Accepts: lowercase letters, digits, _, ., -; 1-64 chars; no leading dot, no double dot.
	cases := []string{
		"ab",
		"abc",
		"foo.key",
		"foo-key",
		"foo_key",
		"a.b.c",
		strings.Repeat("a", 64),
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			body := fmt.Sprintf(`{"version":3,"app":"agentctl","container_port":8080,"health_path":"/healthz","env":[{"name":"X","secret_ref":%q}]}`, ref)
			path := writeDeploy(t, body)
			if _, err := Load(path); err != nil {
				t.Errorf("expected accept %q, got %v", ref, err)
			}
		})
	}
}

func TestValidate_AcceptsVersion3WithoutEnv(t *testing.T) {
	m := &Manifest{
		Version:       3,
		App:           "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
	}
	if err := Validate(m, "agentctl"); err != nil {
		t.Errorf("version 3 without env should validate: %v", err)
	}
}

func TestValidate_AcceptsVersion3WithEnv(t *testing.T) {
	m := &Manifest{
		Version:       3,
		App:           "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
		Env:           []EnvEntry{{Name: "FOO", SecretRef: "foo.key"}},
	}
	if err := Validate(m, "agentctl"); err != nil {
		t.Errorf("version 3 with env should validate: %v", err)
	}
}

func TestValidate_RejectsDuplicateEnvNames(t *testing.T) {
	m := &Manifest{
		Version:       3,
		App:           "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
		Env: []EnvEntry{
			{Name: "FOO", SecretRef: "foo.key"},
			{Name: "FOO", SecretRef: "foo2.key"},
		},
	}
	err := Validate(m, "agentctl")
	if err == nil {
		t.Fatalf("expected error for duplicate env name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected error mentioning duplicate, got %q", err.Error())
	}
}

func TestValidate_RejectsV3WithDangerousContainerPath(t *testing.T) {
	cases := []string{"/", "/proc", "/sys", "/dev", "/run"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			m := &Manifest{
				Version:       3,
				App:           "agentctl",
				ContainerPort: 8080,
				HealthPath:    "/healthz",
				Data:          &ManifestData{Mount: true, HostSource: "/srv/data", ContainerPath: p},
			}
			err := Validate(m, "agentctl")
			if err == nil {
				t.Errorf("expected error for container_path %q", p)
			}
			if !strings.Contains(err.Error(), "container_path") {
				t.Errorf("expected error mentioning container_path, got %q", err.Error())
			}
		})
	}
}

func TestValidate_AcceptsV3WithSafeContainerPath(t *testing.T) {
	m := &Manifest{
		Version:       3,
		App:           "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
		Data:          &ManifestData{Mount: true, HostSource: "/srv/data", ContainerPath: "/srv/app"},
	}
	if err := Validate(m, "agentctl"); err != nil {
		t.Errorf("expected accept /srv/app, got %v", err)
	}
}
