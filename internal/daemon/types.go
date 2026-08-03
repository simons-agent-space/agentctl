package daemon

import (
	"encoding/json"
	"time"

	"github.com/simons-agent-space/agentctl/internal/deploy"
)

// InspectRequest validates a deploy.json manifest and a commit SHA
// against the app-name and SHA rules without performing any side
// effect. The commit is also resolved against the trusted source
// mirror to confirm it is reachable from origin/main, so callers
// can use this endpoint to pre-flight a deploy proposal before
// approval.
type InspectRequest struct {
	// App is the repository short name. Must match the app-name
	// regex and must equal Manifest.App.
	App string `json:"app"`
	// Commit is exactly 40 lowercase hex characters.
	Commit string `json:"commit"`
	// Manifest is the raw deploy.json body. The daemon decodes
	// it with DisallowUnknownFields and rejects trailing data,
	// matching the Load rules in the deploy package.
	Manifest json.RawMessage `json:"manifest"`
}

// InspectResponse describes the result of an inspect call. Valid
// is true when every check passed; Errors lists the validation
// failures when Valid is false. SourceReachable is true when the
// commit was found on origin/main of the trusted mirror. Current
// and Previous summarize the existing deployment state when the
// daemon has a state file for App; they are nil when App is not
// yet managed.
type InspectResponse struct {
	App             string             `json:"app"`
	Commit          string             `json:"commit"`
	ManifestVersion int                `json:"manifest_version"`
	Valid           bool               `json:"valid"`
	SourceReachable bool               `json:"source_reachable"`
	Current         *DeploymentSummary `json:"current,omitempty"`
	Previous        *DeploymentSummary `json:"previous,omitempty"`
	Errors          []string           `json:"errors,omitempty"`
}

// DeployRequest triggers a deployment of an approved commit. The
// manifest is parsed strictly (unknown fields and trailing data
// rejected) and validated before any side effect.
type DeployRequest struct {
	// Commit is exactly 40 lowercase hex characters.
	Commit string `json:"commit"`
	// Manifest is the raw deploy.json body.
	Manifest json.RawMessage `json:"manifest"`
}

// DeployResponse is the success payload of a deployment. It
// mirrors the deploy.DeployResult fields the caller needs to
// confirm the live deployment. Warnings carries non-fatal
// post-commit cleanup issues that did not abort the deployment.
type DeployResponse struct {
	App           string    `json:"app"`
	Commit        string    `json:"commit"`
	Image         string    `json:"image"`
	ContainerName string    `json:"container_name"`
	HostPort      int       `json:"host_port"`
	ContainerPort int       `json:"container_port"`
	Hostname      string    `json:"hostname"`
	Upstream      string    `json:"upstream"`
	DeployedAt    time.Time `json:"deployed_at"`
	Warnings      []string  `json:"warnings,omitempty"`
}

// RollbackRequest rolls an app back to its previous deployment.
// HealthPath is required because the persisted deployment state
// does not include the manifest health path (the health path is a
// manifest value, not a deployment identity value). The daemon
// validates HealthPath against the manifest health-path rules
// before any side effect.
type RollbackRequest struct {
	HealthPath string `json:"health_path"`
}

// RollbackResponse is the success payload of a rollback. It
// mirrors the swapped Current deployment so the caller can
// confirm the result.
type RollbackResponse struct {
	App           string    `json:"app"`
	Commit        string    `json:"commit"`
	Image         string    `json:"image"`
	ContainerName string    `json:"container_name"`
	HostPort      int       `json:"host_port"`
	ContainerPort int       `json:"container_port"`
	Hostname      string    `json:"hostname"`
	Upstream      string    `json:"upstream"`
	DeployedAt    time.Time `json:"deployed_at"`
}

// DeploymentSummary is the daemon-side projection of a persisted
// deployment. It contains only the fields a UDS caller needs to
// reason about a managed app's state. Fields are copied
// explicitly; the daemon never exposes the full deploy.Deployment
// struct, which keeps the wire format stable across deploy-layer
// schema additions.
type DeploymentSummary struct {
	App           string    `json:"app"`
	Commit        string    `json:"commit"`
	Image         string    `json:"image"`
	ContainerName string    `json:"container_name"`
	HostPort      int       `json:"host_port"`
	ContainerPort int       `json:"container_port"`
	Hostname      string    `json:"hostname"`
	Upstream      string    `json:"upstream"`
	DeployedAt    time.Time `json:"deployed_at"`
	MountData     bool      `json:"mount_data"`
	DataReadOnly  bool      `json:"data_read_only,omitempty"`
}

// StateResponse returns the current and previous deployment for a
// managed app. Either or both may be present: a first deployment
// has only Current; an app that has been deployed exactly once
// has no Previous and rollback will fail with ErrNoPreviousDeployment.
type StateResponse struct {
	App      string             `json:"app"`
	Current  *DeploymentSummary `json:"current"`
	Previous *DeploymentSummary `json:"previous,omitempty"`
}

// ListAppsResponse is the response for GET /v1/apps.
type ListAppsResponse struct {
	Apps []string `json:"apps"`
}

// StatusResponse describes the live state of a managed app. The
// container_status field is the result of `docker inspect --format
// {{.State.Running}}` mapped to one of "running", "stopped",
// "absent", or "unknown"; absent when the state file has no
// Current slot. status_check_error is set when the container
// inspect failed for any reason other than "no such container";
// it is omitted when the status was determined cleanly.
type StatusResponse struct {
	App              string             `json:"app"`
	Current          *DeploymentSummary `json:"current,omitempty"`
	Previous         *DeploymentSummary `json:"previous,omitempty"`
	ContainerStatus  string             `json:"container_status,omitempty"`
	StatusCheckError string             `json:"status_check_error,omitempty"`
}

// ErrorResponse is the wire-level error body returned for every
// non-2xx response. Error is a human-readable description, Code
// is a stable string the caller can branch on without parsing
// Error, and Sentinels lists every error sentinel in the chain
// that matches errors.Is (useful for callers that want to
// distinguish "no previous deployment" from "app not managed"
// without re-implementing the deploy-layer sentinel tree).
type ErrorResponse struct {
	Error     string   `json:"error"`
	Code      string   `json:"code"`
	Sentinels []string `json:"sentinels,omitempty"`
}

// summaryFromDeployment projects a deploy.Deployment into a
// DeploymentSummary. Used by every read endpoint.
func summaryFromDeployment(d *deploy.Deployment) *DeploymentSummary {
	if d == nil {
		return nil
	}
	return &DeploymentSummary{
		App:           d.App,
		Commit:        d.Commit,
		Image:         d.Image,
		ContainerName: d.ContainerName,
		HostPort:      d.HostPort,
		ContainerPort: d.ContainerPort,
		Hostname:      d.Hostname,
		Upstream:      d.Upstream,
		DeployedAt:    d.DeployedAt,
		MountData:     d.MountData,
		DataReadOnly:  d.DataReadOnly,
	}
}
