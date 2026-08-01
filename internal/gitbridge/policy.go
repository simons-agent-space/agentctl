// Package gitbridge defines the fixed permission profile that the broker
// will mint tokens for. The broker only ever mints tokens with this
// profile; arbitrary permissions are not accepted from the caller.
// This keeps the principle of least privilege at the API surface.
package gitbridge

import "fmt"

// Name is the typed identifier of a permission profile.
type Name string

// Supported profile names.
const (
	// ProfileBuilder is the only fixed profile accepted by gitbridge.
	// It grants the minimum permissions required to push code, open
	// pull requests, file issues, and report build status.
	ProfileBuilder Name = "builder"
)

// Permission is a single (scope, level) pair. The level is one of the
// strings documented at https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app
type permissionEntry struct {
	Scope  string `json:"scope"`
	Access string `json:"access"`
}

// Profile is the resolved set of permissions for a request.
type Profile struct {
	Name        Name
	Permissions []permissionEntry
}

// all returns the static set of permissions for a known profile name.
// Unknown names return an error so the broker can reject the request.
func all(name Name) (Profile, error) {
	switch name {
	case ProfileBuilder:
		return Profile{
			Name: ProfileBuilder,
			Permissions: []permissionEntry{
				{Scope: "contents", Access: "write"},
				{Scope: "pull_requests", Access: "write"},
				{Scope: "issues", Access: "write"},
				{Scope: "checks", Access: "write"},
				{Scope: "statuses", Access: "write"},
				{Scope: "metadata", Access: "read"},
			},
		}, nil
	default:
		return Profile{}, fmt.Errorf("unknown profile: %q", name)
	}
}

// Resolve returns the canonical Profile for name. It is the single entry
// point used by the broker when validating a request.
func Resolve(name Name) (Profile, error) {
	return all(name)
}
