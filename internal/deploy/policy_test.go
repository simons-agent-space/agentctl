package deploy

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateDataMount_NilManifest(t *testing.T) {
	if err := ValidateDataMount(nil, DataConfig{}); err != nil {
		t.Errorf("expected nil for nil manifest, got %v", err)
	}
}

func TestValidateDataMount_NilData(t *testing.T) {
	m := &Manifest{Version: 2, App: "agentctl", ContainerPort: 8080, HealthPath: "/healthz"}
	if err := ValidateDataMount(m, DataConfig{}); err != nil {
		t.Errorf("expected nil for nil Data, got %v", err)
	}
}

func TestValidateDataMount_MountFalse(t *testing.T) {
	m := &Manifest{Version: 2, App: "agentctl", ContainerPort: 8080, HealthPath: "/healthz",
		Data: &ManifestData{Mount: false}}
	if err := ValidateDataMount(m, DataConfig{}); err != nil {
		t.Errorf("expected nil for mount=false, got %v", err)
	}
}

func TestValidateDataMount_LegacyMountNoHostSource(t *testing.T) {
	// Legacy app-owned mount: no allowlist check is needed.
	m := &Manifest{Version: 2, App: "agentctl", ContainerPort: 8080, HealthPath: "/healthz",
		Data: &ManifestData{Mount: true, ReadOnly: true}}
	if err := ValidateDataMount(m, DataConfig{}); err != nil {
		t.Errorf("expected nil for legacy mount, got %v", err)
	}
}

func TestValidateDataMount_ApprovedHostSource(t *testing.T) {
	m := &Manifest{Version: 3, App: "agentctl", ContainerPort: 8080, HealthPath: "/healthz",
		Data: &ManifestData{Mount: true, HostSource: "/srv/data"}}
	cfg := DataConfig{HostSourceAllowlist: []string{"/srv/data"}}
	if err := ValidateDataMount(m, cfg); err != nil {
		t.Errorf("expected nil for approved host_source, got %v", err)
	}
}

func TestValidateDataMount_UnapprovedHostSource(t *testing.T) {
	m := &Manifest{Version: 3, App: "agentctl", ContainerPort: 8080, HealthPath: "/healthz",
		Data: &ManifestData{Mount: true, HostSource: "/srv/data"}}
	cfg := DataConfig{HostSourceAllowlist: []string{"/srv/other"}}
	err := ValidateDataMount(m, cfg)
	if !errors.Is(err, ErrHostSourceDenied) {
		t.Errorf("expected ErrHostSourceDenied, got %v", err)
	}
}

func TestValidateDataMount_EmptyAllowlistDeniesAll(t *testing.T) {
	m := &Manifest{Version: 3, App: "agentctl", ContainerPort: 8080, HealthPath: "/healthz",
		Data: &ManifestData{Mount: true, HostSource: "/srv/data"}}
	err := ValidateDataMount(m, DataConfig{})
	if !errors.Is(err, ErrHostSourceDenied) {
		t.Errorf("expected ErrHostSourceDenied for empty allowlist, got %v", err)
	}
}

func TestValidateDataMount_NonAbsoluteHostSource(t *testing.T) {
	m := &Manifest{Version: 3, App: "agentctl", ContainerPort: 8080, HealthPath: "/healthz",
		Data: &ManifestData{Mount: true, HostSource: "relative/path"}}
	cfg := DataConfig{HostSourceAllowlist: []string{"/srv/data"}}
	err := ValidateDataMount(m, cfg)
	if err == nil {
		t.Fatalf("expected error for non-absolute host_source")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("expected error mentioning absolute, got %q", err.Error())
	}
}

func TestValidate_EnvEntriesAcceptedOnV3(t *testing.T) {
	m := &Manifest{
		Version:       3,
		App:           "agentctl",
		Repository:    "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
		Env: []EnvEntry{
			{Name: "FOO", SecretRef: "foo.key", Required: true},
			{Name: "BAR", SecretRef: "bar.key"},
		},
	}
	if err := Validate(m); err != nil {
		t.Errorf("v3 with env: %v", err)
	}
}

func TestValidate_EnvNameUnique(t *testing.T) {
	m := &Manifest{
		Version:       3,
		App:           "agentctl",
		Repository:    "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
		Env: []EnvEntry{
			{Name: "FOO", SecretRef: "foo.key"},
			{Name: "FOO", SecretRef: "foo2.key"},
		},
	}
	err := Validate(m)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate name error, got %q", err.Error())
	}
}

func TestValidate_V3HostSourceMountAccepted(t *testing.T) {
	m := &Manifest{
		Version:       3,
		App:           "agentctl",
		Repository:    "agentctl",
		ContainerPort: 8080,
		HealthPath:    "/healthz",
		Data:          &ManifestData{Mount: true, HostSource: "/srv/data"},
	}
	if err := Validate(m); err != nil {
		t.Errorf("v3 host_source: %v", err)
	}
}
