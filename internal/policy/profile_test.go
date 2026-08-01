package policy

import (
	"sort"
	"testing"
)

func TestResolveBuilder(t *testing.T) {
	p, err := Resolve(ProfileBuilder)
	if err != nil {
		t.Fatalf("Resolve(builder): %v", err)
	}
	if p.Name != ProfileBuilder {
		t.Errorf("Name = %q, want %q", p.Name, ProfileBuilder)
	}
	scopes := make([]string, 0, len(p.Permissions))
	for _, perm := range p.Permissions {
		scopes = append(scopes, perm.Scope)
	}
	sort.Strings(scopes)
	want := []string{"checks", "contents", "issues", "metadata", "pull_requests", "statuses"}
	if !equalSorted(scopes, want) {
		t.Errorf("scopes = %v, want %v", scopes, want)
	}
}

func TestResolveUnknown(t *testing.T) {
	if _, err := Resolve("admin"); err == nil {
		t.Errorf("Resolve(admin) succeeded, want error")
	}
	if _, err := Resolve(""); err == nil {
		t.Errorf("Resolve(empty) succeeded, want error")
	}
}

func TestProfileBuilder_MinimumPermissions(t *testing.T) {
	p, err := Resolve(ProfileBuilder)
	if err != nil {
		t.Fatal(err)
	}
	for _, perm := range p.Permissions {
		switch perm.Scope {
		case "metadata":
			if perm.Access != "read" {
				t.Errorf("metadata must be read-only, got %s", perm.Access)
			}
		default:
			if perm.Access != "write" {
				t.Errorf("%s must be write, got %s", perm.Scope, perm.Access)
			}
		}
	}
}

func TestProfileBuilder_NoDangerousScopes(t *testing.T) {
	p, err := Resolve(ProfileBuilder)
	if err != nil {
		t.Fatal(err)
	}
	for _, perm := range p.Permissions {
		switch perm.Scope {
		case "contents", "pull_requests", "issues", "checks", "statuses", "metadata":
			// ok
		default:
			t.Errorf("unexpected scope in builder profile: %s", perm.Scope)
		}
	}
}

func equalSorted(a, b []string) bool {
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
