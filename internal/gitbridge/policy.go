// Package gitbridge defines the fixed permission profile that the broker
// will mint tokens for. The broker only ever mints tokens with this
// profile; arbitrary permissions are not accepted from the caller.
// This keeps the principle of least privilege at the API surface.
//
// The profile's permissions must align with what the GitHub App is
// actually granted and what the calling agent needs to do its job.
// Adding a permission here widens what every minted token can do.
package gitbridge

import "fmt"

// Name is the typed identifier of a permission profile.
type Name string

// Supported profile names.
const (
	// ProfileBuilder is the only fixed profile accepted by gitbridge.
	// It grants the minimum permissions required to push code and open
	// pull requests. Issue filing, checks, statuses, and other
	// non-essential scopes are deliberately omitted: an agent that
	// needs more should justify each scope explicitly rather than
	// inheriting a generous default.
	ProfileBuilder Name = "builder"
)

// PermissionsFor returns the permissions map for the given profile
// name, suitable for serialising directly into the GitHub access-tokens
// request body. Unknown profile names return an error so the broker
// can reject the request before contacting GitHub.
func PermissionsFor(name Name) (map[string]string, error) {
	switch name {
	case ProfileBuilder:
		return map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"metadata":      "read",
		}, nil
	default:
		return nil, fmt.Errorf("unknown profile: %q", name)
	}
}
