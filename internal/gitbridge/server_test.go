package gitbridge

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
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
	ghClient := NewClientWithBase(ghServer.URL)
	var logBuf bytes.Buffer
	log := audit.New(&logBuf)
	srv, err := NewServer(cfg, ghClient, log)
	if err != nil {
		t.Fatal(err)
	}
	return srv, &logBuf
}

// ----------------------------------------------------------------------
// End-to-end server tests (with mocked GitHub API)
// ----------------------------------------------------------------------

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
		var req AccessTokenRequest
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

// ----------------------------------------------------------------------
// JWT key loading and minting tests
// ----------------------------------------------------------------------

func TestLoadPrivateKeyPKCS1(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := writeTempKey(t, pemBytes)
	t.Cleanup(func() { _ = os.Remove(path) })

	loaded, err := loadPrivateKey(path)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	if loaded.D.Cmp(key.D) != 0 {
		t.Errorf("loaded key D does not match original")
	}
}

func TestLoadPrivateKeyPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	path := writeTempKey(t, pemBytes)
	t.Cleanup(func() { _ = os.Remove(path) })

	if _, err := loadPrivateKey(path); err != nil {
		t.Fatalf("loadPrivateKey PKCS#8: %v", err)
	}
}

func TestLoadPrivateKeyBadPEM(t *testing.T) {
	path := writeTempKey(t, []byte("not a pem file"))
	t.Cleanup(func() { _ = os.Remove(path) })
	if _, err := loadPrivateKey(path); err == nil {
		t.Errorf("expected error for bad PEM")
	}
}

func TestLoadPrivateKeyMissing(t *testing.T) {
	if _, err := loadPrivateKey("/nonexistent/key.pem"); err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestMintJWT_StructureAndTiming(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := mintJWT(key, 12345)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 parts, got %d", len(parts))
	}
	hdr, pl := decodeJWTForTest(t, tok)
	if hdr["alg"] != "RS256" {
		t.Errorf("alg = %v, want RS256", hdr["alg"])
	}
	if hdr["typ"] != "JWT" {
		t.Errorf("typ = %v, want JWT", hdr["typ"])
	}
	iat := int64(pl["iat"].(float64))
	exp := int64(pl["exp"].(float64))
	iss := int64(pl["iss"].(float64))
	now := time.Now().Unix()
	if iat > now {
		t.Errorf("iat in future: %d > %d", iat, now)
	}
	if exp <= now {
		t.Errorf("exp not in future: %d <= %d", exp, now)
	}
	if exp-iat < 300 {
		t.Errorf("exp-iat too small: %d", exp-iat)
	}
	if exp-iat > 600 {
		t.Errorf("exp-iat too large: %d", exp-iat)
	}
	if iss != 12345 {
		t.Errorf("iss = %d, want 12345", iss)
	}
}

func TestMintJWT_NeverContainsKeyMaterial(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := mintJWT(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tok, "BEGIN") || strings.Contains(tok, "PRIVATE") {
		t.Errorf("JWT string looks like it contains key material: %s", tok)
	}
}

// ----------------------------------------------------------------------
// Request decoding and authorisation tests
// ----------------------------------------------------------------------

func TestDecodeRequest_OK(t *testing.T) {
	body := `{"repo":"simons-agent-space/agentctl","profile":"builder"}`
	repo, prof, verr := decodeRequest(strings.NewReader(body))
	if verr != nil {
		t.Fatalf("decodeRequest: %v", verr)
	}
	if repo != "simons-agent-space/agentctl" {
		t.Errorf("repo = %q", repo)
	}
	if prof != ProfileBuilder {
		t.Errorf("profile = %q", prof)
	}
}

func TestDecodeRequest_BadJSON(t *testing.T) {
	_, _, verr := decodeRequest(strings.NewReader("not json"))
	if verr == nil {
		t.Errorf("expected error for bad JSON")
	}
}

func TestDecodeRequest_MissingRepo(t *testing.T) {
	body := `{"profile":"builder"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil || verr.Code != "BAD_REQUEST" {
		t.Errorf("expected BAD_REQUEST, got %v", verr)
	}
}

func TestDecodeRequest_MissingProfile(t *testing.T) {
	body := `{"repo":"x/y"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil || verr.Code != "BAD_REQUEST" {
		t.Errorf("expected BAD_REQUEST, got %v", verr)
	}
}

func TestDecodeRequest_UnknownField(t *testing.T) {
	body := `{"repo":"x/y","profile":"builder","sneaky":"value"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil {
		t.Errorf("expected error for unknown field")
	}
}

func TestAuthorize_OK(t *testing.T) {
	c := validConfig()
	_, verr := c.authorize("simons-agent-space/agentctl", ProfileBuilder)
	if verr != nil {
		t.Errorf("authorize: %v", verr)
	}
}

func TestAuthorize_NotBuilder(t *testing.T) {
	c := validConfig()
	_, verr := c.authorize("simons-agent-space/agentctl", "admin")
	if verr == nil || verr.Code != "PROFILE_NOT_ALLOWED" {
		t.Errorf("expected PROFILE_NOT_ALLOWED, got %v", verr)
	}
}

func TestAuthorize_RepoNotAllowed(t *testing.T) {
	c := validConfig()
	_, verr := c.authorize("simons-agent-space/other", ProfileBuilder)
	if verr == nil || verr.Code != "REPO_NOT_ALLOWED" {
		t.Errorf("expected REPO_NOT_ALLOWED, got %v", verr)
	}
}

func TestAuthorize_WrongOrg(t *testing.T) {
	c := validConfig()
	_, verr := c.authorize("other-org/agentctl", ProfileBuilder)
	if verr == nil || verr.Code != "REPO_NOT_ALLOWED" {
		t.Errorf("expected REPO_NOT_ALLOWED, got %v", verr)
	}
}

// ----------------------------------------------------------------------
// shared test helpers
// ----------------------------------------------------------------------

func writeTempKey(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "key-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func decodeJWTForTest(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	plJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	if err := json.Unmarshal(plJSON, &pl); err != nil {
		t.Fatal(err)
	}
	return hdr, pl
}
