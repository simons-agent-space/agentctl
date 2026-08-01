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
type AccessTokenRequest struct {
	Repositories []string     `json:"repositories"`
	Permissions  []Permission `json:"permissions"`
}

// Permission is (scope, access) for the access-token endpoint.
type Permission struct {
	Scope  string `json:"scope"`
	Access string `json:"access"`
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
// The returned AccessTokenResponse must be consumed and discarded by the
// caller: the broker returns only the fields it needs to the UDS client.
func (c *Client) MintInstallationToken(
	ctx context.Context,
	jwt string,
	installationID int64,
	req AccessTokenRequest,
) (*AccessTokenResponse, error) {
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
			Body:       string(respBody),
		}
	}

	var out AccessTokenResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &out, nil
}

// APIError is returned when GitHub responds with a non-2xx status. The
// Body field is intentionally not exposed to the UDS caller; we keep it
// on the broker side for debugging only.
type APIError struct {
	StatusCode int
	Message    string
	Body       string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("github api: %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("github api: %d", e.StatusCode)
}
