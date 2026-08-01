package gitbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/simons-agent-space/agentctl/internal/policy"
)

// request is the JSON body the UDS client sends.
type request struct {
	Repo    string `json:"repo"`
	Profile string `json:"profile"`
}

// response is the JSON body the broker returns on success. The token
// is the GitHub installation token. ExpiresAt is RFC3339.
type response struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Repo      string `json:"repository"`
	Profile   string `json:"profile"`
}

// errorResponse is the body returned for any failure. Code is a stable
// identifier suitable for the client to switch on. Message is a short
// human-readable hint and never contains secret material.
type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// validationError collects the failure reasons for a request.
type validationError struct {
	Code   string
	Reason string
}

func (v *validationError) Error() string {
	return fmt.Sprintf("%s: %s", v.Code, v.Reason)
}

// decodeRequest parses and validates the JSON request body. The returned
// profile is always ProfileBuilder when err is nil; any other profile
// name is rejected at the policy layer.
func decodeRequest(r io.Reader) (string, policy.Name, *validationError) {
	var req request
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return "", "", &validationError{Code: "BAD_REQUEST", Reason: "invalid JSON body"}
	}
	if req.Repo == "" {
		return "", "", &validationError{Code: "BAD_REQUEST", Reason: "repo is required"}
	}
	if req.Profile == "" {
		return "", "", &validationError{Code: "BAD_REQUEST", Reason: "profile is required"}
	}
	return req.Repo, policy.Name(req.Profile), nil
}

// authorize checks the request against the configured allowlist and
// returns the resolved Profile or a validation error.
func (c *Config) authorize(repoSlug string, prof policy.Name) (policy.Profile, *validationError) {
	if prof != policy.ProfileBuilder {
		return policy.Profile{}, &validationError{
			Code:   "PROFILE_NOT_ALLOWED",
			Reason: fmt.Sprintf("only %q is accepted", policy.ProfileBuilder),
		}
	}
	if !c.IsAllowed(repoSlug) {
		return policy.Profile{}, &validationError{
			Code:   "REPO_NOT_ALLOWED",
			Reason: "repository not in allowlist or wrong organisation",
		}
	}
	profile, err := policy.Resolve(prof)
	if err != nil {
		return policy.Profile{}, &validationError{
			Code:   "PROFILE_NOT_ALLOWED",
			Reason: err.Error(),
		}
	}
	return profile, nil
}

// writeError serialises err as an errorResponse with the appropriate HTTP
// status. It does not log; the caller is responsible for audit logging.
func writeError(w http.ResponseWriter, err *validationError) {
	status := http.StatusBadRequest
	switch err.Code {
	case "REPO_NOT_ALLOWED", "PROFILE_NOT_ALLOWED":
		status = http.StatusForbidden
	case "INTERNAL":
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: err.Reason, Code: err.Code})
}

// errInternal wraps an arbitrary error as a validationError with the
// INTERNAL code. The caller is expected to log the underlying error via
// the audit logger first so the original message is preserved.
func errInternal(err error) *validationError {
	var ve *validationError
	if errors.As(err, &ve) {
		return ve
	}
	return &validationError{Code: "INTERNAL", Reason: err.Error()}
}
