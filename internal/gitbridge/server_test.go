package gitbridge

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simons-agent-space/agentctl/internal/audit"
	gh "github.com/simons-agent-space/agentctl/internal/github"
)

// makeTestServer returns a Server whose GitHub API is the supplied
// httptest handler, plus a buffer of the audit log.
func makeTestServer(t *testing.T, ghHandler http.HandlerFunc) (*Server, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(keyPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	ghServer := httptest.NewServer(ghHandler)
	t.Cleanup(ghServer.Close)
	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: keyPath,
		AllowedOrg:     "simons-agent-space",
		AllowedRepos:   []string{"agentctl"},
		SocketPath:     filepath.Join(runDir, "gitbridge.sock"),
	}
	ghClient := gh.NewClientWithBase(ghServer.URL)
	var logBuf bytes.Buffer
	log := audit.New(&logBuf)
	srv, err := NewServer(cfg, ghClient, log)
	if err != nil {
		t.Fatal(err)
	}
	return srv, &logBuf
}

func TestServer_Healthz(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called for healthz")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.handleHealthz(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestServer_RejectsBadMethod(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/token", strings.NewReader(""))
	srv.handleToken(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestServer_RejectsBadJSON(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("not json"))
	srv.handleToken(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestServer_RejectsRepoNotAllowed(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called for disallowed repo")
	})
	body := strings.NewReader(`{"repo":"other-org/foo","profile":"builder"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	var er errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Code != "REPO_NOT_ALLOWED" {
		t.Errorf("code = %q", er.Code)
	}
}

func TestServer_RejectsWrongProfile(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called for disallowed profile")
	})
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"admin"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	var er errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Code != "PROFILE_NOT_ALLOWED" {
		t.Errorf("code = %q", er.Code)
	}
}

func TestServer_MintsToken(t *testing.T) {
	expires := time.Now().Add(1 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("missing Bearer prefix: %q", auth)
		}
		var req gh.AccessTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(req.Repositories) != 1 || req.Repositories[0] != "simons-agent-space/agentctl" {
			t.Errorf("repositories = %v", req.Repositories)
		}
		if len(req.Permissions) == 0 {
			t.Errorf("no permissions")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_test_token",
			"expires_at": expires,
		})
	}
	srv, logBuf := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out response
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Token != "ghs_test_token" {
		t.Errorf("token = %q", out.Token)
	}
	if out.ExpiresAt == "" {
		t.Errorf("expires_at empty")
	}
	if !strings.Contains(logBuf.String(), `"op":"minted"`) {
		t.Errorf("minted event missing in log: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "ghs_test_token") {
		t.Errorf("token leaked into audit log: %s", logBuf.String())
	}
}

func TestServer_GitHubErrorPropagates(t *testing.T) {
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"server is on fire"}`)
	}
	srv, _ := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestServer_LogsRejection(t *testing.T) {
	srv, logBuf := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called")
	})
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"admin"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if !strings.Contains(logBuf.String(), `"op":"rejected"`) {
		t.Errorf("rejected event missing in log: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"reason"`) {
		t.Errorf("reason missing in rejection log: %s", logBuf.String())
	}
}
