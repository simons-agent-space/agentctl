package gitbridge

import (
	"sort"
	"testing"
)

func TestPermissionsFor_Builder(t *testing.T) {
	perms, err := PermissionsFor(ProfileBuilder)
	if err != nil {
		t.Fatalf("PermissionsFor(builder): %v", err)
	}
	scopes := make([]string, 0, len(perms))
	for scope := range perms {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	want := []string{"contents", "metadata", "pull_requests"}
	if !equalStrings(scopes, want) {
		t.Errorf("scopes = %v, want %v", scopes, want)
	}
}

func TestPermissionsFor_BuilderIsMinimum(t *testing.T) {
	perms, err := PermissionsFor(ProfileBuilder)
	if err != nil {
		t.Fatal(err)
	}
	if perms["metadata"] != "read" {
		t.Errorf("metadata must be read, got %q", perms["metadata"])
	}
	if perms["contents"] != "write" {
		t.Errorf("contents must be write, got %q", perms["contents"])
	}
	if perms["pull_requests"] != "write" {
		t.Errorf("pull_requests must be write, got %q", perms["pull_requests"])
	}
}

func TestPermissionsFor_NoExcessiveScopes(t *testing.T) {
	perms, err := PermissionsFor(ProfileBuilder)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"checks", "statuses", "issues", "actions",
		"deployments", "packages", "members", "administration",
	}
	for _, scope := range forbidden {
		if _, present := perms[scope]; present {
			t.Errorf("builder profile must not request %q; got: %v", scope, perms)
		}
	}
}

func TestPermissionsFor_Unknown(t *testing.T) {
	if _, err := PermissionsFor("admin"); err == nil {
		t.Errorf("PermissionsFor(admin) succeeded, want error")
	}
	if _, err := PermissionsFor(""); err == nil {
		t.Errorf("PermissionsFor(empty) succeeded, want error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
