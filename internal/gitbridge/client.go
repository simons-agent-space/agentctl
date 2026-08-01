package gitbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	apiBase = "https://api.github.com"
	apiVer  = "2022-11-28"
	ua      = "gitbridge/0.1 (+https://github.com/simons-agent-space/agentctl)"
)

// AccessTokenRequest is the body of POST /app/installations/{id}/access_tokens.
//
// GitHub's contract for this endpoint:
//   - "repositories" is an array of *repository names* (e.g. "agentctl"),
//     NOT full "org/name" slugs.
//   - "permissions" is a JSON object mapping permission name -> access
//     level (e.g. {"contents": "write"}). An array of {scope,access}
//     objects is rejected by the API.
type AccessTokenRequest struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

// AccessTokenResponse is what GitHub returns. The Token field is the
// short-lived installation token; ExpiresAt is when it stops working.
type AccessTokenResponse struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Repositories        []Repo            `json:"repositories,omitempty"`
	RepositorySelection string            `json:"repository_selection,omitempty"`
	Permissions         map[string]string `json:"permissions,omitempty"`
}

// Repo is the trimmed shape we care about from the response.
type Repo struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
}

// ErrorResponse is the GitHub error body shape.
type ErrorResponse struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url,omitempty"`
}

// Client is a thin HTTP client. It does not retain any state between
// requests beyond the http.Client itself.
type Client struct {
	httpClient *http.Client
	base       string
}

// NewClient returns a Client configured for the production GitHub API.
func NewClient() *Client {
	return NewClientWithBase(apiBase)
}

// NewClientWithBase returns a Client that targets the given base URL.
// It exists for tests that point the broker at an httptest.Server.
func NewClientWithBase(base string) *Client {
	if base == "" {
		base = apiBase
	}
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		base:       base,
	}
}

// MintInstallationToken calls POST /app/installations/{installation_id}/access_tokens.
// jwt is a freshly-minted GitHub App JWT (not an installation token).
// repoName is the short repository name (e.g. "agentctl"); the caller
// must have already validated the full "org/name" slug and extracted
// the name. The returned AccessTokenResponse must be consumed and
// discarded by the caller: the broker returns only the fields it needs
// to the UDS client.
func (c *Client) MintInstallationToken(
	ctx context.Context,
	jwt string,
	installationID int64,
	repoName string,
	permissions map[string]string,
) (*AccessTokenResponse, error) {
	req := AccessTokenRequest{
		Repositories: []string{repoName},
		Permissions:  permissions,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal token request: %w", err)
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.base, installationID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+jwt)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", apiVer)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", ua)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send token request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var er ErrorResponse
		_ = json.Unmarshal(respBody, &er) // best-effort
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Message:    er.Message,
		}
	}

	var out AccessTokenResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if out.Token == "" {
		return nil, &APIError{StatusCode: resp.StatusCode, Message: "missing or empty token in response"}
	}
	if out.ExpiresAt.IsZero() {
		return nil, &APIError{StatusCode: resp.StatusCode, Message: "missing expires_at in response"}
	}
	if !out.ExpiresAt.After(time.Now()) {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("expires_at %v is not in the future", out.ExpiresAt),
		}
	}
	return &out, nil
}

// APIError is returned when GitHub responds with a non-2xx status. The
// StatusCode and Message are kept for broker-side logging; the broker
// returns a generic INTERNAL error to the UDS caller.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("github api: %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("github api: %d", e.StatusCode)
}
